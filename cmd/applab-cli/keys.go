package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// keysCommand groups the per-app key operations.
//
// It is a parent command rather than two verbs at the top level because "keys"
// is the noun a person reaches for: `applab keys shop` reads as asking a
// question, and `applab keys rotate shop` as giving an instruction.
func keysCommand(urlFlag, keyFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys <app>",
		Short: "Show an app's API key",
		Long: `Show an app's API key.

Every app has its own key, which reaches that app and nothing else. It is what
to give whoever deploys the app: they can push, build, deploy and roll back, but
cannot delete the app and cannot touch any other.

The same key works everywhere — the API, the CLI and the console — so one
credential covers all three.

An admin key may read any app's; an app key may read its own.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			key, err := c.GetAppKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			// The key alone on stdout, so it can be captured by a shell without
			// being picked out of a sentence:
			//
			//   export APPLAB_KEY="$(applab keys shop)"
			fmt.Println(key.Key)
			return nil
		},
	}

	cmd.AddCommand(keysRotateCommand(urlFlag, keyFlag))
	return cmd
}

// keysRotateCommand replaces an app's key.
func keysRotateCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate <app>",
		Short: "Replace an app's key, invalidating the old one",
		Long: `Replace an app's key.

The previous key stops working immediately — there is no grace period, because a
rotation is usually performed because a key leaked, and a key that still works
after being rotated away from has not been rotated.

Anything using the old key must be updated. Reads and writes to the applab API
are the obvious ones: a CI job holding the key, and any developer who copied it
into a shell profile.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			key, err := c.RotateAppKey(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			// The warning goes to stderr and the key to stdout, so a shell can
			// capture the key while a person still sees what just happened to
			// the old one:
			//
			//   export APPLAB_KEY="$(applab keys rotate shop)"
			fmt.Fprintf(os.Stderr, "applab: the previous key for %s no longer works; anything using it must be updated\n", args[0])
			fmt.Println(key.Key)
			return nil
		},
	}
}
