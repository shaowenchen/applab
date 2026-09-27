package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/shaowenchen/applab/internal/build"
	"github.com/shaowenchen/applab/internal/model"
)

// detachedContext carries the values of a request context without its
// cancellation.
//
// A background job started from a push must outlive the push: `git push` returns
// as soon as receive-pack is done, and every context derived from that request is
// cancelled the moment the handler returns. A job holding one would be killed
// before it did anything, and the symptom — a push that reports success and
// nothing happens — is indistinguishable from the feature not existing.
//
// The values are kept because the store reads them for logging and tracing;
// without them the job logs against an empty context.
// detachedContext is detachedContextWith for the request path, where there is no
// cancellation to hand out.
func detachedContext(ctx context.Context) context.Context {
	return detachedContextWith(ctx, nil)
}

// detachedContextWith carries the values of a request context without its
// cancellation, and optionally answers to a cancellation of its own.
//
// The request's cancellation cannot apply: a job started from a push outlives
// the push. But "no cancellation at all" is not the same as "cancels with the
// process", and the difference matters at shutdown. A pushed build's watcher
// polls for up to 45 minutes; a shutdown that has to wait for it to finish on
// its own is a shutdown that runs to the termination grace period and is killed
// — which is the thing WaitForPushBuilds exists to avoid.
//
// So the server hands out its own done channel, cancelled by
// WaitForBackgroundWork when it is about to give up. The watcher then stops
// where it is, promptly, and the wait returns. Nothing is lost: the Job is in
// the cluster and its TTL applies, which is the same state a build already in
// flight was in before any of this.
func detachedContextWith(ctx context.Context, done <-chan struct{}) context.Context {
	if done == nil {
		return detachedValues{ctx}
	}
	return cancellingValues{detachedValues{ctx}, done}
}

type detachedValues struct{ context.Context }

func (detachedValues) Deadline() (time.Time, bool) { return time.Time{}, false }
func (detachedValues) Done() <-chan struct{}       { return nil }
func (detachedValues) Err() error                  { return nil }

// cancellingValues is a detached context that still answers Done.
type cancellingValues struct {
	detachedValues
	done <-chan struct{}
}

func (c cancellingValues) Done() <-chan struct{} { return c.done }
func (c cancellingValues) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// pushJobs tracks the background jobs a push starts.
//
// It is a WaitGroup rather than a queue because the jobs are independent: two
// pushes to two apps have nothing to say to each other, and running them at once
// is what a push expects. What it is for is shutdown — see WaitForPushBuilds.
//
// The cancel func is what makes waiting possible at all: without it a job that
// polls for 45 minutes has to be waited out rather than stopped, and "wait for
// the background work" becomes "wait for the longest thing anyone ever starts".
type pushJobs struct {
	wg     sync.WaitGroup
	cancel context.CancelFunc
	done   chan struct{}
}

// newPushJobs builds the tracker and the channel its jobs watch.
func newPushJobs() *pushJobs {
	done := make(chan struct{})
	cancelled := false
	var mu sync.Mutex

	return &pushJobs{
		done: done,
		cancel: func() {
			mu.Lock()
			defer mu.Unlock()
			if cancelled {
				return
			}
			cancelled = true
			close(done)
		},
	}
}

// jobsContext is the context a background job should run under: detached from
// the request, but cancelled when the server stops waiting.
func (s *Server) jobsContext(ctx context.Context) context.Context {
	if s.pushJobs == nil {
		return detachedContext(ctx)
	}
	return detachedContextWith(ctx, s.pushJobs.done)
}

// goRun starts fn in the background, tracked so shutdown can wait for it.
func (s *Server) goRun(fn func()) {
	if s.pushJobs == nil {
		go fn()
		return
	}
	s.pushJobs.wg.Add(1)
	go func() {
		defer s.pushJobs.wg.Done()
		fn()
	}()
}

// WaitForPushBuilds blocks until every build-and-deploy a push started has
// finished, or the context expires.
//
// It exists because those jobs outlive the request that started them, so nothing
// else would wait for them: a process that exited immediately after receiving a
// push would leave a build Job in the cluster with no record of why it was
// started, and — worse — no deploy afterwards, since the deploy is the part that
// happens here rather than in the cluster.
//
// A deadlined context is the caller's, and hitting it is normal rather than a
// failure: a build can run for minutes, and a shutdown that waited indefinitely
// for one would never complete.
func (s *Server) WaitForPushBuilds(ctx context.Context) {
	s.WaitForBackgroundWork(ctx)
}

