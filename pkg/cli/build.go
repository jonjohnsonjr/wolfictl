package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	charmlog "github.com/charmbracelet/log"
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

				defer func() {
					if err := tp.Shutdown(context.WithoutCancel(ctx)); err != nil {
						clog.FromContext(ctx).Errorf("Shutting down trace provider: %v", err)
					}
				}()

				tctx, span := otel.Tracer("wolfictl").Start(ctx, "build")
				defer span.End()
				ctx = tctx
			}

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
					tasks: map[string]map[string]*task{},
					archs: archs,
				}
				for _, arch := range archs {
					h.tasks[arch] = map[string]*task{}
				}
			}

			var eg errgroup.Group

			// Logs will go here to mimic the wolfi Makefile.
			for _, arch := range archs {
				archDir := logdir(dir, arch)
				if err := os.MkdirAll(archDir, os.ModePerm); err != nil {
					return fmt.Errorf("creating buildlogs directory: %w", err)
				}

				newTask := func(pkg string) *task {
					return &task{
						pkg:         pkg,
						dir:         dir,
						pipelineDir: pipelineDir,
						runner:      runner,
						arch:        arch,
						dryrun:      dryrun,
						remove:      remove,
						jobch:       jobch,
						cond:        sync.NewCond(&sync.Mutex{}),
						deps:        map[string]*task{},
						h:           h,
					}
				}

				// We want to ignore info level here during setup, but further down below we pull whatever was passed to use via ctx.
				log := clog.New(charmlog.NewWithOptions(os.Stderr, charmlog.Options{ReportTimestamp: true, Level: charmlog.Level(charmlog.WarnLevel)}))
				setupCtx := clog.WithLogger(ctx, log)
				pkgs, err := dag.NewPackages(setupCtx, os.DirFS(dir), dir, pipelineDir)
				if err != nil {
					return err
				}
				g, err := dag.NewGraph(setupCtx, pkgs,
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

				// Back to normal logger.
				log = clog.FromContext(ctx).With("arch", arch)

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

				todos := tasks
				if len(args) != 0 {
					todos = make(map[string]*task, len(args))

					for _, arg := range args {
						todos[arg] = tasks[arg]
					}
				}

				for _, t := range todos {
					t.maybeStart(ctx)
				}
				count := len(todos)

				eg.Go(func() error {
					errs := []error{}
					for _, t := range todos {
						if err := t.wait(); err != nil {
							errs = append(errs, fmt.Errorf("failed to build %s: %w", t.pkg, err))
							continue
						}

						delete(todos, t.pkg)
						log.Infof("Finished building %s (%d/%d)", t.pkg, count-len(todos), count)
					}

					// If the context is cancelled, it's not useful to print everything, just summarize the count.
					if err := ctx.Err(); err != nil {
						return fmt.Errorf("failed to build %d packages: %w", len(errs), err)
					}

					return errors.Join(errs...)
				})
			}

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

				clog.FromContext(ctx).Infof("%s", l.Addr().String())

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

	arch   string
	dryrun bool
	remove bool

	err  error
	deps map[string]*task

	jobch  chan struct{}
	status string

	started bool
	done    bool
	skipped bool

	cond *sync.Cond

	h *handler

	cfg *config.Configuration
}

