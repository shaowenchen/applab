package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/client"
)

// showCommand prints one app's settings and state.
//
// The console's State card in a terminal, and the gap it fills is the same shape
// as `platform events`: the API serves GET /api/v1/apps/{app} and the console
// renders every field of it, while from a terminal the same facts were scattered
// across `list` (a row), `status` (the cluster's half) and `env` (the
// variables) — none of which shows the dockerfile path, the clone address, the
// branch or the resource bounds. Reading one app meant four commands and a
// join in the reader's head.
//
// It is a read, so it is one screen rather than a table of one row: the fields
// are labels down the left, as `config` and `status` print theirs, because this
// is a thing to read rather than a list to scan.
func showCommand(urlFlag, keyFlag, serverFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <app>",
		Short: "Show one app's settings and state",
		Long: `Show one app's settings and state.

Every field the API records for an app, in one place: what it is called, where it
is served, which repository and branch it deploys from, and what it is doing now.
The console's State card, without a browser.

With --server the app is read from another registered deployment; see
"applab servers". Everything below the app id is that deployment's answer.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			app, err := getAppOnServer(cmd.Context(), c, *serverFlag, args[0])
			if err != nil {
				return err
			}

			fmt.Printf("app          %s\n", app.ID)
			if app.Name != "" {
				fmt.Printf("name         %s\n", app.Name)
			}
			fmt.Printf("port         %d\n", app.Port)
			fmt.Printf("replicas     %d\n", app.Replicas)
			fmt.Printf("dockerfile   %s\n", app.Dockerfile)
			fmt.Printf("domain       %s\n", orDash(app.Domain))
			fmt.Printf("url          %s\n", orDash(app.URL))

			// The address the repository is cloned from, without a credential in
			// it — that is the API's own rule, and it is why this is safe to
			// print. Where a credential is wanted, `applab keys <app>` prints the
			// one with the key inserted.
			fmt.Printf("git_url      %s\n", orDash(app.GitURL))
			fmt.Printf("branch       %s\n", orDash(app.Branch))
			fmt.Printf("commit       %s\n", shortOrDash(app.CommitSHA))
			fmt.Printf("image        %s\n", orDash(app.Image))
			fmt.Printf("auto_deploy  %t\n", app.AutoDeploy)

			// The bounds the app has set for itself. An empty one is this
			// deployment's default rather than "none", which orDash's dash
			// stands for; the bounds actually in force on the container are
			// `applab resources <app>`'s answer, not this one's.
			fmt.Printf("resources    cpu %s/%s memory %s/%s (request/limit, empty means the deployment default)\n",
				orDash(app.Resources.CPURequest), orDash(app.Resources.CPULimit),
				orDash(app.Resources.MemoryRequest), orDash(app.Resources.MemoryLimit))

			fmt.Printf("status       %s\n", app.Status)
			if app.StatusReason != "" {
				fmt.Printf("reason       %s\n", app.StatusReason)
			}

			// The two halves the status folds together, named separately when
			// they say something it does not: a failed build leaves the previous
			// revision serving, so an app can be running and have failed its last
			// build at the same time, and one word cannot carry both.
			if app.RunStatus != "" {
				fmt.Printf("running      %s\n", app.RunStatus)
			}
			if app.BuildStatus != "" {
				fmt.Printf("last build   %s\n", app.BuildStatus)
			}
			fmt.Printf("created      %s\n", humanAge(app.CreatedAt))
			return nil
		},
	}

	cmd.Flags().StringVar(serverFlag, "server", "", "read the app on another registered deployment")
	return cmd
}

// shortOrDash abbreviates an id for a labelled row, and renders its absence as
// a dash.
//
// shortSHA renders an empty string as "(none)", which reads in a table cell
// where its neighbours are all ids. Here the row is a label and a value, and a
// dash is what every other empty field on it prints.
func shortOrDash(id string) string {
	if id == "" {
		return "-"
	}
	return shortSHA(id)
}

// getAppOnServer reads one app, here or on a registered remote.
//
// A context rather than the command, so that the helper is the same shape as
// listAppsOnServer and createAppOnServer beside it — the choice of server is
// what it is about, and the command is not.
func getAppOnServer(ctx context.Context, c *client.Client, server, appID string) (*client.App, error) {
	if remoteServerName(server) == "" {
		return c.GetApp(ctx, appID)
	}
	return c.GetServerApp(ctx, server, appID)
}
