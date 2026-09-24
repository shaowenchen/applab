// Package model holds AppLab's domain types and the rules that give them
// meaning, independent of how they are stored or served.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// PortEnv is the environment variable the deployer sets from an app's Port, so
// the app knows which port to bind and the Service has something to target.
//
// It is named here rather than in the deployer because two packages need it and
// they need to agree: the deployer sets it, and configuration validation refuses
// a caller who tries to set it too. Two literals would drift, and the failure
// that follows is silent — a duplicate declaration in the pod spec, a later
// value winning, and an app listening on a port nothing routes to.
const PortEnv = "PORT"

// AppStatus is where an app is in its lifecycle.
//
// The status is derived rather than authoritative: it is AppLab's summary of
// what it last tried to do and how that went, and the cluster remains the
// source of truth for whether the app is actually up. A status that disagrees
// with the cluster means AppLab's last operation failed, not that the app is
// healthy.
type AppStatus string

const (
	// AppStatusCreated means the app exists but has no source yet.
	AppStatusCreated AppStatus = "created"
	// AppStatusBuilding means a build is in flight.
	AppStatusBuilding AppStatus = "building"
	// AppStatusBuildFailed means the most recent build failed.
	AppStatusBuildFailed AppStatus = "build-failed"
	// AppStatusDeploying means an image was built and is being rolled out.
	AppStatusDeploying AppStatus = "deploying"
	// AppStatusRunning means the most recent rollout completed.
	AppStatusRunning AppStatus = "running"
	// AppStatusFailed means the most recent deploy failed.
	AppStatusFailed AppStatus = "failed"
	// AppStatusDeleted means the app was removed. The row is kept so the id is
	// not silently reusable and so history stays readable.
	AppStatusDeleted AppStatus = "deleted"
)

// BuildStatus is where a single build attempt stands.
type BuildStatus string

const (
	BuildStatusPending   BuildStatus = "pending"
	BuildStatusRunning   BuildStatus = "running"
	BuildStatusSucceeded BuildStatus = "succeeded"
	BuildStatusFailed    BuildStatus = "failed"
)

// Terminal reports whether the build has stopped and its status will not change
// again. Callers polling or streaming a build use this to decide when to stop.
func (s BuildStatus) Terminal() bool {
	return s == BuildStatusSucceeded || s == BuildStatusFailed
}