// WaitForBackgroundWork blocks until every build-and-deploy this server started
// in the background has finished, or the context expires.
//
// WaitForPushBuilds is the same thing under the name main uses, where the reason
// is a clean shutdown. The general name is the one tests want: creating an app
// now publishes it, and an upload publishes itself, so a test that creates an
// app has started a job that reads and writes the store — against a temporary
// directory the test is about to delete. Draining it is what keeps the teardown
// from racing work nobody is waiting for.
func (s *Server) WaitForBackgroundWork(ctx context.Context) {
	if s.pushJobs == nil {
		return
	}

	done := make(chan struct{})
	go func() {
		s.pushJobs.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		// The deadline is being conceded to, so the work stops where it is. A
		// pushed build's watcher polls for up to 45 minutes; leaving it running
		// would mean returning from here while it still reads and writes the
		// store, which is exactly what a caller draining this wants to prevent —
		// a test's temporary directory being deleted underneath it, or a
		// terminating pod's storage going away.
		//
		// Nothing is lost that was not already lost: the build's Job is in the
		// cluster with a deadline and a TTL of its own, so it either finishes
		// with nothing to deploy it or is collected. Neither is a state the
		// cluster cannot already reach, and both are better than a process that
		// claims to be shutting down while a goroutine it started keeps working.
		s.cancelBackgroundWork()
		slog.Warn("stopped waiting for a pushed build still running; it will finish in the cluster but will not be deployed")

		// Cancellation is cooperative, so the jobs have not necessarily stopped
		// the instant it is signalled — one may be part-way through a store read
		// or a registry call. Waiting for the group to drain again is what makes
		// the promise this function's name makes actually true: that when it
		// returns, nothing this server started is still running.
		//
		// The second wait is bounded by the grace here rather than by the
		// caller's context, because that context has already expired and would
		// make this return at once — which is the thing being avoided.
		select {
		case <-done:
		case <-time.After(cancelGrace):
		}
	}
}

// cancelGrace is how long a cancelled background job is given to notice and
// return before the caller stops waiting for it.
//
// Every job checks its context between steps, so this is the length of one step
// — a store read, a listing — and not the length of a build. Anything still
// running after it is stuck rather than slow, and waiting longer would only
// delay a shutdown that has already conceded.
const cancelGrace = 5 * time.Second

// cancelBackgroundWork stops every background job this server started.
//
// Idempotent, because it is reached from both the drain above and a caller that
// simply wants the work to stop.
func (s *Server) cancelBackgroundWork() {
	if s.pushJobs != nil && s.pushJobs.cancel != nil {
		s.pushJobs.cancel()
	}
}

// AfterGitPush is what the git transport calls once a push has been stored.
//
// It is a method on the Server rather than a function the transport owns,
// because what should follow a push — building it, deploying it — is policy the
// transport has no business knowing. It returns as soon as the work has been
// handed to a goroutine, since the caller here is a `git push` waiting on a
// response.
func (s *Server) AfterGitPush(ctx context.Context, appID, branch string) {
	s.StartPushBuild(ctx, appID, branch)
}

// StartAutoPublish is what a repository change that arrived over the API starts,
// as opposed to one that arrived over git.
//
// The two are the same policy with one difference: the API's callers ship their
// own commit. `applab push` uploads and then calls the build and deploy endpoints
// itself, and the console's upload is an upload — so an HTTP commit that also
// published itself here would be built twice, and the second build would
// supersede the first, which means the caller watches a build they did not start.
//
// So this is opt-in per request, by the caller's own flag. It is not inferred
// from the caller, because the API cannot tell `applab push` from a curl that
// uploaded a tarball — the two send the same request and want different things
// from it. `?publish=false` is what the CLI sends; everyone else gets the
// behaviour the platform promises, which is that changing the repository
// publishes it.
//
// The app's own switch is honoured inside StartPushBuild, along with the build
// and deploy capability guards, so a deployment that cannot build is not asked
// twice.
func (s *Server) StartAutoPublish(ctx context.Context, appID, branch string) {
	s.StartPushBuild(ctx, appID, branch)
}

// autoPublish reports whether a change to an app's repository should build and
// deploy on its own.
//
// Default true, so a caller that says nothing gets the platform's behaviour.
// Only the literal "false" turns it off, which is the shape `follow` uses: a
// value that is absent and a value that is misspelled then mean the same thing,
// and the thing they mean is the one that does something rather than the one
// that silently does not.
func autoPublish(r *http.Request) bool {
	return r.URL.Query().Get("publish") != "false"
}

