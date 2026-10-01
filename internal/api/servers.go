package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/shaowenchen/applab/internal/auth"
	"github.com/shaowenchen/applab/internal/client"
	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/serverreg"
)

// This file is the remote half of AppLab: the servers an administrator has
// registered, and the proxies that reach the apps on them.
//
// The shape is forced by one fact about the console: it is same-origin. Its CSP
// allows connections to 'self' and nothing else, and it derives its API base from
// a meta tag the server injects — so a browser can never talk to a second AppLab
// directly. Whoever manages another deployment therefore does it through this
// one, which relays the call and holds the remote's admin key.
//
// Everything here is admin-only, because a server key is an admin key and the
// ability to reach a whole other platform is not something an app key — scoped to
// one app — may borrow.

// serverRegistry is the registration store as this layer needs it.
//
// It is an interface rather than the concrete *serverreg.Store so the handlers
// can be tested with a fake and so a deployment with no cluster can attach nil,
// exactly as the build and deploy halves do.
type serverRegistry interface {
	Ready() bool
	List(ctx context.Context) ([]serverreg.Server, error)
	Get(ctx context.Context, id string) (*serverreg.Server, error)
	Key(ctx context.Context, id string) (string, error)
	Put(ctx context.Context, server serverreg.Server, key string) error
	Remove(ctx context.Context, id string) error
}

// WithServerRegistry attaches the store that holds registered remote servers.
func (s *Server) WithServerRegistry(r serverRegistry) *Server { s.servers = r; return s }

// canProxyToRemote reports whether registrations can be read and written.
//
// False means one thing only: this deployment came up without cluster access, so
// there is nowhere to keep a registration. It does not stop the local entry from
// working — "this deployment" needs no registration to be listed or managed.
func (s *Server) canProxyToRemote() bool { return s.servers != nil && s.servers.Ready() }

// proxyTimeout bounds one proxied call.
//
// It is set here rather than inherited from the server's own write timeout,
// which is measured in tens of minutes for uploads and log streams. A remote that
// accepts a connection and then says nothing would otherwise hold a handler
// goroutine for all of that.
const proxyTimeout = 15 * time.Second

// registerServerRequest is the body of POST /api/v1/servers.
type registerServerRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Key  string `json:"key"`
}

// handleListServers returns the built-in local entry followed by every
// registration.
func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	out := []serverreg.Server{s.localServer(r)}

	if s.canProxyToRemote() {
		servers, err := s.servers.List(r.Context())
		if err != nil {
			fail(w, r, Errorf(http.StatusServiceUnavailable, "list registered servers").Wrap(err))
			return
		}
		out = append(out, servers...)
	}
	respond(w, http.StatusOK, out)
}

