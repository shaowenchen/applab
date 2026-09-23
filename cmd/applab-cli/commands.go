package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/client"
)

// configCommand shows what the deployment says about itself, which is also the
// quickest way to check a URL and key are right.
func configCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "Show the deployment's address, limits and capabilities",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			cfg, err := c.Config(cmd.Context())
			if err != nil {
				return err
			}

			fmt.Printf("url            %s\n", c.BaseURL())
			fmt.Printf("api version    %s\n", cfg.APIVersion)
			fmt.Printf("version        %s\n", cfg.Version)
			// Reported from the server's own domain template rather than
			// reassembled here, so a client cannot describe the deployment's
			// addressing differently from how it routes.
			fmt.Printf("apps served at %s\n", cfg.DomainTemplate)

			fmt.Printf("capabilities  ")
			for _, name := range []string{"source", "git", "build", "deploy"} {
				mark := "no"
				if cfg.Capabilities[name] {
					mark = "yes"
				}
				fmt.Printf(" %s=%s", name, mark)
			}
			fmt.Println()

			return nil
		},
	}
}

// createCommand registers an app without uploading anything.
func createCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		port       int32
		replicas   int32
		dockerfile string
		domain     string
	)

	cmd := &cobra.Command{
		Use:   "create <app>",
		Short: "Create an app",
		Long: `Create an app.

Uploading with ` + "`applab push`" + ` creates the app automatically, so this is only
needed to set an app up ahead of time or to create one whose id differs from its
directory name.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			req := client.CreateAppRequest{ID: args[0], Dockerfile: dockerfile, Domain: domain}
			if port > 0 {
				req.Port = &port
			}
			if replicas > 0 {
				req.Replicas = &replicas
			}

			app, err := c.CreateApp(cmd.Context(), req)
			if err != nil {
				return err
			}

			fmt.Printf("created %s\n", app.ID)
			if app.Hostname != "" {
				fmt.Printf("it will be served at %s\n", app.Hostname)
			}
			fmt.Printf("push source to it with: applab push %s\n", app.ID)
			return nil
		},
	}

	cmd.Flags().Int32Var(&port, "port", 0, "port the app listens on")
	cmd.Flags().Int32Var(&replicas, "replicas", 0, "how many replicas to run")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "", "Dockerfile path within the source")
	cmd.Flags().StringVar(&domain, "domain", "", "hostname to serve the app at")

	return cmd
}

// listCommand lists apps.
func listCommand(urlFlag, keyFlag *string) *cobra.Command {
	var includeDeleted bool

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List apps",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			apps, err := c.ListApps(cmd.Context(), includeDeleted)
			if err != nil {
				return err
			}
			if len(apps) == 0 {
				fmt.Println("no apps yet; create one with: applab push <app>")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "APP\tSTATUS\tCOMMIT\tURL")
			for _, app := range apps {
				url := app.URL
				if url == "" {
					url = app.Hostname
				}
				if url == "" {
					url = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", app.ID, app.Status, shortSHA(app.CommitSHA), url)
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVar(&includeDeleted, "include-deleted", false, "also list deleted apps")
	return cmd
}

// statusCommand reports an app's state.
func statusCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "status <app>",
		Short: "Show what is running and where",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			appID := args[0]

			status, err := c.AppStatus(cmd.Context(), appID)
			if err != nil {
				return err
			}

			fmt.Printf("app      %s\n", status.AppID)
			fmt.Printf("status   %s\n", status.Status)

			if status.Deployed != nil {
				if status.Deployed.CommitSHA != "" {
					fmt.Printf("commit   %s\n", shortSHA(status.Deployed.CommitSHA))
				}
				if status.Deployed.Image != "" {
					fmt.Printf("image    %s\n", status.Deployed.Image)
				}
				if status.Deployed.Reason != "" {
					fmt.Printf("reason   %s\n", status.Deployed.Reason)
				}
			}

			// The live view is reported separately from applab's record rather
			// than merged with it: when the two disagree, the difference is the
			// information — applab thought the rollout succeeded and something
			// has happened since.
			if status.Live != nil {
				if !status.Live.Deployed {
					fmt.Printf("cluster  nothing deployed\n")
				} else {
					fmt.Printf("cluster  %d/%d replicas ready", status.Live.ReadyReplicas, status.Live.DesiredReplicas)
					if status.Live.Available {
						fmt.Printf(" (available)")
					}
					fmt.Println()
					if status.Live.Message != "" {
						fmt.Printf("         %s\n", status.Live.Message)
					}
				}
			}

			if status.URL != "" {
				fmt.Printf("url      %s\n", status.URL)
			} else if status.Host != "" {
				fmt.Printf("host     %s (not reachable yet)\n", status.Host+status.Path)
			}

			return nil
		},
	}
}

// deployCommand deploys without uploading, for redeploying what is already there.
func deployCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		build bool
		watch bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <app> [commit]",
		Short: "Deploy a commit",
		Long: `Deploy a commit, building it first if it has never been built.

With no commit, the app's current tip is deployed. A commit that already has an
image is deployed as-is; --build starts a build for one that does not.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			appID := args[0]
			commit := ""
			if len(args) == 2 {
				commit = args[1]
			}

			result, err := c.Deploy(cmd.Context(), appID, commit, build)
			if err != nil {
				return err
			}

			// A deploy that started a build returns the build rather than a URL,
			// because the build has to finish before there is anything to serve.
			if result.Build != nil {
				fmt.Fprintf(os.Stderr, "applab: building %s\n", shortSHA(result.Commit))
				if watch {
					if err := watchBuild(cmd.Context(), c, appID, result.Build.ID); err != nil {
						return err
					}
					return deployAfterBuild(cmd, c, appID, result.Commit, watch)
				}
				fmt.Printf("build %s started\n", result.Build.ID)
				return nil
			}

			if watch {
				if err := watchDeploy(cmd.Context(), c, appID); err != nil {
					fmt.Fprintf(os.Stderr, "applab: %v\n", err)
				}
			}
			printDeployed(result)
			return nil
		},
	}

	cmd.Flags().BoolVar(&build, "build", false, "build the commit if it has no image yet")
	cmd.Flags().BoolVar(&watch, "watch", true, "wait for the rollout to finish")

	return cmd
}

