package api

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/shaowenchen/applab/internal/appconfig"
)

// appConfigResponse is an app's configuration as the API presents it.
//
// The asymmetry between the two halves is the point, and it is structural rather
// than a matter of remembering: Env is returned with its values because
// environment variables are not sensitive by definition, and Secrets is a list of
// *names* with no values anywhere. There is no route in this package that
// returns a secret's value, and no store method on the request path that could
// supply one — appconfig.Contents exists for the deployer alone.
type appConfigResponse struct {
	AppID string `json:"app_id"`

	// Env is the app's plain configuration, with values.
	Env map[string]string `json:"env"`

	// Secrets are the names of the app's secret values. Never the values.
	Secrets []string `json:"secrets"`
}

// configEndpointsUnavailable is the failure every handler here shares.
//
// Secrets live in the cluster, so a deployment without one cannot hold any.
// That is the same shape as the key and build endpoints — a capability that is
// absent rather than broken — so it reports 501 with the reason rather than a
// 500 that reads as a fault.
func (s *Server) configEndpointsUnavailable(w http.ResponseWriter, r *http.Request) {
	fail(w, r, Errorf(http.StatusNotImplemented,
		"this deployment has no cluster, so app secrets are unavailable; environment variables are not affected"))
}

// setEnvRequest is the body of PUT .../env.
//
// Each value is a pointer so that an explicitly empty string — "set this to
// nothing" — is distinguishable from an absent field. Without that, clearing a
// variable and forgetting it would look the same.
type setEnvRequest struct {
	Env map[string]*string `json:"env"`
}

// setSecretsRequest is the body of PUT .../secrets.
type setSecretsRequest struct {
	Secrets map[string]*string `json:"secrets"`
}

// handleGetAppConfig returns an app's configuration.
//
// Reaching this route at all means the caller is either an admin or a key for
// this exact app; the middleware establishes that from the {app} in the path, so
// the handler does not re-check the scope.
func (s *Server) handleGetAppConfig(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	names, configErr := s.secretNames(r, app.ID)
	if configErr != nil {
		fail(w, r, configErr)
		return
	}

	respond(w, http.StatusOK, appConfigResponse{
		AppID:   app.ID,
		Env:     nonNilEnv(app.Env),
		Secrets: nonNilNames(names),
	})
}

// handleSetAppEnv replaces named environment variables.
//
// Names not mentioned are left alone, so setting one variable does not clear the
// rest. Removal is its own operation rather than setting a variable to the empty
// string, because the two mean different things to a reader of the configuration.
func (s *Server) handleSetAppEnv(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	var req setEnvRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	if len(req.Env) == 0 {
		fail(w, r, BadRequest("no environment variables given; expected {\"env\": {\"NAME\": \"value\"}}"))
		return
	}

	if app.Env == nil {
		app.Env = map[string]string{}
	}
	for name, value := range req.Env {
		if err := appconfig.ValidateName(name); err != nil {
			fail(w, r, BadRequest("%s", err.Error()))
			return
		}
		if value == nil {
			fail(w, r, BadRequest("environment variable %q has no value; to remove it use DELETE", name))
			return
		}
		app.Env[name] = *value
	}

	if err := s.store.UpdateApp(r.Context(), app); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
	}

	names, configErr := s.secretNames(r, app.ID)
	if configErr != nil {
		fail(w, r, configErr)
		return
	}
	respond(w, http.StatusOK, appConfigResponse{AppID: app.ID, Env: nonNilEnv(app.Env), Secrets: nonNilNames(names)})
}

