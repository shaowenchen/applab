// Command applab-cli drives an AppLab deployment from a terminal.
//
// The commands are shaped around what someone actually does, not around the API:
//
//	applab push      upload the current directory, build it, deploy it
//	applab overview  the whole platform at a glance
//	applab env       configure an app — variables and secrets
//	applab keys      show an app's own key
//	applab branch    which branches an app has, and switch between them
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
		urlFlag    string
		keyFlag    string
		serverFlag string
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
    applab branch            which branch is running, and switch it
    applab status            what is running, and where
    applab logs --follow     watch it
    applab diagnose          why it is not working
    applab platform logs     why *AppLab* is not working
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
	root.PersistentFlags().StringVar(&serverFlag, "server", "", "act on another registered AppLab deployment (default \"local\", this one)")

	// The client is built inside a command rather than here, so that --help and
	// a usage error work without a deployment configured.
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if commandNeedsNoDeployment(cmd) {
			return nil
		}
		_, err := newClient(urlFlag, keyFlag)
		return err
	}

	root.AddCommand(
		versionCommand(),
		routesCommand(),
		configCommand(&urlFlag, &keyFlag),
		overviewCommand(&urlFlag, &keyFlag),
		keysCommand(&urlFlag, &keyFlag),
		branchCommand(&urlFlag, &keyFlag),
		envCommand(&urlFlag, &keyFlag),
		createCommand(&urlFlag, &keyFlag, &serverFlag),
		listCommand(&urlFlag, &keyFlag, &serverFlag),
		showCommand(&urlFlag, &keyFlag, &serverFlag),
		serversCommand(&urlFlag, &keyFlag),
		statusCommand(&urlFlag, &keyFlag),
		pushCommand(&urlFlag, &keyFlag),
		deployCommand(&urlFlag, &keyFlag),
		buildCommand(&urlFlag, &keyFlag),
		rollbackCommand(&urlFlag, &keyFlag),
		logsCommand(&urlFlag, &keyFlag),
		buildsCommand(&urlFlag, &keyFlag),
		commitsCommand(&urlFlag, &keyFlag),
		podsCommand(&urlFlag, &keyFlag),
		resourcesCommand(&urlFlag, &keyFlag),
		platformCommand(&urlFlag, &keyFlag),
		eventsCommand(&urlFlag, &keyFlag),
		updateCommand(&urlFlag, &keyFlag),
		restartCommand(&urlFlag, &keyFlag),
		stopCommand(&urlFlag, &keyFlag),
		deleteCommand(&urlFlag, &keyFlag, &serverFlag),
		diagnoseCommand(&urlFlag, &keyFlag),
	)

	return root
}

// commandNeedsNoDeployment reports whether a command must run without one.
//
// Cobra names a command's own help "help", so the list has to include it rather
// than let --help on a leaf fail on the missing address of the deployment the
// help is about. The rest answer a question about the CLI itself rather than
// about a deployment: what version it is, how to complete it, and what it calls.
// `__routes` in particular is read by a parity test, which runs it on a machine
// with no deployment and no environment at all.
func commandNeedsNoDeployment(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "version", "completion", "help", "__routes":
		return true
	}
	return false
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
		// The example carries a path because the Helm chart serves an
		// installation under /applab by default: an address without it reaches
		// whatever else is on the host rather than this deployment.
		return nil, fmt.Errorf("%w\n\nSet them in the environment:\n\n    export APPLAB_URL=https://applab.example.com/applab\n    export APPLAB_KEY=<your key>\n\nThe path is part of the address — a deployment served under one answers nothing at the host's root.", err)
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

// routesCommand prints the API routes this CLI can reach, one per line.
//
// Hidden, and named with the double underscore that says so: it is not a
// command anyone runs to get work done, it is the CLI's declaration of its own
// reach, read by the four-surface parity test that compares this list, the API's
// route table, the console's fetch calls and the seeded script's. A command
// added without a line here is a command the test reports as missing — which is
// the whole point, because the gap this closes was a route the console served
// and no command reached.
func routesCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__routes",
		Short:  "Print the API routes this CLI reaches (for the parity test)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, route := range cliRoutes() {
				fmt.Println(route)
			}
			return nil
		},
	}
}

