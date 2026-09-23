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
must_fail "no gateway"        --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" --set "apps.baseDomain=a.example.com"
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

# The namespace defaults to ops-system, and the prefix model it replaced is gone.
grep -q 'APPLAB_NAMESPACE: "ops-system"' <<<"$out" || fail "the namespace is not ops-system"
if grep -q 'APPLAB_NAMESPACE_PREFIX' <<<"$out"; then
  fail "APPLAB_NAMESPACE_PREFIX is still set; the per-app namespace model is gone"
fi

# applab's own image is pulled always: a re-pushed tag must not be served from a
# node's cache.
grep -q 'imagePullPolicy: Always' <<<"$out" || fail "applab's own image is not pulled always"

# A base domain with no gateway is a deployment where every app is unreachable
# from outside, so it has to be refused rather than rendered.
if helm template applab "$CHART" --namespace "$NS" \
  --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" \
  --set "apps.baseDomain=apps.example.com" >/dev/null 2>&1; then
  fail "a base domain without a gateway should be refused"
fi
# And a gateway that is not namespace/name would not resolve.
if helm template applab "$CHART" --namespace "$NS" \
  --set "auth.keys[0]=k" --set "build.registry=r.example.com/a" \
  --set "apps.baseDomain=apps.example.com" --set "deploy.gateway=just-a-name" >/dev/null 2>&1; then
  fail "a gateway without a namespace should be refused"
fi

helm lint "$CHART" "${BASE[@]}" >/dev/null || fail "helm lint reported a problem"

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
