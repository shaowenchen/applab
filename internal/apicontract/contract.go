package apicontract

import "strings"

// Surface is one of the client surfaces a route may or may not be reachable from.
type Surface string

const (
	Console Surface = "console"
	CLI     Surface = "cli"
	Script  Surface = "script"
	SDK     Surface = "sdk"
)

// Surfaces is every surface, in the order a report reads.
var Surfaces = []Surface{Console, CLI, Script, SDK}

// Coverage is how one surface covers one route: either evidence that it reaches
// it, or a reason it is exempt.
//
// Exactly one of the two is set. A surface that is neither is the failure this
// package exists to catch — it means nobody decided, which is how a capability
// ends up on one surface and no other.
type Coverage struct {
	// Needle is the substring that proves the surface references the route. It
	// is checked against the surface's own text — the console's script block, the
	// rendered shell script — and for the CLI and the SDK the check is structural
	// instead, so their Needle is unused.
	Needle string

	// Why exempts the surface: a sentence saying why this route is deliberately
	// not reachable from here. It is never empty when set — an empty exemption is
	// a route nobody thought about, wearing the shape of one somebody did.
	Why string
}

// Covered reports whether this is evidence rather than an exemption.
func (c Coverage) Covered() bool { return c.Why == "" }

// Entry is one route and how each surface covers it.
type Entry struct {
	// Method and Path are spelled as the route table spells them, so an entry and
	// a route are compared as the same string.
	Method string
	Path   string

	Console Coverage
	CLI     Coverage
	Script  Coverage
	SDK     Coverage
}

// Pattern is the route as the table names it, e.g. "GET /api/v1/apps/{app}".
func (e Entry) Pattern() string { return e.Method + " " + e.Path }

// CoverageFor returns the entry's coverage of one surface.
//
// Exported because the parity test lives in a separate package — it exercises
// the package from outside, the way the surfaces themselves do.
func (e Entry) CoverageFor(s Surface) Coverage {
	switch s {
	case Console:
		return e.Console
	case CLI:
		return e.CLI
	case Script:
		return e.Script
	case SDK:
		return e.SDK
	default:
		panic("apicontract: unknown surface " + string(s))
	}
}

// covered is a route a surface reaches, with the substring that shows it.
func covered(needle string) Coverage { return Coverage{Needle: needle} }

// coveredByMethod marks a route the CLI or SDK reaches, which is verified
// structurally rather than by substring — see the package comment.
func coveredByMethod() Coverage { return Coverage{Needle: "(structural)"} }

// exempt records why a surface deliberately does not reach a route.
func exempt(why string) Coverage {
	if strings.TrimSpace(why) == "" {
		panic("apicontract: an exemption needs a reason")
	}
	return Coverage{Why: why}
}

// Findings are the exemptions that are deliberate but worth a second look: a
// route reachable from the console that the script cannot reach because the
// script is one of the surfaces and should reach everything an app key can. They
// are reported, not enforced, because widening the rule silently is the failure
// mode a report is meant to prevent.
//
// There is nothing in this list right now; it exists so that narrowing coverage
// has to be written down and shows up in the test output.
var Findings = []string{}
