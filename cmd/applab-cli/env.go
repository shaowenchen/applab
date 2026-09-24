package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/client"
)

// envCommand groups the app configuration operations.
//
// One noun with two sub-nouns, so that "is this a variable or a secret?" is
// answered by the command the person typed rather than by a flag they have to
// remember. That distinction is the whole design of the feature — a plain
// variable is fine to read back and a secret is not — so it belongs in the
// command line's shape.
//
// It is a distinction of intent, not of storage: both kinds are kept with the
// app and both reach the container as environment variables. What differs is
// what AppLab will show you.
func envCommand(urlFlag, keyFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env <app>",
		Short: "Show or change an app's configuration",
		Long: `Show or change an app's configuration.

There are two kinds of value, and which one you want matters:

  applab env set        a plain variable — LOG_LEVEL=debug, FEATURE_X=on
  applab env secret set a secret — a password, a token, a connection string

Plain variables are not sensitive: they are stored with the app, returned by the
API and the console, and visible to anyone who can read the app's Deployment.

Secrets are not returned by anything. No endpoint will ever give you their
values — AppLab can tell you which secrets an app has, never what they are — and
if you lose one you set it again.

Be clear about what that does and does not buy you. A secret reaches the
container as an environment variable, so it *is* in the Deployment's spec,
readable by anyone who can run 'kubectl get deploy -o yaml' in the app's
namespace. AppLab will not show it to you; the cluster will.

Changes take effect on the next deploy, not immediately: configuration travels
the same path as code. Run 'applab deploy <app>' when you are done.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			config, err := c.GetAppConfig(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			printConfig(os.Stdout, args[0], config)
			return nil
		},
	}

	cmd.AddCommand(
		envSetCommand(urlFlag, keyFlag),
		envUnsetCommand(urlFlag, keyFlag),
		envSecretCommand(urlFlag, keyFlag),
	)
	return cmd
}

// envSecretCommand groups the secret operations.
func envSecretCommand(urlFlag, keyFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Show or change an app's secrets",
		Long: `Show or change an app's secrets.

A secret's value cannot be read back, by anyone, through any endpoint. This
command lists which secrets an app has and lets you set or remove them — it never
shows a value, not even the one you just set.`,
		// The parent already explains what a secret is; running this bare is a
		// mistake, and the error should say what to type rather than repeat the
		// explanation.
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("expected a subcommand: `applab env secret set <app> KEY=VALUE` or `... unset <app> KEY`")
		},
	}

	cmd.AddCommand(
		envSecretSetCommand(urlFlag, keyFlag),
		envSecretUnsetCommand(urlFlag, keyFlag),
	)
	return cmd
}

// envSetCommand sets plain environment variables.
func envSetCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set <app> KEY=VALUE [KEY=VALUE...]",
		Short: "Set environment variables",
		Long: `Set environment variables.

Names not mentioned are left alone, so this adds to an app's configuration rather
than replacing it. To remove one, use 'applab env unset'.

PORT cannot be set: it comes from the app's port setting, which is also what the
Service targets. Change that with 'applab config' instead.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID := args[0]
			env, err := parsePairs(args[1:])
			if err != nil {
				return err
			}

			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			config, err := c.SetAppEnv(cmd.Context(), appID, env)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "AppLab: set %s; takes effect on the next deploy\n", plural(len(env), "variable"))
			printConfig(os.Stdout, appID, config)
			return nil
		},
	}
}

// envUnsetCommand removes one environment variable.
func envUnsetCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Remove an environment variable",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			config, err := c.DeleteAppEnv(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "AppLab: removed %s; takes effect on the next deploy\n", args[1])
			printConfig(os.Stdout, args[0], config)
			return nil
		},
	}
}

// envSecretSetCommand sets secret values.
func envSecretSetCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "set <app> KEY=VALUE [KEY=VALUE...]",
		Short: "Set secret values",
		Long: `Set secret values.

The values are sent to applab and stored with the app. They are never returned
by any endpoint, so this command's output lists the names it set and not what
they were set to — but they do reach the Deployment as environment variables, so
whoever can read that Deployment can read them.

Be aware that a value given on the command line is visible to anyone who can read
your shell's history or the process list. For anything that matters, prefer
reading it from the environment:

  applab env secret set shop DATABASE_URL="$DATABASE_URL"

Changes take effect on the next deploy.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID := args[0]
			secrets, err := parsePairs(args[1:])
			if err != nil {
				return err
			}

			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			config, err := c.SetAppSecrets(cmd.Context(), appID, secrets)
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "AppLab: set %s; takes effect on the next deploy\n", plural(len(secrets), "secret"))
			printConfig(os.Stdout, appID, config)
			return nil
		},
	}
}

// envSecretUnsetCommand removes one secret.
func envSecretUnsetCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "unset <app> KEY",
		Short: "Remove a secret",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			config, err := c.DeleteAppSecret(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "AppLab: removed %s; takes effect on the next deploy\n", args[1])
			printConfig(os.Stdout, args[0], config)
			return nil
		},
	}
}

// parsePairs turns KEY=VALUE arguments into a map.
//
// A value may contain '=' — connection strings do — so only the first one
// separates the name from the value. An empty value is allowed and means "set
// this to nothing", which is a different thing from not setting it.
func parsePairs(args []string) (map[string]string, error) {
	out := make(map[string]string, len(args))
	for _, arg := range args {
		name, value, found := strings.Cut(arg, "=")
		if !found {
			return nil, fmt.Errorf("%q is not KEY=VALUE", arg)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%q has no name before the '='", arg)
		}
		out[name] = value
	}
	return out, nil
}

// printConfig prints an app's configuration.
//
// The two halves are printed differently on purpose, and the difference is the
// point of the whole feature: variables with their values, secrets as a list of
// names. A reader can see at a glance which of their configuration is readable
// and which is not.
func printConfig(w io.Writer, appID string, config *client.AppConfig) {
	names := make([]string, 0, len(config.Env))
	for name := range config.Env {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		fmt.Fprintf(w, "%s has no environment variables\n", appID)
	} else {
		fmt.Fprintf(w, "%s environment variables\n", appID)
		for _, name := range names {
			fmt.Fprintf(w, "  %s=%s\n", name, config.Env[name])
		}
	}

	if len(config.Secrets) == 0 {
		fmt.Fprintf(w, "%s has no secrets\n", appID)
		return
	}
	fmt.Fprintf(w, "%s secrets (values are never shown)\n", appID)
	for _, name := range config.Secrets {
		fmt.Fprintf(w, "  %s\n", name)
	}
}

// plural renders "1 variable" / "3 variables".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
