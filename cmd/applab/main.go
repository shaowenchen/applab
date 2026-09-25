// Command AppLab runs the control plane.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/buildinfo"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/console"
	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/gitx"
	"github.com/shaowenchen/applab/internal/k8s"
	"github.com/shaowenchen/applab/internal/observe"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

func main() {
	if err := run(); err != nil {
		// Reported on stderr rather than through slog: this runs before logging
		// is configured, and a failure to start should read as plain prose, not
		// as a structured line that looks like every other log entry.
		fmt.Fprintf(os.Stderr, "AppLab: %v\n", err)
		os.Exit(1)
	}
}

// handleFlags deals with the arguments the server accepts and reports whether
// it has finished.
//
// The server is configured entirely by environment, so the only useful argument
// is one asking about the binary itself. Everything else is rejected rather
// than ignored: this runs in a container where a mistyped argument is invisible,
// and silently starting the server makes it look like the argument worked.
func handleFlags(args []string) (done bool, err error) {
	for _, arg := range args {
		switch arg {
		case "-h", "--help":
			fmt.Fprint(os.Stdout, usage)
			return true, nil
		case "--version", "-v":
			fmt.Fprintf(os.Stdout, "AppLab %s (%s, built %s)\n",
				buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime)
			return true, nil
		default:
			return true, fmt.Errorf("unknown argument %q\n\n%s", arg, usage)
		}
	}
	return false, nil
}

const usage = `Run the AppLab control plane.

Configuration comes from the environment, not from arguments. The ones that
matter most:

  APPLAB_KEY              An API key. Required; without one applab refuses to
                          start rather than serving the API openly.
  APPLAB_OBJECT_STORE_ENDPOINT, APPLAB_OBJECT_STORE_BUCKET
                          The bucket AppLab keeps everything in. Both are
                          required: every app, repository and key lives there,
                          and there is no local fallback.
  APPLAB_OBJECT_STORE_ACCESS_KEY, APPLAB_OBJECT_STORE_SECRET_KEY
                          The bucket's credential.
  APPLAB_DATA_DIR         Scratch space for git, which needs a real filesystem.
                          Default ./data. Nothing durable is written there.
  APPLAB_BASE_DOMAIN      Domain apps are exposed under, so an app with id
                          "shop" is served at shop.<domain>.
  APPLAB_BUILD_REGISTRY   Where built images are pushed. Setting this, with the
                          builder and fetcher images, enables the build
                          pipeline; without it applab stores source and deploys
                          but cannot build.
  APPLAB_DEPLOY_TLS_SECRET
                          A wildcard certificate covering the base domain.

The full list, with the reasoning behind each default, is in .env.example and
in the Helm chart's values.yaml.

  --help, -h     Show this.
  --version, -v  Show the build version.
`

