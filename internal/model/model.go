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
// It is computed, never stored. What an app is doing is a fact about the
// cluster — whether its Deployment exists, is rolling out, or is available —
// and AppLab is not the authority on it. The stored version of this field was a
// cache of a cluster fact, and a cache of a cluster fact is wrong the moment the
// cluster is recreated, which is exactly what pointing a new AppLab at an
// existing bucket does: every app read back as "running" with nothing behind it.
//
// It is still a type rather than a string so the vocabulary stays in one place:
// the API reports it, the console colours it and the CLI prints it.
type AppStatus string

const (
	// AppStatusCreated means the app exists but nothing is running for it. That
	// covers an app that has never been deployed and one that was stopped,
	// because from outside they are the same: no workload, and its source and
	// history intact.
	AppStatusCreated AppStatus = "created"
	// AppStatusBuilding means a build is in flight.
	AppStatusBuilding AppStatus = "building"
	// AppStatusBuildFailed means the most recent build failed.
	AppStatusBuildFailed AppStatus = "build-failed"
	// AppStatusDeploying means there is a workload and it has not finished
	// rolling out.
	AppStatusDeploying AppStatus = "deploying"
	// AppStatusRunning means a rollout completed and the app is available.
	AppStatusRunning AppStatus = "running"
	// AppStatusFailed means a rollout cannot progress — a pod that will not start
	// is the usual cause.
	AppStatusFailed AppStatus = "failed"
)

// BuildStatus is where a single build attempt stands.
type BuildStatus string

const (
	BuildStatusPending   BuildStatus = "pending"
	BuildStatusRunning   BuildStatus = "running"
	BuildStatusSucceeded BuildStatus = "succeeded"
	BuildStatusFailed    BuildStatus = "failed"

	// BuildStatusCancelled means the build was stopped before it finished — by a
	// newer upload for the same app, which supersedes the build in flight, or by
	// an explicit request.
	//
	// It is a terminal status of its own rather than "failed" because the two
	// mean opposite things to whoever reads them: a failed build needs looking
	// at, and a superseded one was replaced on purpose and needs nothing. It is
	// deliberately not named "superseded": the same state is reached by a stop
	// request, and a build stopped that way was not superseded by anything.
	BuildStatusCancelled BuildStatus = "cancelled"
)

