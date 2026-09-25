package k8s

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeleteBuildJobs removes every build Job in this namespace.
//
// Deleting every app's objects removes its Jobs too, so this covers less than it
// looks like it does. What it adds is the case where the store could not be read
// — an uninstall with a bucket that is already gone — and the Job whose app
// record is missing or unreadable, which a per-app teardown has no way to name.
// A build Job is the one object here that holds a credential: its pod carries the
// app key in an environment variable, so leaving one behind after an uninstall
// leaves a working key beside a removed installation.
//
// Both labels a build carries are required, not just the app one: LabelApp is
// also on an app's Deployment and Service, and the second label is what says this
// object is a build rather than the running app.
//
// This is a fallback rather than the main path. DeleteEveryAppObject is what
// removes objects app by app, with each object's label verified; this is a
// selector over the whole namespace, which is a weaker guarantee and is why it is
// scoped as narrowly as possible — one kind, both labels.
func (c *Client) DeleteBuildJobs(ctx context.Context) (int, error) {
	if !c.OwnsNamespace(c.namespace) {
		return 0, fmt.Errorf("refusing to delete build jobs in namespace %q: it is not this installation's namespace %q",
			c.namespace, c.namespace)
	}

	jobs, err := c.clientset.BatchV1().Jobs(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelApp + "," + LabelBuild,
	})
	if err != nil {
		return 0, fmt.Errorf("list build jobs in namespace %s: %w", c.namespace, err)
	}

	foreground := metav1.DeletePropagationForeground
	removed := 0
	for i := range jobs.Items {
		job := &jobs.Items[i]

		// The selector already requires both labels, so this is the same
		// belt-and-braces the per-app teardown does: a sweep that deletes by
		// label alone is one mislabeled object away from removing something that
		// belongs to someone else, and this namespace holds objects AppLab did
		// not create.
		if job.Labels[LabelApp] == "" || job.Labels[LabelBuild] == "" {
			continue
		}

		if err := c.clientset.BatchV1().Jobs(c.namespace).Delete(ctx, job.Name,
			metav1.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
			return removed, fmt.Errorf("delete build job %s: %w", job.Name, err)
		}
		removed++
	}

	return removed, nil
}

// WaitForAppsGone blocks until no Deployment carrying the app label remains, or
// the context expires.
//
// Deletion is asynchronous. The API server accepts a delete and the pods
// terminate on their own schedule, so a cleanup that returned as soon as it had
// issued the deletes would report success while every app was still running —
// and the uninstall that follows would leave them running for good.
//
// It waits on Deployments rather than on every kind because the Deployment is
// what owns the pods: once it is gone the replicas are being torn down, and the
// Service, VirtualService and Jobs were removed synchronously above.
func (c *Client) WaitForAppsGone(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		deployments, err := c.clientset.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: LabelApp,
		})
		if err != nil {
			return fmt.Errorf("list deployments in namespace %s: %w", c.namespace, err)
		}
		if len(deployments.Items) == 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			names := make([]string, 0, len(deployments.Items))
			for i := range deployments.Items {
				names = append(names, deployments.Items[i].Name)
			}
			return fmt.Errorf("%d deployment(s) still terminating: %v", len(names), names)
		case <-ticker.C:
		}
	}
}