// App is a deployed application.
type App struct {
	// ID identifies the app everywhere: in URLs, as its git repository's
	// directory name, and as the suffix of its namespace. It is immutable, so
	// nothing has to be rewritten when it changes — which is why there is no
	// rename operation.
	ID string

	// Name is a human label. It may be changed freely; nothing depends on it.
	Name string

	// Port is the port the app's container listens on. It is what the Service
	// targets, so it has to match what the app actually binds.
	Port int32

	// Replicas is how many pod copies to run.
	Replicas int32

	// Dockerfile is the build file's path within the source tree, relative to
	// its root.
	Dockerfile string

	// Domain overrides the hostname the app is served at. Empty means the
	// deployment's default of "<id>.<base domain>".
	Domain string

	// Env is the app's non-secret configuration, applied to the container as
	// environment variables at deploy time.
	//
	// Secrets are deliberately not here. They live in a Kubernetes Secret, read
	// by the deployer and by nothing else, so the database is not a second place
	// for a credential to leak from or to drift in. The split is by sensitivity:
	// this map is returned by the API, and whatever is in it should be fit to
	// print.
	Env map[string]string

	// CommitSHA is the commit currently deployed, and Image the image built from
	// it. Both empty means nothing has been deployed yet.
	CommitSHA string
	Image     string

	// Namespace is where this app's resources live. It is derived from the
	// deployment's prefix and the app id, and carried on the app so that every
	// object AppLab creates for it is named consistently without each call site
	// recomputing — and possibly recomputing differently.
	Namespace string

	// Status and StatusReason describe the most recent attempt.
	Status       AppStatus
	StatusReason string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Commit is one recorded change to an app's source.
type Commit struct {
	AppID     string
	SHA       string
	Message   string
	Author    string
	Files     int
	Bytes     int64
	CreatedAt time.Time
}

// Build is one attempt to turn a commit into an image.
type Build struct {
	ID        string
	AppID     string
	CommitSHA string
	Image     string
	// JobName is the build Job's name in the app's namespace, recorded so a
	// failed build's pods can still be found after the fact.
	JobName    string
	Status     BuildStatus
	Reason     string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

// Upload is an in-progress chunked source upload.
//
// It exists so a source tree too large for one request can be assembled from
// parts that arrive separately. The parts live on disk; this row is what ties
// them together and remembers what the finished commit should say.
type Upload struct {
	ID        string
	AppID     string
	Total     int
	ChunkSize int64
	Message   string
	Author    string
	CreatedAt time.Time
}

// appIDPattern constrains an app id to what can serve as all three of: a URL
// path segment, a git repository directory name, and a Kubernetes namespace
// label after the deployment's prefix is prepended.
//
// Lowercase only, because a namespace name must be a lowercase DNS label — and
// enforcing it here means the constraint fails at creation, where it can be
// explained, rather than at deploy time.
var appIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// reservedIDs are names that would collide with a route or a static asset of
// the service itself, so an app may not take one.
var reservedIDs = map[string]struct{}{
	"api": {}, "health": {}, "metrics": {}, "static": {},
	"assets": {}, "favicon.ico": {}, "index.html": {}, "login": {},
	"console": {}, "version": {}, "config": {}, "system": {}, "apps": {},
}

// ValidateAppID reports whether id may be used for a new app.
func ValidateAppID(id string) error {
	if id == "" {
		return fmt.Errorf("app id must not be empty")
	}
	if !appIDPattern.MatchString(id) {
		return fmt.Errorf("app id %q must start with a letter or digit, contain only lowercase letters, digits and dashes, and be at most 40 characters", id)
	}
	if _, taken := reservedIDs[id]; taken {
		return fmt.Errorf("app id %q is reserved", id)
	}
	return nil
}

// Namespace returns the namespace an app's resources live in: the deployment's
// own.
//
// Every app shares it, which is a deliberate trade. It gives AppLab a namespaced
// Role instead of a ClusterRole — it holds no permission anywhere else in the
// cluster — and it costs the isolation separate namespaces would provide. What
// keeps one app's objects apart from another's is a label (`applab.io/app`),
// not a namespace boundary, so anything that lists or deletes must filter by it.
//
// The app id is accepted for symmetry with the callers that have one to hand; it
// does not affect the result.
func Namespace(namespace, appID string) string {
	return namespace
}

// Address is where an app answers: a host, and a path under it.
//
// Two deployments are possible. With no path prefix every app takes a host of
// its own — "shop.example.com" — and Path is empty. With one, every app shares
// the same host and the path is what says which app is meant, so the host is
// only half of the address and neither field alone identifies an app.
//
// It is a pair rather than a string because the two are used separately: the
// hostnames go in a VirtualService's hosts, the path in its match and its
// rewrite, and joining them early would only mean splitting them again.
type Address struct {
	Host string
	Path string
}

// Empty reports whether this address names nothing, which is a deployment with
// no base domain: apps are then reachable inside the cluster only.
func (a Address) Empty() bool { return a.Host == "" }

// String is the address as a caller would type it, without a scheme.
func (a Address) String() string { return a.Host + a.Path }

// URL is the address with a scheme in front. Empty when there is no address,
// which is what keeps a caller from being handed "http://".
func (a Address) URL(scheme string) string {
	if a.Empty() {
		return ""
	}
	return scheme + "://" + a.Host + a.Path
}

// RoutePath is the path an app is routed on, always ending in a slash so that
// it can be matched as a prefix without also matching a neighbour.
//
// The trailing slash is load-bearing. Istio's prefix match is a plain string
// prefix, not a path-segment match, so a route on "/apps/shop" would also claim
// "/apps/shop-2/anything" — and with cross-VirtualService order undefined, the
// app that won would be whichever istiod happened to apply first. "/apps/shop/"
// cannot match "/apps/shop-2/".
func (a Address) RoutePath() string {
	if a.Path == "" {
		return "/"
	}
	return a.Path + "/"
}

// Address returns where an app is served, given the deployment's base domain
// and its optional shared path prefix.
//
// An app-level Domain wins outright and puts the app at the root of its own
// host: the point of the override is to escape the deployment's convention, so
// carrying the convention's path along with it would defeat it.
//
// This is the only way an app's address is derived. There is deliberately no
// hostname-only helper: with a path prefix the host is half an address, and a
// helper that returned it alone would be a trap — correct in the common
// configuration and quietly wrong in the other one, at every call site that
// reached for it.
func (a App) Address(baseDomain, pathPrefix string) Address {
	if d := strings.TrimSpace(a.Domain); d != "" {
		return Address{Host: d}
	}
	if baseDomain == "" {
		return Address{}
	}
	if pathPrefix != "" {
		// Every app on one host, told apart by path.
		return Address{Host: baseDomain, Path: pathPrefix + "/" + a.ID}
	}
	return Address{Host: a.ID + "." + baseDomain}
}

// NewID returns a random identifier for a record that has no natural key, such
// as a build. Callers must treat a failure as fatal to the operation: falling
// back to something predictable would make ids guessable, and these ids appear
// in URLs.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidSHA reports whether s looks like a git commit id. Source endpoints take
// a commit from the caller when rolling back, so it is checked before use
// rather than trusted.
func ValidSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}
