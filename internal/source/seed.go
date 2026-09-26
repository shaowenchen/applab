package source

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// seedFiles are the files AppLab puts into an app's source tree, so that an
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

// seedPaths are the names of the files AppLab keeps in an app's tree, in a fixed
// order so the injection is deterministic.
//
// These are *kept current*: every upload rewrites them, because they describe
// the deployment's own API and a stale copy is documentation that lies. Nothing
// here may be a file the app's author would also write — see seedOncePaths.
var seedPaths = []string{"applab.sh", "AGENT.md"}

// seedOncePaths are the names written only into a new app's opening commit.
//
// They are the difference between a file AppLab owns and a file AppLab hands
// over. The two above are AppLab's and are replaced on every upload; these are
// the app author's from the moment they exist, and are never touched again.
//
// The example is why the distinction has to exist. A Dockerfile is exactly what
// an app's first upload contains, so writing one on every upload would delete
// the Dockerfile the app is built from — every push would replace the app's own
// build with a static site, and the failure would look like the build system
// ignoring the source. Written only when the repository is created, it is a
// starting point someone either edits or deletes, and either is fine.
var seedOncePaths = []string{"Dockerfile"}

// seedCommits is how many commits a new repository starts with: the single one
// carrying the seeded files.
const seedCommits = 1

// SeededFileCount reports how many files AppLab adds to every app's source tree.
//
// It is exported because a caller counting the files an upload produced has to
// account for them: the count is the whole tree, and the seed is part of it. A
// test asserting "the archive held two files" is asserting about the upload, not
// about what AppLab adds, and hardcoding the difference in each of them is how
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

// SeedValues are what an app's seeded files are rendered against.
//
// It is a struct rather than the app id alone because the script now carries the
// two things a caller would otherwise have to be told: where the deployment is
// and the credential that reaches this app. Both are written in as defaults, and
// both yield to the environment — see the header of applab.sh — so a checkout
// works with nothing exported and still works against a different deployment
// when someone exports these.
type SeedValues struct {
	// App is the app's id.
	App string

	// URL is the address this deployment is reached at by people, with no
	// trailing slash. Empty on a deployment that does not know its own public
	// address, in which case the placeholder is left empty and the environment
	// is the only source.
	URL string

	// Key is the app's own API key. Empty when this deployment mints no app
	// keys, or when the key could not be read.
	//
	// It is a credential, and writing it here is a deliberate decision with a
	// stated cost: the repository is cloned, uploaded, built into an image and
	// mirrored, so the key travels with all of it. See the header of applab.sh,
	// which says so where the person who finds it will read it.
	Key string
}

// seedFor renders the files to inject for one app.
//
// The substitution is a plain string replacement of a token that cannot occur in
// the surrounding text by accident, rather than text/template: the content is
// shell, and a template engine would interpret its `$`, `${}` and backticks. A
// shell script silently mangled at render time is a worse failure than a
// placeholder that never got replaced, which a test covers.
func seedFor(v SeedValues) []seedFile {
	return renderSeed(v, seedPaths)
}

// seedForFirstCommit renders everything a brand-new repository starts with: the
// files AppLab keeps current, plus the ones it only ever hands over.
func seedForFirstCommit(v SeedValues) []seedFile {
	return renderSeed(v, append(append([]string{}, seedPaths...), seedOncePaths...))
}

