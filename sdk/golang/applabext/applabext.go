// Package applabext is the hand-written half of the Go SDK.
//
// Everything else in this package is generated from api/openapi.yaml by
// openapi-generator, and covers an operation per endpoint. Four things are not
// one endpoint, so the generator cannot write them:
//
//   - A chunked upload. The server needs the part count before it receives the
//     first part, so the archive has to be staged first, and the part size comes
//     from a separate call to /api/v1/config. A one-request method cannot.
//   - Following a log. A followed response may never end, so it has to be read
//     line by line as it arrives rather than into a string.
//   - Packing a directory. That is a client concern, not an API operation. The
//     standard library has the tar and gzip writers; what it lacks is a decision
//     about what "this directory" means.
//   - Resolving an abbreviated build id to the full one, refusing when it is
//     ambiguous.
//
// This file is listed in .openapi-generator-ignore, so regeneration never
// touches it. It is deliberately *not* the same as internal/client: that package
// is AppLab's own client and stays inside the module, while this one is public
// and generated, so it is the one an application outside the module can import.
// See sdk/README.md.
//
// The type names here avoid the ones the generated models use — UploadResult,
// Config and App all come out of the generator — so nothing in this file collides
// with a generated symbol.
package applabext

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SkipDirs are the directories a push never includes. The same set the CLI, the
// Python SDK and the TypeScript SDK use, because it is a decision about what a
// person means by "this directory" and it should not differ by language.
var SkipDirs = map[string]bool{
	".git": true, "node_modules": true, "target": true, "dist": true,
	"build": true, ".venv": true, "venv": true, "__pycache__": true,
	".next": true, ".nuxt": true, "vendor": true, ".idea": true,
	".vscode": true, ".DS_Store": true,
}

// DefaultChunkSize is used only when the deployment reports none. It matches the
// server's own default.
const DefaultChunkSize int64 = 8 << 20

// ServerConfig is the deployment's self-description, as far as these helpers need
// it. It is named ServerConfig rather than Config because the generated package
// already has a Config model.
type ServerConfig struct {
	ChunkSize       int64
	MaxSimpleUpload int64
	MaxChunkBytes   int64
}

// PushOptions are the optional parts of an upload.
type PushOptions struct {
	// Message is the commit message.
	Message string

	// Branch commits to another branch instead of the app's active one. Empty
	// means the active branch, which is what a caller almost always wants.
	Branch string

	// NoPublish uploads without building and deploying. The zero value publishes,
	// which is what `applab push` does.
	NoPublish bool
}