// deployAfterBuild deploys a commit whose build just finished.
func deployAfterBuild(cmd *cobra.Command, c *client.Client, appID, commit string, watch bool) error {
	result, err := c.Deploy(cmd.Context(), appID, commit, false)
	if err != nil {
		return fmt.Errorf("deploy after build: %w", err)
	}
	if watch {
		if err := watchDeploy(cmd.Context(), c, appID); err != nil {
			fmt.Fprintf(os.Stderr, "applab: %v\n", err)
		}
	}
	printDeployed(result)
	return nil
}

func printDeployed(result *client.DeployResult) {
	if result.URL != "" {
		fmt.Println(result.URL)
		return
	}
	if result.Host != "" {
		fmt.Printf("deployed to %s (not reachable yet)\n", result.Host+result.Path)
		return
	}
	fmt.Println("deployed")
}

// rollbackCommand returns to an earlier commit.
func rollbackCommand(urlFlag, keyFlag *string) *cobra.Command {
	var list bool

	cmd := &cobra.Command{
		Use:   "rollback <app> [commit]",
		Short: "Deploy an earlier commit",
		Long: `Deploy an earlier commit.

With no commit, the app's history is listed so one can be chosen. A rollback
never builds: the commit's image already exists, or it could not have been
deployed before.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			appID := args[0]

			if list || len(args) == 1 {
				return printCommitChoices(cmd, c, appID)
			}

			result, err := c.Rollback(cmd.Context(), appID, args[1])
			if err != nil {
				return err
			}

			if err := watchDeploy(cmd.Context(), c, appID); err != nil {
				fmt.Fprintf(os.Stderr, "applab: %v\n", err)
			}
			fmt.Printf("rolled back to %s\n", shortSHA(result.Commit))
			if result.URL != "" {
				fmt.Println(result.URL)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&list, "list", false, "list the commits that can be rolled back to")
	return cmd
}

// printCommitChoices lists a commit history, marking which is deployed.
func printCommitChoices(cmd *cobra.Command, c *client.Client, appID string) error {
	app, err := c.GetApp(cmd.Context(), appID)
	if err != nil {
		return err
	}

	history, err := c.ListCommits(cmd.Context(), appID, 20)
	if err != nil {
		return err
	}
	if len(history.Commits) == 0 {
		fmt.Println("no commits yet; upload source with: applab push " + appID)
		return nil
	}

	// Which commits can actually be rolled back to is what makes this list
	// useful: a commit with no image cannot be deployed, and finding that out
	// from a failed rollback is worse than seeing it here.
	builds, err := c.ListBuilds(cmd.Context(), appID, 50)
	if err != nil {
		builds = nil
	}
	buildable := map[string]bool{}
	for _, b := range builds {
		if b.Status == "succeeded" && b.Image != "" {
			buildable[b.CommitSHA] = true
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMMIT\tAGE\tROLLBACK\tMESSAGE")
	for _, commit := range history.Commits {
		marker := ""
		switch {
		case commit.SHA == app.CommitSHA:
			marker = "(deployed)"
		case buildable[commit.SHA]:
			marker = "yes"
		default:
			marker = "no image"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			shortSHA(commit.SHA), humanAge(commit.CreatedAt), marker, firstLine(commit.Message))
	}
	w.Flush()

	fmt.Printf("\nroll back with: applab rollback %s <commit>\n", appID)
	return nil
}

// logsCommand streams an app's log.
func logsCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		follow    bool
		pod       string
		container string
		tail      int
		previous  bool
		since     string
	)

	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Show an app's log",
		Long: `Show an app's log.

Follows by default, so it behaves like tail -f. Use --previous to read the
container's last instance, which is where a crash loop's reason is written.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			opts := client.LogOptions{
				Pod:       pod,
				Container: container,
				Tail:      tail,
				Previous:  previous,
				Since:     since,
			}

			err = c.Logs(cmd.Context(), args[0], opts, os.Stdout, follow)
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", true, "keep streaming new output")
	cmd.Flags().StringVar(&pod, "pod", "", "read a specific pod rather than the newest")
	cmd.Flags().StringVarP(&container, "container", "c", "", "read a specific container")
	cmd.Flags().IntVar(&tail, "tail", 500, "start with this many lines")
	cmd.Flags().BoolVar(&previous, "previous", false, "read the previous container instance (a crash loop's cause)")
	cmd.Flags().StringVar(&since, "since", "", "only show output from the last duration, e.g. 5m")

	return cmd
}

// buildsCommand lists an app's builds, or streams one's log.
func buildsCommand(urlFlag, keyFlag *string) *cobra.Command {
	var showLogs bool

	cmd := &cobra.Command{
		Use:   "builds <app>",
		Short: "List an app's builds",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			appID := args[0]

			builds, err := c.ListBuilds(cmd.Context(), appID, 20)
			if err != nil {
				return err
			}
			if len(builds) == 0 {
				fmt.Println("no builds yet")
				return nil
			}

			if showLogs {
				// The newest build is the one someone asking for logs means.
				return watchBuild(cmd.Context(), c, appID, builds[0].ID)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "BUILD\tCOMMIT\tSTATUS\tAGE\tIMAGE")
			for _, b := range builds {
				image := b.Image
				if image == "" {
					image = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					b.ID[:8], shortSHA(b.CommitSHA), b.Status, humanAge(b.CreatedAt), image)
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVar(&showLogs, "logs", false, "follow the newest build's log instead")
	return cmd
}

// commitsCommand lists an app's commit history.
func commitsCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "commits <app>",
		Short: "List an app's uploaded commits",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			history, err := c.ListCommits(cmd.Context(), args[0], 50)
			if err != nil {
				return err
			}
			if len(history.Commits) == 0 {
				fmt.Println("no commits yet; upload source with: applab push " + args[0])
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "COMMIT\tAGE\tFILES\tMESSAGE")
			for _, commit := range history.Commits {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n",
					shortSHA(commit.SHA), humanAge(commit.CreatedAt), commit.Files, firstLine(commit.Message))
			}
			return w.Flush()
		},
	}
}