func (t *task) start(ctx context.Context) {
	defer func() {
		// When we finish, wake up any goroutines that are waiting on us.
		t.cond.L.Lock()
		t.done = true
		t.cond.Broadcast()
		t.cond.L.Unlock()
	}()

	for _, dep := range t.deps {
		dep.maybeStart(ctx)
	}

	// We do this early because we need this info for t.logfile().
	cfg, err := config.ParseConfiguration(ctx, fmt.Sprintf("%s.yaml", t.pkg), config.WithFS(os.DirFS(t.dir)))
	if err != nil {
		t.err = fmt.Errorf("failed to parse config: %w", err)
		return
	}
	t.cfg = cfg

	arch := types.ParseArchitecture(t.arch).ToAPK()

	// See if we already have the package built.
	apk := fmt.Sprintf("%s-%s-r%d.apk", t.cfg.Package.Name, t.cfg.Package.Version, t.cfg.Package.Epoch)
	apkPath := filepath.Join(t.dir, "packages", arch, apk)
	if _, err := os.Stat(apkPath); err == nil {
		clog.FromContext(ctx).Infof("skipping %s, already built", apkPath)
		t.status = "skipped"
		return
	}
	if t.dryrun {
		clog.FromContext(ctx).Infof("DRYRUN: would have built %s", apkPath)
		t.status = "dryrun"
		return
	}

	f, err := os.Create(t.logfile())
	if err != nil {
		t.err = fmt.Errorf("creating logfile: :%w", err)
		return
	}
	defer f.Close()

	if len(t.deps) != 0 {
		clog.FromContext(ctx).Infof("task %q waiting on %q", t.pkg, maps.Keys(t.deps))
	}

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

	clog.FromContext(ctx).Infof("starting %s", t.pkg)
	t.status = "running"

	// all deps are done and we're clear to launch.
	t.err = t.build(ctx, f)
}

func (t *task) build(ctx context.Context, f io.Writer) error {
	if err := ctx.Err(); err != nil {
		_, span := otel.Tracer("wolfictl").Start(ctx, "build "+t.pkg+" (canceled)")
		defer span.End()
		return err
	}

	ctx, span := otel.Tracer("wolfictl").Start(ctx, "build "+t.pkg)
	defer span.End()

	arch := types.ParseArchitecture(t.arch).ToAPK()

	log := clog.New(slog.NewJSONHandler(f, nil)).With("pkg", t.pkg)
	fctx := clog.WithLogger(ctx, log)

	sdir := filepath.Join(t.dir, t.pkg)
	if _, err := os.Stat(sdir); os.IsNotExist(err) {
		if err := os.MkdirAll(sdir, os.ModePerm); err != nil {
			return fmt.Errorf("creating source directory %s: %v", sdir, err)
		}
	} else if err != nil {
		return fmt.Errorf("creating source directory: %v", err)
	}

	fn := fmt.Sprintf("%s.yaml", t.pkg)

	runner, err := newRunner(fctx, t.runner)
	if err != nil {
		return fmt.Errorf("creating runner: %w", err)
	}

	bc, err := build.New(fctx,
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
		// We Close() with the original context if we're cancelled so we get cleanup logs to stderr.
		ctx := ctx
		if ctx.Err() == nil {
			// On happy path, we don't care about cleanup logs.
			ctx = fctx
		}

		if err := bc.Close(ctx); err != nil {
			log.Errorf("closing build %q: %v", t.pkg, err)
		}
	}()

	if err := bc.BuildPackage(fctx); err != nil {
		return fmt.Errorf("building package (see %q for logs): %w", t.logfile(), err)
	}

	return nil
}

func (t *task) logfile() string {
	if t.cfg == nil {
		return ""
	}
	pkgver := fmt.Sprintf("%s-%s-r%d", t.cfg.Package.Name, t.cfg.Package.Version, t.cfg.Package.Epoch)
	return filepath.Join(logdir(t.dir, t.arch), pkgver) + ".log"
}

// If this task hasn't already been started, start it.
func (t *task) maybeStart(ctx context.Context) {
	t.cond.L.Lock()
	defer t.cond.L.Unlock()

	if !t.started {
		t.started = true
		t.h.addTask(t)
		go t.start(ctx)
	}
}

// Park the calling goroutine until this task finishes.
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

func logdir(dir, arch string) string {
	return filepath.Join(dir, "packages", arch, "buildlogs")
}

type handler struct {
	tasks map[string]map[string]*task
	archs []string
	sync.Mutex
}

