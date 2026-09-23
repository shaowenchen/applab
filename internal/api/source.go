package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shaowenchen/applab/internal/model"
	"github.com/shaowenchen/applab/internal/source"
	"github.com/shaowenchen/applab/internal/store"
)

// maxArchiveBytes bounds what the chunked-upload endpoints will assemble.
//
// It is larger than the single-request limit because the whole point of the
// chunked path is to carry something too big for one request — but it is still
// bounded, since a source tree larger than this is almost always build output
// that should have been excluded.
const maxArchiveBytes int64 = 2 << 30 // 2 GiB

// handleUploadSource accepts a source archive in a single request.
//
// This is the path an agent takes: `curl --data-binary @- .../source`. It reads
// the body as a tar or tar.gz stream, so a caller can pipe `tar czf -` straight
// in without the archive ever touching its own disk.
//
// The request has two forms, distinguished by Content-Type:
//
//   - multipart/form-data with a "file" field, for `curl -F file=@x.tar.gz`
//   - anything else, in which case the raw body is the archive
//
// Both are supported because the two are natural to different callers and the
// distinction is unambiguous.
func (s *Server) handleUploadSource(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.sourceIngest == nil {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment has no source storage configured"))
		return
	}

	body, cleanup, err := s.requestArchive(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	defer cleanup()

	message := strings.TrimSpace(r.URL.Query().Get("message"))
	parent := strings.TrimSpace(r.URL.Query().Get("parent"))

	result, err := s.sourceIngest(r.Context(), app.ID, body, message, parent)
	if err != nil {
		fail(w, r, ingestError(err))
		return
	}

	if result.Empty {
		fail(w, r, BadRequest("the uploaded archive contains no files; nothing was committed"))
		return
	}

	s.recordCommit(r, app, result)
	if s.metrics != nil {
		s.metrics.ObserveUpload(result.Bytes)
	}
	respond(w, http.StatusOK, uploadResponse{
		CommitSHA:    result.SHA,
		Message:      result.Subject,
		Files:        result.Files,
		Bytes:        result.Bytes,
		StrippedRoot: result.Stripped,
	})
}

// requestArchive returns the archive stream and a cleanup function.
//
// A multipart upload is spooled to a temporary file rather than streamed
// directly, because multipart parsing needs to find the part before the archive
// can be read, and the part may arrive after other fields. A raw body is streamed
// as it arrives, which is what lets a large piped archive avoid a second copy.
func (s *Server) requestArchive(r *http.Request) (io.Reader, func(), error) {
	contentType := r.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "multipart/form-data") {
		// ParseMultipartForm's argument is the in-memory threshold; anything
		// larger spills to a temp file that RemoveAll cleans up.
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return nil, func() {}, BadRequest("parse multipart body: %s", err.Error())
		}
		cleanup := func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}

		file, _, err := r.FormFile("file")
		if err != nil {
			cleanup()
			return nil, func() {}, BadRequest("multipart body has no \"file\" field: %s", err.Error())
		}
		return file, func() { file.Close(); cleanup() }, nil
	}

	// A raw body. The size is capped so an over-large upload is refused before
	// it has been fully consumed and expanded on disk.
	limited := http.MaxBytesReader(nil, r.Body, maxArchiveBytes)
	return limited, func() { r.Body.Close() }, nil
}

