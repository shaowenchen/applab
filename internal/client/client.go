// Package client talks to an AppLab deployment.
//
// It is the same code path the CLI uses, kept separate so the API surface is
// exercised through exactly one implementation. The alternative — a CLI that
// builds requests itself — means every new endpoint is written twice and drifts.
package client

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Client is a handle to one AppLab deployment.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// Options configure a Client.
type Options struct {
	// BaseURL is the deployment's address, e.g. https://applab.example.com.
	BaseURL string

	// Key is the API key. A key is the whole identity; there is no user store.
	Key string

	// Timeout bounds a single request. Uploads and log streams override it
	// per-call, because a source archive takes as long as the connection needs
	// and a followed log has no natural end.
	Timeout time.Duration
}

// New creates a Client.
func New(opts Options) (*Client, error) {
	base := strings.TrimSuffix(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("no applab URL given: set APPLAB_URL or pass --url")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		// A bare host is what someone types; assuming https is right for a real
		// deployment and the alternative would send the key in clear text.
		base = "https://" + base
	}
	if strings.TrimSpace(opts.Key) == "" {
		return nil, fmt.Errorf("no API key given: set APPLAB_KEY or pass --key")
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &Client{
		baseURL: base,
		key:     strings.TrimSpace(opts.Key),
		http: &http.Client{
			Timeout: timeout,
			// Redirects are not followed: a request carrying a key that gets
			// redirected would send the key again to wherever it points, and a
			// deployment has no legitimate reason to redirect an API call.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// BaseURL returns the deployment's address.
func (c *Client) BaseURL() string { return c.baseURL }

// APIError is a failure reported by the deployment.
type APIError struct {
	Status int
	// Message is the handler's own text, which is written to be acted on.
	Message string
	// Retryable is the server's explicit signal for the cases where a status
	// alone is ambiguous.
	Retryable bool
}

func (e *APIError) Error() string {
	if e.Retryable {
		return fmt.Sprintf("%s (retryable)", e.Message)
	}
	return e.Message
}

// do performs a request and decodes the response envelope into out.
//
// The key goes in a header and nowhere else: URLs are written to access logs,
// kept in shell history and sent in Referer headers, so a credential in one leaks
// by default.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}

	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, path, err)
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, path, err)
	}
	return nil
}

// decodeError reads the error envelope.
//
// A proxy in front of the deployment can answer with HTML rather than JSON — a
// 413 from an ingress body limit is the usual one — so an undecodable body is
// reported with its status and a trimmed excerpt rather than a parse failure,
// which would hide the actual cause.
func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var envelope struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Error != "" {
		return &APIError{Status: resp.StatusCode, Message: envelope.Error, Retryable: envelope.Retryable}
	}

	excerpt := strings.TrimSpace(string(raw))
	if len(excerpt) > 200 {
		excerpt = excerpt[:200] + "…"
	}
	if excerpt == "" {
		excerpt = "(empty response body)"
	}
	return &APIError{
		Status:  resp.StatusCode,
		Message: fmt.Sprintf("%s: %s", resp.Status, excerpt),
		// A 5xx from something other than AppLab is usually a proxy having a bad
		// moment, which is worth retrying; a 4xx is not.
		Retryable: resp.StatusCode >= 500,
	}
}

// Config is the deployment's self-description.
type Config struct {
	APIVersion string `json:"api_version"`
	Version    string `json:"version"`
	APIBaseURL string `json:"api_base_url"`

	// BaseDomain is the host apps are served under, and DomainTemplate how an
	// app id becomes an address — "*.<domain>" with a subdomain per app, or
	// "<domain><prefix>/<app>" when PathPrefix puts them all on one host.
	BaseDomain     string `json:"base_domain"`
	PathPrefix     string `json:"path_prefix"`
	DomainTemplate string `json:"domain_template"`

	Namespace       string          `json:"namespace"`
	MaxSimpleUpload int64           `json:"max_simple_upload"`
	ChunkSize       int64           `json:"chunk_size"`
	MaxChunkBytes   int64           `json:"max_chunk_bytes"`
	Capabilities    map[string]bool `json:"capabilities"`
}

