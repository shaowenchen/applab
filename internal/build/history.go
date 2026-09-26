package build

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/shaowenchen/applab/internal/k8s"
	"github.com/shaowenchen/applab/internal/model"
)

// Result is one build, as the cluster records it.
//
// It is a value rather than a *model.Build because it is not AppLab's record of
// anything: it is what a build Job says about itself, read back. There is no
// store to write it to and nothing that could disagree with it, and the shape
// being separate is what keeps that visible — the moment this became a
// model.Build, it would look like something that could be saved.
//
// The fields are the ones a caller renders. There is deliberately no Image: an
// image is not a fact about a Job, it is derived from the app, the commit and
// the build configuration, and it is asked for through ImageFor by whoever needs
// it. Recording it on the Job would make it a second answer to a question that
// already has one, and a registry change would leave the two disagreeing.
type Result struct {
	ID        string
	AppID     string
	CommitSHA string
	Branch    string
	JobName   string
	Status    model.BuildStatus
	Reason    string

	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// List returns an app's builds, newest first.
//
// It reads the build Jobs themselves, which is where AppLab's build history
// lives now. Nothing is recorded in the object store, so this is not a cache of
// the cluster — it is the record, and a cluster that has collected a Job has
// collected the build with it.
//
// A limited list is what the console and the CLI show. The limit is applied
// after sorting rather than by asking the API server for a page, because Jobs
// carry no ordering the server could sort by: a Job's creation time is its own,
// and the API server sorts by name for a list of this kind.
func (e *Engine) List(ctx context.Context, namespace, appID string, limit int) ([]Result, error) {
	jobs, err := e.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelApp + "=" + appID + "," + k8s.LabelBuild,
	})
	if err != nil {
		return nil, fmt.Errorf("list build jobs in %s: %w", namespace, err)
	}

	out := e.results(ctx, namespace, jobs.Items)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListAll returns every app's builds in the namespace, newest first.
//
// The cross-app counterpart to List, for the overview's recent-builds panel. One
// list answers for the whole platform because every build Job carries the app
// label — the same reason Deployer.Statuses can read every app's Deployment at
// once.
func (e *Engine) ListAll(ctx context.Context, namespace string, limit int) ([]Result, error) {
	jobs, err := e.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelBuild,
	})
	if err != nil {
		return nil, fmt.Errorf("list build jobs in %s: %w", namespace, err)
	}

	out := e.results(ctx, namespace, jobs.Items)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get returns one build.
//
// It is looked up by its build label rather than by its Job name, because the
// name is derived and truncated: two long app ids could produce the same Job
// name, and a lookup that depended on the derivation would then answer for the
// wrong build. The label holds the id itself, so it cannot.
func (e *Engine) Get(ctx context.Context, namespace, appID, buildID string) (*Result, error) {
	jobs, err := e.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelApp + "=" + appID + "," + k8s.LabelBuild + "=" + buildID,
	})
	if err != nil {
		return nil, fmt.Errorf("read build %s in %s: %w", buildID, namespace, err)
	}
	if len(jobs.Items) == 0 {
		return nil, ErrBuildNotFound
	}

	out := e.results(ctx, namespace, jobs.Items)
	if len(out) == 0 {
		return nil, ErrBuildNotFound
	}
	return &out[0], nil
}

// FindSucceeded returns the most recent successful build of a commit.
//
// It is what makes a rollback reuse an image instead of rebuilding a
// byte-identical one, and what lets a push skip rebuilding a commit it has
// already built. Only a succeeded build qualifies: anything else has nothing
// deployed behind it.
//
// The window is therefore the Job's lifetime, which is what build.ttl_after_finished
// sets. A commit whose Job has been collected is a commit this cannot find, and
// the caller rebuilds — which is the honest outcome, since a rebuild produces
// the same image for the same commit.
func (e *Engine) FindSucceeded(ctx context.Context, namespace, appID, commitSHA string) (*Result, error) {
	builds, err := e.List(ctx, namespace, appID, 0)
	if err != nil {
		return nil, err
	}
	for i := range builds {
		// Newest first, so the first match is the most recent one.
		if builds[i].CommitSHA == commitSHA && builds[i].Status == model.BuildStatusSucceeded {
			return &builds[i], nil
		}
	}
	return nil, ErrBuildNotFound
}

// Unfinished returns an app's builds that have not reached a terminal state.
//
// It is what tells a new build apart from the one already in flight, so an
// upload can supersede it: two builds of one app running at once would race to
// push the same image tag, and which one won would be whichever finished last.
//
// It reads only the phase, never a failure's reason. The question is "is this
// still running", and answering it by assembling the full status of every failed
// build would read the cluster once per failed build in history — the whole
// history, for a call that only wants the ones still going.
func (e *Engine) Unfinished(ctx context.Context, namespace, appID string) ([]Result, error) {
	jobs, err := e.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelApp + "=" + appID + "," + k8s.LabelBuild,
	})
	if err != nil {
		return nil, fmt.Errorf("list build jobs in %s: %w", namespace, err)
	}

	// Oldest first, which is the order the caller stops them in.
	var out []Result
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Labels[k8s.LabelApp] == "" || job.Labels[k8s.LabelBuild] == "" {
			continue
		}
		if status, _ := jobPhase(job); status.Terminal() {
			continue
		}
		out = append(out, e.result(ctx, namespace, job))
	}
	return out, nil
}