// diagnoseCommand explains why an app is not working.
func diagnoseCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "diagnose <app>",
		Short: "Explain why an app is not working",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			appID := args[0]

			d, err := c.Diagnose(cmd.Context(), appID)
			if err != nil {
				return err
			}

			fmt.Printf("app      %s\n", d.AppID)
			fmt.Printf("status   %s\n", d.Status)
			if d.StatusReason != "" {
				fmt.Printf("reason   %s\n", d.StatusReason)
			}

			switch {
			case d.Message != "":
				fmt.Printf("\n%s\n", d.Message)
			case d.Problem != "":
				fmt.Printf("\nproblem  %s\n", d.Problem)
			}
			if d.Next != "" {
				fmt.Printf("next     %s\n", d.Next)
			}
			if d.Error != "" {
				fmt.Printf("error    %s\n", d.Error)
			}

			if len(d.Pods) > 0 {
				fmt.Println("\npods")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "  NAME\tREADY\tRESTARTS\tREASON")
				for _, pod := range d.Pods {
					ready := "no"
					if pod.Ready {
						ready = "yes"
					}
					fmt.Fprintf(w, "  %s\t%s\t%d\t%s\n", pod.Name, ready, pod.Restarts, pod.Reason)
				}
				w.Flush()
			}

			if len(d.Events) > 0 {
				fmt.Println("\nevents")
				for _, e := range d.Events {
					fmt.Printf("  [%s] %s: %s", e.Type, e.Reason, e.Message)
					if e.Count > 1 {
						fmt.Printf(" (%d times)", e.Count)
					}
					fmt.Println()
				}
			}

			// The previous instance's log is where a crash loop's cause is
			// written, so it is shown whenever the server sent one.
			if d.PrevLog != "" {
				fmt.Println("\nlog from the previous container instance")
				fmt.Println(indent(d.PrevLog, "  "))
			} else if d.Logs != "" {
				fmt.Println("\nlog")
				fmt.Println(indent(d.Logs, "  "))
			}

			return nil
		},
	}
}