func run() error {
	if done, err := handleFlags(os.Args[1:]); done {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	setupLogging(cfg.LogLevel)
	slog.Info("starting applab",
		"version", buildinfo.Version,
		"commit", buildinfo.Commit,
		"listen", cfg.Listen)

	// Everything AppLab persists lives in object storage: the apps, their
	// history and their source repositories. A replica therefore holds nothing,
	// and can be replaced at any moment.
	//
	// Opened before the scratch directory is created, and before the logger has
	// anything else to say, because it is the one dependency with no default: a
	// deployment that cannot reach its bucket has nowhere to put an app, and
	// saying so first is the difference between a clear boot failure and a
	// service that comes up and fails every request.
	objects, err := cfg.OpenObjectStore()
	if err != nil {
		return err
	}
	slog.Info("object storage",
		"backend", objects.String(),
		"data_dir", cfg.DataDir)

	// Scratch space for the operations that need a real filesystem, of which
	// git is the only one. Nothing durable goes here, so this is created rather
	// than mounted — and created after the bucket is known to work, so a
	// misconfigured deployment does not leave a directory behind on its way to
	// failing.
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	// A signal-cancelled context is what lets in-flight work stop cleanly. It is
	// created before anything that could block so a Ctrl-C during startup — a
	// slow disk, a hung database — is still handled.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, objects)
	if err != nil {
		return err
	}

	// Source storage is the first half of the pipeline, and a failure to
	// initialise it is fatal: a deployment that accepts an app create but cannot
	// store its source is worse than one that refuses to start, because the
	// caller only finds out at the end of an upload.
	src, err := source.New(source.Options{
		Objects:     objects,
		DataDir:     cfg.DataDir,
		AuthorName:  "applab",
		AuthorEmail: "applab@localhost",
	})
	if err != nil {
		return err
	}

	srv := api.New(cfg, st, auth.New(cfg.Keys)).WithSource(src)

	// The git transport serves repositories over git's smart HTTP protocol. It
	// is mounted rather than absent only when git's own backend could be found,
	// which source.New has already verified.
	// The transport materialises a repository for the length of one request and
	// uploads it afterwards, because git cannot run against a bucket — see
	// source.StartSession. The root it is given is scratch space; PATH_INFO is
	// rebuilt from the materialised path per request.
	gitTransport, err := gitx.New(cfg.DataDir, src.GitPath())
	if err != nil {
		return err
	}
	gitTransport.WithSessions(src)

	// Which repository a credential may reach is decided by the API layer, which
	// owns the two tiers — see Server.AuthorizeGitRepo.
	//
	// Which *branch* a URL with no branch in it means is decided there too,
	// because it is app state: "/git/shop.git" is whatever branch shop is on, and
	// the transport has no way to know that. Both are closures over srv so a
	// request reads the server's state as it is when the request arrives.
	//
	// Prepare is the source store's, and it runs between materialising a
	// repository and handing it to git: the repository's config has to be pinned
	// before receive-pack can spawn the background maintenance that races the
	// upload. See source.Store.applyDeterministicConfig.
	gitTransport.
		Authorize(srv.AuthorizeGitRepo).
		WithActiveBranch(srv.ActiveBranch).
		WithPrepare(src.Prepare)
	srv.WithGit(gitTransport)

	// The console is a client of the same public API, so it holds no privileges
	// and adds no endpoints — it is a page, and everything it does is a call a
	// person could make with curl.
	if consoleHandler, err := console.New(); err != nil {
		slog.Warn("console is unavailable", "error", err)
	} else {
		srv.WithConsole(consoleHandler)
	}

	// The cluster half is optional. A deployment with no cluster is a legitimate
	// way to run AppLab — the API and the source half still work — and it is how
	// this binary is developed. Configuration problems are reported and the
	// deployment continues without the capability rather than refusing to start,
	// since the source half is independently useful.
	srv.WithMetrics(api.NewMetrics())

	if client, err := k8s.New(k8s.Options{
		Kubeconfig: cfg.Kubeconfig,
		Namespace:  cfg.Namespace,
	}); err != nil {
		slog.Warn("running without cluster access: builds and deploys are unavailable", "error", err)
	} else {
		// Namespace provisioning is gone with the per-app namespace. What
		// remains is deletion, which finds an app's objects by label because
		// there is no namespace to drop.
		srv.WithAppObjectsDeleter(client.DeleteAppObjects)
		srv.WithClusterStatus(client.Ready)

		if cfg.Build.Enabled() {
			engine := build.New(client.Clientset(), build.Config{
				KanikoImage:        cfg.Build.KanikoImage,
				Registry:           cfg.Build.Registry,
				Secret:             cfg.Build.Secret,
				InsecureRegistry:   cfg.Build.InsecureRegistry,
				AppLabURL:          cfg.BaseURL,
				CacheRepoPrefix:    cfg.Build.CacheRepoPrefix,
				BuildCPURequest:    cfg.Build.CPURequest,
				BuildMemoryRequest: cfg.Build.MemoryRequest,
				BuildCPULimit:      cfg.Build.CPULimit,
				BuildMemoryLimit:   cfg.Build.MemoryLimit,
				WorkspaceSizeLimit: cfg.Build.WorkspaceSizeLimit,
				ActiveDeadline:     cfg.Build.Timeout,
				TTLAfterFinished:   cfg.Build.TTLAfterFinished,
			})
			srv.WithBuild(engine)
			slog.Info("build pipeline enabled",
				"registry", cfg.Build.Registry,
				"kaniko", cfg.Build.KanikoImage)
		} else {
			slog.Info("build pipeline disabled: no registry or kaniko image configured")
		}

		// The observability half reads the same cluster, so it comes with it.
		srv.WithObserver(observe.New(client.Clientset()))

		// The deploy half shares the cluster client. It is attached whenever the
		// cluster is reachable — an app can be deployed from an image that was
		// built elsewhere, so deploying does not depend on the build half.
		//
		// The dynamic client goes with it because publishing an app means writing
		// an Istio VirtualService, which is not in client-go.
		srv.WithDeployer(deploy.NewWithDynamic(client.Clientset(), client.Dynamic(), deploy.Config{
			Gateway:    cfg.Deploy.Gateway,
			BaseDomain: cfg.BaseDomain,
			// The prefix is what puts every app under one path on a shared host
			// instead of on a subdomain of its own. It has to reach the deployer
			// as well as the API: the API is what advertises an app's address and
			// the deployer is what writes the VirtualService that serves it, so a
			// prefix known to only one of them produces an address that does not
			// route.
			PathPrefix: cfg.PathPrefix,
			// The pull credential is the push credential: one registry, one
			// Secret. There used to be a deploy.imagePullSecret for this that
			// every install set to the same value.
			ImagePullSecret:  cfg.Build.Secret,
			Annotations:      cfg.Deploy.Annotations,
			AppCPURequest:    cfg.Deploy.AppCPURequest,
			AppMemoryRequest: cfg.Deploy.AppMemoryRequest,
			AppCPULimit:      cfg.Deploy.AppCPULimit,
			AppMemoryLimit:   cfg.Deploy.AppMemoryLimit,
		}))
	}

	// Bring any build left in flight by a previous process up to date with the
	// cluster. Without this a build interrupted by a restart would read as
	// "running" forever, and a caller polling it would wait for a Job that
	// finished minutes ago.
	reconcileCtx, cancelReconcile := context.WithTimeout(ctx, 60*time.Second)
	srv.ReconcileBuilds(reconcileCtx)
	cancelReconcile()

	httpServer := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),

		// A generous read timeout: the largest legitimate request is a source
		// archive, and cutting one off mid-upload costs the caller the whole
		// transfer. WriteTimeout is unset deliberately — the log-streaming
		// endpoints hold a response open for as long as a build runs, and any
		// fixed limit there would truncate the very thing the caller is waiting
		// for. Instead, ReadHeaderTimeout bounds the slow-header attack that
		// WriteTimeout would otherwise be covering.
		ReadTimeout:  30 * time.Minute,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,

		// Without a header timeout, a client that opens a connection and sends
		// nothing holds a slot indefinitely; the body can be slow, but the
		// headers never legitimately are.
		ReadHeaderTimeout: 20 * time.Second,

		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}

	// The listener runs in its own goroutine so the main one can wait for a
	// shutdown signal, rather than the process living inside ListenAndServe.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received; draining")
	}

	// A fresh context, because ctx is already cancelled — reusing it would make
	// Shutdown return immediately and drop in-flight requests.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Not fatal: the process is exiting regardless, and the operator needs to
		// know the drain was cut short rather than see a clean exit.
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	slog.Info("stopped")
	return nil
}

// setupLogging configures the default logger.
//
// JSON when it will be read by something that parses it, text when it will be
// read by a person in a terminal. The choice is made from whether this process
// looks like it was started by a terminal, which is a good proxy for whether a
// human is watching.
func setupLogging(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	if termIsTerminal() {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		// Structured in production, where logs are collected and queried rather
		// than watched.
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// termIsTerminal reports whether stderr is a character device, i.e. a terminal.
func termIsTerminal() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
