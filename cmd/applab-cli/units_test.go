package main

import "testing"

// The units the resources command prints in.
//
// Parsed from Kubernetes quantities, and asserted here rather than left to the
// eye because both directions are easy to get subtly wrong: a memory quantity
// arrives as "512Mi" or as a byte count depending on how the bound was set, and a
// CPU quantity arrives normalised to milli-units — "2" comes back as "2000m".
// The console does the same conversion on the page, so a disagreement between
// the two would show one number in a terminal and another in a browser.
func TestResourceUnitsAreRenderedAsCoresAndGiB(t *testing.T) {
	for _, c := range []struct{ quantity, want string }{
		{"500m", "0.5"},
		{"100m", "0.1"},
		{"1500m", "1.5"},
		{"2", "2"},
		{"2000m", "2"},
	} {
		if got := cores(c.quantity); got != c.want {
			t.Errorf("cores(%q) = %q, want %q", c.quantity, got, c.want)
		}
	}

	for _, g := range []struct{ quantity, want string }{
		// The one that matters: 128Mi is this deployment's own memory default,
		// and rounding it would misreport what every unconfigured app runs with.
		{"134217728", "0.125"},
		{"512Mi", "0.5"},
		{"1Gi", "1"},
		{"2Gi", "2"},
	} {
		if got := gib(g.quantity); got != g.want {
			t.Errorf("gib(%q) = %q, want %q", g.quantity, got, g.want)
		}
	}

	// An unparseable value is passed through rather than turned into a zero that
	// would read as a real limit of nothing.
	if got := cores("nonsense"); got != "nonsense" {
		t.Errorf("cores(nonsense) = %q, want it passed through", got)
	}
	if got := gib("nonsense"); got != "nonsense" {
		t.Errorf("gib(nonsense) = %q, want it passed through", got)
	}
}
