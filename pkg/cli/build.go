package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"chainguard.dev/apko/pkg/build/types"
	"chainguard.dev/melange/pkg/build"
	"chainguard.dev/melange/pkg/config"
	"chainguard.dev/melange/pkg/container"
	"chainguard.dev/melange/pkg/container/docker"
	"github.com/chainguard-dev/clog"
	"github.com/skratchdot/open-golang/open"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"

	"github.com/wolfi-dev/wolfictl/pkg/dag"
)

func cmdBuild() *cobra.Command {
	var archs []string
	var dir, pipelineDir, runner string
	var jobs int
	var dryrun bool
	var remove bool
	var web bool
	var extraKeys, extraRepos []string
	var traceFile string

	// TODO: buildworld bool (build deps vs get them from package repo)
	// TODO: builddownstream bool (build things that depend on listed packages)
	cmd := &cobra.Command{
		Use:           "build",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			log := clog.FromContext(ctx)

			if traceFile != "" {
				w, err := os.Create(traceFile)
				if err != nil {
					return fmt.Errorf("creating trace file: %w", err)
				}
				defer w.Close()
				exporter, err := stdouttrace.New(stdouttrace.WithWriter(w))
				if err != nil {
					return fmt.Errorf("creating stdout exporter: %w", err)
				}
				tp := trace.NewTracerProvider(trace.WithBatcher(exporter))
				otel.SetTracerProvider(tp)

				defer tp.Shutdown(context.WithoutCancel(ctx))

				tctx, span := otel.Tracer("wolfictl").Start(ctx, "build")
				defer span.End()
				ctx = tctx
			}

			tmpdir, err := os.MkdirTemp("", "wolfictl-build-logs")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmpdir)

			if jobs == 0 {
				jobs = runtime.GOMAXPROCS(0)
			}
			jobch := make(chan struct{}, jobs)

			if pipelineDir == "" {
				pipelineDir = filepath.Join(dir, "pipelines")
			}

			var h *handler
			if web {
				h = &handler{
					tasks: map[string]*task{},
				}
			}

			newTask := func(pkg string) *task {
				// TODO: Something with ctx.
				return &task{
					pkg:         pkg,
					dir:         dir,
					pipelineDir: pipelineDir,
					runner:      runner,
					archs:       archs,
					dryrun:      dryrun,
					remove:      remove,
					jobch:       jobch,
					cond:        sync.NewCond(&sync.Mutex{}),
					deps:        map[string]*task{},
					h:           h,
					tmpdir:      tmpdir,
				}
			}

			pkgs, err := dag.NewPackages(ctx, os.DirFS(dir), dir, pipelineDir)
			if err != nil {
				return err
			}
			g, err := dag.NewGraph(ctx, pkgs,
				dag.WithKeys(extraKeys...),
				dag.WithRepos(extraRepos...))
			if err != nil {
				return err
			}

			// Only return local packages
			g, err = g.Filter(dag.FilterLocal())
			if err != nil {
				return err
			}

			// Only return main packages (configs)
			g, err = g.Filter(dag.OnlyMainPackages(pkgs))
			if err != nil {
				return err
			}

			m, err := g.Graph.AdjacencyMap()
			if err != nil {
				return err
			}

			tasks := map[string]*task{}
			for _, pkg := range g.Packages() {
				if tasks[pkg] == nil {
					tasks[pkg] = newTask(pkg)
				}
				for k, v := range m {
					// The package list is in the form of "pkg:version",
					// but we only care about the package name.
					if strings.HasPrefix(k, pkg+":") {
						for _, dep := range v {
							d, _, _ := strings.Cut(dep.Target, ":")

							if tasks[d] == nil {
								tasks[d] = newTask(d)
							}
							tasks[pkg].deps[d] = tasks[d]
						}
					}
				}
			}

			if len(tasks) == 0 {
				return fmt.Errorf("no packages to build")
			}

			sorted, err := g.ReverseSorted()
			if err != nil {
				return err
			}

			if got, want := len(tasks), len(sorted); got != want {
				return fmt.Errorf("tasks(%d) != sorted(%d)", got, want)
			}

			todos := args
			if len(todos) == 0 {
				for _, todo := range sorted {
					todos = append(todos, todo.Name())
				}
			}

			for _, todo := range todos {
				t := tasks[todo]
				t.maybeStart(ctx)
			}

			count := len(tasks)

			var eg errgroup.Group

			if web {
				http.Handle("/", h)

				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					return err
				}

				server := &http.Server{
					Addr:              l.Addr().String(),
					ReadHeaderTimeout: 3 * time.Second,
				}

				log.Infof("%s", l.Addr().String())

				eg.Go(func() error {
					server.Serve(l)
					return nil
				})

				eg.Go(func() error {
					open.Run(fmt.Sprintf("http://localhost:%d", l.Addr().(*net.TCPAddr).Port))
					return nil
				})

				eg.Go(func() error {
					<-ctx.Done()
					server.Close()
					return nil
				})
			}

			eg.Go(func() error {
				errs := []error{}
				for _, todo := range todos {
					t := tasks[todo]
					log.Infof("%s status: %q", t.pkg, t.status)
					if err := t.wait(); err != nil {
						errs = append(errs, fmt.Errorf("failed to build %s: %w", t.pkg, err))
						log.Infof("Failed to build %s (%d failures)", t.pkg, len(errs))
						continue
					}
					delete(tasks, t.pkg)
					log.Infof("Finished building %s (%d/%d)", t.pkg, count-len(tasks), count)
				}
				return errors.Join(errs...)
			})

			return eg.Wait()
		},
	}

	cmd.Flags().StringVarP(&dir, "dir", "d", ".", "directory to search for melange configs")
	cmd.Flags().StringVar(&pipelineDir, "pipeline-dir", "", "directory used to extend defined built-in pipelines")
	cmd.Flags().StringVar(&runner, "runner", "docker", "which runner to use to enable running commands, default is based on your platform.")
	cmd.Flags().IntVarP(&jobs, "jobs", "j", 0, "number of jobs to run concurrently (default is GOMAXPROCS)")
	cmd.Flags().StringSliceVar(&archs, "arch", []string{"x86_64", "aarch64"}, "arch of package to build")
	cmd.Flags().BoolVar(&dryrun, "dry-run", false, "print commands instead of executing them")
	cmd.Flags().BoolVar(&remove, "rm", false, "clean up temporary artifacts")
	cmd.Flags().StringSliceVarP(&extraKeys, "keyring-append", "k", []string{"https://packages.wolfi.dev/os/wolfi-signing.rsa.pub"}, "path to extra keys to include in the build environment keyring")
	cmd.Flags().StringSliceVarP(&extraRepos, "repository-append", "r", []string{"https://packages.wolfi.dev/os"}, "path to extra repositories to include in the build environment")
	cmd.Flags().StringVar(&traceFile, "trace", "", "where to write trace output")
	cmd.Flags().BoolVar(&web, "web", false, "launch a browser")
	return cmd
}

