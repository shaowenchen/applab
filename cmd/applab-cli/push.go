package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/shaowenchen/applab/internal/client"
	"github.com/shaowenchen/applab/internal/model"
)

// pushOptions are the flags shared by push and deploy.
type pushOptions struct {
	app        string
	portSet    bool
	message    string
	dir        string
	port       int32
	replicas   int32
	dockerfile string
	domain     string
	create     bool
	watch      bool
	noDeploy   bool
	skip       []string
}

// pushCommand uploads the current directory and ships it.
//
// This is the command the whole platform exists for. It does the four things
// that otherwise each need their own tool — create the app if it is new, package
// the source, upload it, and follow the build through to a running URL — so
// going from an edited file to a live change is one command.
func pushCommand(urlFlag, keyFlag *string) *cobra.Command {
	opts := &pushOptions{}

	cmd := &cobra.Command{
		Use:   "push [app]",
		Short: "Upload the current directory, build it and deploy it",
		Long: `Upload the current directory, build it and deploy it.

The app is created if it does not exist, so the first run on a new project needs
nothing set up beforehand:

    applab push myapp

Source is packaged from --dir (the current directory by default), with build
output and version-control directories left out — see --skip.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(*urlFlag, *keyFlag)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				opts.app = args[0]
			}
			// Recorded here rather than inferred from a non-zero value: --port 0
			// is a mistake worth refusing, and a zero that means "not given" and
			// a zero that means "asked for port 0" are the same number.
			opts.portSet = cmd.Flags().Changed("port")
			return runPush(cmd.Context(), c, opts)
		},
	}

	cmd.Flags().StringVar(&opts.app, "app", "", "app id (or pass it as an argument)")
	cmd.Flags().StringVarP(&opts.message, "message", "m", "", "commit message (default: a generated one)")
	cmd.Flags().StringVarP(&opts.dir, "dir", "C", ".", "directory to upload")
	cmd.Flags().Int32Var(&opts.port, "port", 0, "port the app listens on, 1-65535 (default 80, or keep the existing value)")
	cmd.Flags().Int32Var(&opts.replicas, "replicas", 0, "how many replicas to run")
	cmd.Flags().StringVar(&opts.dockerfile, "dockerfile", "", "Dockerfile path within the source (default: Dockerfile)")
	cmd.Flags().StringVar(&opts.domain, "domain", "", "hostname to serve the app at (default: <app>.<base domain>)")
	cmd.Flags().BoolVar(&opts.create, "create", true, "create the app if it does not exist")
	cmd.Flags().BoolVar(&opts.watch, "watch", true, "follow the build and wait for the deploy to finish")
	cmd.Flags().BoolVar(&opts.noDeploy, "no-deploy", false, "upload and build, but do not deploy")
	cmd.Flags().StringSliceVar(&opts.skip, "skip", nil, "additional directories to leave out of the upload")

	return cmd
}

func runPush(ctx context.Context, c *client.Client, opts *pushOptions) error {
	if strings.TrimSpace(opts.app) == "" {
		// Falling back to the directory name is what makes `applab push` with no
		// argument work, which is the shortest path to shipping a change.
		inferred, err := inferAppID(opts.dir)
		if err != nil {
			return err
		}
		opts.app = inferred
		fmt.Fprintf(os.Stderr, "AppLab: using app %q from the directory name\n", opts.app)
	}

	if err := validateAppIDLocally(opts.app); err != nil {
		return err
	}

	// Reachability first, so a wrong URL is reported as such rather than as a
	// confusing authentication failure.
	cfg, err := c.Config(ctx)
	if err != nil {
		return fmt.Errorf("cannot reach the AppLab deployment: %w", err)
	}

	// The app has to exist before its source can be uploaded to it. Creating it
	// here is what makes the first push of a new project a single command.
	app, err := c.GetApp(ctx, opts.app)
	if err != nil {
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 404 {
			return err
		}
		if !opts.create {
			return fmt.Errorf("app %q does not exist and --create=false; create it with: applab create %s", opts.app, opts.app)
		}

		req := client.CreateAppRequest{ID: opts.app, Dockerfile: opts.dockerfile, Domain: opts.domain}
		// Checked rather than tested for zero, so a mistyped --port 0 is refused
		// rather than dropped: creating the app on a port the caller did not ask
		// for, silently, is worse than saying no.
		if opts.port > 0 || opts.portSet {
			if err := model.ValidatePort(opts.port); err != nil {
				return err
			}
			req.Port = &opts.port
		}
		if opts.replicas > 0 {
			req.Replicas = &opts.replicas
		}

		app, err = c.CreateApp(ctx, req)
		if err != nil {
			return fmt.Errorf("create app %q: %w", opts.app, err)
		}
		fmt.Fprintf(os.Stderr, "AppLab: created app %q\n", app.ID)
	} else if err := applySettingsIfChanged(ctx, c, app, opts); err != nil {
		return err
	}

	// The build and deploy halves are optional in a deployment, and a push that
	// uploaded source to one that cannot build would report success and change
	// nothing. Saying so here is more useful than a later failure.
	if !opts.noDeploy && !cfg.Capabilities["build"] {
		return fmt.Errorf("this deployment cannot build (no registry or kaniko image configured); upload with --no-deploy, or ask the operator to enable the build pipeline")
	}

	if err := uploadDirectory(ctx, c, opts, cfg); err != nil {
		return err
	}

	if opts.noDeploy {
		fmt.Printf("uploaded %s\n", opts.app)
		return nil
	}

	return shipCommit(ctx, c, opts)
}

// applySettingsIfChanged updates an app whose flags differ from what it has.
//
// Only the flags the caller actually passed are considered, so a push that only
// uploads source never disturbs the port or replica count someone set
// deliberately — which is what makes `applab push` safe to run habitually.
func applySettingsIfChanged(ctx context.Context, c *client.Client, app *client.App, opts *pushOptions) error {
	req := client.UpdateAppRequest{}
	changed := false

	if (opts.port > 0 || opts.portSet) && opts.port != app.Port {
		if err := model.ValidatePort(opts.port); err != nil {
			return err
		}
		req.Port = &opts.port
		changed = true
	}
	if opts.replicas > 0 && opts.replicas != app.Replicas {
		req.Replicas = &opts.replicas
		changed = true
	}
	if opts.dockerfile != "" && opts.dockerfile != app.Dockerfile {
		req.Dockerfile = &opts.dockerfile
		changed = true
	}
	if opts.domain != "" && opts.domain != app.Domain {
		req.Domain = &opts.domain
		changed = true
	}

	if !changed {
		return nil
	}
	_, err := c.UpdateApp(ctx, app.ID, req)
	return err
}

// uploadDirectory packages and uploads the source.
func uploadDirectory(ctx context.Context, c *client.Client, opts *pushOptions, cfg *client.Config) error {
	dir, err := filepath.Abs(opts.dir)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", opts.dir, err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	// The Dockerfile is checked before uploading: a build fails without one, and
	// finding out after the upload wastes the transfer and reports the problem
	// from the wrong place.
	dockerfile := opts.dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if _, err := os.Stat(filepath.Join(dir, dockerfile)); err != nil {
		return fmt.Errorf("no %s in %s\n\napplab builds from a Dockerfile. Create one, or point at a different one with --dockerfile", dockerfile, dir)
	}

	skip := append(append([]string{}, client.DefaultSkipDirs...), opts.skip...)

	// The archive is built into a pipe so a large tree never has to fit in
	// memory, and the upload streams from the other end. The goroutine reports
	// its error through a channel because a failure mid-archive has to reach the
	// caller rather than silently truncating the upload.
	pr, pw := io.Pipe()
	archiveErr := make(chan error, 1)

	go func() {
		err := client.ArchiveDir(ctx, dir, pw, skip)
		// CloseWithError propagates the failure to the reader, which is what
		// makes a broken archive fail the upload instead of sending a truncated
		// one that would commit half a source tree.
		pw.CloseWithError(err)
		archiveErr <- err
	}()

	fmt.Fprintf(os.Stderr, "AppLab: uploading %s\n", dir)

	result, err := c.UploadSource(ctx, opts.app, pr, true, opts.message)
	if err != nil {
		// Drain the pipe so the archiving goroutine is not left blocked on a
		// write nobody will read.
		pr.CloseWithError(err)
		<-archiveErr
		return fmt.Errorf("upload source: %w", err)
	}
	if archiveErr := <-archiveErr; archiveErr != nil {
		return fmt.Errorf("package source: %w", archiveErr)
	}

	if result.StrippedRoot != "" {
		fmt.Fprintf(os.Stderr, "AppLab: stripped the wrapping directory %q\n", result.StrippedRoot)
	}
	if result.Files == 0 {
		return fmt.Errorf("no files were uploaded; check --skip and --dir")
	}

	fmt.Fprintf(os.Stderr, "AppLab: committed %s (%d files, %s)\n",
		shortSHA(result.CommitSHA), result.Files, humanBytes(result.Bytes))

	// Recorded so shipCommit does not have to look it up.
	opts.message = result.CommitSHA
	return nil
}

// shipCommit builds and deploys, then waits for the result.
//
// It is separate from uploadDirectory so that --no-deploy stops cleanly after the
// upload, and so the sequencing — build, wait, deploy, wait — reads as what it
// is rather than as a pile of nested conditionals.
func shipCommit(ctx context.Context, c *client.Client, opts *pushOptions) error {
	commitSHA := opts.message // set by uploadDirectory

	fmt.Fprintf(os.Stderr, "AppLab: building %s\n", shortSHA(commitSHA))

	build, err := c.StartBuild(ctx, opts.app, commitSHA)
	if err != nil {
		return fmt.Errorf("start build: %w", err)
	}

	if opts.watch {
		if err := watchBuild(ctx, c, opts.app, build.ID); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "AppLab: build %s started; follow it with: applab builds %s --logs\n", build.ID, opts.app)
		return nil
	}

	deployed, err := c.Deploy(ctx, opts.app, commitSHA, false)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}

	if opts.watch {
		if err := watchDeploy(ctx, c, opts.app); err != nil {
			// A rollout that did not complete is not a failed push — the deploy
			// was issued. It is reported so the caller knows to look, and the
			// exit code stays zero because nothing about their push was wrong.
			fmt.Fprintf(os.Stderr, "AppLab: %v\n", err)
		}
	}

	if deployed.URL != "" {
		fmt.Println(deployed.URL)
	} else if deployed.Host != "" {
		// With a path prefix the host alone is not the whole address, so it is
		// reported as it was given rather than silently joined without one.
		fmt.Println(deployed.Host + deployed.Path)
	} else {
		fmt.Printf("%s deployed\n", opts.app)
	}
	return nil
}

// watchBuild follows a build to completion, printing its log.
func watchBuild(ctx context.Context, c *client.Client, appID, buildID string) error {
	// The log follows the build and ends when it does, so it is streamed
	// directly rather than polled — the server closes the response itself.
	err := c.BuildLogs(ctx, appID, buildID, os.Stderr, true)
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("follow build log: %w", err)
	}

	// The stream ending means the build stopped; its verdict is read from the
	// build itself rather than inferred from the log's contents.
	build, err := c.GetBuild(ctx, appID, buildID)
	if err != nil {
		return fmt.Errorf("read build result: %w", err)
	}

	switch build.Status {
	case "succeeded":
		fmt.Fprintf(os.Stderr, "AppLab: build succeeded\n")
		return nil
	case "failed":
		return fmt.Errorf("build failed: %s", build.Reason)
	default:
		// The stream ended without the build reaching a terminal state — the
		// connection dropped, or the process was interrupted. Not a failure of
		// the build, so it is reported without claiming one.
		fmt.Fprintf(os.Stderr, "AppLab: build is still %s; check it with: applab builds %s\n", build.Status, appID)
		return nil
	}
}

// watchDeploy waits for a rollout to finish.
func watchDeploy(ctx context.Context, c *client.Client, appID string) error {
	deadline := time.Now().Add(5 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastReady int32 = -1

	for {
		status, err := c.AppStatus(ctx, appID)
		if err != nil {
			return fmt.Errorf("read status: %w", err)
		}

		if status.Live != nil {
			// Progress is printed only when it changes, so a slow rollout does
			// not fill the terminal with the same line.
			if status.Live.ReadyReplicas != lastReady {
				fmt.Fprintf(os.Stderr, "AppLab: %d/%d replicas ready\n",
					status.Live.ReadyReplicas, status.Live.DesiredReplicas)
				lastReady = status.Live.ReadyReplicas
			}

			if status.Live.Available {
				return nil
			}
			// A rollout that cannot progress will never become available, so
			// waiting out the deadline would only delay the bad news.
			if status.Live.Message != "" {
				return fmt.Errorf("the rollout is not progressing: %s", status.Live.Message)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("the app did not become ready within 5m; check it with: applab diagnose %s", appID)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// inferAppID derives an app id from a directory name.
//
// The name is lowercased and stripped of anything an id may not contain, because
// a directory called "My_App" is common and refusing it would make the no-argument
// form of push useless exactly when it is most convenient.
func inferAppID(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}

	name := strings.ToLower(filepath.Base(abs))

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ' ':
			b.WriteRune('-')
		}
	}

	id := strings.Trim(b.String(), "-")
	// Repeated separators would otherwise leave runs of dashes.
	for strings.Contains(id, "--") {
		id = strings.ReplaceAll(id, "--", "-")
	}

	if id == "" {
		return "", fmt.Errorf("cannot infer an app id from the directory name %q; pass one: applab push <app>", filepath.Base(abs))
	}
	if len(id) > 40 {
		id = strings.Trim(id[:40], "-")
	}
	return id, nil
}

// validateAppIDLocally checks the id against the same rule the server applies,
// so a typo is reported before an upload rather than after it.
func validateAppIDLocally(id string) error {
	if id == "" {
		return fmt.Errorf("app id must not be empty")
	}
	if len(id) > 40 {
		return fmt.Errorf("app id %q is longer than 40 characters", id)
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(id)-1 {
				return fmt.Errorf("app id %q must not start or end with a dash", id)
			}
		default:
			return fmt.Errorf("app id %q may contain only lowercase letters, digits and dashes", id)
		}
	}
	return nil
}

// humanBytes renders a byte count the way a person reads it.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// shortSHA abbreviates a commit for display.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
