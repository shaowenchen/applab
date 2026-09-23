package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadRequiresAKey pins the boot invariant: a deployment with nothing to
// authenticate against refuses to start rather than serving the API openly.
func TestLoadRequiresAKey(t *testing.T) {
	clearEnv(t)

	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with no key configured; an open API is a far worse outcome than a failed boot")
	} else if !strings.Contains(err.Error(), "key") {
		t.Errorf("the error should name the missing keys, got: %v", err)
	}
}

// TestKeyEnvironmentForms covers the several ways an operator may supply keys.
func TestKeyEnvironmentForms(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "single key",
			env:  map[string]string{"APPLAB_KEY": "one"},
			want: []string{"one"},
		},
		{
			name: "plural list",
			env:  map[string]string{"APPLAB_KEYS": "one,two,three"},
			want: []string{"one", "two", "three"},
		},
		{
			name: "list with whitespace and empties",
			env:  map[string]string{"APPLAB_KEYS": " one , ,two, "},
			want: []string{"one", "two"},
		},
		{
			name: "both forms merge",
			env:  map[string]string{"APPLAB_KEY": "one", "APPLAB_KEYS": "two,three"},
			want: []string{"one", "two", "three"},
		},
		{
			name: "duplicates collapse",
			env:  map[string]string{"APPLAB_KEYS": "one,one,two"},
			want: []string{"one", "two"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			t.Setenv("APPLAB_DATA_DIR", t.TempDir())

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(cfg.Keys) != len(tc.want) {
				t.Fatalf("got keys %v, want %v", cfg.Keys, tc.want)
			}
			for i := range tc.want {
				if cfg.Keys[i] != tc.want[i] {
					t.Errorf("key %d = %q, want %q", i, cfg.Keys[i], tc.want[i])
				}
			}
		})
	}
}

// TestDefaultsAreInternallyConsistent asserts the shipped defaults satisfy the
// validation applied to any other configuration — otherwise a fresh install
// would fail its own checks.
func TestDefaultsAreInternallyConsistent(t *testing.T) {
	clearEnv(t)
	t.Setenv("APPLAB_KEY", "k")
	t.Setenv("APPLAB_DATA_DIR", t.TempDir())

	if _, err := Load(); err != nil {
		t.Fatalf("the default configuration fails validation: %v", err)
	}
}

// TestUploadLimitValidation asserts inconsistent limits are rejected, because a
// chunk size above the simple-upload limit would tell a client to split an
// upload it would then accept whole.
func TestUploadLimitValidation(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{
			name:    "chunk larger than simple limit",
			env:     map[string]string{"APPLAB_CHUNK_SIZE": "100", "APPLAB_MAX_SIMPLE_UPLOAD": "50"},
			wantErr: true,
		},
		{
			name:    "max chunk below chunk size",
			env:     map[string]string{"APPLAB_CHUNK_SIZE": "100", "APPLAB_MAX_CHUNK_BYTES": "50"},
			wantErr: true,
		},
		{
			name: "consistent limits",
			env: map[string]string{
				"APPLAB_CHUNK_SIZE": "50", "APPLAB_MAX_SIMPLE_UPLOAD": "50", "APPLAB_MAX_CHUNK_BYTES": "100",
			},
			wantErr: false,
		},
		{
			name:    "non-numeric limit keeps the default rather than failing",
			env:     map[string]string{"APPLAB_CHUNK_SIZE": "not-a-number"},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("APPLAB_KEY", "k")
			t.Setenv("APPLAB_DATA_DIR", t.TempDir())
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if tc.wantErr && err == nil {
				t.Error("expected a validation error, got none")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected success, got: %v", err)
			}
		})
	}
}

