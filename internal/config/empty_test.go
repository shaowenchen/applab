package config

import (
	"testing"
)

// TestAnEmptyEnvironmentVariableClearsTheSetting is the bug the chart's
// "turn the build pipeline off" path depended on and did not get.
//
// The chart renders APPLAB_BUILD_REGISTRY, APPLAB_BUILD_KANIKO_IMAGE and
// APPLAB_BUILD_SECRET as "" to disable the build pipeline — the server decides
// whether it can build from those three being non-empty. Empty used to be
// treated as "not set", so all three silently kept their defaults: an
// installation that asked for no build pipeline came up with one, pointing at a
// registry credential nothing had created, and every build failed for a Secret
// the operator had already turned off.
//
// The distinction is real and only LookupEnv can make it. An operator blanking a
// value in the chart, or in the environment, means to blank it.
func TestAnEmptyEnvironmentVariableClearsTheSetting(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		value string
		read  func(Config) string
	}{
		// The three the chart blanks to disable the build pipeline — the values
		// the server reads to decide whether it can build at all.
		{"the build registry", "APPLAB_BUILD_REGISTRY", "registry.example.com/apps", func(c Config) string { return c.Build.Registry }},
		{"the kaniko image", "APPLAB_BUILD_KANIKO_IMAGE", "ghcr.io/example/kaniko:v1", func(c Config) string { return c.Build.KanikoImage }},
		{"the registry credential", "APPLAB_BUILD_SECRET", "a-registry-secret", func(c Config) string { return c.Build.Secret }},

		// And the settings an operator blanks for their own reasons, which failed
		// the same silent way: a domain or a prefix removed stayed set.
		{"the base domain", "APPLAB_BASE_DOMAIN", "apps.example.com", func(c Config) string { return c.BaseDomain }},
		{"the object store prefix", "APPLAB_OBJECT_STORE_PREFIX", "team-a", func(c Config) string { return c.ObjectStore.Prefix }},
		// The gateway is validated on load, so it needs a value in the form Istio
		// resolves one by.
		{"the gateway", "APPLAB_DEPLOY_GATEWAY", "istio-system/gateway", func(c Config) string { return c.Deploy.Gateway }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			// A key and a bucket, so Load gets far enough to hand back a
			// configuration rather than refusing the deployment.
			t.Setenv("APPLAB_KEY", "test-key")
			t.Setenv("APPLAB_OBJECT_STORE_ENDPOINT", "http://objects.example.com")
			t.Setenv("APPLAB_OBJECT_STORE_BUCKET", "applab")
			t.Setenv("APPLAB_OBJECT_STORE_ACCESS_KEY", "key")
			t.Setenv("APPLAB_OBJECT_STORE_SECRET_KEY", "secret")

			// Set it, so the assertion below is about clearing a value rather
			// than about a default that happened to be empty already.
			t.Setenv(tc.env, tc.value)
			set, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.read(set); got != tc.value {
				t.Fatalf("%s was not applied at all (%q), so this test would not notice the bug it exists for", tc.env, got)
			}

			// Now blank it. The value must go, not fall back to the default.
			t.Setenv(tc.env, "")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := tc.read(cfg); got != "" {
				t.Errorf("%s=\"\" left %q in place; an empty variable must clear the setting, not restore the default",
					tc.env, got)
			}
		})
	}
}
