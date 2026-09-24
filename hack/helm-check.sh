#!/usr/bin/env bash
#
# Render the chart and assert the things that were wrong before.
#
# A chart fails differently from code: `helm template` exits 0 for a template
# that renders valid YAML with the wrong value in it, so "it rendered" is not
# evidence of anything. Every check below corresponds to a defect that shipped
# and was found by reading the output rather than by running it.
#
# Needs helm; skips loudly if it is absent, since helm is not required to build
# or test the Go code.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v helm >/dev/null 2>&1; then
  echo "helm not found; skipping chart checks" >&2
  exit 0
fi

CHART=charts/applab
NS=ops-system

# Scratch space for a render that is read as a file rather than a here-string.
# Removed on exit, including on the failures below, so a red check leaves
# nothing behind.
RUNTIME_DIR="$(mktemp -d)"
trap 'rm -rf "$RUNTIME_DIR"' EXIT

# A configuration that exercises every branch the guards protect, so the
# defaults in values.yaml are not what is being checked.
BASE=(
  --namespace "$NS"
  --set "auth.keys[0]=test-key-do-not-use"
  --set "apps.baseDomain=apps.example.com"
  --set "deploy.gateway=$NS/gateway"
  --set "build.registry=registry.example.com/apps"
  --set "build.pushSecret=regcred"
  --set "deploy.imagePullSecret=regpull"
)

render() { helm template applab "$CHART" "${BASE[@]}" "$@"; }

fail() { echo "FAIL: $*" >&2; exit 1; }

# Every manifest has to be parseable YAML before any assertion about its
# contents means anything. Python's YAML parser is stricter about the mistakes
# a template makes (a stray tab, a mis-indented block) than helm's own output
# check, which only catches some of them.
if command -v python3 >/dev/null 2>&1; then
  render | python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
print(f"  {len(docs)} documents parse as YAML")
'
fi

out="$(render)"

# The upload limits were rendered as 8.388608e+06 — YAML's float notation for a
# large integer — which the server's integer parser rejects, silently leaving
# the default in place.
grep -q 'APPLAB_MAX_SIMPLE_UPLOAD: "8388608"' <<<"$out" \
  || fail "upload limits are not integers; a value like 8.388608e+06 will be ignored by the server"
grep -q 'APPLAB_MAX_CHUNK_BYTES: "33554432"' <<<"$out" \
  || fail "max chunk bytes is not an integer"

# These were absent from the ConfigMap entirely, so the settings they carry
# reached the server as nothing at all.
grep -q 'APPLAB_BUILD_PUSH_SECRET: "regcred"' <<<"$out" \
  || fail "build.pushSecret is not passed to the server; builds cannot push"
grep -q 'APPLAB_BUILD_TTL_AFTER_FINISHED: "24h"' <<<"$out" \
  || fail "build.ttlAfterFinished is not passed to the server"
grep -q 'APPLAB_DEPLOY_IMAGE_PULL_SECRET: "regpull"' <<<"$out" \
  || fail "deploy.imagePullSecret is not passed to the server"

# deploy.imagePullSecret and build.pushSecret are different credentials for
# different jobs; wiring one to both would look right and be wrong.
if grep -q 'APPLAB_DEPLOY_IMAGE_PULL_SECRET: "regcred"' <<<"$out"; then
  fail "deploy.imagePullSecret is being fed build.pushSecret"
fi

# Every environment variable the server reads must appear in the rendered
# ConfigMap or Secret. This is the check that would have caught the two settings
# above the day they were written.
want="$(grep -o 'APPLAB_[A-Z_]*' internal/config/config.go | sort -u)"
have="$(render | grep -o 'APPLAB_[A-Z_]*' | sort -u)"
# APPLAB_CONFIG, APPLAB_KEY and APPLAB_KEYS are set elsewhere (command line and
# Secret), and the path overrides are for running outside a cluster.
ignored=$'APPLAB_CONFIG\nAPPLAB_DATA_DIR\nAPPLAB_DB_PATH\nAPPLAB_KEY\nAPPLAB_KEYS\nAPPLAB_KUBECONFIG'
missing="$(comm -23 <(echo "$want") <(echo "$have") | grep -vxF "$ignored" || true)"
if [ -n "$missing" ]; then
  fail "the server reads these but the chart never sets them: $(tr '\n' ' ' <<<"$missing")"
fi

# build.enabled=false has to disable the pipeline. The server decides that from
# the registry and the two images, so all three have to be emptied; blanking
# only the registry would leave it enabled against the default builder.
off="$(render --set build.enabled=false)"
for var in APPLAB_BUILD_REGISTRY APPLAB_BUILD_BUILDER_IMAGE APPLAB_BUILD_FETCHER_IMAGE; do
  grep -q "^  $var: \"\"" <<<"$off" \
    || fail "build.enabled=false leaves $var set, so the build pipeline still comes up"
