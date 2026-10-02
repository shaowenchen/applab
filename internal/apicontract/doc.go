// Package apicontract holds the test that keeps AppLab's four client surfaces in
// step with the API: the web console, the CLI, the seeded applab.sh script, and
// the generated SDKs.
//
// # Why this exists
//
// The rule is that every surface offers the same capabilities. It was only ever
// enforced between two of them — the console and the seeded script, by a test in
// internal/source — and the CLI was explicitly left out of that, on the reasoning
// that keeping a third list in step was more likely to be forgotten than kept.
// The result was predictable: the platform's own events route was reachable from
// the console and from nowhere else, because nothing was checking.
//
// So this test starts from the API and asks, of each of the four surfaces, "can
// this actually reach that route". Every route has to have an answer, and the
// answer is either evidence or a named exemption. A route nobody classified is a
// build failure, which is what makes "the surfaces agree" a property rather than
// an intention.
//
// # How each surface is checked, and how strong that is
//
//   - CLI: authoritative. The test runs `applab __routes`, which enumerates what
//     the command tree can actually reach. A comment cannot satisfy it.
//   - SDK: authoritative for the specification. It parses api/openapi.yaml and
//     looks up the route, so an operation missing from the document fails.
//   - Script: strong. The seeded script is rendered and searched for the call.
//   - Console: the weakest. It serves one HTML file whose JavaScript builds paths
//     by concatenation (`"/api/v1/apps/" + app + "/builds"`), so the check looks
//     for the static part of a path rather than the whole thing. It can be
//     satisfied by a string that is present but unused, and it cannot see that a
//     console button sends the wrong body. It proves the route is *referenced*,
//     not that it is *reachable*.
//
// That weakness is stated rather than hidden because the honest alternative —
// driving a headless browser through every control — is a different project, and
// a test that overstates its reach is worse than one whose limits are written
// down. What it does catch is the thing that actually went wrong: a capability
// added to one surface and forgotten everywhere else.
package apicontract