// handleRegisterServer validates a remote and stores it.
//
// Nothing is written until the remote has answered, so a wrong address or a wrong
// key is a 400 the caller sees immediately rather than an entry that fails later
// with no indication of which part was wrong.
func (s *Server) handleRegisterServer(w http.ResponseWriter, r *http.Request) {
	if !s.canProxyToRemote() {
		fail(w, r, Errorf(http.StatusNotImplemented,
			"this deployment cannot store servers: it is running without cluster access, and registrations live in a Kubernetes Secret"))
		return
	}

	var req registerServerRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	id := strings.TrimSpace(req.ID)
	if err := model.ValidateAppID(id); err != nil {
		fail(w, r, BadRequest("%s", err.Error()))
		return
	}
	if id == serverreg.LocalID {
		fail(w, r, BadRequest("%q is the built-in entry for this deployment and cannot be registered", serverreg.LocalID))
		return
	}

	address := strings.TrimSpace(req.URL)
	key := strings.TrimSpace(req.Key)
	if key == "" {
		fail(w, r, BadRequest("a server needs an admin key"))
		return
	}
	if err := s.validateServerURL(r, address); err != nil {
		fail(w, r, BadRequest("%s", err.Error()))
		return
	}

	// Probe before storing. Config needs no key, so it proves the address is
	// reachable and speaks AppLab; Overview is the cheapest admin-only route, so
	// it proves the key is an *admin* key rather than any working key. (Listing
	// apps would accept an app key and return one app, validating nothing.)
	probe, err := client.New(client.Options{BaseURL: address, Key: key, Timeout: proxyTimeout})
	if err != nil {
		fail(w, r, BadRequest("%s", err.Error()))
		return
	}
	if _, err := probe.Config(r.Context()); err != nil {
		fail(w, r, BadRequest("could not reach an AppLab at %q: %s", address, remoteMessage(err)))
		return
	}
	if _, err := probe.Overview(r.Context()); err != nil {
		fail(w, r, BadRequest("the key for %q was refused: %s", id, remoteMessage(err)))
		return
	}

	name := strings.TrimSpace(req.Name)
	server := serverreg.Server{ID: id, Name: name, URL: address, CreatedAt: time.Now().UTC()}
	if err := s.servers.Put(r.Context(), server, key); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "register server %q", id).Wrap(err))
		return
	}

	// serverreg fills an empty name from the id on read; mirror that here so the
	// response matches what a later GET returns.
	if server.Name == "" {
		server.Name = id
	}
	respond(w, http.StatusCreated, server)
}

// handleGetServer returns one registration, without its key.
func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("server")
	if id == serverreg.LocalID {
		respond(w, http.StatusOK, s.localServer(r))
		return
	}
	if !s.canProxyToRemote() {
		fail(w, r, notFoundRemote(id))
		return
	}
	server, err := s.servers.Get(r.Context(), id)
	if err != nil {
		fail(w, r, fromServerError(err, id))
		return
	}
	respond(w, http.StatusOK, server)
}

// handleRemoveServer forgets a registration.
//
// It touches the local record only. A decommissioned remote may be gone entirely,
// and removing its registration must not require it to still answer.
func (s *Server) handleRemoveServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("server")
	if id == serverreg.LocalID {
		fail(w, r, Conflict("%q is this deployment and cannot be removed", serverreg.LocalID))
		return
	}
	if !s.canProxyToRemote() {
		fail(w, r, notFoundRemote(id))
		return
	}
	if err := s.servers.Remove(r.Context(), id); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "remove server %q", id).Wrap(err))
		return
	}
	respond(w, http.StatusOK, map[string]any{"id": id, "removed": true})
}

// localRequest marks a request reaching a local handler through this file as the
// admin tier.
//
// It is not cosmetic. The handlers for apps narrow their result set by the
// identity in the context, and the plain admin middleware that guards every route
// here does not put one there — it authenticates and forgets. Handing
// handleListApps an unmarked request would therefore read as anonymous and filter
// out every app, so `GET /api/v1/servers/local/apps` would answer with an empty
// list while `GET /api/v1/apps` answered with all of them.
//
// A route here is admin-only by construction — the middleware refuses anything
// else — so the identity is known without re-resolving the key, and re-resolving
// it would be a second look at the key store on every such call.
func localRequest(r *http.Request) *http.Request {
	return withIdentity(r, auth.Identity{})
}

// handleServerListApps lists the apps on a server.
func (s *Server) handleServerListApps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("server")
	if id == serverreg.LocalID {
		// In process, on this deployment's own store. Not an HTTP call to our own
		// public address: that would re-enter auth, resolve the base path a second
		// time, and turn one call into two.
		s.handleListApps(w, localRequest(r))
		return
	}

	remote, err := s.remoteClient(r, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	apps, err := remote.ListApps(r.Context())
	if err != nil {
		s.failFromRemote(w, r, id, err)
		return
	}
	respond(w, http.StatusOK, apps)
}

