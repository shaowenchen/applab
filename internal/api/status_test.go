package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/shaowenchen/applab/internal/model"
)

// The status these tests assert is derived from the cluster, so what they set up
// is the cluster's state — a Deployment that is available, one that cannot
// progress, or none at all. Nothing is written to the app to place it in a
// status, because a status is not a field of an app any more.

// TestAFreshInstallReportsAppsAsNotDeployed is the case this whole change is
// for.
//
// An AppLab pointed at a bucket it did not write — a reinstall, a second
// environment, a restored backup — lists every app with its settings intact. It
// must not claim any of them is running, because the cluster it has just been
// pointed at has nothing: "running" here would be a green pill and an Open
// button for an app that does not exist.
func TestAFreshInstallReportsAppsAsNotDeployed(t *testing.T) {
	srv, _, st := newDeployServer(t)
	h := srv.Handler()

	// Apps as they exist in a bucket: a name, a port, a branch, history.
	for _, id := range []string{"shop", "blog"} {
		if err := st.CreateApp(context.Background(), &model.App{
			ID: id, Name: id, Port: 8080, Replicas: 1, Dockerfile: "Dockerfile",
			Namespace: "ops-system",
		}); err != nil {
			t.Fatalf("create app %s: %v", id, err)
		}
	}

	var list []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		URL    string `json:"url"`
	}
	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps", nil)
	decodeData(t, rec, &list)

	if len(list) != 2 {
		t.Fatalf("listed %d apps, want both", len(list))
	}
	for _, app := range list {
		if app.Status != "created" {
			t.Errorf("app %s reports %q with no Deployment; a fresh install must not claim it is running",
				app.ID, app.Status)
		}
		// The address is reported even though nothing serves it. It is where the
		// app is served — a fact about its settings, which a fresh install reads
		// from the bucket — and the status above is what says nothing is running.
		// Withholding it would cost a caller the one thing they need in order to
		// know where the first deploy will put the app.
		if app.URL == "" {
			t.Errorf("app %s reports no address; where it is served is known even when nothing is running", app.ID)
		}
	}

	// And the two endpoints agree, which is the whole reason both read the same
	// derivation.
	for _, id := range []string{"shop", "blog"} {
		rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/"+id+"/status", nil)
		var status struct {
			Status string `json:"status"`
			Live   any    `json:"live"`
		}
		decodeData(t, rec, &status)
		if status.Status != "created" {
			t.Errorf("%s/status reports %q; the list and the detail disagree", id, status.Status)
		}
		if status.Live != nil {
			t.Errorf("%s/status reports live state with no Deployment: %+v", id, status.Live)
		}
	}
}

// TestStatusComesFromTheDeployment asserts the three states a Deployment can be
// in are read off it, and that the commit comes from the cluster.
//
// The commit is the one field that had nowhere else to live: the app record used
// to carry it, and a Deployment's annotation is what replaced it. If this stops
// reading it, a deployed app shows no commit anywhere.
func TestStatusComesFromTheDeployment(t *testing.T) {
	const commit = "abc123def456789012345678901234567890abcd"

	cases := []struct {
		name     string
		create   func(t *testing.T, client *fake.Clientset, appID string)
		want     string
		wantLive bool
	}{
		{
			name:     "no deployment",
			create:   func(*testing.T, *fake.Clientset, string) {},
			want:     "created",
			wantLive: false,
		},
		{
			name: "available",
			create: func(t *testing.T, client *fake.Clientset, appID string) {
				createAppDeployment(t, client, appID, []appsv1.DeploymentCondition{
					{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
				}, commit)
			},
			want:     "running",
			wantLive: true,
		},
		{
			name: "rolling out",
			create: func(t *testing.T, client *fake.Clientset, appID string) {
				createAppDeployment(t, client, appID, nil, commit)
			},
			want:     "deploying",
			wantLive: true,
		},
		{
			name: "cannot progress",
			create: func(t *testing.T, client *fake.Clientset, appID string) {
				createAppDeployment(t, client, appID, []appsv1.DeploymentCondition{
					{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
						Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet has timed out progressing"},
				}, commit)
			},
			want:     "failed",
			wantLive: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, client, st := newDeployServer(t)
			h := srv.Handler()

			if err := st.CreateApp(context.Background(), &model.App{
				ID: "shop", Name: "Shop", Port: 8080, Replicas: 1, Namespace: "ops-system",
			}); err != nil {
				t.Fatalf("create app: %v", err)
			}
			tc.create(t, client, "shop")

			var status struct {
				Status string `json:"status"`
				Live   *struct {
					Deployed bool   `json:"deployed"`
					Commit   string `json:"commit_sha"`
				} `json:"live"`
			}
			rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop/status", nil)
			decodeData(t, rec, &status)

			if status.Status != tc.want {
				t.Errorf("status = %q, want %q", status.Status, tc.want)
			}
			if (status.Live != nil) != tc.wantLive {
				t.Fatalf("live block present = %v, want %v", status.Live != nil, tc.wantLive)
			}
			if tc.wantLive && status.Live.Commit != commit {
				t.Errorf("commit = %q, want %q read from the Deployment's annotation",
					status.Live.Commit, commit)
			}
		})
	}
}

// TestBuildInFlightOutranksTheCluster asserts an app with a build running
// reports as building even though the Deployment it will replace is still
// available.
//
// This is the one part of a status the cluster cannot answer — a build's outcome
// lives in the bucket's build records, because a build Job is collected half an
// hour after it finishes — so it is the one part read from the store, and the
// precedence between the two is worth pinning.
func TestBuildInFlightOutranksTheCluster(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	if err := st.CreateApp(ctx, &model.App{
		ID: "shop", Name: "Shop", Port: 8080, Replicas: 1, Namespace: "ops-system",
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}
	createAppDeployment(t, client, "shop", []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
	}, "abc123def456789012345678901234567890abcd")

	// A build that has not finished: a Job still running, which is the only kind
	// of build record there is.
	makeBuildJob(t, client, "shop", "b1", "job-b1")

	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil)
	var app struct {
		Status string `json:"status"`
	}
	decodeData(t, rec, &app)

	if app.Status != "building" {
		t.Errorf("status = %q, want building — a build in flight is newer than the rollout it will replace", app.Status)
	}
}

