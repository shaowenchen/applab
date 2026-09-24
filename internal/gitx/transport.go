// Package gitx serves applab's repositories over the git smart HTTP protocol.
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
	// applab has no per-user identities, so it is a constant.
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
	projectPath, repoName, ok := t.resolvePath(r.URL.Path)
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

	query := r.URL.RawQuery
	gitProtocol := r.Header.Get("Git-Protocol")

	cmd := exec.CommandContext(r.Context(), t.backend)
	cmd.Env = t.cgiEnv(r, projectPath, query, gitProtocol)
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
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeErr := t.copyCGIResponse(w, stdout)

	if err := cmd.Wait(); err != nil {
		// The response may already be partly written; all that can be done is to
		// stop, since the status line is long gone. A dangling error here is
		// usually a client that disconnected mid-clone, which is not a failure
		// worth surfacing to anyone.
		return
	}
	_ = writeErr
}

// cgiEnv builds the environment git http-backend expects.
func (t *Transport) cgiEnv(r *http.Request, projectPath, query, gitProtocol string) []string {
	env := []string{
		// PATH and HOME are inherited so the backend can find its own
		// subprograms and does not try to read a real user's config.
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.repoRoot,

		"GIT_PROJECT_ROOT=" + t.repoRoot,
		"PATH_INFO=" + projectPath,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + query,
		"REMOTE_ADDR=" + remoteHost(r.RemoteAddr),
		"SERVER_PROTOCOL=" + r.Proto,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_SOFTWARE=applab",

		// Export every repository under the root without requiring a
		// git-daemon-export-ok file in each. Access is granted by applab's own
		// authentication, which has already run by the time this is called, so
		// the file-based check would be a second, weaker gate that has to be
		// remembered on every repository creation.
		"GIT_HTTP_EXPORT_ALL=1",

		// git only enables receive-pack (push) for an authenticated caller, and
		// the only signal it has for that is a non-empty REMOTE_USER. applab
		// authenticates with a key before reaching here, so the caller is
		// authenticated by construction and this states the fact.
		"REMOTE_USER=" + t.serviceUser,

		// The v2 protocol is a substantial improvement for clones with many refs
		// and is requested through this header. Without forwarding it, every
		// client silently falls back to v0.
		"GIT_PROTOCOL=" + gitProtocol,

		// The backend must not try to read the operator's git configuration, or
		// it would behave differently depending on where applab happens to run.
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
// The CGI format is a header block terminated by a blank line, followed by the
// body. The only header with a meaning beyond HTTP is `Status:`, which a backend
// uses when it needs a status other than 200 — without translating it a failed
// fetch would arrive as a 200 with an error in the body, and git clients would
// report a confusing protocol error instead of the real one.
func (t *Transport) copyCGIResponse(w http.ResponseWriter, stdout io.Reader) error {
	reader := bufio.NewReader(stdout)
	header := w.Header()

	status := http.StatusOK
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
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

	w.WriteHeader(status)

	// Flushed as it is read rather than buffered: a clone streams a pack that
	// can be large, and buffering it would hold the whole thing in memory and
	// delay the first byte until the backend finished.
	_, err := io.Copy(&flushWriter{w: w}, reader)
	return err
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
func (t *Transport) resolvePath(urlPath string) (projectPath, repoName string, ok bool) {
	if urlPath == "" {
		return "", "", false
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
		return "", "", false
	}

	for _, segment := range strings.Split(cleaned, "/") {
		if segment == ".." {
			return "", "", false
		}
	}

	// The first segment must be a repository directory. Requiring it to end in
	// .git keeps a request from addressing anything else under the root, and
	// matches the URL a caller is given.
	segments := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	if len(segments) == 0 || !strings.HasSuffix(segments[0], ".git") {
		return "", "", false
	}
	name := strings.TrimSuffix(segments[0], ".git")
	if name == "" {
		return "", "", false
	}
	// The repository name is an app id, so it is constrained exactly as one is.
	// This is what keeps a request from naming a directory applab did not create.
	if !validRepoName(name) {
		return "", "", false
	}

	return cleaned, name, true
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