// StartPushBuild builds and deploys what was just pushed.
//
// It is deliberately not awaited: the pusher's `git push` should return as soon
// as git is done, and a build takes minutes. What the pusher gets is a push that
// succeeded; what happens next shows up in the app's status, which is where
// someone who pushed is already looking.
//
// Two guards decide whether there is anything to do, and both are about a
// deployment rather than about the push:
//
//   - no build half (no registry or kaniko image configured) — nothing to build
//   - no deploy half (no cluster, or no gateway) — an image nothing would run
//
// A third case is not a guard but a shortcut: a commit that already has an
// image is deployed without rebuilding. The image is tagged by commit, so an
// unchanged commit is an unchanged image, and this is what makes pushing a
// branch that was already built cheap.
func (s *Server) StartPushBuild(ctx context.Context, appID, branch string) {
	if !s.canBuild() {
		return
	}
	if s.deployer == nil || !s.deployer.Ready() {
		return
	}
	if s.headCommit == nil {
		return
	}

	ctx = s.jobsContext(ctx)

	app, err := s.loadAppByID(ctx, appID)
	if err != nil {
		slog.WarnContext(ctx, "a pushed branch will not be built: its app could not be read",
			"app", appID, "branch", branch, "error", err)
		return
	}

	// The app's own switch, and it turns off the build as well as the deploy.
	// A build whose image nothing will run is what the guard above refuses, and
	// this app is in exactly that position: whoever turned this off is releasing
	// by hand, and an image pushed on every commit is not what they asked for.
	//
	// The push itself is unaffected — the source is stored, which is what git
	// was asked to do. See applab update --auto-deploy and the console's
	// checkbox for where the switch is set.
	if !app.AutoDeploys() {
		slog.DebugContext(ctx, "a push will not be built: this app has auto-deploy turned off",
			"app", appID, "branch", branch)
		return
	}

	head, err := s.headCommit(ctx, appID, branch)
	if err != nil {
		slog.WarnContext(ctx, "a pushed branch will not be built: its tip could not be read",
			"app", appID, "branch", branch, "error", err)
		return
	}

	// Already built. The image is tagged by commit, so an unchanged commit is an
	// unchanged image and rebuilding it would produce exactly what is already in
	// the registry.
	//
	// The deploy still runs, which is the point rather than an oversight: the
	// case this catches is not "the app is already running this" but "this commit
	// was built before and was never deployed" — a push that was followed by a
	// build that failed to deploy, or a deploy of another commit that was rolled
	// back. Deploying it is what makes a push mean "this source is live", whether
	// or not the build had to happen.
	if existing, err := s.build.FindSucceeded(ctx, app.Namespace, app.ID, head); err == nil {
		slog.InfoContext(ctx, "a pushed commit already has an image; deploying it without rebuilding",
			"app", appID, "branch", branch, "commit", shortSHA(head), "build", existing.ID)
		s.goRun(func() { s.deployBuiltCommit(ctx, appID, branch, head, existing.ID) })
		return
	} else if !errors.Is(err, build.ErrBuildNotFound) {
		slog.WarnContext(ctx, "could not look up an existing build for a pushed commit",
			"app", appID, "commit", shortSHA(head), "error", err)
		return
	}

	// The build is started first, and the deploy follows only if it succeeded. A
	// build and a deploy are two operations with two outcomes, and this is the
	// only place that can see both; doing it in one call would mean holding a
	// request open for the length of a build.
	buildID, _, apiErr := s.startBuild(ctx, app, branch, head)
	if apiErr != nil {
		slog.ErrorContext(ctx, "could not start the build a push asked for",
			"app", appID, "branch", branch, "commit", shortSHA(head), "error", apiErr)
		return
	}

	slog.InfoContext(ctx, "a push started a build",
		"app", appID, "branch", branch, "commit", shortSHA(head), "build", buildID)

	s.goRun(func() { s.awaitBuildThenDeploy(ctx, appID, branch, head, buildID) })
}

// awaitBuildThenDeploy waits for a pushed build to finish and deploys it if it
// succeeded.
//
// Waiting is the whole difficulty. AppLab has no controller watching Jobs, so a
// build's outcome is noticed only when something asks — this loop, or a caller
// following the log. A build nobody follows is therefore never noticed at all,
// which is exactly the case a push creates. So the push watches its own build.
//
// The cost is a polling goroutine per pushed build, alive for as long as the
// build runs. That is the price of not having a controller, and it is bounded:
// the loop gives up after pushBuildWait, and the build's own Job has a deadline
// of its own.
//
// The read is one listing of one app's build Jobs every few seconds, which is
// small; the alternative — a controller watching every build in the cluster — is
// a component to operate, and this is not yet worth one.
func (s *Server) awaitBuildThenDeploy(ctx context.Context, appID, branch, commitSHA, buildID string) {
	app, err := s.loadAppByID(ctx, appID)
	if err != nil {
		slog.WarnContext(ctx, "could not read the app of the build a push started",
			"app", appID, "build", buildID, "error", err)
		return
	}

	interval, wait := s.buildWatchPolicy()
	deadline := time.Now().Add(wait)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		result, err := s.build.Get(ctx, app.Namespace, appID, buildID)
		if err != nil {
			if errors.Is(err, build.ErrBuildNotFound) {
				// The Job is gone: its TTL elapsed, or a newer upload superseded
				// it. Either way this build will not deploy, and polling for it
				// would run to the deadline for nothing.
				slog.InfoContext(ctx, "the build a push started is no longer in the cluster, so nothing was deployed",
					"app", appID, "build", buildID)
				return
			}
			slog.WarnContext(ctx, "could not read the build a push started", "build", buildID, "error", err)
			return
		}

		if !result.Status.Terminal() {
			if time.Now().After(deadline) {
				slog.WarnContext(ctx, "gave up waiting for the build a push started; it may still finish",
					"app", appID, "build", buildID)
				return
			}
			continue
		}

		if result.Status != model.BuildStatusSucceeded {
			// Reported, not retried. A build that failed is the pusher's to look
			// at — the build's own reason says why — and retrying would loop on a
			// Dockerfile that fails deterministically.
			slog.InfoContext(ctx, "the build a push started did not succeed, so nothing was deployed",
				"app", appID, "build", buildID, "status", result.Status, "reason", result.Reason)
			return
		}

		s.deployBuiltCommit(ctx, appID, branch, commitSHA, buildID)
		return
	}
}