// cliRoutes is every API route the CLI calls, in the API's own pattern notation.
//
// It is a declaration, not a derivation: nothing walks the client and collects
// the strings it would send, because a list built by running the commands would
// need a deployment, and this has to be answerable on a machine that has none.
// So it is written out, and kept true by the parity test rather than by
// construction.
//
// One line per route, `METHOD /api/v1/...`, with the path in the placeholder
// form internal/api/router.go uses — `GET /api/v1/apps/{app}`, not the concrete
// `/api/v1/apps/shop` a command would send. The placeholder is what makes the
// comparison possible at all: the API declares patterns, and a client that
// declared addresses could never be matched against them.
//
// Adding a command that reaches a route not listed here is a test failure by
// name. Removing a command without removing its line is the same failure from
// the other side, which is why the list is kept sorted by route rather than by
// the command that reaches it: it is a set of routes, and two commands reaching
// one route — `builds --build` and `builds --logs` both read a build's log —
// appear once.
//
// It deliberately leaves out the routes no command reaches: the agent file
// routes under /apps/{app}/agent, the commit and single-source routes, and
// GET /api/v1/describe, which is a caller's orientation call rather than
// something a person types.
func cliRoutes() []string {
	return []string{
		// The deployment, and the overview the console's dashboard is built on.
		"GET /api/v1/config",
		"GET /api/v1/overview",

		// Apps: list, create, read, change one setting, delete. `show` is the
		// read, `create` and `delete` the ends, and `update` the only writer of
		// the app's own fields.
		"GET /api/v1/apps",
		"POST /api/v1/apps",
		"GET /api/v1/apps/{app}",
		"PATCH /api/v1/apps/{app}",
		"DELETE /api/v1/apps/{app}",

		// An app's own key, which `keys` reads and `keys rotate` replaces.
		"GET /api/v1/apps/{app}/key",
		"POST /api/v1/apps/{app}/key/rotate",

		// Configuration: the two readable halves, and the four writes.
		"GET /api/v1/apps/{app}/config",
		"PUT /api/v1/apps/{app}/env",
		"DELETE /api/v1/apps/{app}/env/{name}",
		"PUT /api/v1/apps/{app}/secrets",
		"DELETE /api/v1/apps/{app}/secrets/{name}",

		// Source: the single-request upload, and the three calls the chunked
		// path falls back to when the deployment refuses one that size.
		"POST /api/v1/apps/{app}/source",
		"POST /api/v1/apps/{app}/source/uploads",
		"PUT /api/v1/apps/{app}/source/uploads/{upload}/parts/{index}",
		"POST /api/v1/apps/{app}/source/uploads/{upload}/complete",

		// Branches and commits: which branch runs, which commits exist.
		"GET /api/v1/apps/{app}/branches",
		"PUT /api/v1/apps/{app}/branch",
		"GET /api/v1/apps/{app}/commits",

		// Builds: start, list, read, read a log, cancel.
		"GET /api/v1/apps/{app}/builds",
		"POST /api/v1/apps/{app}/builds",
		"GET /api/v1/apps/{app}/builds/{build}",
		"GET /api/v1/apps/{app}/builds/{build}/logs",
		"DELETE /api/v1/apps/{app}/builds/{build}",

		// Shipping: deploy, roll back, restart, stop.
		"POST /api/v1/apps/{app}/deploy",
		"POST /api/v1/apps/{app}/rollback",
		"POST /api/v1/apps/{app}/restart",
		"POST /api/v1/apps/{app}/stop",

		// Observing one app: what is running, what it uses, its pods, its log,
		// the namespace's events, and the diagnosis that reads all of them.
		"GET /api/v1/apps/{app}/status",
		"GET /api/v1/apps/{app}/resources",
		"GET /api/v1/apps/{app}/pods",
		"GET /api/v1/apps/{app}/logs",
		"GET /api/v1/apps/{app}/events",
		"GET /api/v1/apps/{app}/diagnose",

		// The same reads for AppLab itself, under `platform`.
		"GET /api/v1/platform/pods",
		"GET /api/v1/platform/events",
		"GET /api/v1/platform/logs",
		"GET /api/v1/platform/resources",

		// The other deployments this one can reach, and the apps on them.
		"GET /api/v1/servers",
		"POST /api/v1/servers",
		"GET /api/v1/servers/{server}",
		"DELETE /api/v1/servers/{server}",
		"GET /api/v1/servers/{server}/apps",
		"POST /api/v1/servers/{server}/apps",
		"GET /api/v1/servers/{server}/apps/{app}",
		"DELETE /api/v1/servers/{server}/apps/{app}",
	}
}
