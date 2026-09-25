// Package gitx serves AppLab's repositories over the git smart HTTP protocol.
//
// It runs git's own `git http-backend` as a CGI program and translates between
// it and net/http, rather than reimplementing the protocol. The protocol is
// subtle — pack negotiation, capability advertisement, protocol v2 — and git's
// own backend is the authority on it; anything hand-written here would be a
// second, worse implementation that drifts.
//
// The CGI contract is narrow, which is what makes this tractable:
//
//   - The request's path becomes PATH_INFO, joined against GIT_PROJECT_ROOT to
//     locate the repository.
//   - The request body is the CGI program's stdin; its stdout is the response.
//   - Certain variables say who the caller is and which services are allowed.
package gitx

import (
	"bufio"
	"context"
	"log/slog"
	"path/filepath"

	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// Transport serves repositories over HTTP.
type Transport struct {
	// repoRoot is the directory holding the repositories, passed to git as
	// GIT_PROJECT_ROOT. Requests can only address repositories beneath it.
	repoRoot string

	// gitBin is the resolved git executable, whose sibling http-backend is
	// invoked. Resolved once so a missing backend fails at boot.
	gitBin string

	// backend is the path to git http-backend.
	backend string

	// serviceUser is the name git records as the pusher for reflog entries.
	// AppLab has no per-user identities, so it is a constant.
	serviceUser string

	// authorize decides whether a request may reach one repository, given the
	// app id the repository belongs to.
	//
	// It runs here rather than in the mux because this handler is mounted as a
	// single prefix covering every repository: the Go pattern that routes it has
	// no {app} placeholder for a middleware to read, and enumerating git's
	// protocol surface as separate patterns would be a list to fall out of date.
	// The app id is available at exactly this point, so this is where the
	// decision belongs.
	//
	// Nil means this transport does not authorize on its own, which is correct
	// for a caller that has already done so — the API's own key middleware, and
	// the tests that drive the transport directly.
	authorize func(r *http.Request, appID string) bool

	// sessions materialises a repository for the duration of a request and
	// uploads it afterwards.
	//
	// The repositories live in object storage, which git cannot read, so every
	// request works on a local copy and the copy is what git http-backend is
	// pointed at. It is an interface rather than a concrete dependency so this
	// package keeps knowing nothing about where the source is kept — and so a
	// test can serve a directory.
	sessions Sessions

	// activeBranch resolves the branch to serve when the URL named none.
	//
	// A URL without a branch means "the app's active one", which is app state
	// this package does not hold and should not: it knows how to serve a
	// repository over HTTP and nothing about what a branch is for. The lookup is
	// attached by the API layer, which owns that state.
	//
	// Nil means an unresolved branch is a request for nothing, which is what a
	// caller with no such state should get rather than a guess.
	activeBranch func(ctx context.Context, appID string) string

	// prepare makes a materialised repository safe to hand to git, and is where
	// the repository's own config is pinned.
	//
	// It is a hook rather than something this package does itself because the
	// settings are this package's problem and not its knowledge: see
	// source.Store.applyDeterministicConfig for what they are. What matters here
	// is *when* it runs — after the repository is materialised and before
	// git http-backend is started — because the one that bit is `gc.auto`, which
	// has to be off before receive-pack can spawn the maintenance that races the
	// upload.
	//
	// Nil means the repository is used exactly as it was downloaded.
	prepare func(ctx context.Context, repoPath string) error
}

// Sessions is how a repository is made available to git for one request.
//
// The path is only valid until Done is called, and Done must be called exactly
// once for every Open — it is what uploads a push and removes the local copy.
//
// The branch is always a real branch name by the time it arrives here: a request
// that named none has been resolved through activeBranch, and a request that
// named one has been validated against the same rule an app id is.
type Sessions interface {
	Open(ctx context.Context, appID, branch string) (repoPath string, done func() error, err error)
}

// WithSessions attaches the source of repositories.
//
// Without it the transport serves nothing: a request that has passed
// authorization finds no repository to serve, which is the honest answer for a
// deployment whose source storage was not configured.
func (t *Transport) WithSessions(s Sessions) *Transport {
	t.sessions = s
	return t
}

// WithPrepare attaches the step that makes a materialised repository safe to
// serve.
//
// A failure is answered as "repository not found" rather than as an error: the
// caller cannot act on it, and disclosing that an app's storage could not be
// prepared is disclosing that the app exists.
func (t *Transport) WithPrepare(fn func(ctx context.Context, repoPath string) error) *Transport {
	t.prepare = fn
	return t
}

// WithActiveBranch attaches the lookup that turns an app id into the branch to
// serve when the request's URL did not name one.
//
// An empty branch — a push to an unknown app, or a deployment with no source
// storage behind it — is answered as an empty string, which the caller treats as
// "nothing to serve".
func (t *Transport) WithActiveBranch(fn func(ctx context.Context, appID string) string) *Transport {
	t.activeBranch = fn
	return t
}

// Authorize attaches the per-repository authorization check.
//
// It is called once the repository name is known and before git is invoked, so a
// refusal never starts a git process. A hook rather than a concrete dependency
// keeps this package from importing the auth and appkey halves it would
// otherwise need to make the decision.
func (t *Transport) Authorize(fn func(r *http.Request, appID string) bool) *Transport {
	t.authorize = fn
	return t
}

// New creates a Transport.
//
// It resolves `git http-backend` from the git executable's directory rather than
// searching PATH, because the two must be the same git installation: a
// http-backend from a different version than the client-facing tooling is a
// class of bug that is miserable to diagnose.
func New(repoRoot, gitBin string) (*Transport, error) {
	backend, err := resolveBackend(gitBin)
	if err != nil {
		return nil, err
	}
	return &Transport{
		repoRoot:    repoRoot,
		gitBin:      gitBin,
		backend:     backend,
		serviceUser: "applab",
	}, nil
}

// resolveBackend finds git-http-backend next to the given git executable.
func resolveBackend(gitBin string) (string, error) {
	// Ask git where its executables live; that directory is authoritative and
	// does not depend on how this process's PATH happens to be arranged.
	out, err := exec.Command(gitBin, "--exec-path").Output()
	if err == nil {
		candidate := path.Join(strings.TrimSpace(string(out)), "git-http-backend")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	// Fall back to a PATH lookup, which is what a conventional install needs.
	if found, err := exec.LookPath("git-http-backend"); err == nil {
		return found, nil
	}

	return "", fmt.Errorf("git-http-backend not found; it ships with git and is required to serve repositories over HTTP")
}

// ServeHTTP handles one git request.
//
// The caller is responsible for having authenticated the request: this handler
// serves whatever repository the path names, so it must not be reachable
// without a valid key. When an Authorize hook is attached it additionally
// enforces *which* repository a credential reaches, because authentication alone
// would let any key read any app's source.
func (t *Transport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	projectPath, repoName, branch, ok := t.resolvePath(r.URL.Path)
	if !ok {
		// The path failed validation. The response is deliberately the same
		// 404 a nonexistent repository gives, so a caller probing for a way out
		// of the repository root learns nothing from the difference.
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	// Before git is started, and with the same 404 an unknown repository gives:
	// a distinguishable "forbidden" would confirm that the named app exists,
	// which turns this endpoint into a way to enumerate every app for anyone
	// holding one app's key.
	if t.authorize != nil && !t.authorize(r, repoName) {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	// A URL that named no branch means the app's active one. This is resolved
	// after authorization and before the repository is opened, so a request that
	// was not allowed to reach the app never causes a lookup of its state.
	if branch == "" {
		if t.activeBranch == nil {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		if branch = t.activeBranch(r.Context(), repoName); branch == "" {
			// The app has no active branch to speak of — it does not exist, or
			// its record could not be read. The same 404 either way.
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
	}

	// The repository is materialised now and uploaded again when the request is
	// done — which is what makes a push durable, since git has only ever written
	// to the local copy.
	if t.sessions == nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	repoPath, done, err := t.sessions.Open(r.Context(), repoName, branch)
	if err != nil {
		// A repository that is not there and one that could not be read are the
		// same answer to a caller: this endpoint does not disclose which apps
		// exist, and a storage failure is not theirs to diagnose.
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	// Called from every path out of this handler, because the upload is what
	// makes a push durable and skipping it to report a different error would
	// trade a lost push for a tidier log line.
	//
	// Not before git runs: this uploads the working copy and then removes it, so
	// it is the last thing done with repoPath. For a push that is also why the
	// response is buffered — the answer git produced is held until the outcome of
	// this is known, since a response already streamed cannot be taken back.
	stored := func() bool {
		if err := done(); err != nil {
			slog.ErrorContext(r.Context(), "could not store the repository after serving it",
				"app", repoName, "branch", branch, "error", err)
			return false
		}
		return true
	}

	// Before git is started, so receive-pack inherits the settings rather than
	// discovering them part-way through a push.
	if t.prepare != nil {
		if err := t.prepare(r.Context(), repoPath); err != nil {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
	}

	// The backend resolves PATH_INFO against GIT_PROJECT_ROOT, so both are built
	// from where the repository actually is rather than from the configured root:
	// the repository is materialised in scratch space, and the root the transport
	// was constructed with is not where it lives.
	//
	// The request's own path cannot be reused, and that is new. It used to be —
	// "/<app>.git/..." was the name of the materialised directory too — but the
	// URL carries "@branch" where the directory carries "/branch/", so the two
	// differ by more than a prefix. What is reused is the tail: everything after
	// the repository segment is git's own protocol path (/info/refs,
	// /git-receive-pack) and is passed through exactly as it arrived.
	projectRoot := filepath.Dir(repoPath)
	projectPath = "/" + filepath.ToSlash(filepath.Base(repoPath)) + protocolPathSuffix(r.URL.Path)
	if strings.HasSuffix(projectPath, ".git") {
		projectPath += "/"
	}

	query := r.URL.RawQuery
	gitProtocol := r.Header.Get("Git-Protocol")

	cmd := exec.CommandContext(r.Context(), t.backend)
	cmd.Env = t.cgiEnv(r, projectRoot, projectPath, query, gitProtocol)
	cmd.Stdin = r.Body
	defer r.Body.Close()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The backend's stderr is diagnostics for an operator, not for the caller:
	// it can name filesystem paths. It is captured and only surfaced on a
	// failure the client cannot otherwise understand.
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		_ = stored()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// A push is small and a fetch is not, and the difference decides how the
	// response is written.
	//
	// git's push response is buffered. Everything that can fail after it has been
	// produced — the upload below — is only knowable once it is complete, and a
	// response already streamed cannot be taken back. Holding it is what lets a
	// failed upload reach the pusher as a failure, instead of as "ok" and a line
	// in a log the pusher will never read. This is not theoretical: it is how a
	// push that git accepted and that object storage rejected reported success
	// while leaving the app with no commit.
	//
	// A fetch is streamed as it always was. It writes nothing, so there is no
	// later failure to report, and buffering a pack would hold a whole repository
	// in memory before the client saw its first byte.
	if isPush(r) {
		// The headers are copied before anything is decided. They are already
		// accurate — git's answer was produced against the push it accepted — and
		// git's client needs Content-Type to read the ref advertisement at all.
		reader, status, headerErr := readCGIHeaders(w, stdout)

		var body []byte
		bodyErr := headerErr
		if bodyErr == nil {
			body, bodyErr = io.ReadAll(reader)
		}
		waitErr := cmd.Wait()
		ok := stored()

		if bodyErr != nil || waitErr != nil {
			// git itself did not answer. Reporting this as a storage failure
			// would name the wrong fault, and the headers are already committed —
			// so this is the one case where the status has to be corrected after
			// the fact rather than chosen.
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "the push was received but could not be stored; push again", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	writeErr := t.copyCGIResponse(w, stdout)

	if err := cmd.Wait(); err != nil {
		// The response may already be partly written; all that can be done is to
		// stop, since the status line is long gone. A dangling error here is
		// usually a client that disconnected mid-clone, which is not a failure
		// worth surfacing to anyone.
		_ = stored()
		return
	}
	_ = stored()
	_ = writeErr
}

// isPush reports whether a request belongs to a push — git's receive-pack, the
// half that writes.
//
// Read from the request rather than from the response: receive-pack is a POST to
// /git-receive-pack, and the ref advertisement that precedes it asks for that
// service with `?service=git-receive-pack`. Between the two, every request of a
// push is recognised.
func isPush(r *http.Request) bool {
	if strings.Contains(r.URL.Path, "git-receive-pack") {
		return true
	}
	return r.URL.Query().Get("service") == "git-receive-pack"
}

// cgiEnv builds the environment git http-backend expects.
func (t *Transport) cgiEnv(r *http.Request, projectRoot, projectPath, query, gitProtocol string) []string {
	env := []string{
		// PATH and HOME are inherited so the backend can find its own
		// subprograms and does not try to read a real user's config.
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + projectRoot,

		"GIT_PROJECT_ROOT=" + projectRoot,
		"PATH_INFO=" + projectPath,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + query,
		"REMOTE_ADDR=" + remoteHost(r.RemoteAddr),
		"SERVER_PROTOCOL=" + r.Proto,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_SOFTWARE=applab",

		// Export every repository under the root without requiring a
		// git-daemon-export-ok file in each. Access is granted by AppLab's own
		// authentication, which has already run by the time this is called, so
		// the file-based check would be a second, weaker gate that has to be
		// remembered on every repository creation.
		"GIT_HTTP_EXPORT_ALL=1",

		// git only enables receive-pack (push) for an authenticated caller, and
		// the only signal it has for that is a non-empty REMOTE_USER. AppLab
		// authenticates with a key before reaching here, so the caller is
		// authenticated by construction and this states the fact.
		"REMOTE_USER=" + t.serviceUser,

		// The v2 protocol is a substantial improvement for clones with many refs
		// and is requested through this header. Without forwarding it, every
		// client silently falls back to v0.
		"GIT_PROTOCOL=" + gitProtocol,

		// The backend must not try to read the operator's git configuration, or
		// it would behave differently depending on where AppLab happens to run.
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}

	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		env = append(env, "HTTP_CONTENT_ENCODING="+enc)
	}

	return env
}

// copyCGIResponse reads the CGI response from the backend and writes it to the
// client.
//
// Split in two because the halves are needed at different times: a fetch writes
// the headers and streams the body straight through, while a push has to know
// the outcome of the upload before it commits to either — see writePushResponse.
func (t *Transport) copyCGIResponse(w http.ResponseWriter, stdout io.Reader) error {
	reader, status, err := readCGIHeaders(w, stdout)
	if err != nil {
		return err
	}

	// Flushed as it is read rather than buffered: a clone streams a pack that
	// can be large, and buffering it would hold the whole thing in memory and
	// delay the first byte until the backend finished.
	w.WriteHeader(status)
	_, err = io.Copy(&flushWriter{w: w}, reader)
	return err
}

// readCGIHeaders consumes the CGI header block, copying every header it carries
// onto w, and returns a reader positioned at the body along with the status.
//
// The CGI format is a header block terminated by a blank line, followed by the
// body. The only header with a meaning beyond HTTP is `Status:`, which a backend
// uses when it needs a status other than 200 — without translating it a failed
// fetch would arrive as a 200 with an error in the body, and git clients would
// report a confusing protocol error instead of the real one.
//
// `Content-Type` is among the copied headers and is not decoration: git's client
// decides whether a ref advertisement is a ref advertisement by it, and without
// it `git push` refuses with "not valid: is this a git repository?".
func readCGIHeaders(w http.ResponseWriter, stdout io.Reader) (*bufio.Reader, int, error) {
	reader := bufio.NewReader(stdout)
	header := w.Header()

	status := http.StatusOK
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return reader, status, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)

		if strings.EqualFold(name, "Status") {
			// The value is a CGI status: the number, optionally followed by a
			// reason phrase.
			codeText, _, _ := strings.Cut(value, " ")
			if code, err := strconv.Atoi(strings.TrimSpace(codeText)); err == nil {
				status = code
			}
			continue
		}
		header.Add(name, value)
	}

	// Content-Length from the backend is trustworthy for the responses git
	// sends, but a mismatched value would truncate or hang the client. Removing
	// it lets net/http frame the response itself, which is always correct.
	header.Del("Content-Length")

	return reader, status, nil
}

// flushWriter flushes after each write so a streaming response reaches the
// client promptly.
type flushWriter struct {
	w http.ResponseWriter
}

func (f *flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// resolvePath validates a request path and turns it into the PATH_INFO the
// backend expects, along with the repository name.
//
// The path is caller-controlled, so this is the boundary where a traversal would
// happen. It refuses rather than sanitises: a request whose path leaves the
// repository root is not a request for a file inside it under another name, and
// silently rewriting it would serve a repository the caller did not name.
//
// The repository name is returned as well as the PATH_INFO because the name *is*
// the app id — repositories are named "<app>.git" — and authorization is per
// app. Deriving it twice, once here and once by the caller, would be two
// implementations of the same rule, and the one that mattered would be the
// caller's.
// The stem is split on "@": the part before it is the app id and the part after
// it is the branch, so "/git/shop@dev.git" is the dev branch of shop. A URL with
// no "@" names no branch, which means the app's active one — see activeBranch.
//
// "@" rather than a path segment is a deliberate choice. A second segment
// ("/git/shop/dev.git") would put the branch inside the repository path that
// http-backend receives, and git reads a nested path as a subdirectory of a
// repository rather than as a name of its own. The "@" form keeps the repository
// one path segment, which is the shape both git and http-backend already agree
// on — verified against git's own URL parser, which strips credentials from the
// authority and leaves an "@" later in the path untouched.
func (t *Transport) resolvePath(urlPath string) (projectPath, repoName, branch string, ok bool) {
	if urlPath == "" {
		return "", "", "", false
	}

	// Work on the cleaned path so "//" and "." segments cannot hide a traversal
	// from the checks below.
	cleaned := path.Clean(urlPath)
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}

	// An encoded NUL or a backslash has no legitimate place in a repository URL
	// and exists only to confuse a later consumer of the string.
	if strings.ContainsAny(cleaned, "\x00\\") {
		return "", "", "", false
	}

	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", "", "", false
		}
	}

	// The first segment must be a repository directory. Requiring it to end in
	// .git keeps a request from addressing anything else under the root, and
	// matches the URL a caller is given.
	segments := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	if len(segments) == 0 || !strings.HasSuffix(segments[0], ".git") {
		return "", "", "", false
	}
	stem := strings.TrimSuffix(segments[0], ".git")
	if stem == "" {
		// "/.git" — a request for a repository that has no name, which on a bare
		// repository would be its own directory.
		return "", "", "", false
	}

	// At most one "@": a second one is not a branch name with an @ in it, and
	// treating it as one would mean deciding how to read "a@b@c", which has no
	// answer worth having.
	name, branch, _ := strings.Cut(stem, "@")
	if strings.Contains(branch, "@") {
		return "", "", "", false
	}

	// The repository name is an app id, so it is constrained exactly as one is.
	// This is what keeps a request from naming a directory AppLab did not create.
	if !validRepoName(name) {
		return "", "", "", false
	}
	// An empty branch is not a defect here: it means the URL named none, and the
	// caller resolves that through activeBranch. A branch that was given is
	// checked against the same rule the rest of AppLab uses, mirrored below.
	if branch != "" && !validBranchName(branch) {
		return "", "", "", false
	}

	return cleaned, name, branch, true
}

// validBranchName reports whether branch could be a branch name.
//
// It mirrors model.ValidateBranchName for the same reason validRepoName mirrors
// the app id rule: this is a security boundary, and a branch name reaches both a
// bucket key and git's argv from here. It is shorter than the model's version
// because it answers yes-or-no rather than explaining, and because the two have
// to agree rather than to be the same code — a test asserts they do.
func validBranchName(branch string) bool {
	if branch == "" || len(branch) > 255 {
		return false
	}
	// The shapes that would let a name out of the repository it belongs to, or
	// change what git is being asked to do.
	if strings.HasPrefix(branch, "-") ||
		strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") ||
		strings.HasSuffix(branch, ".") ||
		strings.HasSuffix(branch, ".lock") ||
		strings.Contains(branch, "//") ||
		strings.Contains(branch, "..") ||
		strings.Contains(branch, "@{") ||
		branch == "@" {
		return false
	}
	for _, r := range branch {
		switch r {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validRepoName reports whether name could be an app id. It mirrors the model's
// rule deliberately: this is a security boundary, and depending on a function
// that might later be relaxed for another purpose would quietly widen it.
func validRepoName(name string) bool {
	if name == "" || len(name) > 40 {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			// always allowed
		case c == '-':
			// A dash may not lead or trail, because the name has to be usable as
			// a DNS label in a namespace.
			if i == 0 || i == len(name)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// protocolPathSuffix returns the part of a request path after its first segment.
//
// It is what separates "which repository" from "what git is being asked to do":
// "/shop@dev.git/info/refs" leaves "/info/refs", which is the same string
// whatever the repository segment said. An empty result means the request named
// the repository and nothing else.
func protocolPathSuffix(urlPath string) string {
	trimmed := strings.TrimPrefix(path.Clean(urlPath), "/")
	if i := strings.Index(trimmed, "/"); i >= 0 {
		return trimmed[i:]
	}
	return ""
}

// remoteHost strips the port from a RemoteAddr.
func remoteHost(addr string) string {
	host, _, err := splitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// splitHostPort splits "host:port", tolerating a bare host.
func splitHostPort(addr string) (string, string, error) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, "", nil
	}
	return addr[:idx], addr[idx+1:], nil
}

// RepoRoot returns the directory holding the repositories, for callers that need
// to mount this transport somewhere.
func (t *Transport) RepoRoot() string { return t.repoRoot }