// uploadResponse is what a successful upload returns.
type uploadResponse struct {
	CommitSHA string `json:"commit_sha"`

	// Message is the commit's subject — the message the caller gave, or the one
	// applab generated. It is returned because a generated one is otherwise
	// invisible until the commit is read back.
	Message string `json:"message"`

	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`

	// StrippedRoot names a single wrapping directory that was removed. Reported
	// because it changes where the Dockerfile ends up, and a caller that did not
	// expect it should be able to see that applab did it deliberately.
	StrippedRoot string `json:"stripped_root,omitempty"`
}

// recordCommit writes the commit into applab's own history.
//
// A failure here is logged rather than returned: the commit is in the
// repository, which is the record of truth, and failing the upload would tell
// the caller their source was not stored when it was. The database row is an
// index for faster listing, and can be rebuilt from git.
func (s *Server) recordCommit(r *http.Request, app *model.App, result *source.IngestResult) {
	err := s.store.RecordCommit(r.Context(), &model.Commit{
		AppID:   app.ID,
		SHA:     result.SHA,
		Message: result.Subject,
		Files:   result.Files,
		Bytes:   result.Bytes,
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to record commit; the repository has it, so the upload succeeded",
			"app", app.ID, "commit", result.SHA, "error", err)
	}
}

// ingestError maps an ingest failure onto an HTTP error.
//
// The distinction that matters is whose fault it is: a malformed or hostile
// archive is the caller's (400), while a failure to write to applab's storage is
// ours (500) and worth retrying.
func ingestError(err error) *apiError {
	switch {
	case errors.Is(err, source.ErrNoCommits):
		return BadRequest("the repository has no commits to build on")
	case isArchiveProblem(err):
		return BadRequest("%s", err.Error())
	case errors.Is(err, context.Canceled):
		return Errorf(499, "the request was cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return Errorf(http.StatusServiceUnavailable, "the upload timed out").Retryable()
	default:
		if strings.Contains(err.Error(), "no such app") {
			return NotFound("%s", err.Error())
		}
		return Errorf(http.StatusInternalServerError, "store source").Wrap(err)
	}
}

// isArchiveProblem reports whether an error describes a bad archive rather than
// an applab failure.
//
// The source package reports these as plain errors, so the classification is by
// message. It is confined here so a change in wording has one place to fix, and
// the default is to treat an unrecognised error as applab's fault — which errs
// toward telling the caller to retry rather than blaming them for something they
// cannot fix.
func isArchiveProblem(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"escapes the destination",
		"has an absolute path",
		"contains a null byte",
		"archive contains more than",
		"archive expands to more than",
		"read archive",
		"read gzip stream",
		"read archive header",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// handleListCommits returns an app's commit history.
//
// The order comes from git, not from applab's own table. Git's history is a
// parent chain, so it is ordered by construction; a timestamp is not — two
// uploads within the same second share a stored timestamp, and ordering by it
// would list them arbitrarily. The database row is still consulted, but only for
// the file and byte counts, which git does not record.
func (s *Server) handleListCommits(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	limit, apiErr := intQuery(r, "limit", 50)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	head, err := s.headCommit(r.Context(), app.ID)
	if err != nil && !errors.Is(err, source.ErrNoCommits) {
		fail(w, r, Errorf(http.StatusInternalServerError, "read commit history").Wrap(err))
		return
	}

	history, err := s.sourceLog(r.Context(), app.ID, limit)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "read commit history").Wrap(err))
		return
	}

	// The recorded rows are read once and indexed, rather than queried per
	// commit: a history of fifty commits would otherwise be fifty round trips.
	recorded := make(map[string]*model.Commit)
	if rows, err := s.store.ListCommits(r.Context(), app.ID, limit); err == nil {
		for _, c := range rows {
			recorded[c.SHA] = c
		}
	} else {
		// Not fatal: the history is still correct without the counts, and git
		// remains the record of what is actually there.
		slog.DebugContext(r.Context(), "could not read recorded commits; history will lack file counts",
			"app", app.ID, "error", err)
	}

	out := make([]commitResponse, 0, len(history))
	for _, h := range history {
		resp := commitResponse{
			SHA:       h.SHA,
			Message:   h.Subject,
			Author:    h.Author,
			CreatedAt: h.CreatedAt,
		}
		if row, ok := recorded[h.SHA]; ok {
			resp.Files = row.Files
			resp.Bytes = row.Bytes
			if row.Message != "" {
				resp.Message = row.Message
			}
		}
		out = append(out, resp)
	}

	respond(w, http.StatusOK, map[string]any{
		"commits": out,
		"head":    head,
		"count":   len(out),
	})
}

// commitResponse is one commit as the API presents it.
type commitResponse struct {
	SHA       string `json:"sha"`
	Message   string `json:"message"`
	Author    string `json:"author,omitempty"`
	Files     int    `json:"files"`
	Bytes     int64  `json:"bytes"`
	CreatedAt any    `json:"created_at,omitempty"`
}

// handleGetCommit returns one commit.
func (s *Server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	rev := r.PathValue("sha")
	if rev == "" {
		fail(w, r, BadRequest("no commit in the request path"))
		return
	}

	sha, err := s.resolveCommit(r.Context(), app.ID, rev)
	if err != nil {
		fail(w, r, NotFound("commit %q in app %q", rev, app.ID))
		return
	}

	commit, err := s.store.GetCommit(r.Context(), app.ID, sha)
	if err != nil {
		// The commit exists in git but has no recorded row — possible if the
		// row write failed after a successful upload, since that is logged and
		// not fatal. Reporting what git knows is still useful.
		fail(w, r, NotFound("commit %q in app %q", rev, app.ID))
		return
	}

	respond(w, http.StatusOK, commitResponse{
		SHA:       commit.SHA,
		Message:   commit.Message,
		Author:    commit.Author,
		Files:     commit.Files,
		Bytes:     commit.Bytes,
		CreatedAt: commit.CreatedAt,
	})
}

// handleChunkedUploadStart begins a chunked source upload.
//
// The protocol is the one gh-upload established and is worth keeping because it
// is already proven: declare the total part count and size up front, send each
// part addressed by its index, then finalise. That lets the server place each
// part without buffering the whole archive and lets a client retry a single
// failed part rather than the whole upload.
func (s *Server) handleChunkedUploadStart(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	var req struct {
		Total     int    `json:"total"`
		ChunkSize int64  `json:"chunk_size"`
		Message   string `json:"message"`
	}
	if err := decodeJSON(r, &req); err != nil {
		fail(w, r, err)
		return
	}

	if req.Total < 1 || req.Total > 10_000 {
		fail(w, r, BadRequest("total must be between 1 and 10000, got %d", req.Total))
		return
	}
	if req.ChunkSize <= 0 || req.ChunkSize > s.cfg.MaxChunkBytes {
		fail(w, r, BadRequest("chunk_size must be between 1 and %d, got %d", s.cfg.MaxChunkBytes, req.ChunkSize))
		return
	}
	if int64(req.Total)*req.ChunkSize > maxArchiveBytes {
		fail(w, r, BadRequest("the declared upload would exceed the %d byte limit", maxArchiveBytes))
		return
	}

	id, err := model.NewID()
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "generate upload id").Wrap(err))
		return
	}

	dir := s.uploadPartDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "create upload staging directory").Wrap(err))
		return
	}

	upload := &model.Upload{
		ID:        id,
		AppID:     app.ID,
		Total:     req.Total,
		ChunkSize: req.ChunkSize,
		Message:   strings.TrimSpace(req.Message),
	}
	if err := s.store.CreateUpload(r.Context(), upload); err != nil {
		_ = os.RemoveAll(dir)
		fail(w, r, Errorf(http.StatusInternalServerError, "create upload").Wrap(err))
		return
	}

	respond(w, http.StatusCreated, map[string]any{
		"upload_id":  id,
		"total":      req.Total,
		"chunk_size": req.ChunkSize,
		"parts_url":  fmt.Sprintf("/api/v1/apps/%s/source/uploads/%s/parts", app.ID, id),
	})
}

// handleChunkedUploadPart stores one part.
func (s *Server) handleChunkedUploadPart(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.sourceIngest == nil {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment has no source storage configured"))
		return
	}

	upload, dir, apiErr := s.loadUpload(r, app.ID)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 1 || index > upload.Total {
		fail(w, r, BadRequest("part index must be between 1 and %d", upload.Total))
		return
	}

	// Bounded to the declared part size plus a little slack: a part that is
	// larger than declared means the client's arithmetic disagrees with the
	// server's, and accepting it would assemble a file that is not what was
	// described.
	limit := upload.ChunkSize + 1<<20
	limited := http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()

	partPath := filepath.Join(dir, fmt.Sprintf("part-%06d", index))
	file, err := os.OpenFile(partPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "open part file").Wrap(err))
		return
	}

	written, err := io.Copy(file, limited)
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(partPath)
		fail(w, r, Errorf(http.StatusInternalServerError, "write part").Wrap(err))
		return
	}
	if closeErr != nil {
		_ = os.Remove(partPath)
		fail(w, r, Errorf(http.StatusInternalServerError, "write part").Wrap(closeErr))
		return
	}

	respond(w, http.StatusOK, map[string]any{
		"upload_id": upload.ID,
		"index":     index,
		"size":      written,
	})
}

// handleChunkedUploadComplete assembles the parts and commits them.
func (s *Server) handleChunkedUploadComplete(w http.ResponseWriter, r *http.Request) {
	app, apiErr := s.loadApp(r)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}
	if s.sourceIngest == nil {
		fail(w, r, Errorf(http.StatusNotImplemented, "this deployment has no source storage configured"))
		return
	}

	upload, dir, apiErr := s.loadUpload(r, app.ID)
	if apiErr != nil {
		fail(w, r, apiErr)
		return
	}

	// The upload is claimed by removing its row before assembling. Two
	// concurrent completions would otherwise both assemble and both commit, and
	// the caller would get two commits for one logical upload.
	if err := s.store.DeleteUpload(r.Context(), upload.ID); err != nil {
		fail(w, r, Errorf(http.StatusInternalServerError, "claim upload").Wrap(err))
		return
	}
	defer os.RemoveAll(dir)

	for i := 1; i <= upload.Total; i++ {
		partPath := filepath.Join(dir, fmt.Sprintf("part-%06d", i))
		if _, err := os.Stat(partPath); err != nil {
			fail(w, r, BadRequest("part %d of %d is missing; send every part before completing", i, upload.Total))
			return
		}
	}

	assembled := &multiPartReader{dir: dir, total: upload.Total, index: 1}
	defer assembled.Close()

	var message string
	if upload.Message != "" {
		message = upload.Message
	}
	if q := strings.TrimSpace(r.URL.Query().Get("message")); q != "" {
		message = q
	}

	result, err := s.sourceIngest(r.Context(), app.ID, assembled, message, "")
	if err != nil {
		fail(w, r, ingestError(err))
		return
	}
	if result.Empty {
		fail(w, r, BadRequest("the assembled archive contains no files; nothing was committed"))
		return
	}

	s.recordCommit(r, app, result)
	respond(w, http.StatusOK, uploadResponse{
		CommitSHA:    result.SHA,
		Message:      result.Subject,
		Files:        result.Files,
		Bytes:        result.Bytes,
		StrippedRoot: result.Stripped,
	})
}

// loadUpload reads {upload} and verifies it belongs to the named app.
func (s *Server) loadUpload(r *http.Request, appID string) (*model.Upload, string, *apiError) {
	id := r.PathValue("upload")
	if id == "" {
		return nil, "", BadRequest("no upload id in the request path")
	}

	upload, err := s.store.GetUpload(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, "", NotFound("upload %q", id)
		}
		return nil, "", Errorf(http.StatusInternalServerError, "read upload").Wrap(err)
	}

	// An upload id is a bearer token for those parts. Checking it against the
	// app it was created for means a leaked id cannot be used to plant source
	// into a different app.
	if upload.AppID != appID {
		return nil, "", NotFound("upload %q", id)
	}

	return upload, s.uploadPartDir(id), nil
}

// uploadPartDir is where an upload's parts live.
func (s *Server) uploadPartDir(uploadID string) string {
	return filepath.Join(s.cfg.DataDir, "uploads", uploadID)
}

// multiPartReader concatenates part files into one stream.
//
// It opens each part in turn rather than holding them all open: an upload may
// declare thousands of parts, and a file descriptor per part would exhaust the
// process's limit long before the data ran out.
type multiPartReader struct {
	dir     string
	total   int
	index   int
	current *os.File
}

func (m *multiPartReader) Read(p []byte) (int, error) {
	for {
		if m.current == nil {
			if m.index > m.total {
				return 0, io.EOF
			}
			file, err := os.Open(filepath.Join(m.dir, fmt.Sprintf("part-%06d", m.index)))
			if err != nil {
				return 0, fmt.Errorf("open part %d: %w", m.index, err)
			}
			m.current = file
		}

		n, err := m.current.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			m.current.Close()
			m.current = nil
			m.index++
			continue
		}
		return 0, err
	}
}

func (m *multiPartReader) Close() error {
	if m.current != nil {
		err := m.current.Close()
		m.current = nil
		return err
	}
	return nil
}
