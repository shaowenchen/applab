package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/client"
)

// serversCommand is the group for the other AppLab deployments this one can
// manage.
//
// It is the terminal half of a capability whose browser half is the console's
// Servers view: an administrator registers a remote by address and admin key, and
// from then on the remote's apps can be listed, created and deleted without
// leaving this deployment. "local" is always present and means this deployment
// itself.
func serversCommand(urlFlag, keyFlag *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "servers",
		Aliases: []string{"server"},
		Short:   "Manage the AppLab deployments this one can reach",
		Long: `Manage the other AppLab deployments this one can reach.

A server is registered by its address and an admin key. Once registered, its apps
can be listed, created and deleted from here — the request is relayed by this
deployment, which holds the key; the browser console cannot reach a second
deployment directly, so it goes through the same relay.

    applab servers                    the deployments this one can reach
    applab servers add lab-2 --url https://applab-2.example.com --key <admin key>
    applab servers remove lab-2

Then point any command at one of them with --server:

    applab list --server lab-2
    applab create shop --server lab-2
    applab delete shop --server lab-2 --yes

"local" is this deployment, and is always in the list.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServersList(cmd, *urlFlag, *keyFlag)
		},
	}

	cmd.AddCommand(
		serversListCommand(urlFlag, keyFlag),
		serversAddCommand(urlFlag, keyFlag),
		serversRemoveCommand(urlFlag, keyFlag),
	)
	return cmd
}

func serversListCommand(urlFlag, keyFlag *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the AppLab deployments this one can reach",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServersList(cmd, *urlFlag, *keyFlag)
		},
	}
}

func runServersList(cmd *cobra.Command, urlFlag, keyFlag string) error {
	c, err := newClient(urlFlag, keyFlag)
	if err != nil {
		return err
	}

	servers, err := c.ListServers(cmd.Context())
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tNAME\tADDRESS")
	for _, s := range servers {
		name := s.Name
		if s.Builtin {
			name = "(this deployment)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", s.ID, name, s.URL)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// A deployment with no cluster cannot keep registrations, and an empty list
	// would read as "none registered" rather than "registrations are unavailable
	// here". The local entry is the tell: it is always present.
	if len(servers) <= 1 {
		fmt.Println("\nno other servers are registered; add one with: applab servers add <id> --url <address> --key <admin key>")
	}
	return nil
}

func serversAddCommand(urlFlag, keyFlag *string) *cobra.Command {
	var (
		url  string
		key  string
		name string
	)

	cmd := &cobra.Command{
		Use:   "add <id>",
		Short: "Register another AppLab deployment",
		Long: `Register another AppLab deployment by its address and an admin key.

The address is checked and the key verified against it before anything is stored,
so a wrong address or a key that is not an admin key is refused here rather than
failing later. The address must include any path prefix the remote is served
under — a deployment served at "/applab" is registered as
"https://host/applab", not "https://host".

The key is read from --key, or from APPLAB_SERVER_KEY. Prefer the environment
variable: an argument is recorded in the shell's history and in the process list.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}

			remoteKey := key
			if remoteKey == "" {
				remoteKey = os.Getenv("APPLAB_SERVER_KEY")
			}
			if strings.TrimSpace(url) == "" {
				return fmt.Errorf("an address is required: pass --url")
			}
			if strings.TrimSpace(remoteKey) == "" {
				return fmt.Errorf("an admin key for the remote is required: pass --key or set APPLAB_SERVER_KEY")
			}

			server, err := c.RegisterServer(cmd.Context(), client.RegisterServerRequest{
				ID:   args[0],
				Name: name,
				URL:  url,
				Key:  remoteKey,
			})
			if err != nil {
				return err
			}

			fmt.Printf("registered %s at %s\n", server.ID, server.URL)
			fmt.Printf("manage its apps with: applab list --server %s\n", server.ID)
			return nil
		},
	}

	cmd.Flags().StringVar(&url, "url", "", "the remote's address, path prefix included (required)")
	cmd.Flags().StringVar(&key, "key", "", "an admin key for the remote (default $APPLAB_SERVER_KEY)")
	cmd.Flags().StringVar(&name, "name", "", "a label for the server (default: its id)")
	return cmd
}

func serversRemoveCommand(urlFlag, keyFlag *string) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Forget a registered deployment",
		Long: `Forget a registered deployment.

This removes the registration here and nothing else — the remote and its apps are
untouched, and the remote does not need to be reachable. "local" cannot be
removed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			if !yes {
				// A plain y/N rather than the type-the-id prompt `delete` uses:
				// forgetting a registration destroys nothing on the remote — it
				// can be added again — so a heavier prompt would be friction
				// without a matching risk.
				fmt.Fprintf(os.Stderr, "Remove the registration for %s? The remote and its apps are untouched. [y/N] ", args[0])
				var answer string
				fmt.Fscanln(os.Stdin, &answer)
				switch strings.ToLower(strings.TrimSpace(answer)) {
				case "y", "yes":
				default:
					fmt.Println("cancelled")
					return nil
				}
			}
			if err := c.RemoveServer(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("removed %s\n", args[0])
			return nil
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	return cmd
}