// Terminal reports whether the build has stopped and its status will not change
// again. Callers polling or streaming a build use this to decide when to stop.
func (s BuildStatus) Terminal() bool {
	return s == BuildStatusSucceeded || s == BuildStatusFailed || s == BuildStatusCancelled
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

	// Branch is the app's active branch: the one a deploy builds from and the
	// one a clone with no branch named gets.
	//
	// Empty means unset, which resolves to DefaultBranch rather than to "no
	// branch". An app always has one — there is no state in which nothing is
	// active — so an empty field is a record that predates branches or was
	// written without one, not a third mode.
	//
	// Any branch may be pushed and stored; this is which of them runs. Switching
	// it is what `applab branch use` does, and it redeploys, because an active
	// branch that has not been built is not active in any sense a caller cares
	// about.
	Branch string

	// AutoDeploy is whether a push to the active branch builds and deploys on
	// its own. Nil means unset, which resolves to true — the same treatment
	// Branch gets below, and for the same reason: every app written before this
	// field existed behaves as though it were set, so a nil has to mean "on"
	// rather than "off".
	//
	// The pointer is what makes that possible. A plain bool would have "off" as
	// its zero value, so every existing app would silently stop deploying on a
	// push the moment this field was added.
	AutoDeploy *bool

	// Env is the app's plain configuration, applied to the container as
	// environment variables at deploy time.
	//
	// It is returned by the API in full, so whatever is in it should be fit to
	// print. Secrets are the other half, in the field below.
	Env map[string]string

	// Secrets is the app's secret configuration, applied to the container as
	// environment variables alongside Env.
	//
	// The split from Env is by intent rather than by handling: a secret is a
	// password, a token or a connection string, and AppLab never returns these
	// values on any route — the API reports the names and nothing more. They are
	// stored here rather than in a Kubernetes Secret because AppLab no longer
	// uses Secret objects at all, which means a value written this way reaches
	// the container through the Deployment's own spec and is readable by anyone
	// who can read that.
	//
	// It is why secrets are not in the app's *interface*: appResponse carries the
	// names, not this map.
	Secrets map[string]string

	// Nothing here records what is *running*. The commit deployed, the image it
	// came from and whether the app is up are facts about the cluster, and the
	// cluster is where they are read from — see the AppStatus comment above and
	// Deployer.Statuses. An app loaded from the bucket answers "what is this
	// app", never "what is it doing".

	// Namespace is where this app's resources live. It is derived from the
	// deployment's prefix and the app id, and carried on the app so that every
	// object AppLab creates for it is named consistently without each call site
	// recomputing — and possibly recomputing differently.
	Namespace string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Commit is one recorded change to an app's source.
type Commit struct {
	AppID string
	SHA   string

	// Branch is the branch this commit was pushed to.
	//
	// It is recorded rather than derived because it cannot be derived: a commit
	// is a snapshot and carries no record of which refs point at it, and two
	// branches commonly share history, so the same SHA can legitimately arrive
	// on more than one branch. History is what a person reads to find out what
	// was pushed where, and without this it could only say "something was
	// recorded" — which is the question it exists to answer.
	Branch string

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

// DefaultBranch is the branch an app starts on and the one a record without a
// branch is taken to mean.
//
// "main" rather than "master" is what `git init` itself produces now, and the
// repositories here are created by git, so matching it means an app's first
// branch is whatever a caller would have got from init.
const DefaultBranch = "main"

// ActiveBranch returns the branch an operation should act on, given an app's
// possibly-empty Branch field.
//
// Every caller that needs a branch goes through this rather than testing for
// empty, so that "no branch recorded" has one meaning — the default — instead of
// being resolved at each call site and possibly resolved differently.
func (a App) ActiveBranch() string {
	if a.Branch == "" {
		return DefaultBranch
	}
	return a.Branch
}

// AutoDeploys reports whether a push should build and deploy this app without
// being asked.
//
// Every caller goes through this rather than testing the pointer, so "no value
// recorded" has one meaning — yes — instead of being resolved at each call site
// and possibly resolved differently. See AutoDeploy for why the zero value of
// the field cannot be the answer.
func (a App) AutoDeploys() bool {
	return a.AutoDeploy == nil || *a.AutoDeploy
}

// ValidateBranchName reports whether name may be used as a branch.
//
// It is the same kind of check as ValidateAppID and for the same reason: a
// branch name becomes a path segment in three places at once — a bucket key, a
// URL in the clone address, and an argument to git — so a name that is legal in
// one and not another is a bug that shows up far from where it was typed.
//
// The rules below are the subset of git's own check-ref-format that this needs.
// They are written out rather than delegated to `git check-ref-format` because
// this is a validation on the request path, and running a subprocess to decide
// whether to answer 400 would put a process spawn in front of every push.
//
// Two rules are AppLab's rather than git's:
//
//   - A leading dash is refused. Branch names reach `git` as command arguments,
//     and "--upload-pack=..." is a valid ref-name component to git while being
//     an option to the program. Refusing the shape removes the whole class
//     rather than relying on every call site remembering a "--" separator.
//   - So is a name that cannot be a single bucket key segment. git permits a
//     space in a ref name; a space in a key is legal too, but a name that needs
//     escaping in one system and not the other is a mismatch waiting to happen,
//     and no one names a branch with a space on purpose.
//
// Empty is refused: an empty branch is not a name, and the one place that means
// something — "the app's active branch" — is resolved by ActiveBranch before it
// reaches here.
func ValidateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("a branch needs a name")
	}
	if len(name) > maxBranchLength {
		return fmt.Errorf("branch name %q is longer than %d characters", name, maxBranchLength)
	}

	// A ref name is a sequence of slash-separated components, and every rule
	// below applies to the whole string except where noted.
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("branch name %q must not start with a dash: a branch name becomes an argument to git, and a leading dash is indistinguishable from an option", name)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("branch name %q must not start or end with a slash", name)
	}
	if strings.Contains(name, "//") {
		return fmt.Errorf("branch name %q must not contain an empty component (//)", name)
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("branch name %q must not end with a dot", name)
	}
	if strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("branch name %q must not end with .lock: git keeps a lock file of that name beside the ref", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("branch name %q must not contain ..: a name becomes a path, and .. would leave the repository it belongs to", name)
	}
	if strings.Contains(name, "@{") {
		return fmt.Errorf("branch name %q must not contain @{, which git reads as a revision operator", name)
	}
	if name == "@" {
		return fmt.Errorf("branch name %q is git's shorthand for HEAD", name)
	}

	for _, r := range name {
		// Control characters and DEL, which git refuses outright, plus the
		// characters that give a ref name a meaning of its own in git's
		// revision syntax or in a path.
		switch r {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return fmt.Errorf("branch name %q contains %q, which git does not allow in a ref name", name, r)
		}
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("branch name %q contains a control character", name)
		}
	}
	return nil
}

// maxBranchLength bounds a branch name.
//
// It is not git's limit — git has none worth enforcing — but the bucket's and
// the URL's: the name is a key segment and a path segment, and both stop being
// pleasant well before this. 255 is the longest a single key component can be
// on every object store AppLab talks to.
const maxBranchLength = 255

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

// The bounds on an app's port and replica count.
//
// They are exported because four places enforce them — the API, the console, the
// CLI's create and the CLI's update — and a bound that lives in four copies is
// one that drifts. The messages matter as much as the numbers: a person who
// types --port 70000 in a terminal and one who types it into the console should
// read the same sentence about why it was refused.
const (
	// MinPort and MaxPort bound the port the app's container listens on. The
	// range is the TCP port range, and the Service targets whatever this is, so
	// a value outside it is a pod nothing can reach.
	MinPort = 1
	MaxPort = 65535

	// MaxReplicas bounds how many copies run. Zero is refused rather than
	// treated as "stop": it reads as running to anything checking readiness,
	// which is a trap rather than a feature, and stopping is the endpoint for
	// that.
	MaxReplicas = 50
)

// ValidatePort reports whether port is one an app may listen on.
func ValidatePort(port int32) error {
	if port < MinPort || port > MaxPort {
		return fmt.Errorf("port %d must be a number between %d and %d", port, MinPort, MaxPort)
	}
	return nil
}

// ValidateReplicas reports whether n copies is a sensible number to run.
func ValidateReplicas(n int32) error {
	if n < 1 {
		return fmt.Errorf("replicas must be at least 1, not %d; use the stop endpoint to scale an app down", n)
	}
	if n > MaxReplicas {
		return fmt.Errorf("replicas %d exceeds the maximum of %d", n, MaxReplicas)
	}
	return nil
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