// TestBaseDomainRejectsURLs asserts a base domain with a scheme, port or path is
// refused — each would produce hostnames that silently fail to resolve.
func TestBaseDomainRejectsURLs(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		want    string
	}{
		{in: "apps.example.com", want: "apps.example.com"},
		{in: "apps.example.com.", want: "apps.example.com"},
		{in: "https://apps.example.com", wantErr: true},
		{in: "apps.example.com:8080", wantErr: true},
		{in: "apps.example.com/path", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("APPLAB_KEY", "k")
			t.Setenv("APPLAB_DATA_DIR", t.TempDir())
			t.Setenv("APPLAB_BASE_DOMAIN", tc.in)
			// A base domain needs a gateway to serve it; without one the apps
			// would get hostnames nothing answers on. Set one so these cases
			// exercise the domain parsing rather than that rule.
			t.Setenv("APPLAB_DEPLOY_GATEWAY", "ops-system/gateway")

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected %q to be rejected", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.BaseDomain != tc.want {
				t.Errorf("BaseDomain = %q, want %q", cfg.BaseDomain, tc.want)
			}
		})
	}
}

// TestConfigFileIsLoaded covers the YAML path, including that the environment
// wins over it — in Kubernetes the environment is what an operator sets per
// deployment, and it should not be silently overridden by a file in the image.
func TestConfigFileIsLoaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "applab.yaml")
	content := `
listen: ":9999"
base_domain: "from-file.example.com"
deploy:
  gateway: "ops-system/from-file"
keys:
  - file-key
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	clearEnv(t)
	t.Setenv("APPLAB_CONFIG", path)
	t.Setenv("APPLAB_DATA_DIR", t.TempDir())

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("Listen = %q, want \":9999\" from the file", cfg.Listen)
	}
	if cfg.BaseDomain != "from-file.example.com" {
		t.Errorf("BaseDomain = %q, want the file's value", cfg.BaseDomain)
	}
	if len(cfg.Keys) != 1 || cfg.Keys[0] != "file-key" {
		t.Errorf("Keys = %v, want [file-key] from the file", cfg.Keys)
	}

	// The environment must win.
	t.Setenv("APPLAB_BASE_DOMAIN", "from-env.example.com")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BaseDomain != "from-env.example.com" {
		t.Errorf("BaseDomain = %q; the environment must override the file", cfg.BaseDomain)
	}
}

// TestMissingConfigFileIsAnError asserts a named-but-unreadable file is reported
// rather than silently ignored: a typo in the path should not start a server
// with defaults the operator did not intend.
func TestMissingConfigFileIsAnError(t *testing.T) {
	clearEnv(t)
	t.Setenv("APPLAB_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	t.Setenv("APPLAB_KEY", "k")
	t.Setenv("APPLAB_DATA_DIR", t.TempDir())

	if _, err := Load(); err == nil {
		t.Error("a missing config file was silently ignored")
	}
}

// TestDataDirIsMadeAbsolute asserts paths are resolved before use: the data
// directory is also the root the git handlers validate against, and a relative
// root would change meaning with the process's working directory.
func TestDataDirIsMadeAbsolute(t *testing.T) {
	clearEnv(t)
	t.Setenv("APPLAB_KEY", "k")
	t.Setenv("APPLAB_DATA_DIR", "./relative-data")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Errorf("DataDir = %q, want an absolute path", cfg.DataDir)
	}
	if !filepath.IsAbs(cfg.DBPath) {
		t.Errorf("DBPath = %q, want an absolute path", cfg.DBPath)
	}
	if !strings.HasSuffix(cfg.DBPath, "applab.db") {
		t.Errorf("DBPath = %q, want it to default inside the data directory", cfg.DBPath)
	}
}

// TestLogLevelValidation asserts an unknown level is rejected instead of
// quietly falling back, so a typo is visible at boot.
func TestLogLevelValidation(t *testing.T) {
	clearEnv(t)
	t.Setenv("APPLAB_KEY", "k")
	t.Setenv("APPLAB_DATA_DIR", t.TempDir())
	t.Setenv("APPLAB_LOG_LEVEL", "verbose")

	if _, err := Load(); err == nil {
		t.Error("an invalid log level was accepted")
	}
}

// clearEnv removes every APPLAB_ variable so a case is not influenced by the
// ambient environment — including the developer's own shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "APPLAB_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
}