// handleServerCreateApp creates an app on a server.
//
// The remote's response is relayed whole, app key included: creating an app is
// the one moment its key is handed out, and the administrator who just asked for
// it is entitled to it — the same answer the local create gives. The key in the
// body is the *new app's* key, not the remote's admin key, which never leaves
// this process.
func (s *Server) handleServerCreateApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("server")
	if id == serverreg.LocalID {
		s.handleCreateApp(w, localRequest(r))
		return
	}

	var req createAppRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	remote, err := s.remoteClient(r, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	created, err := remote.CreateApp(r.Context(), client.CreateAppRequest{
		ID:         req.ID,
		Name:       req.Name,
		Port:       req.Port,
		Replicas:   req.Replicas,
		Dockerfile: req.Dockerfile,
		Domain:     req.Domain,
		AutoDeploy: req.AutoDeploy,
	})
	if err != nil {
		s.failFromRemote(w, r, id, err)
		return
	}
	respond(w, http.StatusCreated, created)
}

// handleServerGetApp returns one app on a server.
func (s *Server) handleServerGetApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("server")
	if id == serverreg.LocalID {
		s.handleGetApp(w, localRequest(r))
		return
	}

	remote, err := s.remoteClient(r, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	app, err := remote.GetApp(r.Context(), r.PathValue("app"))
	if err != nil {
		s.failFromRemote(w, r, id, err)
		return
	}
	respond(w, http.StatusOK, app)
}

// handleServerDeleteApp deletes an app on a server.
func (s *Server) handleServerDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("server")
	if id == serverreg.LocalID {
		s.handleDeleteApp(w, localRequest(r))
		return
	}

	remote, err := s.remoteClient(r, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	keepSource := r.URL.Query().Get("keep_source") == "true"
	appID := r.PathValue("app")
	if err := remote.DeleteApp(r.Context(), appID, keepSource); err != nil {
		s.failFromRemote(w, r, id, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"id": appID, "deleted": true, "keep_source": keepSource})
}

// remoteClient builds a client for one registered server.
//
// The key is read here, used to build the client, and dropped. It is never put
// into a response and never logged: the client sends it in a header and nowhere
// else, which is the property internal/client already guarantees.
func (s *Server) remoteClient(r *http.Request, id string) (*client.Client, error) {
	if !s.canProxyToRemote() {
		return nil, notFoundRemote(id)
	}
	server, err := s.servers.Get(r.Context(), id)
	if err != nil {
		return nil, fromServerError(err, id)
	}
	key, err := s.servers.Key(r.Context(), id)
	if err != nil {
		// A registration whose Secret lost its key. Reported as an internal
		// problem with the record rather than as the remote's fault.
		return nil, Errorf(http.StatusInternalServerError, "the registration for server %q has no key; re-register it", id).Wrap(err)
	}
	remote, err := client.New(client.Options{BaseURL: server.URL, Key: key, Timeout: proxyTimeout})
	if err != nil {
		return nil, Errorf(http.StatusInternalServerError, "the registration for server %q is unusable", id).Wrap(err)
	}
	return remote, nil
}

// failFromRemote maps a remote's failure onto this deployment's response.
//
// The one non-obvious line is the 401: a remote that refuses our stored key is
// *our* problem, not the caller's — the caller's own key authenticated fine
// against this deployment. Reporting 401 would send them to check a key that is
// not the broken one; 502 with a message naming the server is the actionable
// answer, and it mirrors how a key store that cannot be reached is a 503 rather
// than a 401.
func (s *Server) failFromRemote(w http.ResponseWriter, r *http.Request, id string, err error) {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusUnauthorized:
			fail(w, r, Errorf(http.StatusBadGateway,
				"server %q refused the key registered for it; re-register it", id))
		case apiErr.Status >= 500:
			fail(w, r, Errorf(http.StatusBadGateway, "server %q failed: %s", id, apiErr.Message).Retryable())
		default:
			// A 4xx is about the request, and the remote's own wording is what
			// the caller needs — passed through with the server named so a
			// message about "app \"shop\"" is unambiguous about where.
			fail(w, r, Errorf(apiErr.Status, "server %q: %s", id, apiErr.Message))
		}
		return
	}
	fail(w, r, Errorf(http.StatusBadGateway, "cannot reach server %q", id).Retryable().Wrap(err))
}