// PushResult is what an upload produced. It is named PushResult rather than
// UploadResult because the generated package already has an UploadResult model.
type PushResult struct {
	Commit  string `json:"commit_sha"`
	Message string `json:"message"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
}

// Ext is a small client for the operations the generator cannot express. It
// takes the same two things the generated client does — an address and a key —
// rather than depending on the generated client's own shapes, so regenerating
// that client cannot break this.
type Ext struct {
	baseURL string
	key     string
	http    *http.Client
}

// NewExt builds a helper for one deployment.
//
// An empty key is refused rather than sent: the API reads an empty bearer token
// as no credential, and a caller who forgot theirs would get a 401 that says
// nothing about which of the two things was missing.
func NewExt(baseURL, key string) (*Ext, error) {
	base := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("applabext: no deployment address given")
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("applabext: no API key given")
	}
	return &Ext{
		baseURL: base,
		key:     strings.TrimSpace(key),
		http: &http.Client{
			Timeout: 30 * time.Second,
			// Redirects are not followed, for the same reason the API's own
			// client does not follow them: a request carrying a key that is
			// redirected sends that key wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// apiError is a failure the deployment reported.
type apiError struct {
	Status    int
	Message   string
	Retryable bool
}

func (e *apiError) Error() string {
	if e.Retryable {
		return fmt.Sprintf("%s (HTTP %d, retryable)", e.Message, e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

// Config reads the deployment's limits.
func (e *Ext) Config(ctx context.Context) (*ServerConfig, error) {
	var out struct {
		Data struct {
			ChunkSize       int64 `json:"chunk_size"`
			MaxSimpleUpload int64 `json:"max_simple_upload"`
			MaxChunkBytes   int64 `json:"max_chunk_bytes"`
		} `json:"data"`
	}
	if err := e.do(ctx, http.MethodGet, "/api/v1/config", nil, "", nil, &out); err != nil {
		return nil, err
	}
	cfg := &ServerConfig{
		ChunkSize:       out.Data.ChunkSize,
		MaxSimpleUpload: out.Data.MaxSimpleUpload,
		MaxChunkBytes:   out.Data.MaxChunkBytes,
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = DefaultChunkSize
	}
	return cfg, nil
}

// do performs one request and decodes the {"data": ...} envelope into out.
//
// It always closes the response body — nothing here streams. The log routes,
// whose body may never end, go through openStream instead. A version of this that
// returned the body for a caller to close was a leak waiting to happen: the
// chunked upload's part requests pass no out, and a caller that ignores a
// (nil, nil) return leaks one response body per part.
func (e *Ext) do(ctx context.Context, method, path string, body io.Reader, contentType string, query url.Values, out any) error {
	target := e.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("applabext: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("applabext: reach %s: %w", e.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}

	// A caller that wants only the status — a part request — passes no out. The
	// body is still drained and closed, which is what keeps the connection
	// reusable.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("applabext: read response: %w", err)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("applabext: decode response: %w", err)
	}
	return nil
}

// decodeError turns a failure response into an apiError, reading the error
// envelope. A body that is not JSON — a proxy's HTML, a 413 from something in
// front of the deployment — is reported by status with an excerpt, which is more
// useful than a JSON decode failure on top of the real one.
func decodeError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var envelope struct {
		Error     string `json:"error"`
		Retryable bool   `json:"retryable"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != "" {
		return &apiError{Status: resp.StatusCode, Message: envelope.Error, Retryable: envelope.Retryable}
	}
	excerpt := strings.TrimSpace(string(raw))
	if len(excerpt) > 200 {
		excerpt = excerpt[:200]
	}
	return &apiError{
		Status:    resp.StatusCode,
		Message:   fmt.Sprintf("HTTP %d: %s", resp.StatusCode, excerpt),
		Retryable: resp.StatusCode >= 500,
	}
}

// PackageDirectory tars and gzips a directory into a temporary file and returns
// its path. The caller owns the file and must remove it.
//
// The archive is deterministic: entries are walked in sorted order, names are
// relative and forward-slashed, and mtimes are preserved. That is what makes a
// re-push of an unchanged directory a no-op commit rather than a diff of
// timestamps.
func PackageDirectory(directory string) (string, error) {
	root := strings.TrimSuffix(filepath.Clean(directory), string(filepath.Separator))
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("applabext: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("applabext: %s is not a directory", root)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	paths, err := collectFiles(root)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return "", fmt.Errorf("applabext: %w", err)
		}
		name := filepath.ToSlash(rel)
		fileInfo, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("applabext: %w", err)
		}
		header := &tar.Header{
			Name:    name,
			Mode:    int64(fileInfo.Mode().Perm()),
			Size:    fileInfo.Size(),
			ModTime: fileInfo.ModTime(),
			// A regular file. Symlinks are not followed: an archive that followed
			// one out of the tree would upload something the caller did not mean
			// to send.
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			return "", fmt.Errorf("applabext: write tar header: %w", err)
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("applabext: %w", err)
		}
		_, copyErr := io.Copy(tw, file)
		file.Close()
		if copyErr != nil {
			return "", fmt.Errorf("applabext: write tar body: %w", copyErr)
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("applabext: close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("applabext: close gzip: %w", err)
	}

	tmp, err := os.CreateTemp("", "applab-upload-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("applabext: %w", err)
	}
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", fmt.Errorf("applabext: write archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("applabext: close archive: %w", err)
	}
	return tmp.Name(), nil
}

// collectFiles walks a directory and returns the files to archive, sorted.
func collectFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && SkipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Only regular files: a symlink or a device is skipped rather than
		// archived, for the reason given on the header above.
		if entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("applabext: walk %s: %w", root, err)
	}
	sort.Strings(files)
	return files, nil
}

