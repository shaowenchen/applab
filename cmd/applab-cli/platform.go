package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/client"
)

// platformCommand reads AppLab's own pods and log — the deployment the client is
// pointed at, not the apps it manages.
//
// The commands beside it answer "why is this app broken"; this one answers "why
// is *AppLab* broken", which is the question that has no other answer from a
// terminal. A build that never starts, a deploy that silently does nothing, a
// push that is accepted and then goes nowhere: the app's own log is empty or
// cheerful in all three, because the control plane is what failed.
//
// One command with a subcommand, rather than two: they are read together, and
// the pods list is usually just what tells you which instance's log to ask for.
func platformCommand(urlFlag, keyFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "platform",
		Short: "Show AppLab's own pods and log",
		Long: `Show AppLab's own pods and log.

These describe the deployment serving you rather than any app it manages:
` + "`platform pods`" + ` lists the control plane's instances, and ` + "`platform logs`" + `
reads its log. Both are admin-key only — the control plane runs alongside every
app, and its log names them.`,
	}

	cmd.AddCommand(platformPodsCommand(urlFlag, keyFlag), platformLogsCommand(urlFlag, keyFlag))
	return cmd
}

// platformPodsCommand lists AppLab's own pods.
func platformPodsCommand(urlFlag, keyFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pods",
		Short: "List AppLab's own pods",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			list, err := c.SelfPods(cmd.Context())
			if err != nil {
				return err
			}

			if len(list.Pods) == 0 {
				fmt.Println("no AppLab pods found; it may be mid-rollout, or the namespace may be wrong")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "POD\tREADY\tRESTARTS\tPHASE\tREASON")
			for _, pod := range list.Pods {
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
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", pod.Name, ready, pod.Restarts, pod.Phase, reason)
			}
			w.Flush()
			return nil
		},
	}
	return cmd
}

// platformLogsCommand streams AppLab's own log.
func platformLogsCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		follow    bool
		pod       string
		container string
		tail      int
		previous  bool
		since     string
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show AppLab's own log",
		Long: `Show AppLab's own log.

Follows by default, so it behaves like tail -f. Use --previous to read the last
container instance, which is where a crash loop's reason is written.`,
		Args: cobra.NoArgs,
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

			// A cancelled follow is how every stream ends, so it is not an error.
			err = c.SelfLogs(cmd.Context(), opts, os.Stdout, follow)
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
