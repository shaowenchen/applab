package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shaowenchen/applab/internal/config"
	"github.com/shaowenchen/applab/internal/k8s"
)

// runSubcommand runs the one subcommand this binary has beyond serving.
//
// It exists for uninstall. `helm uninstall` deletes the objects Helm created and
// nothing besides, and everything AppLab made at runtime — the apps' Deployments,
// Services and VirtualServices, and every build Job — was created through the
// Kubernetes API, so Helm has never seen it. A pre-delete hook runs this to
// remove it, because AppLab itself is the component that knows what it created.
func runSubcommand(cmd string) error {
	switch cmd {
	case "cleanup":
		return runCleanup()
	default:
		return fmt.Errorf("unknown argument %q\n\n%s", cmd, usage)
	}
}

// runCleanup removes everything AppLab created at runtime.
//
// It deliberately does not go through config.Load. That function refuses a
// deployment with no API key and no object store, and both are legitimately gone
// by the time this runs: the keys live in a Secret that is part of the release
// — Helm deletes it, and a pre-delete hook runs after the release's objects have
// been removed — and the bucket may be decommissioned before the uninstall. The
// only things needed here are the namespace and cluster access, which come from
// the environment and the ServiceAccount.
//
// It is also why this is a subcommand rather than a flag on the server: nothing
// below is shared with serving.
func runCleanup() error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// The namespace is read straight from the environment rather than through
	// config.Load, which would fail for the reasons above. It has a default for
	// the same reason the server's does.
	namespace := os.Getenv("APPLAB_NAMESPACE")
	if namespace == "" {
		namespace = config.Default().Namespace
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, err := k8s.New(k8s.Options{
		Kubeconfig: os.Getenv("APPLAB_KUBECONFIG"),
		Namespace:  namespace,
	})
	if err != nil {
		return fmt.Errorf("connect to the cluster: %w", err)
	}

	slog.Info("removing everything applab created", "namespace", namespace)

	// The apps' objects first, through the same teardown an app deletion uses —
	// so each object's label is verified before it is removed, and an object that
	// is not AppLab's stops the sweep rather than being deleted.
	//
	// Failure is not fatal to the rest of the cleanup. An uninstall must remove
	// what it can: refusing to do anything because one object could not be read
	// leaves every app running, which is the outcome this exists to prevent.
	if err := client.DeleteEveryAppObject(ctx); err != nil {
		slog.Warn("could not remove every app's objects; the build jobs below are still removed", "error", err)
	}

	// And then any build Job still standing, which covers the ones the sweep
	// above could not reach — a Job whose app could not be listed, or one created
	// while the first pass was running. A build Job's pod is the one object here
	// holding a credential: the app's key is in its environment, readable by
	// anything with `get pods` in the namespace.
	removed, err := client.DeleteBuildJobs(ctx)
	if err != nil {
		return fmt.Errorf("remove build jobs: %w", err)
	}
	slog.Info("removed build jobs", "count", removed)

	// Waiting for the Deployments to actually go is what makes the uninstall
	// complete rather than merely requested. Deletion is asynchronous: the API
	// server accepts it, the pods terminate on their own schedule, and the
	// namespace still holds them for a while after this returns.
	//
	// It is bounded, because a pod that will not stop must not hold the uninstall
	// open — the caller is Helm, and a hook that never returns is an uninstall
	// that never finishes.
	waitCtx, cancelWait := context.WithTimeout(ctx, 60*time.Second)
	defer cancelWait()
	if err := client.WaitForAppsGone(waitCtx); err != nil {
		slog.Warn("some app resources were still terminating when the wait expired", "error", err)
	}

	slog.Info("cleanup finished")
	return nil
}
