package source

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// seedFiles are the files applab puts into an app's source tree, so that an
// agent handed an app's key has a way to drive its whole lifecycle without
// being told anything else.
//
// They are embedded rather than built as string literals in this file. The
// content is a shell script and a markdown document, and both are read by people
// as much as by agents: keeping them as files means they can be opened, linted
// and diffed as what they are rather than as a run of escaped newlines.
//
//go:embed seed
var seedFS embed.FS

// seedPaths are the names the seeded files take in an app's tree, in a fixed
// order so the injection is deterministic.
var seedPaths = []string{"applab.sh", "AGENT.md"}

// seedCommits is how many commits a new repository starts with: the single one
// carrying the seeded files.
const seedCommits = 1

// SeededFileCount reports how many files applab adds to every app's source tree.
//
// It is exported because a caller counting the files an upload produced has to
// account for them: the count is the whole tree, and the seed is part of it. A
// test asserting "the archive held two files" is asserting about the upload, not
// about what applab adds, and hardcoding the difference in each of them is how
// those tests silently stop meaning anything the next time a file is seeded.
func SeededFileCount() int { return len(seedPaths) }

// SeededCommitCount reports how many commits a newly created repository holds
// before anything is uploaded to it.
func SeededCommitCount() int { return seedCommits }

// seedFile is one file to write into a source tree.
type seedFile struct {
	// Name is the path in the tree, relative to its root.
	Name string

	// Mode is the file's permission bits. The script is executable so it can be
	// run from a fresh clone without a chmod.
	Mode int64

	// Body is the rendered content.
	Body string
}

// seedFor renders the files to inject for one app.
//
// appToken is substituted wherever the app's id has to appear. It is written as
// a literal in the templates rather than text/template because the content
// contains shell — `$`, `${}`, backticks — that a template engine would try to
// interpret, and a shell script silently mangled at render time is a worse
// failure than a placeholder that never got replaced (a test covers that it is).
//
// The substitution is a plain string replacement of a token that cannot occur in
// the surrounding text by accident.
func seedFor(appID string) []seedFile {
	out := make([]seedFile, 0, len(seedPaths))
	for _, name := range seedPaths {
		raw, err := seedFS.ReadFile("seed/" + name + ".tmpl")
		if err != nil {
			// Unreachable: the files are embedded at build time, so a missing
			// one is a build failure rather than a runtime condition. Panicking
			// keeps it from being silently skipped if that ever changes.
			panic("applab: embedded seed file " + name + " is missing: " + err.Error())
		}
		mode := int64(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		out = append(out, seedFile{
			Name: name,
			Mode: mode,
			Body: strings.ReplaceAll(string(raw), "{{APP}}", appID),
		})
	}
	return out
}

// writeSeed puts the seeded files into a source tree, replacing whatever is
// there.
//
// Replacing rather than skipping an existing one is deliberate. These files
// describe the deployment's own API, so an app carrying a stale copy — from a
// version before an endpoint changed — is an app whose documentation lies. The
// copy in the tree is applab's to keep current, not the uploader's to preserve;
// the files say so at the top of each.
func writeSeed(workTree, appID string) error {
	for _, f := range seedFor(appID) {
		if err := replaceWithFile(filepath.Join(workTree, f.Name), []byte(f.Body), os.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	return nil
}

// AgentFileName reports the files applab keeps in an app's source tree, and
// whether a name is one of them.
//
// A caller asks for one by name — the API serves them individually — so the
// lookup has to answer both questions at once rather than returning a list the
// caller then searches.
func AgentFile(appID, name string) (seedFile, bool) {
	for _, f := range seedFor(appID) {
		if f.Name == name {
			return f, true
		}
	}
	return seedFile{}, false
}

// AgentFileNames lists the files applab keeps in an app's source tree, in the
// order they appear in it.
func AgentFileNames() []string {
	out := make([]string, len(seedPaths))
	copy(out, seedPaths)
	return out
}

// seedCommit builds the opening commit of a new repository, containing only the
// seeded files.
//
// It runs on a repository that was just created and therefore has no tip, so the
// commit it makes is the root of the history — which is what makes a freshly
// created app cloneable into something with content rather than an empty tree.
func (s *Store) seedCommit(ctx context.Context, appID, repoPath string) error {
	workDir, err := os.MkdirTemp(s.tmpDir(), "seed-")
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	defer os.RemoveAll(workDir)

	workTree := filepath.Join(workDir, "tree")
	if err := os.MkdirAll(workTree, 0o700); err != nil {
		return fmt.Errorf("create working tree: %w", err)
	}
	if err := writeSeed(workTree, appID); err != nil {
		return err
	}

	const subject = "applab: how to work with this app"
	if _, _, _, err := s.commitWorkTree(ctx, appID, repoPath, workTree, workDir, subject, ""); err != nil {
		return err
	}
	return nil
}

// replaceWithFile writes a file at path, removing whatever is there first.
//
// The tree is caller-supplied — it came out of an uploaded archive — so the
// name could already be a symlink, a directory, or nothing at all. Each of the
// three has to be handled deliberately:
//
//   - A symlink would be followed by a plain write, putting the seed's content
//     wherever it points. Removing it first is what keeps the write inside the
//     tree, and it is also what we want: the file is applab's to replace.
//   - A directory makes the write fail with EISDIR, so it is removed too, for
//     the same reason — the seed has to be present after every upload.
//   - A regular file is simply overwritten.
//
// The mode is set explicitly rather than left to the write, because WriteFile
// only applies permissions when it creates the file: a pre-existing file with
// the wrong mode would otherwise keep it, and the script has to be executable.
func replaceWithFile(path string, body []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("clear %s before seeding it: %w", path, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s before seeding it: %w", path, err)
	}

	if err := os.WriteFile(path, body, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", path, err)
	}
	return nil
}
