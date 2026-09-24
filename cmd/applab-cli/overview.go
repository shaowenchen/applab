package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/client"
)

// overviewCommand reports the platform at a glance.
//
// It is the console's dashboard in a terminal, and it exists because the
// question "is this deployment healthy" should not require opening a browser —
// or listing every app and reading each one.
func overviewCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "overview",
		Short: "Show the whole platform at a glance",
		Long: `Show the whole platform at a glance.

Reports how many apps are in each state, how many builds have succeeded or
failed, whether the cluster is reachable, and what this deployment is configured
to do — the same summary the console opens on.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			o, err := c.Overview(cmd.Context())
			if err != nil {
				return err
			}

			fmt.Printf("apps     %d total", o.Apps.Total)
			if o.Apps.Running > 0 {
				fmt.Printf(", %d running", o.Apps.Running)
			}
			fmt.Println()

			// Only the states that are actually occupied are listed, so the line
			// stays one line on a healthy deployment and names what is wrong
			// when something is. A row of zeroes is noise a person reads past.
			printCounts("         ", []count{
				{"created", o.Apps.Created},
				{"building", o.Apps.Building},
				{"deploying", o.Apps.Deploying},
				{"failed", o.Apps.Failed},
				{"build-failed", o.Apps.BuildFailed},
			})

			if o.Apps.NeedsAttention > 0 {
				// The one line worth acting on, called out rather than left to
				// be inferred from the counts above.
				fmt.Printf("         %d app(s) need attention: applab diagnose <app>\n", o.Apps.NeedsAttention)
			}

			fmt.Printf("builds   %d total", o.Builds.Total)
			if o.Builds.Running > 0 {
				fmt.Printf(", %d running", o.Builds.Running)
			}
			fmt.Println()
			printCounts("         ", []count{
				{"pending", o.Builds.Pending},
				{"succeeded", o.Builds.Succeeded},
				{"failed", o.Builds.Failed},
			})

			// The cluster is reported as three distinct states rather than two:
			// a deployment with no cluster is a legitimate way to run applab,
			// and calling it unreachable would read as a fault.
			switch {
			case !o.Cluster.Configured:
				fmt.Println("cluster  not configured; builds and deploys are unavailable")
			case o.Cluster.Reachable:
				fmt.Println("cluster  reachable")
			default:
				fmt.Println("cluster  configured but UNREACHABLE; builds and deploys will fail")
			}

			fmt.Printf("version  %s (%s)\n", o.Deployment.Version, o.Deployment.APIVersion)
			fmt.Printf("where    %s\n", o.Deployment.DomainTemplate)
			fmt.Printf("ns       %s\n", o.Deployment.Namespace)

			fmt.Printf("caps    ")
			for _, name := range []string{"source", "git", "build", "deploy"} {
				mark := "no"
				if o.Deployment.Capabilities[name] {
					mark = "yes"
				}
				fmt.Printf(" %s=%s", name, mark)
			}
			fmt.Println()

			if len(o.Builds.Recent) > 0 {
				fmt.Println()
				fmt.Println("recent builds")
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "  APP\tBUILD\tCOMMIT\tSTATUS\tAGE")
				for _, b := range o.Builds.Recent {
					fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n",
						b.AppID, short(b.ID), shortSHA(b.CommitSHA), b.Status, humanAge(b.CreatedAt))
				}
				w.Flush()
			}

			return nil
		},
	}
}

// count is one named tally, for the compact status lines.
type count struct {
	name  string
	value int
}

// printCounts prints the non-zero entries of counts, indented, or nothing at all
// when every entry is zero.
func printCounts(indent string, counts []count) {
	if !anyNonZero(counts) {
		return
	}
	fmt.Print(indent)
	first := true
	for _, c := range counts {
		if c.value == 0 {
			continue
		}
		if !first {
			fmt.Print(", ")
		}
		fmt.Printf("%d %s", c.value, c.name)
		first = false
	}
	fmt.Println()
}

// anyNonZero reports whether any count is non-zero.
func anyNonZero(counts []count) bool {
	for _, c := range counts {
		if c.value != 0 {
			return true
		}
	}
	return false
}

// podsCommand lists an app's pods.
//
// The API and the client have supported this since observability was added; it
// had no command, so the per-container state that explains a pod which is not
// ready was reachable only from the console.
func podsCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "pods <app>",
		Short: "List an app's pods and their container state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			result, err := c.Pods(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(result.Pods) == 0 {
				fmt.Printf("no pods for %s; deploy it with: applab deploy %s\n", args[0], args[0])
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "POD\tREADY\tRESTARTS\tPHASE\tREASON")
			for _, pod := range result.Pods {
				ready := "no"
				if pod.Ready {
					ready = "yes"
				}
				reason := pod.Reason
				if reason == "" {
					reason = pod.Message
				}
				if reason == "" {
					reason = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n",
					pod.Name, ready, pod.Restarts, pod.Phase, reason)
			}
			w.Flush()

			// Per-container detail, but only for pods that are not healthy:
			// printing it for every pod would bury the one that is failing.
			for _, pod := range result.Pods {
				if pod.Ready && pod.Restarts == 0 {
					continue
				}
				fmt.Printf("\n%s\n", pod.Name)
				for _, ct := range pod.Containers {
					state := ct.State
					if ct.Reason != "" {
						state += " (" + ct.Reason + ")"
					}
					fmt.Printf("  %-20s %s", ct.Name, state)
					if ct.LastTerminatedReason != "" {
						fmt.Printf("  last: %s (exit %d)", ct.LastTerminatedReason, ct.LastExitCode)
					}
					fmt.Println()
				}
			}

			return nil
		},
	}
}

// eventsCommand lists an app's events.
//
// Warnings first, as the API returns them — a namespace's events are mostly
// image pulls and scheduling notes, and the one that explains a failure is the
// reason this exists.
func eventsCommand(urlFlag, keyFlag *string) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "events <app>",
		Short: "List an app's Kubernetes events, warnings first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			result, err := c.Events(cmd.Context(), args[0], limit)
			if err != nil {
				return err
			}
			if len(result.Events) == 0 {
				fmt.Println("no events")
				return nil
			}

			if result.Warnings > 0 {
				fmt.Printf("%d warning(s)\n\n", result.Warnings)
			}

			for _, e := range result.Events {
				fmt.Printf("[%s] %s: %s", e.Type, e.Reason, e.Message)
				if e.Count > 1 {
					fmt.Printf(" (%d times)", e.Count)
				}
				fmt.Println()
			}
			return nil
		},
	}

	// The limit is passed through to the API, which caps it server-side.
	cmd.Flags().IntVar(&limit, "limit", 50, "how many events to fetch")
	return cmd
}

// buildCommand starts a build of a commit.
//
// Until now a build could only be started indirectly, by asking a deploy to
// build. That is the common path, but it makes "build this without deploying it"
// — a commit that should be compiled and checked before it goes anywhere —
// impossible from the CLI.
func buildCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		watch bool
		logs  bool
	)

	cmd := &cobra.Command{
		Use:   "build <app> [commit]",
		Short: "Build an image from a commit",
		Long: `Build an image from a commit, without deploying it.

