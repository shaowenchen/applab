// Package model holds applab's domain types and the rules that give them
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

// AppStatus is where an app is in its lifecycle.
//
// The status is derived rather than authoritative: it is applab's summary of
// what it last tried to do and how that went, and the cluster remains the
// source of truth for whether the app is actually up. A status that disagrees
// with the cluster means applab's last operation failed, not that the app is
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

	// CommitSHA is the commit currently deployed, and Image the image built from
	// it. Both empty means nothing has been deployed yet.
	CommitSHA string
	Image     string

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
	"api": {}, "health": {}, "llms.txt": {}, "metrics": {}, "static": {},
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

// Namespace returns the namespace an app is deployed into.
//
// The prefix is the deployment's, not the app's, so an operator can run several
// applab installations in one cluster without their apps colliding.
func Namespace(prefix, appID string) string {
	return prefix + appID
}

// Hostname returns the hostname an app is served at, given the deployment's
// base domain. An app-level Domain wins, which is what lets one app take a
// memorable name without changing the domain every other app sits under.
func (a App) Hostname(baseDomain string) string {
	if d := strings.TrimSpace(a.Domain); d != "" {
		return d
	}
	if baseDomain == "" {
		return ""
	}
	return a.ID + "." + baseDomain
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