done

# A malformed configuration must be refused at render time rather than deployed
# and discovered later. Each of these has a guard in _helpers.tpl.
must_fail() {
  local desc="$1"; shift
  if helm template applab "$CHART" --namespace "$NS" "$@" >/dev/null 2>&1; then
    fail "expected a render failure: $desc"
  fi
}
must_fail "no API keys"       --set apps.baseDomain=a.example.com
must_fail "no registry"       --set "auth.keys[0]=k" --set apps.baseDomain=a.example.com --set "deploy.gateway=$NS/gateway"
must_fail "bad gateway"       --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" --set "apps.baseDomain=a.example.com" --set "deploy.gateway=nope"
# deploy.gateway now has a default, so "unset" no longer produces an empty one;
# these two blank it explicitly to reach the guard.
must_fail "blanked gateway"   --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" --set "apps.baseDomain=a.example.com" --set "deploy.gateway="
must_fail "two replicas"      "${BASE[@]}" --set replicaCount=2

# Every manifest's top-level keys have to be ones Kubernetes knows. Text emitted
# outside a YAML structure — a warning written as bare prose, say — becomes a
# field of the document, and the manifest still parses as YAML, so the mistake
# survives every check that only asks whether the output is valid.
if command -v python3 >/dev/null 2>&1; then
  render --set build.rootless=false | python3 -c '
import sys, yaml

# The fields a Kubernetes manifest may carry at the top level, across every kind
# this chart renders.
allowed = {
    "apiVersion", "kind", "metadata", "spec", "data", "stringData", "type",
    "rules", "roleRef", "subjects", "automountServiceAccountToken",
    "imagePullSecrets", "secrets",
}

bad = []
for doc in yaml.safe_load_all(sys.stdin):
    if not doc:
        continue
    kind = doc.get("kind", "?")
    for key in doc:
        if key not in allowed:
            bad.append(kind + ": " + repr(key))

if bad:
    print("unknown top-level fields (text emitted outside the YAML structure?):")
    for entry in bad:
        print("  " + entry)
    sys.exit(1)
'
fi

# The privileged-build warning is the only signal an operator gets that the
# escape hatch is on, so it has to actually appear. It is a YAML comment, so it
# reaches the rendered manifest rather than stderr.
warn="$(render --set build.rootless=false 2>/dev/null)"
grep -q 'WARNING: build.rootless is false' <<<"$warn" \
  || fail "build.rootless=false does not warn that builds run privileged"
# And it must not appear when the default is left alone.
if render 2>/dev/null | grep -q 'WARNING: build.rootless is false'; then
  fail "the privileged-build warning appears with build.rootless=true"
fi

# RBAC is a Role, not a ClusterRole: applab keeps everything in one namespace,
# so it has no business holding any permission outside it. A ClusterRole
# reappearing here would silently undo the point of that.
if grep -q 'kind: ClusterRole' <<<"$out"; then
  fail "a ClusterRole is rendered; applab runs in one namespace and needs only a Role"
fi
grep -q 'kind: Role$' <<<"$out" || fail "no Role is rendered"
grep -q 'kind: RoleBinding' <<<"$out" || fail "no RoleBinding is rendered"
if grep -q 'resources: \["namespaces"\]' <<<"$out"; then
  fail "the Role grants namespace permissions, which it does not need"
fi

# Publishing apps is Istio's job here: the cluster routes through a gateway, and
# an Ingress applab created would be ignored by it.
grep -q 'APPLAB_DEPLOY_GATEWAY: "ops-system/gateway"' <<<"$out" \
  || fail "deploy.gateway does not reach the server; apps would have no route"

# The default gateway has to reach the server too. A default that stops in
# values.yaml and never renders is a deployment where every app is unreachable,
# discovered from a browser rather than at install. Rendered without setting
# deploy.gateway at all, which is the only way to observe the chart's default —
# `--set deploy.gateway=` blanks it rather than restoring it.
defaulted="$(helm template applab "$CHART" --namespace "$NS" \
  --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" \
  --set "apps.baseDomain=apps.example.com")"
grep -q 'APPLAB_DEPLOY_GATEWAY: "istio-ingress/istio-ingress"' <<<"$defaulted" \
  || fail "the default deploy.gateway does not reach the server"