// restartCommand rolls an app's pods.
func restartCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "restart <app>",
		Short: "Restart an app's pods, keeping the same image",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			if err := c.Restart(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("restarting %s\n", args[0])
			return nil
		},
	}
}

// stopCommand stops an app without deleting it.
func stopCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <app>",
		Short: "Stop an app, keeping its source and history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			if err := c.Stop(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("stopped %s; its source is kept, so: applab deploy %s\n", args[0], args[0])
			return nil
		},
	}
}

// deleteCommand deletes an app.
func deleteCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		yes        bool
		keepSource bool
	)

	cmd := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app and everything applab recorded for it",
		Long: `Delete an app, its namespace and its source.

This cannot be undone. --keep-source retains the git repository so the code is
still recoverable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			appID := args[0]

			if !yes {
				// Confirmation is required because this destroys the source as
				// well as the running app, and there is no undo.
				fmt.Fprintf(os.Stderr, "This deletes %s%s.\nType the app id to confirm: ", appID,
					map[bool]string{true: " (keeping its source)", false: " and its source"}[keepSource])

				var confirm string
				fmt.Fscanln(os.Stdin, &confirm)
				if strings.TrimSpace(confirm) != appID {
					return fmt.Errorf("aborted")
				}
			}

			if err := c.DeleteApp(cmd.Context(), appID, keepSource); err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", appID)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&keepSource, "keep-source", false, "keep the app's source repository")

	return cmd
}

// humanAge renders how long ago something happened, at a scale a person reads.
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}

	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// firstLine is the first line of a message, for a table cell.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	line, _, _ := strings.Cut(s, "\n")
	if len(line) > 70 {
		line = line[:70] + "…"
	}
	return line
}

// indent prefixes every line, for embedding a log in a report.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}
