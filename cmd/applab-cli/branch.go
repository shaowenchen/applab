package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// branchCommand shows and changes which branch an app runs.
//
// A noun, like `keys` and `env`: `applab branch shop` asks a question and
// `applab branch use shop dev` gives an instruction. The alternative — a
// `--branch` flag on `deploy` — would make switching a modifier of deploying
// rather than an operation in its own right, and it would leave "which branches
// are there?" with no home.
func branchCommand(urlFlag, keyFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "branch <app>",
		Short: "Show which branches an app has, and which one is running",
		Long: `Show which branch an app runs.

An app can hold several branches and runs one of them. Every branch is a
repository of its own, reachable at its own address:

    https://<host>/git/<app>.git        the active branch
    https://<host>/git/<app>@dev.git    the branch named dev

A branch comes into being when something is pushed to it — that is git's own
rule, and there is no separate "create a branch" here:

    git push https://x:$APPLAB_KEY@<host>/git/<app>@dev.git HEAD:dev

Switching which one runs is "applab branch use", and it deploys the branch it
switches to: an app that reported itself on a branch whose code was not running
would be lying about what is deployed.

Only one branch runs at a time, so switching replaces what the app is serving.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			branches, err := c.ListBranches(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			for _, branch := range branches.Branches {
				if branch == branches.Active {
					// The active one is marked rather than listed separately: a
					// reader compares the list against what they know, and the
					// mark is what makes that a glance rather than a search.
					fmt.Printf("* %s\n", branch)
					continue
				}
				fmt.Printf("  %s\n", branch)
			}
			if len(branches.Branches) == 0 {
				// Not an error: an app with no source yet has no branches, and
				// saying so names the next thing to do.
				fmt.Fprintf(cmd.ErrOrStderr(), "AppLab: %s has no branches yet; push source to it first\n", branches.AppID)
			}
			return nil
		},
	}

	cmd.AddCommand(branchUseCommand(urlFlag, keyFlag))
	return cmd
}

// branchUseCommand makes a branch active and deploys it.
func branchUseCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "use <app> <branch>",
		Short: "Switch the app to a branch and deploy it",
		Long: `Switch the app to a branch and deploy it.

The branch must already have been pushed to. The app is redeployed from the tip
of that branch, which replaces what it is currently serving — one app runs one
branch, so this is a switch rather than an addition.

The app is recorded as being on the new branch before the rollout starts, so a
rollout that fails leaves the app pointed at the branch you chose rather than
silently back on the old one. If that happens, "applab status" says so.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID, branch := args[0], args[1]

			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			result, err := c.UseBranch(cmd.Context(), appID, branch)
			if err != nil {
				return err
			}

			// The build case is reported as such rather than as a successful
			// deploy: the branch is switched and the app is being rebuilt from
			// it, and a caller told "deployed" would expect it to be serving.
			if result.Build != nil {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"AppLab: %s switched to %s; building %s\n", appID, branch, shortSHA(result.Commit))
				fmt.Fprintf(cmd.ErrOrStderr(),
					"AppLab: follow it with `applab logs %s` or `applab status %s`\n", appID, appID)
				return nil
			}

			fmt.Fprintf(cmd.ErrOrStderr(), "AppLab: %s switched to %s and deployed\n", appID, branch)
			if result.URL != "" {
				fmt.Println(result.URL)
			}
			return nil
		},
	}
}