type task struct {
	pkg, dir, pipelineDir, runner string

	archs  []string
	dryrun bool
	remove bool

	err  error
	deps map[string]*task

	jobch  chan struct{}
	status string

	started bool
	done    bool

	cond *sync.Cond

	h *handler

	tmpdir string
}

func (t *task) start(ctx context.Context) {
	defer func() {
		t.cond.L.Lock()
		clog.FromContext(ctx).Infof("finished %q, err=%v", t.pkg, t.err)
		t.status = "done"
		t.done = true
		t.cond.Broadcast()
		t.cond.L.Unlock()
	}()

	for _, dep := range t.deps {
		dep.maybeStart(ctx)
	}

	log := clog.New(clog.FromContext(ctx).Handler()).With("package", t.pkg)
	ctx = clog.WithLogger(ctx, log)

	for depname, dep := range t.deps {
		t.status = "waiting on " + depname
		if err := dep.wait(); err != nil {
			t.err = err
			return
		}
	}

	t.status = "waiting for semaphore"

	// Block on jobch, to limit concurrency. Remove from jobch when done.
	t.jobch <- struct{}{}
	defer func() { <-t.jobch }()

	clog.FromContext(ctx).Infof("starting %q", t.pkg)
	t.status = "running"

	// all deps are done and we're clear to launch.
	t.err = t.do(ctx)
}

func (t *task) logfile() string {
	return filepath.Join(t.tmpdir, t.pkg+".txt")
}