// remoteMessage renders a probe failure as one line for a 400 body.
//
// The cause is deliberately not wrapped: a probe failure is reported to the
// caller directly, and the underlying error can carry the remote's address. The
// kind of failure is what helps — refused, timed out, not an AppLab.
func remoteMessage(err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("%s (HTTP %d)", apiErr.Message, apiErr.Status)
	}
	return "the connection failed"
}

// localServer is the composed entry for this deployment.
//
// Its address is the deployment's public URL, exactly what a caller would set
// APPLAB_URL to — not a bare base path, which is only half an address and would
// read as a relative path in a list.
func (s *Server) localServer(r *http.Request) serverreg.Server {
	return serverreg.Server{
		ID:      serverreg.LocalID,
		Name:    "This deployment",
		URL:     s.publicURL(r),
		Builtin: true,
	}
}

// notFoundRemote is the answer for a server that is not registered.
//
// A deployment with no cluster has no registrations at all, so every id is
// unknown — answered as "no such server", which is true, rather than as a 501
// that would describe an internal limitation on a read.
func notFoundRemote(id string) *apiError {
	return NotFound("server %q", id)
}

// fromServerError maps a registry error onto an HTTP one.
func fromServerError(err error, id string) *apiError {
	switch {
	case errors.Is(err, serverreg.ErrNotFound):
		return NotFound("server %q", id)
	case errors.Is(err, serverreg.ErrReserved):
		return BadRequest("%q is the built-in entry for this deployment", serverreg.LocalID)
	default:
		return Errorf(http.StatusInternalServerError, "read server %q", id).Wrap(err)
	}
}

// validateServerURL refuses an address that could be a liability rather than a
// remote AppLab.
//
// The caller is an administrator, so this is not a privilege boundary — it is a
// guard against the ordinary mistakes and the one interesting attack: a
// registered URL is fetched by this server with a key attached, so
// `http://169.254.169.254/` is a request to have the platform read a cloud
// metadata service on the administrator's behalf. Refused by default, with no
// override in this version.
func (s *Server) validateServerURL(r *http.Request, address string) error {
	if address == "" {
		return fmt.Errorf("a server needs an address")
	}

	parsed, err := url.Parse(address)
	if err != nil {
		return fmt.Errorf("server address %q is not a URL: %s", address, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// No scheme is refused rather than assumed. client.New would prepend
		// https://, which is right for a typed host and wrong here: we store what
		// the caller gave us, so what they see must be what is used.
		return fmt.Errorf("server address %q must start with http:// or https://", address)
	}
	if parsed.Host == "" {
		return fmt.Errorf("server address %q has no host", address)
	}
	if parsed.User != nil {
		// A credential in the URL would sit beside the key we add and end up in
		// any log that records the request line. The key belongs in the header,
		// which is the only place client sends it.
		return fmt.Errorf("server address %q must not carry a username or password; the key is sent as a header", address)
	}

	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("server address %q has no host", address)
	}
	if isLinkLocal(host) {
		return fmt.Errorf("server address %q points at a link-local or metadata address, which this deployment will not call", address)
	}

	// This deployment's own address is refused so a registration cannot be a
	// proxy loop. Compared by host against the address this request reached us
	// at, which is the one address we know is ours.
	if self, err := url.Parse(s.publicURL(r)); err == nil && strings.EqualFold(self.Hostname(), host) {
		return fmt.Errorf("server address %q is this deployment's own address", address)
	}
	return nil
}

// isLinkLocal reports whether a host is one this deployment will not call.
//
// Literal addresses only — no DNS is resolved, because resolving here and storing
// the result would pin a remote to one address and break the moment it moved.
// The link-local range is what cloud metadata services live on, and it is the one
// destination a legitimate registration never has.
func isLinkLocal(host string) bool {
	if strings.EqualFold(host, "metadata.google.internal") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}
