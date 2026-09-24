// Command applab runs the control plane.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shaowenchen/applab/internal/api"
	"github.com/shaowenchen/applab/internal/appconfig"
	"github.com/shaowenchen/applab/internal/appkey"
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
	"github.com/shaowenchen/applab/internal/sourcetoken"
	"github.com/shaowenchen/applab/internal/store"
)

func main() {
	if err := run(); err != nil {
		// Reported on stderr rather than through slog: this runs before logging
		// is configured, and a failure to start should read as plain prose, not
		// as a structured line that looks like every other log entry.
		fmt.Fprintf(os.Stderr, "applab: %v\n", err)
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
			fmt.Fprintf(os.Stdout, "applab %s (%s, built %s)\n",
				buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime)
			return true, nil
		default:
			return true, fmt.Errorf("unknown argument %q\n\n%s", arg, usage)
		}
	}
	return false, nil
}

const usage = `Run the applab control plane.

Configuration comes from the environment, not from arguments. The ones that
matter most:

  APPLAB_KEY              An API key. Required; without one applab refuses to
                          start rather than serving the API openly.
  APPLAB_DATA_DIR         Where the database and the apps' source live.
                          Default ./data.
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
		"listen", cfg.Listen,
		"data_dir", cfg.DataDir)

	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	// A signal-cancelled context is what lets in-flight work stop cleanly. It is
	// created before anything that could block so a Ctrl-C during startup — a
	// slow disk, a hung database — is still handled.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	// Source storage is the first half of the pipeline, and a failure to
	// initialise it is fatal: a deployment that accepts an app create but cannot
	// store its source is worse than one that refuses to start, because the
	// caller only finds out at the end of an upload.
	src, err := source.New(source.Options{
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
	gitTransport, err := gitx.New(filepath.Join(cfg.DataDir, "repos"), src.GitPath())
	if err != nil {
		return err
	}

	// Which repository a credential may reach is decided by the API layer, which
	// owns the two tiers — see Server.AuthorizeGitRepo.
	//
	// The hook is a closure over srv rather than a check made here, because the
	// per-app key store is attached further down (it comes with the cluster,
	// which is optional). A closure reads srv's fields when the request arrives,
	// so it sees whatever was attached by then, and a deployment with no cluster
	// simply has no app keys to resolve against.
	srv.WithGit(gitTransport.Authorize(srv.AuthorizeGitRepo))

	// The console is a client of the same public API, so it holds no privileges
	// and adds no endpoints — it is a page, and everything it does is a call a
	// person could make with curl.
	if consoleHandler, err := console.New(); err != nil {
		slog.Warn("console is unavailable", "error", err)
	} else {
		srv.WithConsole(consoleHandler)
	}

	// The cluster half is optional. A deployment with no cluster is a legitimate
	// way to run applab — the API and the source half still work — and it is how
	// this binary is developed. Configuration problems are reported and the
	// deployment continues without the capability rather than refusing to start,
	// since the source half is independently useful.
	sourceTokens := sourcetoken.NewIssuer(sourcetoken.DefaultTTL)
	srv.WithSourceTokens(sourceTokens)
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

		// Per-app API keys live in Secrets, so they come with the cluster the
		// same way builds and deploys do. Without one the deployment keeps
		// working on the admin tier alone — which is the documented way to run
		// applab with no cluster at all.
		srv.WithAppKeys(appkey.New(client.Clientset(), cfg.Namespace))

		// An app's secrets live in a Secret beside it, so they need a cluster
		// for the same reason the keys do. Without one an app still deploys and
		// still gets its environment variables; only the secret endpoints
		// report themselves unavailable.
		appConfig := appconfig.New(client.Clientset(), cfg.Namespace)
		srv.WithAppConfig(appConfig)

		if cfg.Build.Enabled() {
			engine := build.New(client.Clientset(), build.Config{
				BuilderImage:       cfg.Build.BuilderImage,
				FetcherImage:       cfg.Build.FetcherImage,
				Registry:           cfg.Build.Registry,
				PushSecret:         cfg.Build.PushSecret,
				InsecureRegistry:   cfg.Build.InsecureRegistry,
				Rootless:           cfg.Build.RootlessBuild(),
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
				"rootless", cfg.Build.RootlessBuild())
		} else {
			slog.Info("build pipeline disabled: no registry, builder image or fetcher image configured")
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
			PathPrefix:       cfg.PathPrefix,
			ImagePullSecret:  cfg.Deploy.ImagePullSecret,
			Annotations:      cfg.Deploy.Annotations,
			AppCPURequest:    cfg.Deploy.AppCPURequest,
			AppMemoryRequest: cfg.Deploy.AppMemoryRequest,
			AppCPULimit:      cfg.Deploy.AppCPULimit,
			AppMemoryLimit:   cfg.Deploy.AppMemoryLimit,
			// The deployer is the one thing that reads secret values, to hash
			// them into the pod template.
			Secrets: appConfig,
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