func (h *handler) dashFragment(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "<h1>wolfictl build</h1>")
	arch := h.arch(w, r)
	if arch == "" {
		return
	}
	vals := maps.Values(h.tasks[arch])
	slices.SortFunc(vals, func(a, b *task) int {
		return strings.Compare(a.pkg, b.pkg)
	})

	running := []*task{}
	failed := []*task{}
	done := []*task{}
	queued := []*task{}
	blocked := []*task{}
	for _, t := range vals {
		if t.done {
			if t.err != nil {
				failed = append(failed, t)
			} else {
				done = append(done, t)
			}
		} else if t.status == "running" {
			running = append(running, t)
		} else if t.status == "waiting for semaphore" {
			queued = append(queued, t)
		} else if strings.HasPrefix(t.status, "waiting on") {
			blocked = append(blocked, t)
		}
	}

	fmt.Fprintln(w, "<h2>running</h2>")
	fmt.Fprintln(w, "<ul>")
	for _, t := range running {
		fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s&arch=%s\">%s</a></li>\n", t.pkg, arch, t.pkg)
	}
	fmt.Fprintln(w, "</ul>")

	fmt.Fprintln(w, "<details>")
	fmt.Fprintf(w, "<summary><h2>failed (%d)</h2></summary>\n", len(failed))
	fmt.Fprintln(w, "<ul>")
	for _, t := range failed {
		fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s&arch=%s\">%s</a>: %v</li>", t.pkg, t.pkg, arch, t.err)
	}
	fmt.Fprintln(w, "</ul>")
	fmt.Fprintln(w, "</details>")

	fmt.Fprintln(w, "<details>")
	fmt.Fprintf(w, "<summary><h2>done (%d)</h2></summary>\n", len(done))
	fmt.Fprintln(w, "<ul>")
	for _, t := range done {
		fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s&arch=%s\">%s</a>: %s</li>", t.pkg, arch, t.pkg, t.status)
	}
	fmt.Fprintln(w, "</ul>")
	fmt.Fprintln(w, "</details>")

	fmt.Fprintln(w, "<details>")
	fmt.Fprintf(w, "<summary><h2>queued (%d)</h2></summary>\n", len(queued))
	fmt.Fprintln(w, "<ul>")
	for _, t := range queued {
		fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s&arch=%s\">%s</a></li>", t.pkg, arch, t.pkg)
	}
	fmt.Fprintln(w, "</ul>")
	fmt.Fprintln(w, "</details>")

	fmt.Fprintln(w, "<details>")
	fmt.Fprintf(w, "<summary><h2>blocked (%d)</h2></summary>\n", len(blocked))
	fmt.Fprintln(w, "<ul>")
	for _, t := range blocked {
		fmt.Fprintf(w, "\t<li><a href=\"/?pkg=%s&arch=%s\">%s</a>: %s</li>", t.pkg, arch, t.pkg, t.status)
	}
	fmt.Fprintln(w, "</ul>")
	fmt.Fprintln(w, "</details>")
}

func (h *handler) pkgFragment(w http.ResponseWriter, r *http.Request, pkg string) {
	fmt.Fprintf(w, "<h1>%s</h1>\n", pkg)

	arch := h.arch(w, r)
	if arch == "" {
		return
	}

	t, ok := h.tasks[arch][pkg]
	if !ok {
		panic(pkg)
	}

	if len(t.deps) != 0 {
		fmt.Fprintln(w, "<h2>deps</h2>")
		fmt.Fprintln(w, "<ul>")
		for pkg, t := range t.deps {
			fmt.Fprintf(w, "\t<li><span><a href=\"/?pkg=%s&arch=%s\">%s</a>: %s</span></li>\n", pkg, arch, pkg, t.status)
		}
		fmt.Fprintf(w, "</ul>\n")
	}

	fmt.Fprintln(w, "<h2>logs</h2>")
	fmt.Fprintf(w, "<p>$ tail -f %s | jq -r '.level + \" \" + .time + \" \" + .msg'</p>\n", t.logfile())
	fmt.Fprintf(w, "<pre>")
	h.logs(w, r, t, 0)
	fmt.Fprintf(w, "</pre>")
}