func (t *task) do(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		_, span := otel.Tracer("wolfictl").Start(ctx, "build "+t.pkg+" (canceled)")
		defer span.End()
		return err
	}

	f, err := os.Create(t.logfile())
	if err != nil {
		return err
	}
	defer f.Close()

	ctx = clog.WithLogger(ctx, clog.New(slog.NewTextHandler(f, nil)))

	ctx, span := otel.Tracer("wolfictl").Start(ctx, "build "+t.pkg)
	defer span.End()

	cfg, err := config.ParseConfiguration(ctx, fmt.Sprintf("%s.yaml", t.pkg), config.WithFS(os.DirFS(t.dir)))
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	for _, arch := range t.archs {
		arch := types.ParseArchitecture(arch).ToAPK()

		log := clog.New(clog.FromContext(ctx).Handler()).With("arch", arch)
		ctx := clog.WithLogger(ctx, log)

		// See if we already have the package built.
		apk := fmt.Sprintf("%s-%s-r%d.apk", cfg.Package.Name, cfg.Package.Version, cfg.Package.Epoch)
		apkPath := filepath.Join(t.dir, "packages", arch, apk)
		if _, err := os.Stat(apkPath); err == nil {
			log.Infof("skipping %s, already built", apkPath)
			continue
		}

		sdir := filepath.Join(t.dir, t.pkg)
		if _, err := os.Stat(sdir); os.IsNotExist(err) {
			if err := os.MkdirAll(sdir, os.ModePerm); err != nil {
				return fmt.Errorf("creating source directory %s: %v", sdir, err)
			}
		} else if err != nil {
			return fmt.Errorf("creating source directory: %v", err)
		}

		fn := fmt.Sprintf("%s.yaml", t.pkg)
		if t.dryrun {
			log.Infof("DRYRUN: would have built %s", apkPath)
			continue
		}

		runner, err := newRunner(ctx, t.runner)
		if err != nil {
			return fmt.Errorf("creating runner: %w", err)
		}

		log.Infof("will build: %s", apkPath)
		bc, err := build.New(ctx,
			build.WithArch(types.ParseArchitecture(arch)),
			build.WithConfig(filepath.Join(t.dir, fn)),
			build.WithPipelineDir(t.pipelineDir),
			build.WithExtraKeys([]string{"https://packages.wolfi.dev/os/wolfi-signing.rsa.pub"}), // TODO: flag
			build.WithExtraRepos([]string{"https://packages.wolfi.dev/os"}),                      // TODO: flag
			build.WithSigningKey(filepath.Join(t.dir, "local-melange.rsa")),
			build.WithRunner(runner),
			build.WithEnvFile(filepath.Join(t.dir, fmt.Sprintf("build-%s.env", arch))),
			build.WithNamespace("wolfi"), // TODO: flag
			build.WithSourceDir(sdir),
			// build.WithCacheSource("gs://wolfi-sources/"), // TODO: flag
			// build.WithCacheDir("./melange-cache/"),       // TODO: flag
			build.WithOutDir(filepath.Join(t.dir, "packages")),
			build.WithRemove(t.remove),
		)
		if err != nil {
			return err
		}
		defer func() {
			if err := bc.Close(ctx); err != nil {
				log.Errorf("closing build %q: %v", t.pkg, err)
			}
		}()
		if err := bc.BuildPackage(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (t *task) maybeStart(ctx context.Context) {
	t.cond.L.Lock()
	defer t.cond.L.Unlock()

	if !t.started {
		clog.FromContext(ctx).Infof("starting %s", t.pkg)
		t.started = true
		t.h.addTask(t)
		go t.start(ctx)
	}
}

func (t *task) wait() error {
	t.cond.L.Lock()
	for !t.done {
		t.cond.Wait()
	}
	t.cond.L.Unlock()

	return t.err
}

func newRunner(ctx context.Context, runner string) (container.Runner, error) {
	switch runner {
	case "docker":
		return docker.NewRunner(ctx)
	case "bubblewrap":
		return container.BubblewrapRunner(), nil
	}

	return nil, fmt.Errorf("runner %q not supported", runner)
}

type handler struct {
	tasks map[string]*task
	sync.Mutex
}

func (h *handler) dashFragment(w http.ResponseWriter, r *http.Request) {
	vals := maps.Values(h.tasks)
	slices.SortFunc(vals, func(a, b *task) int {
		return strings.Compare(a.pkg, b.pkg)
	})
	fmt.Fprintln(w, "<h1>wolfictl build</h1>")
	fmt.Fprintln(w, "<h2>running</h2>")
	fmt.Fprintln(w, "<ul>")
	for _, t := range vals {
		if t.status == "running" {
			fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s\">%s</a></li>\n", t.pkg, t.pkg)
		}
	}
	fmt.Fprintln(w, "</ul>")

	fmt.Fprintln(w, "<h2>done</h2>")
	fmt.Fprintln(w, "<ul>")
	for _, t := range vals {
		if t.done {
			if t.err != nil {
				fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s\">%s</a>: %v</li>", t.pkg, t.pkg, t.err)
			} else {
				fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s\">%s</a>: %s</li>", t.pkg, t.pkg, t.status)
			}
		}
	}
	fmt.Fprintln(w, "</ul>")

	fmt.Fprintln(w, "<h2>queued</h2>")
	fmt.Fprintln(w, "<ul>")
	for _, t := range vals {
		if t.status == "waiting for semaphore" {
			fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s\">%s</a></li>", t.pkg, t.pkg)
		}
	}
	fmt.Fprintln(w, "</ul>")

	fmt.Fprintln(w, "<h2>blocked</h2>")
	fmt.Fprintln(w, "<ul>")
	for _, t := range vals {
		if strings.HasPrefix(t.status, "waiting on") {
			fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s\">%s</a>: %s</li>", t.pkg, t.pkg, t.status)
		}
	}
	fmt.Fprintln(w, "</ul>")
}