// renderSeed renders the named templates, in the order given.
func renderSeed(v SeedValues, names []string) []seedFile {
	replacements := map[string]string{
		"{{APP}}": v.App,
		"{{URL}}": v.URL,
		"{{KEY}}": v.Key,
	}
	out := make([]seedFile, 0, len(names))
	for _, name := range names {
		raw, err := seedFS.ReadFile("seed/" + name + ".tmpl")
		if err != nil {
			// Unreachable: the files are embedded at build time, so a missing
			// one is a build failure rather than a runtime condition. Panicking
			// keeps it from being silently skipped if that ever changes.
			panic("AppLab: embedded seed file " + name + " is missing: " + err.Error())
		}
		mode := int64(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		body := string(raw)
		for token, value := range replacements {
			body = strings.ReplaceAll(body, token, value)
		}
		out = append(out, seedFile{Name: name, Mode: mode, Body: body})
	}
	return out
}

// writeSeed puts the kept-current files into a source tree, replacing whatever
// is there.
//
// Replacing rather than skipping an existing one is deliberate. These files
// describe the deployment's own API, so an app carrying a stale copy — from a
// version before an endpoint changed — is an app whose documentation lies. The
// copy in the tree is AppLab's to keep current, not the uploader's to preserve;
// the files say so at the top of each.
//
// The seed-once files are deliberately not here. An upload is where an app's own
// Dockerfile arrives, and rewriting it would delete the thing being built.
func writeSeed(workTree string, v SeedValues) error {
	for _, f := range seedFor(v) {
		if err := replaceWithFile(filepath.Join(workTree, f.Name), []byte(f.Body), os.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	return nil
}

// AgentFileName reports the files AppLab keeps in an app's source tree, and
// whether a name is one of them.
//
// A caller asks for one by name — the API serves them individually — so the
// lookup has to answer both questions at once rather than returning a list the
// caller then searches.
func AgentFile(v SeedValues, name string) (seedFile, bool) {
	for _, f := range seedFor(v) {
		if f.Name == name {
			return f, true
		}
	}
	return seedFile{}, false
}

// AgentFileNames lists the files AppLab keeps in an app's source tree, in the
// order they appear in it.
func AgentFileNames() []string {
	out := make([]string, len(seedPaths))
	copy(out, seedPaths)
	return out
}

// seedValues assembles what an app's seeded files are rendered against: its id,
// the address people reach this deployment at, and the app's own key.
//
// The address comes from the Store because it is a deployment-wide setting that
// every app's tree carries a copy of. The key is fetched through the injected
// lookup rather than read from the object store directly, because the key store
// is not this package's — see Options.SeedKey.
//
// A key that cannot be read is not an error. The seeded script is documentation
// and a convenience: a deployment that mints no keys, or an app whose key was
// never created, produces a tree whose APPLAB_KEY line is left blank to fill in,
// which is what every app got before this. Failing the upload instead would make
// an optional convenience into a hard dependency.
func (s *Store) seedValues(ctx context.Context, appID string) SeedValues {
	v := SeedValues{App: appID, URL: strings.TrimSuffix(strings.TrimSpace(s.publicURL), "/")}
	if s.seedKey == nil {
		return v
	}
	key, err := s.seedKey(ctx, appID)
	if err != nil {
		return v
	}
	v.Key = key
	return v
}

// seedCommit builds the opening commit of a new repository, containing only the
// seeded files.
//
// It runs on a repository that was just created and therefore has no tip, so the
// commit it makes is the root of the history — which is what makes a freshly
// created app cloneable into something with content rather than an empty tree.
func (s *Store) seedCommit(ctx context.Context, appID, branch, repoPath string) error {
	workDir, err := os.MkdirTemp(s.tmpDir(), "seed-")
	if err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	defer os.RemoveAll(workDir)

	workTree := filepath.Join(workDir, "tree")
	if err := os.MkdirAll(workTree, 0o700); err != nil {
		return fmt.Errorf("create working tree: %w", err)
	}
	for _, f := range seedForFirstCommit(s.seedValues(ctx, appID)) {
		if err := replaceWithFile(filepath.Join(workTree, f.Name), []byte(f.Body), os.FileMode(f.Mode)); err != nil {
			return err
		}
	}

	const subject = "AppLab: how to work with this app"
	if _, _, _, err := s.commitWorkTree(ctx, appID, branch, repoPath, workTree, workDir, subject, ""); err != nil {
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
//     tree, and it is also what we want: the file is AppLab's to replace.
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