// PushDirectory packages a directory and uploads it as a commit. This is the
// `applab push` path.
func (e *Ext) PushDirectory(ctx context.Context, appID, directory string, opts PushOptions) (*PushResult, error) {
	archive, err := PackageDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer os.Remove(archive)
	return e.UploadArchive(ctx, appID, archive, opts)
}

// UploadArchive uploads an already-packaged archive, choosing the chunked path
// directly.
//
// The chunked path is used even for a small tree because it has no size limit,
// and choosing between two paths on a size the server has not been asked about
// yet is how a push fails at exactly the wrong moment.
func (e *Ext) UploadArchive(ctx context.Context, appID, archivePath string, opts PushOptions) (*PushResult, error) {
	return e.UploadChunked(ctx, appID, archivePath, 0, opts)
}

// UploadChunked uploads an archive in parts and returns the resulting commit.
//
// The archive must be a file: the part count has to be known before the first
// part is sent, which a stream cannot tell you. A caller with a stream therefore
// has to stage it somewhere first — this refuses to guess.
func (e *Ext) UploadChunked(ctx context.Context, appID, archivePath string, chunkSize int64, opts PushOptions) (*PushResult, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("applabext: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("applabext: %w", err)
	}
	size := info.Size()

	cfg, err := e.Config(ctx)
	if err != nil {
		return nil, err
	}
	if chunkSize <= 0 {
		chunkSize = cfg.ChunkSize
	}
	if cfg.MaxChunkBytes > 0 && chunkSize > cfg.MaxChunkBytes {
		return nil, fmt.Errorf("applabext: chunk size %d exceeds the deployment's limit of %d", chunkSize, cfg.MaxChunkBytes)
	}

	total := (size + chunkSize - 1) / chunkSize
	if total < 1 {
		total = 1
	}

	startBody, _ := json.Marshal(map[string]any{
		"total":      total,
		"chunk_size": chunkSize,
		"message":    opts.Message,
	})
	var started struct {
		Data struct {
			UploadID string `json:"upload_id"`
		} `json:"data"`
	}
	if err := e.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/apps/%s/source/uploads", url.PathEscape(appID)),
		bytes.NewReader(startBody), "application/json", nil, &started); err != nil {
		return nil, err
	}
	uploadID := started.Data.UploadID
	if uploadID == "" {
		return nil, fmt.Errorf("applabext: the deployment did not return an upload id")
	}

	part := make([]byte, chunkSize)
	for index := int64(1); index <= total; index++ {
		n, err := io.ReadFull(file, part)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// The last part is short; ReadFull reports that as an error, and it
			// is not one.
			if n == 0 {
				break
			}
		} else if err != nil {
			return nil, fmt.Errorf("applabext: read part %d: %w", index, err)
		}
		// The index is 1-based and is which part this is, not a byte offset; the
		// server rejects a gap.
		path := fmt.Sprintf("/api/v1/apps/%s/source/uploads/%s/parts/%d",
			url.PathEscape(appID), url.PathEscape(uploadID), index)
		if err := e.do(ctx, http.MethodPut, path, bytes.NewReader(part[:n]), "application/octet-stream", nil, nil); err != nil {
			return nil, err
		}
	}

	query := url.Values{}
	if opts.Message != "" {
		query.Set("message", opts.Message)
	}
	if opts.Branch != "" {
		query.Set("branch", opts.Branch)
	}
	if opts.NoPublish {
		query.Set("publish", "false")
	}
	path := fmt.Sprintf("/api/v1/apps/%s/source/uploads/%s/complete", url.PathEscape(appID), url.PathEscape(uploadID))
	var out struct {
		Data PushResult `json:"data"`
	}
	if err := e.do(ctx, http.MethodPost, path, nil, "", query, &out); err != nil {
		return nil, err
	}
	return &out.Data, nil
}

// UploadSimple uploads an archive in one request, falling back to the chunked
// path on a 413.
//
// It exists for a caller who already knows the tree is small; the fallback means
// a caller who guessed wrong is not left to handle the rejection themselves.
func (e *Ext) UploadSimple(ctx context.Context, appID, archivePath string, opts PushOptions) (*PushResult, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("applabext: %w", err)
	}
	defer file.Close()

	query := url.Values{}
	if opts.Message != "" {
		query.Set("message", opts.Message)
	}
	if opts.Branch != "" {
		query.Set("branch", opts.Branch)
	}
	if opts.NoPublish {
		query.Set("publish", "false")
	}

	path := fmt.Sprintf("/api/v1/apps/%s/source", url.PathEscape(appID))
	var out struct {
		Data PushResult `json:"data"`
	}
	if err := e.do(ctx, http.MethodPost, path, file, "application/gzip", query, &out); err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusRequestEntityTooLarge {
			return e.UploadArchive(ctx, appID, archivePath, opts)
		}
		return nil, err
	}
	return &out.Data, nil
}