// Config reads the deployment's limits and capabilities.
//
// It needs no key, so it doubles as the reachability check a command runs before
// doing anything else — a clear "cannot reach the deployment" is a far better
// first error than an authentication failure from a typo in the URL.
func (c *Client) Config(ctx context.Context) (*Config, error) {
	var out Config
	if err := c.do(ctx, http.MethodGet, "/api/v1/config", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Overview is the platform at a glance.
//
// It is one call rather than a list-then-tally on the client, because the
// counting belongs where the database can do it and because "how many apps are
// failing" should not change meaning as the deployment grows.
type Overview struct {
	Apps    OverviewApps    `json:"apps"`
	Builds  OverviewBuilds  `json:"builds"`
	Cluster OverviewCluster `json:"cluster"`

	// Deployment is the same self-description Config returns.
	Deployment Config `json:"deployment"`
}

// OverviewApps counts apps by status. Deleted apps are not counted.
type OverviewApps struct {
	Total       int `json:"total"`
	Created     int `json:"created"`
	Building    int `json:"building"`
	Deploying   int `json:"deploying"`
	Running     int `json:"running"`
	Failed      int `json:"failed"`
	BuildFailed int `json:"build_failed"`

	// NeedsAttention is Failed plus BuildFailed: the apps that are not running
	// because something went wrong, as opposed to those simply in progress.
	NeedsAttention int `json:"needs_attention"`
}

// OverviewBuilds counts builds and carries the most recent ones.
type OverviewBuilds struct {
	Total     int     `json:"total"`
	Pending   int     `json:"pending"`
	Running   int     `json:"running"`
	Succeeded int     `json:"succeeded"`
	Failed    int     `json:"failed"`
	Recent    []Build `json:"recent"`
}

// OverviewCluster reports whether the cluster half is usable.
//
// Configured and Reachable are separate because the answers differ: a deployment
// with no cluster is a legitimate way to run AppLab, while one whose cluster it
// cannot reach is broken. Reporting a single boolean would make the first look
// like the second.
type OverviewCluster struct {
	Configured bool `json:"configured"`
	Reachable  bool `json:"reachable"`
}

// Overview reads the platform's state at a glance.
func (c *Client) Overview(ctx context.Context) (*Overview, error) {
	var out Overview
	if err := c.do(ctx, http.MethodGet, "/api/v1/overview", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AppKey is an app's own API key.
//
// It is the second tier of credential: it reaches this app and nothing else, so
// it is what to hand to whoever deploys the app rather than the admin key, which
// can delete every app the installation manages.
type AppKey struct {
	AppID string `json:"app_id"`
	Key   string `json:"key"`
}

// GetAppKey reads an app's key.
//
// It works with either credential: an admin key may read any app's, and an app
// key may read its own.
func (c *Client) GetAppKey(ctx context.Context, appID string) (*AppKey, error) {
	var out AppKey
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/key", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RotateAppKey replaces an app's key, invalidating the previous one at once.
//
// It also creates a key for an app that has none, so this is the way to recover
// one that was lost rather than a separate operation.
func (c *Client) RotateAppKey(ctx context.Context, appID string) (*AppKey, error) {
	var out AppKey
	if err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/key/rotate", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Branches is what an app has source for, and which of them is running.
type Branches struct {
	AppID    string   `json:"app_id"`
	Branches []string `json:"branches"`
	Active   string   `json:"active"`
}

// ListBranches reports the branches an app has.
//
// A branch exists once something has been pushed to it, so this is the answer to
// "what can I switch to".
func (c *Client) ListBranches(ctx context.Context, appID string) (*Branches, error) {
	var out Branches
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/branches", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UseBranch makes a branch active and deploys it.
//
// It is one call rather than two because a branch that is active but not
// deployed is not active: the app would report itself on a branch whose code is
// not running.
func (c *Client) UseBranch(ctx context.Context, appID, branch string) (*DeployResult, error) {
	body, err := json.Marshal(map[string]string{"branch": branch})
	if err != nil {
		return nil, err
	}
	var out DeployResult
	if err := c.do(ctx, http.MethodPut, "/api/v1/apps/"+appID+"/branch",
		bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AppConfig is an app's configuration.
//
// The asymmetry is the API's, and it is deliberate: Env carries values because
// environment variables are not sensitive, and Secrets carries only names —
// there is no route anywhere that returns a secret's value.
type AppConfig struct {
	AppID   string            `json:"app_id"`
	Env     map[string]string `json:"env"`
	Secrets []string          `json:"secrets"`
}

// GetAppConfig reads an app's configuration.
func (c *Client) GetAppConfig(ctx context.Context, appID string) (*AppConfig, error) {
	var out AppConfig
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/config", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetAppEnv sets environment variables, leaving any name not given alone.
func (c *Client) SetAppEnv(ctx context.Context, appID string, env map[string]string) (*AppConfig, error) {
	body, err := json.Marshal(map[string]any{"env": env})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var out AppConfig
	if err := c.do(ctx, http.MethodPut, "/api/v1/apps/"+appID+"/env", bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAppEnv removes one environment variable.
func (c *Client) DeleteAppEnv(ctx context.Context, appID, name string) (*AppConfig, error) {
	var out AppConfig
	path := "/api/v1/apps/" + appID + "/env/" + url.PathEscape(name)
	if err := c.do(ctx, http.MethodDelete, path, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetAppSecrets writes secret values, leaving any name not given alone.
//
// The response lists names only. The values went out in the request and are not
// echoed back — there is nothing to read them from afterwards either, by design.
func (c *Client) SetAppSecrets(ctx context.Context, appID string, secrets map[string]string) (*AppConfig, error) {
	body, err := json.Marshal(map[string]any{"secrets": secrets})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var out AppConfig
	if err := c.do(ctx, http.MethodPut, "/api/v1/apps/"+appID+"/secrets", bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAppSecret removes one secret.
func (c *Client) DeleteAppSecret(ctx context.Context, appID, name string) (*AppConfig, error) {
	var out AppConfig
	path := "/api/v1/apps/" + appID + "/secrets/" + url.PathEscape(name)
	if err := c.do(ctx, http.MethodDelete, path, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// App is an app as the API presents it.
type App struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	Port     int32 `json:"port"`
	Replicas int32 `json:"replicas"`

	Dockerfile string `json:"dockerfile"`
	Domain     string `json:"domain"`

	// EnvCount is how many environment variables the app has. The variables
	// themselves come from AppConfig, so a list does not carry them.
	EnvCount int `json:"env_count"`

	// Hostname is the host an app answers on and Path where under it. With a
	// shared path prefix the host is the deployment's rather than the app's, so
	// neither field alone is the address; URL is the whole thing.
	Hostname string `json:"hostname"`
	Path     string `json:"path"`
	URL      string `json:"url"`

	// Branch is the app's active branch: what a deploy builds from, and the
	// branch a clone with no branch named gets.
	Branch string `json:"branch"`

	CommitSHA string `json:"commit_sha"`
	Image     string `json:"image"`

	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateApp creates an app.
func (c *Client) CreateApp(ctx context.Context, req CreateAppRequest) (*App, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var out App
	if err := c.do(ctx, http.MethodPost, "/api/v1/apps", bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateAppRequest is the body of a create.
//
// The optional fields are pointers so that "not set" is distinguishable from
// "set to zero", which is what lets the server's defaults apply.
type CreateAppRequest struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Port       *int32 `json:"port,omitempty"`
	Replicas   *int32 `json:"replicas,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Domain     string `json:"domain,omitempty"`
}

// ListApps returns every app.
func (c *Client) ListApps(ctx context.Context, includeDeleted bool) ([]App, error) {
	path := "/api/v1/apps"
	if includeDeleted {
		path += "?include_deleted=true"
	}

	var out []App
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetApp returns one app.
func (c *Client) GetApp(ctx context.Context, id string) (*App, error) {
	var out App
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+id, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteApp deletes an app.
func (c *Client) DeleteApp(ctx context.Context, id string, keepSource bool) error {
	path := "/api/v1/apps/" + id
	if keepSource {
		path += "?keep_source=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, "", nil)
}

// UpdateAppRequest is the body of a settings change. Every field is a pointer so
// an omitted one is left alone.
type UpdateAppRequest struct {
	Name       *string `json:"name,omitempty"`
	Port       *int32  `json:"port,omitempty"`
	Replicas   *int32  `json:"replicas,omitempty"`
	Dockerfile *string `json:"dockerfile,omitempty"`
	Domain     *string `json:"domain,omitempty"`
}

// UpdateApp changes an app's settings.
func (c *Client) UpdateApp(ctx context.Context, id string, req UpdateAppRequest) (*App, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var out App
	if err := c.do(ctx, http.MethodPatch, "/api/v1/apps/"+id, bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadResult is what a source upload produced.
type UploadResult struct {
	CommitSHA    string `json:"commit_sha"`
	Message      string `json:"message"`
	Files        int    `json:"files"`
	Bytes        int64  `json:"bytes"`
	StrippedRoot string `json:"stripped_root"`
}

// UploadSource sends a source archive.
//
// The body is streamed as the request body rather than buffered, so a large
// archive does not have to fit in memory. Over the simple-upload limit the caller
// is told to use the chunked path instead of being rejected with an opaque 413.
func (c *Client) UploadSource(ctx context.Context, appID string, archive io.Reader, compressed bool, message string) (*UploadResult, error) {
	path := "/api/v1/apps/" + appID + "/source"
	if message != "" {
		path += "?message=" + urlQueryEscape(message)
	}

	contentType := "application/x-tar"
	if compressed {
		contentType = "application/gzip"
	}

	var out UploadResult
	if err := c.do(ctx, http.MethodPost, path, archive, contentType, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadSourceChunked sends an archive in parts.
//
// This is the fallback for an archive above the simple-upload limit. It is
// deliberately not the default: for a source tree of ordinary size the single
// request is faster and has fewer ways to go wrong, and the deployment reports
// its own limit through Config so the choice can be made correctly rather than
// by guessing.
func (c *Client) UploadSourceChunked(ctx context.Context, appID string, archive io.Reader, message string, chunkSize int64) (*UploadResult, error) {
	if chunkSize <= 0 {
		chunkSize = 8 << 20
	}

	// The archive is read in parts and each is sent as it is read, so the whole
	// thing never has to be in memory.
	type part struct {
		index int
		data  []byte
	}
	var parts []part

	reader := io.LimitReader(archive, 2<<30)
	buffer := make([]byte, chunkSize)
	for index := 1; ; index++ {
		n, err := io.ReadFull(reader, buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buffer[:n])
			parts = append(parts, part{index: index, data: chunk})
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read source archive: %w", err)
		}
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("the source archive is empty")
	}

	beginBody, _ := json.Marshal(map[string]any{
		"total":      len(parts),
		"chunk_size": chunkSize,
		"message":    message,
	})

	var begin struct {
		UploadID string `json:"upload_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/source/uploads",
		bytes.NewReader(beginBody), "application/json", &begin); err != nil {
		return nil, err
	}
	if begin.UploadID == "" {
		return nil, fmt.Errorf("the deployment did not return an upload id")
	}

	for _, p := range parts {
		path := fmt.Sprintf("/api/v1/apps/%s/source/uploads/%s/parts/%d", appID, begin.UploadID, p.index)
		if err := c.do(ctx, http.MethodPut, path, bytes.NewReader(p.data), "application/octet-stream", nil); err != nil {
			return nil, fmt.Errorf("send part %d of %d: %w", p.index, len(parts), err)
		}
	}

	var out UploadResult
	completePath := fmt.Sprintf("/api/v1/apps/%s/source/uploads/%s/complete", appID, begin.UploadID)
	if err := c.do(ctx, http.MethodPost, completePath, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Commit is one recorded source change.
type Commit struct {
	SHA       string    `json:"sha"`
	Message   string    `json:"message"`
	Author    string    `json:"author"`
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// CommitList is the history along with the current tip.
type CommitList struct {
	Commits []Commit `json:"commits"`
	Head    string   `json:"head"`
	Count   int      `json:"count"`
}

// ListCommits returns an app's commit history.
func (c *Client) ListCommits(ctx context.Context, appID string, limit int) (*CommitList, error) {
	path := "/api/v1/apps/" + appID + "/commits"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}

	var out CommitList
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Build is one build attempt.
type Build struct {
	ID        string    `json:"id"`
	AppID     string    `json:"app_id"`
	CommitSHA string    `json:"commit_sha"`
	Status    string    `json:"status"`
	Image     string    `json:"image"`
	JobName   string    `json:"job_name"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// StartBuild starts a build of a commit. An empty commit builds the current tip.
func (c *Client) StartBuild(ctx context.Context, appID, commitSHA string) (*Build, error) {
	body, _ := json.Marshal(map[string]any{"commit_sha": commitSHA})

	var out Build
	if err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/builds",
		bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetBuild returns one build, with its status read from the cluster.
func (c *Client) GetBuild(ctx context.Context, appID, buildID string) (*Build, error) {
	var out Build
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/builds/"+buildID, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListBuilds returns an app's builds.
func (c *Client) ListBuilds(ctx context.Context, appID string, limit int) ([]Build, error) {
	path := "/api/v1/apps/" + appID + "/builds"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}

	var out []Build
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// BuildLogs streams a build's log to w.
//
// Follow is disabled here: a CLI that wants to watch a build polls the build's
// state and streams the log between polls, because the log endpoint's own follow
// mode holds a connection that a Ctrl-C cannot interrupt cleanly.
func (c *Client) BuildLogs(ctx context.Context, appID, buildID string, w io.Writer, follow bool) error {
	path := fmt.Sprintf("/api/v1/apps/%s/builds/%s/logs", appID, buildID)
	if !follow {
		path += "?follow=false"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)

	// A followed log has no natural end, so the client's own timeout must not
	// apply — it would cut the stream off mid-build.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}

	// Copied line by line so each arrives as it is written, which is what makes
	// a build's progress visible rather than appearing all at once at the end.
	reader := newLineReader(resp.Body)
	for {
		line, err := reader.read()
		if line != "" {
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				return nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read build log: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

// DeployResult is what a deploy returns.
type DeployResult struct {
	App    App    `json:"app"`
	Commit string `json:"commit"`
	Image  string `json:"image"`

	// Host is where the app answers and Path where under it, so a caller can
	// see the two parts. URL is them joined with a scheme, which is what most
	// callers want.
	Host string `json:"host"`
	Path string `json:"path"`
	URL  string `json:"url"`

	// Build is set instead of the fields above when the deploy started a build,
	// which happens only when the caller asked for one.
	Build *Build `json:"build"`
}

// Deploy deploys a commit. An empty commit deploys the current tip; withBuild
// starts a build when no image exists yet.
func (c *Client) Deploy(ctx context.Context, appID, commitSHA string, withBuild bool) (*DeployResult, error) {
	body, _ := json.Marshal(map[string]any{
		"commit_sha": commitSHA,
		"build":      withBuild,
	})

	var out DeployResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/deploy",
		bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rollback deploys an earlier commit.
func (c *Client) Rollback(ctx context.Context, appID, commitSHA string) (*DeployResult, error) {
	body, _ := json.Marshal(map[string]any{"commit_sha": commitSHA})

	var out DeployResult
	if err := c.do(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/rollback",
		bytes.NewReader(body), "application/json", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Stop removes an app's running resources, keeping its source.
func (c *Client) Stop(ctx context.Context, appID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/stop", nil, "", nil)
}

// Restart rolls an app's pods.
func (c *Client) Restart(ctx context.Context, appID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/apps/"+appID+"/restart", nil, "", nil)
}

// Status is an app's live state alongside AppLab's record.
type Status struct {
	AppID  string `json:"app_id"`
	Status string `json:"status"`

	Deployed *struct {
		CommitSHA string `json:"commit_sha"`
		Image     string `json:"image"`
		Status    string `json:"status"`
		Reason    string `json:"status_reason"`
	} `json:"deployed"`

	Live *struct {
		Deployed        bool   `json:"deployed"`
		Available       bool   `json:"available"`
		ReadyReplicas   int32  `json:"ready_replicas"`
		DesiredReplicas int32  `json:"desired_replicas"`
		CurrentImage    string `json:"current_image"`
		Message         string `json:"message"`
	} `json:"live"`

	Host string `json:"host"`
	Path string `json:"path"`
	URL  string `json:"url"`
}

// AppStatus reads an app's status.
func (c *Client) AppStatus(ctx context.Context, appID string) (*Status, error) {
	var out Status
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/status", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Pod is one pod of an app.
type Pod struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	Image    string `json:"image"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`

	Containers []struct {
		Name                 string `json:"name"`
		Ready                bool   `json:"ready"`
		Restarts             int32  `json:"restarts"`
		State                string `json:"state"`
		Reason               string `json:"reason"`
		LastTerminatedReason string `json:"last_terminated_reason"`
		LastExitCode         int32  `json:"last_exit_code"`
	} `json:"containers"`
}

// PodList is an app's pods.
type PodList struct {
	AppID string `json:"app_id"`
	Pods  []Pod  `json:"pods"`
	Count int    `json:"count"`
}

// Pods lists an app's pods.
func (c *Client) Pods(ctx context.Context, appID string) (*PodList, error) {
	var out PodList
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/pods", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LogOptions selects which log to read.
type LogOptions struct {
	Pod       string
	Container string
	Tail      int
	Previous  bool
	Since     string
}

// Logs streams an app's pod log to w.
func (c *Client) Logs(ctx context.Context, appID string, opts LogOptions, w io.Writer, follow bool) error {
	return c.streamLogs(ctx, "/api/v1/apps/"+appID+"/logs", opts, w, follow)
}

// SelfPods lists AppLab's own pods — the deployment this client is talking to,
// as opposed to the apps it manages.
//
// The two are the same kind of thing from a caller's side, which is why the
// answer is a PodList rather than a shape of its own. What differs is where it
// comes from: an app's pods are the app's, and these are the control plane's.
func (c *Client) SelfPods(ctx context.Context) (*PodList, error) {
	var out PodList
	if err := c.do(ctx, http.MethodGet, "/api/v1/platform/pods", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SelfLogs streams AppLab's own log to w.
//
// The reason it exists is the case where nothing else helps: a build that never
// starts, a deploy that does nothing. An app's log says nothing about either,
// because the control plane is what failed.
func (c *Client) SelfLogs(ctx context.Context, opts LogOptions, w io.Writer, follow bool) error {
	return c.streamLogs(ctx, "/api/v1/platform/logs", opts, w, follow)
}

// streamLogs reads a log endpoint, which is one of two by path and identical
// otherwise — same query, same streaming, same text/plain body.
//
// Shared rather than written twice because the streaming half is where the
// subtle mistakes live (an unflushed buffer, a body that is never closed), and a
// second copy is a second place for them.
func (c *Client) streamLogs(ctx context.Context, path string, opts LogOptions, w io.Writer, follow bool) error {
	params := []string{}
	if opts.Pod != "" {
		params = append(params, "pod="+urlQueryEscape(opts.Pod))
	}
	if opts.Container != "" {
		params = append(params, "container="+urlQueryEscape(opts.Container))
	}
	if opts.Tail > 0 {
		params = append(params, fmt.Sprintf("tail=%d", opts.Tail))
	}
	if opts.Previous {
		params = append(params, "previous=true")
	}
	if opts.Since != "" {
		params = append(params, "since="+urlQueryEscape(opts.Since))
	}
	if !follow {
		params = append(params, "follow=false")
	}

	if len(params) > 0 {
		path += "?" + strings.Join(params, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)

	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("reach %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}

	_, err = io.Copy(w, resp.Body)
	return err
}

// Event is one Kubernetes event.
type Event struct {
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Object    string    `json:"object"`
	Count     int32     `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// EventList is an app's events.
type EventList struct {
	AppID    string  `json:"app_id"`
	Events   []Event `json:"events"`
	Count    int     `json:"count"`
	Warnings int     `json:"warnings"`
}

// Events lists an app's events, warnings first. A limit of zero uses the
// server's default.
func (c *Client) Events(ctx context.Context, appID string, limit int) (*EventList, error) {
	path := "/api/v1/apps/" + appID + "/events"
	if limit > 0 {
		path += fmt.Sprintf("?limit=%d", limit)
	}

	var out EventList
	if err := c.do(ctx, http.MethodGet, path, nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Diagnosis is the answer to "why is it not working".
type Diagnosis struct {
	AppID        string `json:"app_id"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
	Problem      string `json:"problem"`
	Message      string `json:"message"`
	Next         string `json:"next"`
	Error        string `json:"error"`

	Pods    []Pod   `json:"pods"`
	Events  []Event `json:"events"`
	Logs    string  `json:"logs"`
	PrevLog string  `json:"previous_logs"`
}

// Diagnose asks why an app is not working.
func (c *Client) Diagnose(ctx context.Context, appID string) (*Diagnosis, error) {
	var out Diagnosis
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps/"+appID+"/diagnose", nil, "", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- source archive helpers ------------------------------------------------

// ArchiveDir writes a directory as a gzipped tar stream into w.
//
// It mirrors what the server does on ingest, so a source tree round-trips: paths
// are relative to dir, and a single wrapping directory is *not* stripped here —
// the server strips it, because only it can decide from the whole listing.
//
// skip names directories to leave out. Build output — node_modules, target,
// .git — is the reason this exists: uploading it makes a build slower and the
// archive far larger for no benefit.
func ArchiveDir(ctx context.Context, dir string, w io.Writer, skip []string) error {
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipSet[filepath.Clean(s)] = true
	}

	gz := gzip.NewWriter(w)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	// Files are collected and sorted first, so the archive is reproducible: two
	// uploads of identical source produce identical bytes, which is what makes
	// the server's "unchanged source is a no-op" check fire.
	var paths []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		base := filepath.Base(rel)
		if skipSet[base] || skipSet[rel] {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// A symlink is recorded as a link rather than followed: following one
		// could walk outside the tree, and the server stores links as their
		// target text anyway.
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", dir, err)
	}
	sort.Strings(paths)

	for _, rel := range paths {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		full := filepath.Join(dir, rel)
		info, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("stat %s: %w", rel, err)
		}

		// The archive uses forward slashes regardless of the host's separator,
		// because tar and the server are both POSIX about it.
		name := filepath.ToSlash(rel)

		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(full); err != nil {
				return fmt.Errorf("read link %s: %w", rel, err)
			}
		}

		header := &tar.Header{
			Name:     name,
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Typeflag: tar.TypeReg,
		}
		if info.IsDir() {
			header.Typeflag = tar.TypeDir
			header.Size = 0
			header.Name = name + "/"
		} else if link != "" {
			header.Typeflag = tar.TypeSymlink
			header.Size = 0
			header.Linkname = link
		}

		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write header for %s: %w", rel, err)
		}

		if header.Typeflag == tar.TypeReg && info.Size() > 0 {
			file, err := os.Open(full)
			if err != nil {
				return fmt.Errorf("open %s: %w", rel, err)
			}
			if _, err := io.Copy(tw, file); err != nil {
				file.Close()
				return fmt.Errorf("write %s: %w", rel, err)
			}
			file.Close()
		}
	}

	return nil
}

// DefaultSkipDirs are the directories a source upload leaves out by default.
//
// Every one of them is either reproducible from the source or belongs to the
// machine rather than the project, and shipping them makes a build slower and an
// archive larger for nothing.
var DefaultSkipDirs = []string{
	".git",
	"node_modules",
	"target",
	"dist",
	"build",
	".venv",
	"venv",
	"__pycache__",
	".next",
	".nuxt",
	"vendor",
	".idea",
	".vscode",
	".DS_Store",
}

// urlQueryEscape escapes a query parameter value.
//
// net/url is used rather than a hand-rolled escape so that a value containing a
// space, an ampersand or a unicode character — all of which appear in commit
// messages — is encoded correctly.
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// newLineReader splits a stream into lines without buffering the whole thing.
func newLineReader(r io.Reader) *lineReader { return &lineReader{r: bufio.NewReader(r)} }

// lineReader reads whole lines, keeping the terminator so that writing them out
// reproduces the input exactly.
type lineReader struct{ r *bufio.Reader }

func (l *lineReader) read() (string, error) {
	line, err := l.r.ReadString('\n')
	return line, err
}
