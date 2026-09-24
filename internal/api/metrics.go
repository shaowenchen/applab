package api

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shaowenchen/applab/internal/buildinfo"
)

// Metrics is the platform's own instrumentation.
//
// It is written by hand in the Prometheus text exposition format rather than
// pulled from a client library. The whole surface is a handful of counters, and
// the format is stable and small; a dependency would add more to the module
// graph than it removes from this file.
//
// What is exposed deliberately stops short of per-app series: an AppLab that
// manages hundreds of apps would otherwise publish hundreds of series that
// nothing scrapes by name, and the per-app answers are available from the API.
// These are the numbers that describe the platform itself — is it handling
// requests, are builds failing, can it reach the cluster.
type Metrics struct {
	started time.Time

	requestsTotal   atomic.Int64
	requestErrors   atomic.Int64
	requestDuration atomic.Int64 // nanoseconds

	buildsStarted  atomic.Int64
	buildsFailed   atomic.Int64
	deploysTotal   atomic.Int64
	deploysFailed  atomic.Int64
	uploadsTotal   atomic.Int64
	uploadBytes    atomic.Int64
	appsCreated    atomic.Int64
	authRejections atomic.Int64

	// gauges holds values sampled at scrape time, since they are read from
	// elsewhere rather than counted here.
	gauges   map[string]func() float64
	gaugesMu sync.Mutex
}

// NewMetrics creates a Metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		started: time.Now(),
		gauges:  map[string]func() float64{},
	}
}

// RegisterGauge attaches a value sampled when the endpoint is scraped.
//
// A function rather than a value because these change without AppLab doing
// anything — the number of apps is whatever the object store holds — and a counter
// that has to be remembered to be updated is a counter that goes stale.
func (m *Metrics) RegisterGauge(name, help string, fn func() float64) {
	m.gaugesMu.Lock()
	defer m.gaugesMu.Unlock()
	m.gauges[name] = fn
	_ = help
}

// ObserveRequest records one request.
func (m *Metrics) ObserveRequest(status int, d time.Duration) {
	m.requestsTotal.Add(1)
	m.requestDuration.Add(int64(d))
	if status >= 400 {
		m.requestErrors.Add(1)
	}
}

// ObserveBuild records a build that was started or that failed.
func (m *Metrics) ObserveBuild(failed bool) {
	m.buildsStarted.Add(1)
	if failed {
		m.buildsFailed.Add(1)
	}
}

// ObserveDeploy records a deploy attempt and whether it failed.
func (m *Metrics) ObserveDeploy(failed bool) {
	m.deploysTotal.Add(1)
	if failed {
		m.deploysFailed.Add(1)
	}
}

// ObserveUpload records a source upload.
func (m *Metrics) ObserveUpload(bytes int64) {
	m.uploadsTotal.Add(1)
	m.uploadBytes.Add(bytes)
}

// ObserveAppCreated records an app creation.
func (m *Metrics) ObserveAppCreated() { m.appsCreated.Add(1) }

// ObserveAuthRejection records a request refused for a bad credential.
//
// It is worth its own counter because it is the one signal that distinguishes a
// misconfigured client from someone probing: a steady rate means the latter, and
// nothing else in the platform would show it.
func (m *Metrics) ObserveAuthRejection() { m.authRejections.Add(1) }

// ServeHTTP writes the metrics.
//
// It needs no key: a Prometheus scraper holds credentials awkwardly, the numbers
// describe the platform rather than any tenant's data, and the endpoint is
// expected to be network-restricted by the deployment. That is a deliberate
// trade, and is reported by GET /api/v1/describe so a client can see it.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder

	writeMetric := func(name, help, kind string, value float64) {
		b.WriteString("# HELP " + name + " " + help + "\n")
		b.WriteString("# TYPE " + name + " " + kind + "\n")
		fmt.Fprintf(&b, "%s %g\n", name, value)
	}

	uptime := time.Since(m.started).Seconds()
	writeMetric("applab_uptime_seconds", "Seconds since this process started.", "gauge", uptime)
	writeMetric("applab_build_info", "Always 1; the version labels carry the build identity.", "gauge", 1)
	writeMetric("applab_requests_total", "HTTP requests handled, by whether they were errors.", "counter", float64(m.requestsTotal.Load()))
	writeMetric("applab_request_errors_total", "HTTP requests answered with a 4xx or 5xx.", "counter", float64(m.requestErrors.Load()))
	writeMetric("applab_auth_rejections_total", "Requests refused for a missing or unrecognised key.", "counter", float64(m.authRejections.Load()))
	writeMetric("applab_apps_created_total", "Apps created since this process started.", "counter", float64(m.appsCreated.Load()))
	writeMetric("applab_uploads_total", "Source uploads accepted.", "counter", float64(m.uploadsTotal.Load()))
	writeMetric("applab_upload_bytes_total", "Bytes of source accepted.", "counter", float64(m.uploadBytes.Load()))
	writeMetric("applab_builds_started_total", "Builds started.", "counter", float64(m.buildsStarted.Load()))
	writeMetric("applab_builds_failed_total", "Builds that failed.", "counter", float64(m.buildsFailed.Load()))
	writeMetric("applab_deploys_total", "Deploys attempted.", "counter", float64(m.deploysTotal.Load()))
	writeMetric("applab_deploys_failed_total", "Deploys that failed.", "counter", float64(m.deploysFailed.Load()))

	if n := m.requestsTotal.Load(); n > 0 {
		writeMetric("applab_request_duration_seconds_mean",
			"Mean request duration in seconds since start.",
			"gauge", float64(m.requestDuration.Load())/float64(n)/float64(time.Second))
	}

	// Go's own runtime statistics, which are what tell an operator whether the
	// process or the cluster is the problem.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	writeMetric("applab_go_goroutines", "Goroutines currently running.", "gauge", float64(runtime.NumGoroutine()))
	writeMetric("applab_go_memory_alloc_bytes", "Bytes of heap currently allocated.", "gauge", float64(mem.Alloc))
	writeMetric("applab_go_gc_total", "Completed garbage collection cycles.", "counter", float64(mem.NumGC))

	// The version is carried as a labelled series rather than as the value, so a
	// scrape after an upgrade shows which version each sample came from.
	b.WriteString("# HELP applab_version Build information; the value is always 1.\n")
	b.WriteString("# TYPE applab_version gauge\n")
	fmt.Fprintf(&b, "applab_version{version=%q,commit=%q,go_version=%q} 1\n",
		buildinfo.Version, buildinfo.Commit, runtime.Version())

	// Registered gauges, in a stable order so a scrape is diffable.
	m.gaugesMu.Lock()
	names := make([]string, 0, len(m.gauges))
	for name := range m.gauges {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		fmt.Fprintf(&b, "# TYPE %s gauge\n", name)
		fmt.Fprintf(&b, "%s %g\n", name, m.gauges[name]())
	}
	m.gaugesMu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// metricsMiddleware records every request.
//
// It wraps the whole handler, so the numbers include requests that were refused
// before reaching a route — which is the point for the auth counter.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	if s.metrics == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The metrics endpoint itself is excluded: scraping it would add one
		// request per scrape interval to its own counter, so the number would
		// measure the scraping rather than the traffic.
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		s.metrics.ObserveRequest(rec.status, time.Since(start))
	})
}