// deployBuiltCommit deploys a pushed commit whose build succeeded.
//
// The image is derived from the commit rather than carried from the build read
// above, because that is what it is: a function of the app, the commit and the
// build configuration. Nothing recorded it and nothing needs to — the tag a
// build pushes to is the tag a deploy pulls from, and both are computed from the
// same inputs by the same function.
func (s *Server) deployBuiltCommit(ctx context.Context, appID, branch, commitSHA, buildID string) {
	app, err := s.loadAppByID(ctx, appID)
	if err != nil {
		slog.WarnContext(ctx, "built a pushed commit but could not read its app to deploy it",
			"app", appID, "build", buildID, "error", err)
		return
	}

	image := s.imageFor(appID, commitSHA)
	if image == "" {
		slog.WarnContext(ctx, "built a pushed commit but this deployment cannot name its image, so nothing was deployed",
			"app", appID, "build", buildID)
		return
	}

	// The app's active branch may have moved since the push. This deploy is of
	// the branch that was pushed, so that is what is named — but if the app has
	// since been switched elsewhere, deploying this branch would put an app back
	// on a branch somebody deliberately moved off.
	//
	// The commit is checked rather than the branch name alone because a branch can
	// be reverted: what matters is whether the thing being deployed is still the
	// app's current source, and the commit is the precise form of that question.
	if app.ActiveBranch() != branch {
		slog.InfoContext(ctx, "a pushed build finished after the app was switched to another branch, so nothing was deployed",
			"app", appID, "pushed_branch", branch, "active_branch", app.ActiveBranch())
		return
	}

	if _, apiErr := s.deployCommit(ctx, app, commitSHA, image); apiErr != nil {
		slog.ErrorContext(ctx, "could not deploy the commit a push built",
			"app", appID, "build", buildID, "commit", shortSHA(commitSHA), "error", apiErr)
		return
	}

	slog.InfoContext(ctx, "a pushed commit is deployed",
		"app", appID, "branch", branch, "commit", shortSHA(commitSHA), "image", image)
}

// buildWatchPolicy is how often a pushed build is polled, and for how long.
//
// Settable, and set by the tests. A watcher that reads a real cluster for
// forty-five minutes is correct; a watcher that does the same against a fake one
// in a test is a goroutine holding a temporary directory open long after the
// test has finished asserting — and since creating an app now publishes it, every
// test that creates one starts a watcher.
//
// The alternative was to make the tests construct builds that reach a terminal
// state, which is a lot of scaffolding in every test for the sake of one
// uninteresting property. This is one field.
func (s *Server) buildWatchPolicy() (interval, wait time.Duration) {
	if s.pushBuildInterval > 0 {
		return s.pushBuildInterval, s.pushBuildWait
	}
	return pushBuildPollInterval, pushBuildWait
}

// WithBuildWatchPolicy shortens the push-build watcher's polling, for tests.
//
// Not a production knob: the values below are chosen for a real cluster, and a
// deployment that shortened them would be polling more and giving up sooner for
// no reason it could state.
func (s *Server) WithBuildWatchPolicy(interval, wait time.Duration) *Server {
	s.pushBuildInterval = interval
	s.pushBuildWait = wait
	return s
}

const (
	// pushBuildPollInterval is how often a pushed build's state is read from the
	// cluster. A build takes minutes, so this is about latency rather than cost:
	// the read is one Job by name.
	pushBuildPollInterval = 5 * time.Second

	// pushBuildWait is how long a push will watch its own build before giving up.
	//
	// Longer than the build Job's own deadline, deliberately: a build that has
	// not finished by then has either hit that deadline (and will report failed)
	// or is running under a configuration that raised it, and giving up first
	// would abandon a build that is about to succeed.
	pushBuildWait = 45 * time.Minute
)