# The namespace defaults to ops-system, and the prefix model it replaced is gone.
grep -q 'APPLAB_NAMESPACE: "ops-system"' <<<"$out" || fail "the namespace is not ops-system"
if grep -q 'APPLAB_NAMESPACE_PREFIX' <<<"$out"; then
  fail "APPLAB_NAMESPACE_PREFIX is still set; the per-app namespace model is gone"
fi

# applab's own image is pulled always: a re-pushed tag must not be served from a
# node's cache.
grep -q 'imagePullPolicy: Always' <<<"$out" || fail "applab's own image is not pulled always"

# A base domain with no gateway is a deployment where every app is unreachable
# from outside, so it has to be refused rather than rendered. That case is
# covered by `must_fail "blanked gateway"` above: deploy.gateway has a default
# now, so leaving it unset is no longer the way to reach the guard.

# A gateway that is not namespace/name would not resolve.
if helm template applab "$CHART" --namespace "$NS" \
  --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" \
  --set "apps.baseDomain=apps.example.com" --set "deploy.gateway=just-a-name" >/dev/null 2>&1; then
  fail "a gateway without a namespace should be refused"
fi

# apps.pathPrefix has to reach the server, or every app would be given a host of
# its own while the gateway served them under a path.
prefixed="$(render --set "apps.pathPrefix=/apps")"
grep -q 'APPLAB_PATH_PREFIX: "/apps"' <<<"$prefixed" \
  || fail "apps.pathPrefix does not reach the server; apps would be routed by subdomain"
# A prefix with no domain cannot route: the prefix is the only thing telling one
# app from another on a shared host, so every app would be unreachable.
if helm template applab "$CHART" --namespace "$NS" \
  --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" \
  --set "apps.pathPrefix=/apps" >/dev/null 2>&1; then
  fail "a path prefix without a base domain should be refused"
fi

helm lint "$CHART" "${BASE[@]}" >/dev/null || fail "helm lint reported a problem"