// handleDeleteAppEnv removes one environment variable.
func (s *Server) handleDeleteAppEnv(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	name, err := configName(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	delete(app.Env, name)
	if err := s.store.UpdateApp(r.Context(), app); err != nil {
		fail(w, r, fromStoreError(err, fmt.Sprintf("app %q", app.ID)))
		return
	}

	names, configErr := s.secretNames(r, app.ID)
	if configErr != nil {
		fail(w, r, configErr)
		return
	}
	respond(w, http.StatusOK, appConfigResponse{AppID: app.ID, Env: nonNilEnv(app.Env), Secrets: nonNilNames(names)})
}

// handleSetAppSecrets writes secret values.
//
// The response lists names, and nothing else — writing a secret is the one
// request that legitimately carries its value, and echoing it back would put it
// in a response body, a log line and a shell history for no gain.
func (s *Server) handleSetAppSecrets(w http.ResponseWriter, r *http.Request) {
	if s.appConfig == nil || !s.appConfig.Ready() {
		s.configEndpointsUnavailable(w, r)
		return
	}

	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	var req setSecretsRequest
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	if len(req.Secrets) == 0 {
		fail(w, r, BadRequest("no secrets given; expected {\"secrets\": {\"NAME\": \"value\"}}"))
		return
	}

	values := make(map[string]string, len(req.Secrets))
	for name, value := range req.Secrets {
		if err := appconfig.ValidateName(name); err != nil {
			fail(w, r, BadRequest("%s", err.Error()))
			return
		}
		if value == nil {
			fail(w, r, BadRequest("secret %q has no value; to remove it use DELETE", name))
			return
		}
		values[name] = *value
	}

	names, setErr := s.appConfig.Set(r.Context(), app.ID, values)
	if setErr != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "store configuration for app %q", app.ID).Wrap(setErr))
		return
	}
	respond(w, http.StatusOK, appConfigResponse{AppID: app.ID, Env: nonNilEnv(app.Env), Secrets: nonNilNames(names)})
}

// handleDeleteAppSecret removes one secret value.
func (s *Server) handleDeleteAppSecret(w http.ResponseWriter, r *http.Request) {
	if s.appConfig == nil || !s.appConfig.Ready() {
		s.configEndpointsUnavailable(w, r)
		return
	}

	app, err := s.loadApp(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	name, err := configName(r)
	if err != nil {
		fail(w, r, err)
		return
	}

	names, removeErr := s.appConfig.Remove(r.Context(), app.ID, []string{name})
	if removeErr != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "remove configuration for app %q", app.ID).Wrap(removeErr))
		return
	}
	respond(w, http.StatusOK, appConfigResponse{AppID: app.ID, Env: nonNilEnv(app.Env), Secrets: nonNilNames(names)})
}

// secretNames lists an app's secret names, tolerating a deployment without a
// cluster.
//
// A deployment with no cluster can hold no secrets, so an empty list is the
// truthful answer rather than an error — the app's environment variables are
// still perfectly readable, and failing the whole request because the other half
// is unavailable would hide them for no reason.
func (s *Server) secretNames(r *http.Request, appID string) ([]string, *apiError) {
	if s.appConfig == nil || !s.appConfig.Ready() {
		return nil, nil
	}
	names, err := s.appConfig.Names(r.Context(), appID)
	if err != nil {
		return nil, Errorf(http.StatusInternalServerError, "read the configuration for app %q", appID).Wrap(err)
	}
	return names, nil
}

// configName reads the {name} path segment, decoded.
//
// The segment arrives percent-encoded and is decoded here rather than being used
// raw, because an environment variable name is validated against a pattern and a
// name that reached the validator still encoded would be rejected for the wrong
// reason — "FOO%5F BAR is not valid" instead of the real problem.
func configName(r *http.Request) (string, *apiError) {
	raw := r.PathValue("name")
	if raw == "" {
		return "", BadRequest("no configuration name in the request path")
	}
	name, err := url.PathUnescape(raw)
	if err != nil {
		return "", BadRequest("invalid configuration name %q in the request path", raw)
	}
	if err := appconfig.ValidateName(name); err != nil {
		return "", BadRequest("%s", err.Error())
	}
	return name, nil
}

// nonNilNames makes an absent secret list serialise as [] rather than null.
//
// A client iterating the field would otherwise have to handle null, and "this
// app has no secrets" is better said as an empty list than as an absence. It is
// the same reasoning as nonNilEnv, on the other half of the response.
func nonNilNames(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// nonNilEnv makes an absent environment map serialise as {} rather than null.
//
// A client iterating the field would otherwise have to handle null, and "this
// app has no variables" is better said as an empty object than as an absence.
func nonNilEnv(env map[string]string) map[string]string {
	if env == nil {
		return map[string]string{}
	}
	return env
}
