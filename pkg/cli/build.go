package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"chainguard.dev/apko/pkg/build/types"
	"chainguard.dev/melange/pkg/build"
	"chainguard.dev/melange/pkg/config"
	"chainguard.dev/melange/pkg/container"
	"chainguard.dev/melange/pkg/container/docker"
	"github.com/chainguard-dev/clog"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/wolfi-dev/wolfictl/pkg/dag"
)

func cmdBuild() *cobra.Command {
	var archs []string
	var dir, pipelineDir, runner string
	var jobs int
	var dryrun bool
	var extraKeys, extraRepos []string
	var traceFile string

	// TODO: allow building only named packages, taking deps into account.
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

			if jobs == 0 {
				jobs = runtime.GOMAXPROCS(0)
			}
			jobch := make(chan struct{}, jobs)

			if pipelineDir == "" {
				pipelineDir = filepath.Join(dir, "pipelines")
			}

			newTask := func(_ context.Context, pkg string) *task {
				// TODO: Something with ctx.
				return &task{
					pkg:         pkg,
					dir:         dir,
					pipelineDir: pipelineDir,
					runner:      runner,
					archs:       archs,
					dryrun:      dryrun,
					jobch:       jobch,
					cond:        sync.NewCond(&sync.Mutex{}),
					deps:        map[string]*task{},
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
				log := clog.New(log.Handler()).With("package", pkg)
				ctx := clog.WithLogger(ctx, log)

				if tasks[pkg] == nil {
					tasks[pkg] = newTask(ctx, pkg)
				}
				for k, v := range m {
					// The package list is in the form of "pkg:version",
					// but we only care about the package name.
					if strings.HasPrefix(k, pkg+":") {
						for _, dep := range v {
							d, _, _ := strings.Cut(dep.Target, ":")

							if tasks[d] == nil {
								tasks[d] = newTask(ctx, d)
							}
							tasks[pkg].deps[d] = tasks[d]
						}
					}
				}
			}

			if len(tasks) == 0 {
				return fmt.Errorf("no packages to build")
			}

			sorted, err := g.Sorted()
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
		},
	}

	cmd.Flags().StringVarP(&dir, "dir", "d", ".", "directory to search for melange configs")
	cmd.Flags().StringVar(&pipelineDir, "pipeline-dir", "", "directory used to extend defined built-in pipelines")
	cmd.Flags().StringVar(&runner, "runner", "docker", "which runner to use to enable running commands, default is based on your platform.")
	cmd.Flags().IntVarP(&jobs, "jobs", "j", 0, "number of jobs to run concurrently (default is GOMAXPROCS)")
	cmd.Flags().StringSliceVar(&archs, "arch", []string{"x86_64", "aarch64"}, "arch of package to build")
	cmd.Flags().BoolVar(&dryrun, "dry-run", false, "print commands instead of executing them")
	cmd.Flags().StringSliceVarP(&extraKeys, "keyring-append", "k", []string{"https://packages.wolfi.dev/os/wolfi-signing.rsa.pub"}, "path to extra keys to include in the build environment keyring")
	cmd.Flags().StringSliceVarP(&extraRepos, "repository-append", "r", []string{"https://packages.wolfi.dev/os"}, "path to extra repositories to include in the build environment")
	cmd.Flags().StringVar(&traceFile, "trace", "", "where to write trace output")
	return cmd
}

type task struct {
	pkg, dir, pipelineDir, runner string
	archs                         []string
	dryrun                        bool

	err  error
	deps map[string]*task

	jobch  chan struct{}
	status string

	started bool
	done    bool

	cond *sync.Cond
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

	for depname, dep := range t.deps {
		t.status = "waiting on " + depname
		if err := dep.wait(); err != nil {
			t.err = err
			return
		}
	}

	t.status = "waiting on jobch"

	// Block on jobch, to limit concurrency. Remove from jobch when done.
	t.jobch <- struct{}{}
	defer func() { <-t.jobch }()

	clog.FromContext(ctx).Infof("starting %q", t.pkg)
	t.status = "running"

	// all deps are done and we're clear to launch.
	t.err = t.do(ctx)
}

func (t *task) do(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		_, span := otel.Tracer("wolfictl").Start(ctx, "build "+t.pkg+" (canceled)")
		defer span.End()
		return err
	}

	ctx, span := otel.Tracer("wolfictl").Start(ctx, "build "+t.pkg)
	defer span.End()

	cfg, err := config.ParseConfiguration(ctx, fmt.Sprintf("%s.yaml", t.pkg), config.WithFS(os.DirFS(t.dir)))
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	for _, arch := range t.archs {
		arch := types.ParseArchitecture(arch).ToAPK()

		// TODO: Handle these logs in an interesting way instead of discarding them.
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
			build.WithRemove(true),
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

// https://go-review.googlesource.com/c/go/+/547956
var discard slog.Handler = discardHandler{}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (d discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return d }
func (d discardHandler) WithGroup(string) slog.Handler           { return d }
