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
	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/buildinfo"
	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/deploy"
	"github.com/shaowenchen/applab/internal/gitx"
	"github.com/shaowenchen/applab/internal/k8s"
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

func run() error {
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
	srv.WithGit(gitTransport)

	// The cluster half is optional. A deployment with no cluster is a legitimate
	// way to run applab — the API and the source half still work — and it is how
	// this binary is developed. Configuration problems are reported and the
	// deployment continues without the capability rather than refusing to start,
	// since the source half is independently useful.
	sourceTokens := sourcetoken.NewIssuer(sourcetoken.DefaultTTL)
	srv.WithSourceTokens(sourceTokens)

	if client, err := k8s.New(k8s.Options{
		Kubeconfig:      cfg.Kubeconfig,
		NamespacePrefix: cfg.NamespacePrefix,
		OwnNamespace:    cfg.Namespace,
	}); err != nil {
		slog.Warn("running without cluster access: builds and deploys are unavailable", "error", err)
	} else {
		srv.WithNamespace(client.EnsureNamespace, client.DeleteNamespace)
		srv.WithClusterStatus(client.Ready)

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
			})
			srv.WithBuild(engine)
			slog.Info("build pipeline enabled",
				"registry", cfg.Build.Registry,
				"rootless", cfg.Build.RootlessBuild())
		} else {
			slog.Info("build pipeline disabled: no registry, builder image or fetcher image configured")
		}

		// The deploy half shares the cluster client. It is attached whenever the
		// cluster is reachable — an app can be deployed from an image that was
		// built elsewhere, so deploying does not depend on the build half.
		srv.WithDeployer(deploy.New(client.Clientset(), deploy.Config{
			IngressClass:    cfg.Deploy.IngressClass,
			BaseDomain:      cfg.BaseDomain,
			TLSSecret:       cfg.Deploy.TLSSecret,
			ClusterIssuer:   cfg.Deploy.ClusterIssuer,
			ImagePullSecret: cfg.Deploy.ImagePullSecret,
			Annotations:     cfg.Deploy.Annotations,
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
