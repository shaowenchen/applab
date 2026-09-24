// Command applab-cli drives an AppLab deployment from a terminal.
//
// The commands are shaped around what someone actually does, not around the API:
//
//	applab push      upload the current directory, build it, deploy it
//	applab overview  the whole platform at a glance
//	applab env       configure an app — variables and secrets
//	applab keys      show an app's own key
//	applab logs      watch an app
//	applab status    is it up, and where
//	applab diagnose  why it is not working
//	applab rollback  go back to an earlier upload
//
// `push` is the one that matters. Everything else exists so that a person can
// answer a question without opening a browser, and each command is a thin layer
// over internal/client — the same code the console's fetch calls reach the same
// endpoints — so the CLI cannot drift from the API.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/buildinfo"
	"github.com/shaowenchen/applab/internal/client"
)

func main() {
	// A signal-cancelled context is what lets a Ctrl-C stop a followed log or an
	// in-flight build poll cleanly, rather than killing the process mid-request.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCommand().ExecuteContext(ctx); err != nil {
		// A command that already printed its own explanation exits non-zero
		// without a second, redundant message.
		var silent *silentError
		if errors.As(err, &silent) {
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "AppLab: %v\n", err)
		os.Exit(1)
	}
}

// silentError reports a failure whose explanation has already been printed.
type silentError struct{ err error }

func (e *silentError) Error() string { return e.err.Error() }
func (e *silentError) Unwrap() error { return e.err }

func rootCommand() *cobra.Command {
	var (
		urlFlag string
		keyFlag string
	)

	root := &cobra.Command{
		Use:   "applab",
		Short: "Deploy an application to Kubernetes by uploading its source",
		Long: `AppLab deploys an application to Kubernetes by uploading its source.

The address and key come from APPLAB_URL and APPLAB_KEY, or from --url and --key.
Point them at a deployment and these commands are everything needed to ship:

    applab push              upload, build and deploy the current directory
    applab overview          the whole platform at a glance
    applab env               show an app's configuration
    applab status            what is running, and where
    applab logs --follow     watch it
    applab diagnose          why it is not working
    applab rollback          go back to the previous upload

There are two kinds of key, and which one is in APPLAB_KEY decides what these
commands reach. An admin key sees and does everything. An app key belongs to one
app and reaches only that app — it can push, build, deploy, roll back and read
logs, but cannot delete the app and cannot see any other. Read an app's own key
with "applab keys <app>".`,
		SilenceUsage:  true,
		SilenceErrors: true,

		// A bare `AppLab` prints help rather than doing something surprising.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	root.PersistentFlags().StringVar(&urlFlag, "url", "", "applab deployment URL (default $APPLAB_URL)")
	root.PersistentFlags().StringVar(&keyFlag, "key", "", "API key (default $APPLAB_KEY)")

	// The client is built inside a command rather than here, so that --help and
	// a usage error work without a deployment configured.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if cmd.Name() == "version" || cmd.Name() == "completion" || cmd.Name() == "help" {
			return nil
		}
		_, err := newClient(urlFlag, keyFlag)
		return err
	}

	root.AddCommand(
		versionCommand(),
		configCommand(&urlFlag, &keyFlag),
		overviewCommand(&urlFlag, &keyFlag),
		keysCommand(&urlFlag, &keyFlag),
		envCommand(&urlFlag, &keyFlag),
		createCommand(&urlFlag, &keyFlag),
		listCommand(&urlFlag, &keyFlag),
		statusCommand(&urlFlag, &keyFlag),
		pushCommand(&urlFlag, &keyFlag),
		deployCommand(&urlFlag, &keyFlag),
		buildCommand(&urlFlag, &keyFlag),
		rollbackCommand(&urlFlag, &keyFlag),
		logsCommand(&urlFlag, &keyFlag),
		buildsCommand(&urlFlag, &keyFlag),
		commitsCommand(&urlFlag, &keyFlag),
		podsCommand(&urlFlag, &keyFlag),
		eventsCommand(&urlFlag, &keyFlag),
		updateCommand(&urlFlag, &keyFlag),
		restartCommand(&urlFlag, &keyFlag),
		stopCommand(&urlFlag, &keyFlag),
		deleteCommand(&urlFlag, &keyFlag),
		diagnoseCommand(&urlFlag, &keyFlag),
	)

	return root
}

// newClient builds a client from the flags, falling back to the environment.
//
// The flags win so that a one-off can point at a different deployment without
// disturbing the configured one, and the environment is the default so the
// common case needs no flags at all.
func newClient(urlFlag, keyFlag string) (*client.Client, error) {
	baseURL := urlFlag
	if baseURL == "" {
		baseURL = os.Getenv("APPLAB_URL")
	}
	key := keyFlag
	if key == "" {
		key = os.Getenv("APPLAB_KEY")
	}

	c, err := client.New(client.Options{BaseURL: baseURL, Key: key})
	if err != nil {
		return nil, fmt.Errorf("%w\n\nSet them in the environment:\n\n    export APPLAB_URL=https://applab.example.com\n    export APPLAB_KEY=<your key>", err)
	}
	return c, nil
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("applab %s (%s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime)
			return nil
		},
	}
}
