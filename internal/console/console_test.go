package console

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestConsoleRendering asserts the console's own JavaScript renders an app's
// address correctly, by running it.
//
// The console shows an app's address in two places, and with a shared path
// prefix the host belongs to the deployment rather than the app — so a version
// that showed the host alone labelled every row identically. That is a display
// bug no Go test can see: the API was right, the page was wrong, and nothing in
// `make check` rendered the page at all.
//
// The script is executed rather than its text matched. A regex for
// "app.hostname + app.path" would pass on code that computes an address and then
// never uses it, which is precisely the mistake worth catching.
//
// Skipped when node is absent, because the console is a static file with no
// build step and requiring a JavaScript runtime to build the server would undo
// that — but never skipped in CI.
//
// A silent skip in CI is the one outcome worth refusing: it would look exactly
// like the check having run, and the bug this test exists to catch would ship
// with a green tick beside it. So a missing node is a failure there and a skip
// only on a developer's machine.
func TestConsoleRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("node is not installed and this is CI: the console rendering "+
				"checks would be skipped silently. Install node in the workflow (%v)", err)
		}
		t.Skip("node is not installed; skipping the console rendering checks")
	}

	script, err := filepath.Abs(filepath.Join("render_test.js"))
	if err != nil {
		t.Fatalf("resolve the test script: %v", err)
	}

	cmd := exec.Command(node, script)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the console does not render correctly (node %s):\n%s", runtime.Version(), out)
	}
	t.Logf("%s", out)
}