func (h *handler) pkgFragment(w http.ResponseWriter, r *http.Request, pkg string) {
	t, ok := h.tasks[pkg]
	if !ok {
		panic(pkg)
	}
	fmt.Fprintf(w, "<h1>%s: %s</h1>\n", pkg, t.status)
	fmt.Fprintln(w, "<h2>deps</h2>")
	fmt.Fprintln(w, "<ul>")
	for pkg, t := range t.deps {
		fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s\">%s</a>: %s</li>\n", pkg, pkg, t.status)
	}
	fmt.Fprintf(w, "</ul>\n")

	fmt.Fprintf(w, "<pre>")
	h.logs(w, r, t, 0)
	fmt.Fprintf(w, "</pre>")
}

func (h *handler) logFragment(w http.ResponseWriter, r *http.Request) {
	pkg := r.URL.Query().Get("pkg")
	if pkg == "" {
		panic("missing pkg")
	}

	t, ok := h.tasks[pkg]
	if !ok {
		panic(pkg)
	}

	if !t.started {
		// TODO: Return something that polls.
		return
	}

	qoffset := r.URL.Query().Get("offset")
	if qoffset == "" {
		panic("missing offset")
	}

	offset, err := strconv.Atoi(qoffset)
	if err != nil {
		panic(err)
	}

	h.logs(w, r, t, int64(offset))
}

func (h *handler) logs(w http.ResponseWriter, r *http.Request, t *task, offset int64) {
	if !t.started {
		return
	}
	f, err := os.Open(t.logfile())
	if err != nil {
		log.Printf("opening file failed: %v", err)
		return
	}
	defer f.Close()

	if offset != 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			log.Printf("seeking file failed: %v", err)
			return
		}
	}

	copied, err := io.Copy(w, f)
	if err != nil {
		log.Printf("copying err: %v", err)
	}
	if !t.done {
		fmt.Fprintf(w, `<div
			hx-get="/logs?pkg=%s&offset=%d"
    	hx-trigger="load delay:1s"
    	hx-swap="outerHTML show:bottom"
			>
			</div>`, t.pkg, offset+copied)
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Less coarse.
	h.Lock()
	defer h.Unlock()

	if r.URL.Path == "/logs" {
		h.logFragment(w, r)
		return
	}

	fmt.Fprintf(w, `<html>
<head>
<title>wolfictl build</title>
<script src="https://unpkg.com/htmx.org@1.9.10" integrity="sha384-D1Kt99CQMDuVetoL1lrYwg5t+9QdHe7NLX/SoJYkXDFfX37iInKRy5xLSi8nO7UC" crossorigin="anonymous"></script>
</head>
<body>
`)

	if pkg := r.URL.Query().Get("pkg"); pkg != "" {
		h.pkgFragment(w, r, pkg)
	} else {
		h.dashFragment(w, r)
	}

	fmt.Fprintln(w, "</body>")
	fmt.Fprintln(w, "</html>")
}

func (h *handler) addTask(t *task) {
	if h == nil {
		return
	}

	h.Lock()
	defer h.Unlock()

	h.tasks[t.pkg] = t
}