// InFlight reports whether an app has a build that has not finished.
//
// One listing answers for every app rather than one per app: build Jobs carry
// the app label, so a page of apps costs the same read a page of one app does.
// The set is returned rather than a count because its caller has a list of apps
// in hand and wants to know which of them is building.
func (e *Engine) InFlight(ctx context.Context, namespace string) (map[string]bool, error) {
	jobs, err := e.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelBuild,
	})
	if err != nil {
		return nil, fmt.Errorf("list build jobs in %s: %w", namespace, err)
	}

	out := map[string]bool{}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		appID := job.Labels[k8s.LabelApp]
		if appID == "" || job.Labels[k8s.LabelBuild] == "" {
			continue
		}
		if status, _ := jobPhase(job); !status.Terminal() {
			out[appID] = true
		}
	}
	return out, nil
}

// LatestPerApp returns each app's most recent build, keyed by app id.
//
// It is what a listing of apps needs, and it is deliberately not ListAll with a
// limit: a limit applies across all apps, so an app whose last build was a while
// ago would fall out of the page and read as never built — the opposite of what
// the column says. One listing, grouped here, answers for every app and costs
// the same read whatever the app count.
//
// Only the phase is read, never a failure's reason. The reason costs a pod read,
// and a list of apps would pay it once per failed build — while the column it
// feeds reports "failed", which is the whole of what it says. The app's own page
// and its build table are where the reason is worth that read.
func (e *Engine) LatestPerApp(ctx context.Context, namespace string) (map[string]Result, error) {
	jobs, err := e.client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.LabelBuild,
	})
	if err != nil {
		return nil, fmt.Errorf("list build jobs in %s: %w", namespace, err)
	}

	out := make(map[string]Result, len(jobs.Items))
	for i := range jobs.Items {
		job := &jobs.Items[i]
		appID := job.Labels[k8s.LabelApp]
		if appID == "" || job.Labels[k8s.LabelBuild] == "" {
			continue
		}

		// The newest per app, which is a comparison of creation times rather
		// than of position: a Job list comes back in name order, so the last one
		// seen for an app is not the latest build of it.
		prev, seen := out[appID]
		if seen && !job.CreationTimestamp.After(prev.CreatedAt) {
			continue
		}

		status, _ := jobPhase(job)
		out[appID] = Result{
			ID:        job.Labels[k8s.LabelBuild],
			AppID:     appID,
			CommitSHA: job.Annotations[AnnotationCommit],
			Branch:    job.Annotations[AnnotationBranch],
			JobName:   job.Name,
			Status:    status,
			CreatedAt: job.CreationTimestamp.Time,
		}
	}
	return out, nil
}

// results renders a set of Jobs, newest first.
func (e *Engine) results(ctx context.Context, namespace string, jobs []batchv1.Job) []Result {
	out := make([]Result, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]

		// The selector asks for both labels, so this is the same belt-and-braces
		// the uninstall sweep does: a listing that showed a Job AppLab did not
		// create would report a build that never happened.
		if job.Labels[k8s.LabelApp] == "" || job.Labels[k8s.LabelBuild] == "" {
			continue
		}

		out = append(out, e.result(ctx, namespace, job))
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// result reads one Job as a build.
func (e *Engine) result(ctx context.Context, namespace string, job *batchv1.Job) Result {
	status, reason := e.jobStatus(ctx, namespace, job)

	res := Result{
		ID:        job.Labels[k8s.LabelBuild],
		AppID:     job.Labels[k8s.LabelApp],
		CommitSHA: job.Annotations[AnnotationCommit],
		Branch:    job.Annotations[AnnotationBranch],
		JobName:   job.Name,
		Status:    status,
		Reason:    reason,
		CreatedAt: job.CreationTimestamp.Time,
	}
	if job.Status.StartTime != nil {
		res.StartedAt = job.Status.StartTime.Time
	}
	res.FinishedAt = finishedAt(job)
	return res
}

// finishedAt reports when a finished Job stopped.
//
// A succeeded Job sets CompletionTime; a failed one does not — it carries the
// time only on the condition that recorded the failure, so that is read instead.
// Without this a failed build would show no end time at all, which reads as a
// build that is still running.
func finishedAt(job *batchv1.Job) time.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time
	}
	for _, condition := range job.Status.Conditions {
		if condition.Status == "True" && (condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobComplete) {
			return condition.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// ErrBuildNotFound means no build Job matches.
//
// It is a sentinel rather than a bare error because two callers branch on it and
// one of them is the deploy path: "this commit has no build" is a 409 that names
// the fix, and "the cluster would not answer" is a 500. Telling them apart
// matters more than the message.
var ErrBuildNotFound = errors.New("build: no such build")