With no commit, the app's current tip is built. The build runs as a Job in the
cluster; --watch waits for it and --logs follows its output.`,
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

			b, err := c.StartBuild(cmd.Context(), appID, commit)
			if err != nil {
				return err
			}
			fmt.Printf("build %s started for %s\n", short(b.ID), shortSHA(b.CommitSHA))

			if !watch && !logs {
				fmt.Printf("watch it with: applab builds %s --logs\n", appID)
				return nil
			}
			return watchBuild(cmd.Context(), c, appID, b.ID)
		},
	}

	cmd.Flags().BoolVar(&watch, "watch", false, "wait for the build to finish")
	cmd.Flags().BoolVar(&logs, "logs", false, "follow the build's output (implies --watch)")
	return cmd
}

// updateCommand changes an app's settings.
//
// Every one of these is a field the API's PATCH accepts and the console can
// edit; from a terminal the only way to change a port or a replica count was to
// write the request out by hand.
func updateCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		name       string
		port       int32
		replicas   int32
		dockerfile string
		domain     string
	)

	cmd := &cobra.Command{
		Use:   "update <app>",
		Short: "Change an app's settings",
		Long: `Change an app's settings.

Only the flags you pass are changed; everything else is left as it is. Changing
a port, an image or a dockerfile path takes effect on the next deploy, not
immediately — deploy the app again with ` + "`applab deploy <app>`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			var req client.UpdateAppRequest
			changed := false

			// Each flag is applied only when it was set, so an omitted one is
			// left alone rather than being reset to its zero value — which is
			// what the pointer fields in the request exist for.
			if cmd.Flags().Changed("name") {
				req.Name = &name
				changed = true
			}
			if cmd.Flags().Changed("port") {
				req.Port = &port
				changed = true
			}
			if cmd.Flags().Changed("replicas") {
				req.Replicas = &replicas
				changed = true
			}
			if cmd.Flags().Changed("dockerfile") {
				req.Dockerfile = &dockerfile
				changed = true
			}
			if cmd.Flags().Changed("domain") {
				req.Domain = &domain
				changed = true
			}

			if !changed {
				return fmt.Errorf("nothing to change: pass at least one of --name, --port, --replicas, --dockerfile, --domain")
			}

			app, err := c.UpdateApp(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}

			fmt.Printf("updated %s\n", app.ID)
			fmt.Printf("port     %d\n", app.Port)
			fmt.Printf("replicas %d\n", app.Replicas)
			if app.Hostname != "" {
				fmt.Printf("served   %s%s\n", app.Hostname, app.Path)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "a human label for the app")
	cmd.Flags().Int32Var(&port, "port", 0, "port the app listens on")
	cmd.Flags().Int32Var(&replicas, "replicas", 0, "how many replicas to run")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "", "Dockerfile path within the source")
	cmd.Flags().StringVar(&domain, "domain", "", "hostname to serve the app at (empty to use the deployment default)")

	return cmd
}

// short abbreviates an id for a table cell.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "-"
	}
	return id
}