// LogOptions select which log lines to read.
type LogOptions struct {
	Follow    bool
	Pod       string
	Container string
	Tail      int
	Previous  bool
	Since     string
}

// StreamLogs reads an app's log, calling yield once per line.
//
// A callback rather than a returned slice: with Follow set the response may never
// end, so a function that returned when the body ended would never return. The
// body is read with bufio-like framing as it arrives, which is the point of
// following a log at all.
//
// Returning an error from yield stops the read and returns that error.
func (e *Ext) StreamLogs(ctx context.Context, appID string, opts LogOptions, yield func(line string) error) error {
	body, err := e.openStream(ctx, fmt.Sprintf("/api/v1/apps/%s/logs", url.PathEscape(appID)), logQuery(opts))
	if err != nil {
		return err
	}
	defer body.Close()
	return scanLines(body, yield)
}

// StreamBuildLogs reads a build's log, the build variant of StreamLogs.
func (e *Ext) StreamBuildLogs(ctx context.Context, appID, buildID string, follow bool, yield func(line string) error) error {
	query := url.Values{}
	if follow {
		query.Set("follow", "true")
	}
	path := fmt.Sprintf("/api/v1/apps/%s/builds/%s/logs", url.PathEscape(appID), url.PathEscape(buildID))
	body, err := e.openStream(ctx, path, query)
	if err != nil {
		return err
	}
	defer body.Close()
	return scanLines(body, yield)
}

// openStream issues a request whose response body is plain text to be read
// incrementally. It uses a client with no timeout, because a followed log has no
// natural end and the request timeout would cut it.
func (e *Ext) openStream(ctx context.Context, path string, query url.Values) (io.ReadCloser, error) {
	target := e.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("applabext: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.key)

	// No Timeout: a followed log is expected to stay open.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("applabext: reach %s: %w", e.baseURL, err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp.Body, nil
}

// logQuery builds the query the log routes read.
func logQuery(opts LogOptions) url.Values {
	query := url.Values{}
	if opts.Follow {
		query.Set("follow", "true")
	}
	if opts.Pod != "" {
		query.Set("pod", opts.Pod)
	}
	if opts.Container != "" {
		query.Set("container", opts.Container)
	}
	if opts.Tail > 0 {
		query.Set("tail", strconv.Itoa(opts.Tail))
	}
	if opts.Previous {
		query.Set("previous", "true")
	}
	if opts.Since != "" {
		query.Set("since", opts.Since)
	}
	return query
}

// scanLines frames a stream into lines, keeping the terminators so a caller can
// reproduce the output exactly. A final line without a newline is emitted too.
func scanLines(body io.Reader, yield func(string) error) error {
	reader := bufio.NewReader(body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if yieldErr := yield(line); yieldErr != nil {
				return yieldErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("applabext: read log: %w", err)
		}
	}
}

// ResolveBuildID turns an abbreviated build id into the full one.
//
// Builds are shown abbreviated and typed abbreviated. This is deliberately
// strict: two builds sharing a prefix is an error rather than a guess, because
// guessing is how the wrong build gets cancelled.
func (e *Ext) ResolveBuildID(ctx context.Context, appID, prefix string) (string, error) {
	query := url.Values{}
	query.Set("limit", "50")

	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/api/v1/apps/%s/builds", url.PathEscape(appID))
	if err := e.do(ctx, http.MethodGet, path, nil, "", query, &out); err != nil {
		return "", err
	}
	var matches []string
	for _, build := range out.Data {
		if strings.HasPrefix(build.ID, prefix) {
			matches = append(matches, build.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("applabext: no build of %s starts with %q", appID, prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("applabext: %q matches %d builds; be more specific", prefix, len(matches))
	}
}