// TestAFreshInstallDoesNotBuildAnything asserts nothing is rebuilt at startup.
//
// Reading the bucket and reacting to it are different things, and only the first
// is wanted: an installation that brought up every app it found would surprise
// whoever runs the cluster, and would undo an app that had been stopped on
// purpose.
func TestAFreshInstallDoesNotBuildAnything(t *testing.T) {
	srv, engine, st, _ := pushServer(t)

	if err := st.CreateApp(context.Background(), &model.App{
		ID: "shop", Name: "Shop", Port: 8080, Replicas: 1, Namespace: "ops-system",
	}); err != nil {
		t.Fatalf("create app: %v", err)
	}

	// Nothing in AppLab builds on startup any more. There is no reconcile pass
	// to invoke, so what this pins is that reading an existing app is inert: the
	// request below is the whole of what happens, and it must not start anything.
	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/apps", nil)

	if got := engine.startedJobs(); len(got) != 0 {
		t.Errorf("reading the app list started %d builds; recreating workloads is explicit", len(got))
	}

	// And the app is still listed, with its settings.
	var list []struct {
		ID     string `json:"id"`
		Port   int32  `json:"port"`
		Status string `json:"status"`
	}
	decodeData(t, rec, &list)
	if len(list) != 1 || list[0].ID != "shop" || list[0].Port != 8080 {
		t.Errorf("the app was not listed with its settings: %+v", list)
	}
}

// TestADeployRecreatesADeploymentThatIsGone asserts the explicit rebuild path:
// an app whose objects are absent gets them back from a deploy.
func TestADeployRecreatesADeploymentThatIsGone(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	commit := setupAppWithCommit(t, srv, h, "shop")
	registerBuildJob(t, srv, st, "shop", commit)

	// Nothing is running to begin with, which is the fresh-install state.
	if _, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab-shop", metav1.GetOptions{}); err == nil {
		t.Fatal("a Deployment already exists; this test needs an app with none")
	}

	rec := doRequest(t, h, http.MethodPost, "/api/v1/apps/shop/deploy", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("deploy: %d (%s)", rec.Code, rec.Body.String())
	}

	deployment, err := client.AppsV1().Deployments("ops-system").Get(ctx, "applab-shop", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the deploy did not create the Deployment: %v", err)
	}
	if got := deployment.Annotations["applab.io/commit"]; got != commit {
		t.Errorf("deployed commit = %q, want %q", got, commit)
	}

	// And the API now reports it as running, from the cluster.
	if rec := doRequest(t, h, http.MethodGet, "/api/v1/apps/shop", nil); strings.Contains(rec.Body.String(), `"url"`) != true {
		t.Errorf("the app does not report a URL after a deploy:\n%s", rec.Body.String())
	}
}

// createAppDeployment creates the Deployment a deploy would have created, with
// the conditions and the commit annotation the API reads.
func createAppDeployment(t *testing.T, client *fake.Clientset, appID string, conditions []appsv1.DeploymentCondition, commitSHA string) {
	t.Helper()

	_, err := client.AppsV1().Deployments("ops-system").Create(context.Background(),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "applab-" + appID,
				Namespace: "ops-system",
				Labels:    map[string]string{"applab.io/app": appID},
				Annotations: map[string]string{
					"applab.io/commit": commitSHA,
					"applab.io/image":  "registry.example.com/apps/" + appID + ":abc123",
				},
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"applab.io/app": appID}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"applab.io/app": appID}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "app",
							Image: "registry.example.com/apps/" + appID + ":abc123",
						}},
					},
				},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Conditions: conditions},
		}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the deployment for %s: %v", appID, err)
	}
}

// TestListingManyAppsReadsTheClusterOnce asserts the list route does not do one
// cluster read per row.
func TestListingManyAppsReadsTheClusterOnce(t *testing.T) {
	srv, client, st := newDeployServer(t)
	h := srv.Handler()
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if err := st.CreateApp(ctx, &model.App{
			ID: id, Name: id, Port: 8080, Replicas: 1, Namespace: "ops-system",
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		createAppDeployment(t, client, id, []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue},
		}, "abc123def456789012345678901234567890abcd")
	}

	client.ClearActions()
	rec := doRequest(t, h, http.MethodGet, "/api/v1/apps", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}

	lists := 0
	gets := 0
	for _, a := range client.Actions() {
		verb := a.GetVerb()
		if verb != "list" && verb != "get" {
			continue
		}
		if strings.Contains(a.GetResource().Resource, "deployment") {
			if verb == "list" {
				lists++
			} else {
				gets++
			}
		}
	}
	t.Logf("deployment reads for a 5-app listing: %d list(s), %d get(s)", lists, gets)
	if lists > 1 || gets > 0 {
		t.Errorf("listing 5 apps took %d list(s) and %d get(s) of Deployments; want one list and no gets", lists, gets)
	}
}
