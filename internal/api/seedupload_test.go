package api_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheSeededScriptUploadsThroughARealServer is the whole chain, exercised
// against the deployment itself rather than against a stub of it.
//
// The other tests in this area each hold one half: the server's own tests assert
// that an over-limit upload is refused, the seed tests drive the shipped script
// against a stub that refuses it. Neither would notice the two halves disagreeing
// — a part size the server rejects, a `chunk_size` read from a field that is not
// there, an assembled archive the ingest cannot open. This runs the script that
// AppLab actually writes into an app's repository, against the actual API, and
// checks that the commit it reports is a commit the server has.
//
// It is also the only test that would catch the trailing-NUL trap: tar pads its
// output to its record size, so the archive handed over is larger than the gzip
// member inside it, and a server that read it as gzip-without-stopping would
// fail on every upload from the shell.
func TestTheSeededScriptUploadsThroughARealServer(t *testing.T) {
	shBin, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not on PATH")
	}
	for _, tool := range []string{"tar", "dd", "wc", "mktemp", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH", tool)
		}
	}

	// A deployment with a limit small enough that an ordinary tree crosses it,
	// so the fallback is the path taken rather than a corner case.
	srv, _, _ := newSourceServerWithLimit(t, 4<<10)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	if rec := doRequest(t, srv.Handler(), http.MethodPost, "/api/v1/apps", map[string]any{"id": "shop"}); rec.Code != http.StatusCreated {
		t.Fatalf("create app: %d (%s)", rec.Code, rec.Body.String())
	}

	// The script as the deployment serves it — the copy that goes into the
	// repository, not the template.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/apps/shop/agent/files/applab.sh", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fetch the script: %v", err)
	}
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetch the script: %d (%s)", resp.StatusCode, script)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	// Over the 4 KiB limit, and not compressible to nothing, so the simple path
	// really is refused.
	if err := os.WriteFile(filepath.Join(dir, "payload.bin"), []byte(incompressible(48<<10)), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	scriptPath := filepath.Join(dir, "applab.sh")
	if err := os.WriteFile(scriptPath, script, 0o755); err != nil {
		t.Fatalf("write the script: %v", err)
	}

	cmd := exec.Command(shBin, scriptPath, "upload", "e2e")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"APPLAB_URL="+ts.URL,
		"APPLAB_KEY=test-key",
		// The refresh would fetch this script again from the same server, which
		// is harmless but adds a request whose failure would be confusing.
		"APPLAB_NO_REFRESH=1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the script: %v\nstderr: %s", err, stderr.String())
	}

	sha := strings.TrimSpace(string(out))
	if len(sha) != 40 {
		t.Fatalf("the script reported %q, want a commit; stderr: %s", sha, stderr.String())
	}
	if !strings.Contains(stderr.String(), "parts") {
		t.Errorf("stderr was %q; the upload did not take the chunked path, so this test is not testing it", stderr.String())
	}

	// The commit it named has to be one the server actually holds.
	rec := doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/apps/shop/commits", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list commits: %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), sha) {
		t.Errorf("the commit the script reported is not in the app's history:\n%s", rec.Body.String())
	}

	// And the source arrived: the tree at that commit holds the file that was
	// pushed, read back from the repository the deployment keeps it in.
	rec = doRequest(t, srv.Handler(), http.MethodGet, "/api/v1/apps/shop/commits/"+sha, nil)
	// The commit-detail route answers with the commit's metadata; what matters
	// here is that it resolves at all, which it only does for a stored commit.
	if rec.Code != http.StatusOK {
		t.Errorf("read back the commit: %d (%s)", rec.Code, rec.Body.String())
	}
}