func (h *handler) logFragment(w http.ResponseWriter, r *http.Request) {
	arch := r.URL.Query().Get("arch")
	pkg := r.URL.Query().Get("pkg")
	if pkg == "" {
		panic("missing pkg")
	}

	t, ok := h.tasks[arch][pkg]
	if !ok {
		panic(pkg)
	}

	if !t.started {
		return
	}

	qoffset := r.URL.Query().Get("offset")
	if qoffset == "" {
		log.Printf("missing offset")
		return
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

	if logfile := t.logfile(); logfile != "" {
		f, err := os.Open(logfile)
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

		type line struct {
			Time  time.Time
			Level string
			Msg   string
		}

		dec := json.NewDecoder(f)
		l := line{}
		for {
			if err := dec.Decode(&l); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				log.Printf("parsing json failed: %v", err)
				return
			}

			ts := l.Time.Format("15:04:05.000")
			fmt.Fprintf(w, "%s %s %s\n", l.Level[0:1], ts, l.Msg)
		}

		offset += dec.InputOffset()
	}
	if !t.done {
		href := fmt.Sprintf("/logs?pkg=%s&arch=%s&offset=%d", t.pkg, t.arch, offset)
		fmt.Fprintf(w, `<div id="load" hx-get=%q hx-trigger="load delay:1s" hx-swap="outerHTML show:bottom"></div>`, href)
	}
}

func (h *handler) arch(w http.ResponseWriter, r *http.Request) string {
	arch := r.URL.Query().Get("arch")
	if len(h.archs) == 1 {
		arch = h.archs[0]
	} else {
		fmt.Fprintln(w, "<ul>")
		for _, a := range h.archs {
			if a == arch {
				fmt.Fprintf(w, "\t<li>%s</li>\n", a)
			} else {
				// For lack of Clone().
				u, _ := url.Parse(r.URL.String())
				qs := u.Query()
				qs.Set("arch", a)
				u.RawQuery = qs.Encode()
				fmt.Fprintf(w, "\t<li><a href=%q>%s</a></li>\n", u, a)
			}
		}
		fmt.Fprintln(w, "</ul>")
	}

	return arch
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO: Less coarse locking.
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
%s
</head>
<body>
`, style)

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

	h.tasks[t.arch][t.pkg] = t
}

const style = `<style>
body {
  color-scheme: dark;
  background: black;
  color: white;
  padding-left: 5%;
  padding-right: 5%;
  padding-top: 40px;
  font-family: "Helvetica Neue", "Arial";
}

body > img {
  position: absolute;
  left: 10%;
  top: 10;
  height: 50px;
}

pre {
  display: inline-block;
  font-size: 14px;
  align-content: center;
  margin: 0;
  margin-top: 3px;
  margin-right: 12px;
  margin-left: 3px;
}

h1 {
  text-align: center;
  margin-bottom: 40px;
  display: flex;
  align-items: center;
  justify-content: center;
}

h2 {
  text-transform: capitalize;
  padding-top: 5px;
}

ul {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  list-style-type: none;
  padding-left: 0;
  padding-bottom: 20px;
  row-gap: 10px;
  column-gap: 40px;
}

li,
a {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

ul:nth-of-type(2),
ul:nth-of-type(4) {
  display: table;
  table-layout: fixed;
  flex-direction: column;
  width: 100%;
}
ul:nth-of-type(2) > li,
ul:nth-of-type(4) > li {
  display: table-row;
  width: 100%;
}
ul:nth-of-type(2) > li > a,
ul:nth-of-type(4) > li > a {
  display: table-cell;
  width: 33%;
  padding-bottom: 10px;
}

p {
  font-family: monospace;
}

@keyframes pending {
  0% {
    opacity: 1;
  }
  50% {
    opacity: 0.8;
  }
  100% {
    opacity: 1;
  }
}

ul:nth-of-type(1),
ul:nth-of-type(3) {
  animation: pending 2s ease-in-out infinite;
}

ul:nth-of-type(4) {
  color: orange;
}

details summary {
  cursor: pointer;
}

details summary > * {
  display: inline;
}
</style>`