# The console is reachable, and the way in flips with the Ingress.
#
# A cluster with no ingress controller needs another front door, or the console
# is reachable only by port-forward — which is what the debugger environment did
# before, with a proxy in front of it. The three cases below are the whole
# decision, and the third is the one that must not silently ship: a base domain
# with no Ingress and no gateway is an installation nobody can open.
console_vs() { render "$@" | python3 -c '
import sys, yaml
for doc in yaml.safe_load_all(sys.stdin):
    if doc and doc.get("kind") == "VirtualService" and doc["metadata"]["name"].endswith("-console"):
        print("found")
        break
'; }

# With an Ingress there is no gateway route for the console: two front doors to
# one Service is one more than the release needs. BASE enables the Ingress.
if [ "$(console_vs)" = "found" ]; then
  fail "the console VirtualService is rendered even though an Ingress is enabled"
fi

# Without one it is rendered, and on the host the apps share and through the
# same gateway, so a request for "/" reaches applab rather than the gateway's
# own 404 handler.
if [ "$(console_vs --set ingress.enabled=false)" != "found" ]; then
  fail "the console is unreachable without an Ingress: no VirtualService is rendered for it"
fi

# Who serves it, on which host, through which gateway.
#
# It has to be a catch-all, and that is what makes it safe rather than lazy: the
# apps' routes ("/<prefix>/<app>/") are the more specific ones and Istio sorts a
# catch-all to the end of the virtual host, so the console is evaluated only
# after every app has declined the request. A match of its own could claim an
# app's path; a destination other than the release's Service would answer 503.
render --set ingress.enabled=false > "$RUNTIME_DIR/gatewayed.yaml"
python3 - "$RUNTIME_DIR/gatewayed.yaml" <<'PY'
import sys, yaml

with open(sys.argv[1]) as fh:
    docs = [d for d in yaml.safe_load_all(fh) if d]

console = [d for d in docs if d.get("kind") == "VirtualService"
           and d["metadata"]["name"].endswith("-console")]
if len(console) != 1:
    print(f"expected exactly one console VirtualService, found {len(console)}", file=sys.stderr)
    sys.exit(1)
vs = console[0]
hosts = vs["spec"]["hosts"]
gateways = vs["spec"]["gateways"]

if hosts != ["apps.example.com"]:
    print(f"the console is served on {hosts}, not the apps base domain", file=sys.stderr)
    sys.exit(1)
if gateways != ["ops-system/gateway"]:
    print(f"the console is attached to {gateways}, not the apps gateway", file=sys.stderr)
    sys.exit(1)

http = vs["spec"]["http"]
if len(http) != 1:
    print(f"the console has {len(http)} http routes, want one", file=sys.stderr)
    sys.exit(1)
if "match" in http[0]:
    print("the console route carries a match; it must be a catch-all so it can never claim an app path", file=sys.stderr)
    sys.exit(1)
dest = http[0]["route"][0]["destination"]
if dest["host"] != "applab" or dest["port"]["number"] != 80:
    print(f"the console routes to {dest}, not the release Service on its port", file=sys.stderr)
    sys.exit(1)
PY

# With no base domain there is no host to put the console on, so nothing is
# rendered for it: a VirtualService with an empty host is one Istio cannot
# match. The release is internal-only instead, which the notes explain, and it
# still has to be a working Deployment rather than a render failure.
internal="$(render --set ingress.enabled=false --set apps.baseDomain=)"
grep -q 'kind: Deployment' <<<"$internal" \
  || fail "an Ingress-less internal release does not render a Deployment"
if console_vs --set ingress.enabled=false --set apps.baseDomain= | grep -q found; then
  fail "the console VirtualService is rendered with no base domain, so it would have no host to match"
fi

# A host set on its own gets the root path.
#
# `--set ingress.hosts[0].host=example.com` reads like setting one field and is
# not: --set replaces the whole list element, so the paths values.yaml puts under
# it are gone. That spelling is what the README's quick start teaches, so the
# chart has to make it work — an Ingress rule with no paths is rejected by the API
# server, and the caller who asked for a hostname would get an error naming a
# field they never mentioned.
#
# Asserted as the object it produces rather than as "it rendered": rendering
# succeeded before this was defaulted too, with `paths: null` in it.
render --set "ingress.hosts[0].host=only.example.com" > "$RUNTIME_DIR/hostonly.yaml"
render \
  --set "ingress.hosts[0].host=h.example.com" \
  --set "ingress.hosts[0].paths[0].path=/admin" \
  --set "ingress.hosts[0].paths[0].pathType=Exact" > "$RUNTIME_DIR/explicit.yaml"
python3 - "$RUNTIME_DIR/hostonly.yaml" "$RUNTIME_DIR/explicit.yaml" <<'PY'
import sys, yaml

def ingress(path):
    with open(path) as fh:
        docs = [d for d in yaml.safe_load_all(fh) if d]
    found = [d for d in docs if d.get("kind") == "Ingress"]
    if len(found) != 1:
        print(f"{path}: expected one Ingress, found {len(found)}", file=sys.stderr)
        sys.exit(1)
    return found[0]

rules = ingress(sys.argv[1])["spec"]["rules"]
if len(rules) != 1:
    print(f"expected one rule, found {len(rules)}", file=sys.stderr)
    sys.exit(1)
if rules[0]["host"] != "only.example.com":
    print(f"host is {rules[0]['host']}, not the one that was set", file=sys.stderr)
    sys.exit(1)

paths = rules[0]["http"]["paths"]
if not paths:
    print("a host set on its own produced no paths; the API server would reject this Ingress", file=sys.stderr)
    sys.exit(1)
if paths[0]["path"] != "/" or paths[0]["pathType"] != "Prefix":
    print(f"the defaulted path is {paths[0]['path']}/{paths[0]['pathType']}, want / + Prefix", file=sys.stderr)
    sys.exit(1)
if paths[0]["backend"]["service"]["name"] != "applab":
    print(f"the defaulted path points at {paths[0]['backend']['service']['name']}, not the release's Service", file=sys.stderr)
    sys.exit(1)

# The paths an explicit --set supplies still win over the default.
explicit = ingress(sys.argv[2])["spec"]["rules"][0]["http"]["paths"][0]
if explicit["path"] != "/admin" or explicit["pathType"] != "Exact":
    print(f"an explicit path was overridden by the default: {explicit}", file=sys.stderr)
    sys.exit(1)
PY

# NOT NOTES.txt: `helm template` does not render it, and `helm install --dry-run`
# needs a reachable cluster, so its contents cannot be checked here. Asserting on
# it without a cluster means asserting on the empty output of a failed command,
# which passes whatever the template says.

# .env.example is the documented reference for every setting, and the README
# points at it as such. A variable that exists but is undocumented is one nobody
# will find; this is how the build and deploy settings went missing from it.
want_env="$(grep -o 'APPLAB_[A-Z_]*' internal/config/config.go | sort -u)"
have_env="$(grep -o 'APPLAB_[A-Z_]*' .env.example | sort -u)"
# APPLAB_KEYS is documented in .env.example's auth block by name rather than as
# an assignment, since APPLAB_KEY is the form the file uses.
undocumented="$(comm -23 <(echo "$want_env") <(printf '%s\nAPPLAB_KEYS\n' "$have_env") || true)"
if [ -n "$undocumented" ]; then
  fail ".env.example does not document: $(tr '\n' ' ' <<<"$undocumented")"
fi

echo "chart checks passed"
