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
  --set "auth.key=test-key-do-not-use"
  --set "ingress.host=apps.example.com"
  --set "deploy.gateway=$NS/gateway"
  --set "build.registry=registry.example.com/apps"
  --set "build.secret=regcred"
  # A bucket is required now, and the reason is worth stating where it is
  # exercised: without one the server falls back to ./data/objects, which in
  # this chart is an emptyDir — so a deployment that omitted it would render,
  # install, run, and lose every app on the next restart. The guard below is
  # what refuses it; this is what a correct install looks like.
  --set "objectStore.endpoint=http://minio.ops-system:9000"
  --set "objectStore.bucket=applab"
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
grep -q 'APPLAB_BUILD_SECRET: "regcred"' <<<"$out" \
  || fail "build.secret is not passed to the server; builds cannot push and apps cannot pull"

# There is one registry credential, not two. A deploy.imagePullSecret would be a
# second name for the same Secret, which every install set to the same value —
# the shape that lets the two disagree. The variable is gone entirely, and so is
# the name it had; either coming back means the split is coming back with it.
if grep -qE 'APPLAB_(DEPLOY_IMAGE_PULL_SECRET|BUILD_PUSH_SECRET)' <<<"$out"; then
  fail "a second registry-credential setting is rendered again; there is one Secret"
fi
grep -q 'APPLAB_BUILD_TTL_AFTER_FINISHED: "24h"' <<<"$out" \
  || fail "build.ttlAfterFinished is not passed to the server"

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
# the registry and the kaniko image, so both have to be emptied; blanking only
# the registry would leave it enabled against the default image.
#
# The push Secret goes with them. It is the registry's credential, and it has a
# non-empty default, so leaving it set on a deployment with no registry would send
# the deployer looking for a Secret nothing created — and since the deploy path
# refuses a credential it cannot find, every deploy on that installation would
# fail for a credential it never meant to use.
off="$(render --set build.enabled=false)"
for var in APPLAB_BUILD_REGISTRY APPLAB_BUILD_KANIKO_IMAGE APPLAB_BUILD_SECRET; do
  grep -q "^  $var: \"\"" <<<"$off" \
    || fail "build.enabled=false leaves $var set, so the build pipeline still comes up"
done

# A malformed configuration must be refused at render time rather than deployed
# and discovered later. Each of these has a guard in _helpers.tpl.
# Every value that is not the one under test is supplied, so each call reaches
# the guard it names. Without that, adding a guard for a later setting turns an
# earlier check into a test of the new guard — it still fails, so it still looks
# like it is working, and the defect it was written for goes unnoticed.
OK=(--set "auth.key=k" --set "build.registry=r.example.com/a"
    --set "objectStore.endpoint=http://minio:9000" --set "objectStore.bucket=applab")
must_fail() {
  local desc="$1"; shift
  if helm template applab "$CHART" --namespace "$NS" "$@" >/dev/null 2>&1; then
    fail "expected a render failure: $desc"
  fi
}
must_fail "no API keys"       --set ingress.host=a.example.com "${OK[@]}" --set "auth.key="
must_fail "no registry"       --set ingress.host=a.example.com --set "deploy.gateway=$NS/gateway" "${OK[@]}" --set "build.registry="
must_fail "bad gateway"       --set "ingress.host=a.example.com" --set "deploy.gateway=nope" "${OK[@]}"
# deploy.gateway now has a default, so "unset" no longer produces an empty one;
# these two blank it explicitly to reach the guard.
must_fail "blanked gateway"   --set "ingress.host=a.example.com" --set "deploy.gateway=" "${OK[@]}"
# A bucket is what the whole deployment is stored in, and the fallback when it
# is unset is a directory in the pod's emptyDir — so this one is not a broken
# deployment, it is one that works and then loses everything.
must_fail "no bucket"         --set "auth.key=k" --set "build.registry=r.example.com/a" --set "objectStore.endpoint=http://minio:9000" --set "objectStore.bucket="
must_fail "no endpoint"       --set "auth.key=k" --set "build.registry=r.example.com/a" --set "objectStore.bucket=applab"
# The credential's Secret does not carry the address, so it does not stand in
# for one — reaching for it instead of an endpoint is the mistake this catches.
must_fail "existingSecret without an endpoint" --set "auth.key=k" --set "build.registry=r.example.com/a" --set "objectStore.existingSecret=my-bucket"
# More than one replica is allowed now — AppLab holds nothing on a replica, so
# there is no volume to detach and nothing to corrupt. The check is the other
# way round: it must render, and it must render without a claim.
scaled="$(render --set replicaCount=3)"
grep -qE '^  replicas: 3$' <<<"$scaled" \
  || fail "replicaCount did not reach the Deployment"
grep -q 'persistentVolumeClaim' <<<"$scaled" \
  && fail "the chart still claims a volume; AppLab keeps nothing on a replica"
grep -qE '^kind: PersistentVolumeClaim$' <<<"$scaled" \
  && fail "the chart still renders a PersistentVolumeClaim"

# One key is the whole auth surface a release carries, so the value has to reach
# the Secret the server reads — and only there.
keyed="$(render)"
grep -q 'APPLAB_KEYS: "test-key-do-not-use"' <<<"$keyed" \
  || fail "auth.key does not reach the Secret's APPLAB_KEYS"
[[ "$(grep -c 'APPLAB_KEYS' <<<"$keyed")" == 1 ]] \
  || fail "APPLAB_KEYS appears more than once; the key is written somewhere it should not be"

# An existing Secret replaces this chart's entirely, and the Deployment has to
# point at it — that path is the one that carries more than one key.
#
# The two Secrets are separate — the API keys and the bucket's credential have
# different lifetimes — so setting one to an existing Secret must suppress that
# one and only that one.
existing="$(render --set auth.existingSecret=my-keys --set auth.key=leak-canary-2f9a)"
grep -q 'name: my-keys' <<<"$existing" \
  || fail "auth.existingSecret does not reach the Deployment"
grep -qE '^  name: .*-auth$' <<<"$existing" \
  && fail "auth.existingSecret is set but the chart still renders its own auth Secret"
if grep -q 'leak-canary-2f9a' <<<"$existing"; then
  fail "auth.key is written into the release even though auth.existingSecret is set"
fi

# And the bucket's credential, the same way.
bucket="$(render --set objectStore.existingSecret=my-bucket --set objectStore.secretKey=leak-canary-3c1b)"
grep -q 'name: my-bucket' <<<"$bucket" \
  || fail "objectStore.existingSecret does not reach the Deployment"
grep -qE '^  name: .*-objectstore$' <<<"$bucket" \
  && fail "objectStore.existingSecret is set but the chart still renders its own Secret"
if grep -q 'leak-canary-3c1b' <<<"$bucket"; then
  fail "objectStore.secretKey is written into the release even though objectStore.existingSecret is set"
fi

# Every manifest's top-level keys have to be ones Kubernetes knows. Text emitted
# outside a YAML structure — a warning written as bare prose, say — becomes a
# field of the document, and the manifest still parses as YAML, so the mistake
# survives every check that only asks whether the output is valid.
if command -v python3 >/dev/null 2>&1; then
  render | python3 -c '
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

# A build runs the Dockerfile of whoever pushed the source, as root — kaniko has
# no unprivileged mode. That is the one thing an operator cannot learn from the
# values file without reading the engine, so the Deployment says it. It is a YAML
# comment, so it reaches the rendered manifest rather than stderr.
build_note="$(render 2>/dev/null)"
grep -q 'builds run as root' <<<"$build_note" \
  || fail "build.enabled=true does not say that builds run as root"
# And it must not appear when the pipeline is off, where there is nothing to warn
# about — a note that is always there is one nobody reads.
if render --set build.enabled=false 2>/dev/null | grep -q 'builds run as root'; then
  fail "the build-runs-as-root note appears with build.enabled=false"
fi

# The server's low port has to be one the container may bind.
#
# AppLab listens on 80 by default, which is the port it is reached at — so the
# Service, the container port and APPLAB_LISTEN all agree. The container runs as
# uid 1000, and since Linux 5.7 a bind below 1024 is refused unless the pod's own
# network namespace lowers ip_unprivileged_port_start. Without that the bind
# fails at startup and the failure is a CrashLoopBackOff whose reason is
# "permission denied" rather than anything naming this value.
#
# The capability route is checked *against*, because it is the obvious fix and it
# does not work: securityContext.capabilities.add reaches only the bounding set
# for a non-root container, so NET_BIND_SERVICE is present and grants nothing —
# the kernel recomputes permitted and effective at execve, and a non-root process
# with no file capabilities gets an empty effective set. Ambient capabilities
# would work and Kubernetes cannot express them. An installation that granted the
# capability bound 80 and exited with "bind: permission denied", which is why
# this asserts the sysctl and refuses the capability.
listen_port="$(render --set auth.key=x | grep -m1 'APPLAB_LISTEN' | sed 's/.*:\([0-9]*\)".*/\1/')"
if [ "$listen_port" = "80" ]; then
  sysobj="$(render --set auth.key=x | python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
deploy = [d for d in docs if d.get("kind") == "Deployment"]
if not deploy:
    print("none")
    sys.exit(0)
pod = deploy[0]["spec"]["template"]["spec"]
sysctls = pod.get("securityContext", {}).get("sysctls") or []
hit = [s for s in sysctls if s.get("name") == "net.ipv4.ip_unprivileged_port_start"]
if not hit:
    print("missing")
elif str(hit[0].get("value")) != "0":
    print("value:" + str(hit[0].get("value")))
else:
    print("ok")
')"
  case "$sysobj" in
    ok) ;;
    *) fail "AppLab listens on 80 but the pod does not set net.ipv4.ip_unprivileged_port_start=0 (got: $sysobj), so an unprivileged bind to 80 is refused and the container crash-loops with 'permission denied'" ;;
  esac

  # And the capability is not silently offered as the answer instead. Adding it
  # back would read as a fix in the rendered manifest while doing nothing at
  # runtime, which is a worse state than either working arrangement.
  if render --set auth.key=x | grep -A6 'capabilities:' | grep -q 'NET_BIND_SERVICE'; then
    fail "the container is granted NET_BIND_SERVICE for its port 80 bind, which does not work for a non-root container: the capability reaches only the bounding set, so the bind still fails. Remove it — podSecurityContext.sysctls is what allows the low port"
  fi
  grep -q 'drop:' <<<"$(render --set auth.key=x | grep -A4 'capabilities:')" \
    || fail "the container does not drop capabilities; an applab container needs none of them"
fi

# The uninstall cleanup has to be there, and has to be a pre-delete hook.
#
# `helm uninstall` removes what the chart created; every app and every build Job
# was created by applab at runtime, so without this they outlive the installation
# — still serving, with an app key in each build pod's environment.
#
# Pre-delete rather than post-delete is forced: the hook runs with the release's
# ServiceAccount, and its Role is an ordinary resource of the release, so a
# post-delete hook would start with no permission to delete anything.
hook="$(render)"
if ! grep -q '"helm.sh/hook": pre-delete' <<<"$hook"; then
  fail "the uninstall cleanup is not a pre-delete hook; a post-delete one would run after the release's own Role was deleted and could remove nothing"
fi
grep -q 'name: applab-cleanup' <<<"$hook" \
  || fail "no cleanup Job is rendered, so uninstalling leaves every app and build Job behind"
# The hook deletes itself when it succeeds and is kept when it fails — the log of
# a failed cleanup is the only place its reason appears.
grep -q 'helm.sh/hook-delete-policy": "before-hook-creation,hook-succeeded"' <<<"$hook" \
  || fail "the cleanup Job is not deleted after a successful uninstall, or a failed one is not kept for its log"
# It runs the applab image, because applab is what knows which label it put on
# what. A kubectl pipeline would be a second copy of that knowledge.
grep -q '"applab-cleanup"' <<<"$hook" || true
grep -q 'args: \["cleanup"\]' <<<"$hook" \
  || fail "the cleanup hook does not run the cleanup subcommand"
if render --set cleanup.onUninstall=false | grep -q 'name: applab-cleanup'; then
  fail "cleanup.onUninstall=false still renders the cleanup Job"
fi

# The labels the cleanup selects on are spelled in three places — the Go
# constants, the chart's helper, and this check — and a sweep that used a label
# nothing carries would delete nothing while reporting success.
grep -q 'applab.io/app' internal/k8s/client.go \
  || fail "the app label constant is gone; the cleanup's selector would match nothing"
grep -q 'applab.io/build' internal/k8s/client.go \
  || fail "the build label constant is gone; the build-job sweep would match nothing"
if grep -q 'applab.io/app' <<<"$(render --set apps.pathPrefix= --set ingress.host=)"; then
  fail "a chart object carries applab.io/app; the cleanup sweep would delete it as though it were an app"
fi

# AppLab's own pods have to carry the label the server finds them by.
#
# The platform log endpoint lists pods with observe.selfSelector and reads one's
# log. That selector looks for app.kubernetes.io/part-of, which the chart puts on
# applab.labels — and applab.labels is what objects get, not what the pod
# template gets: a pod carries selectorLabels alone. So the selector matched
# nothing, and the panel answered every request with "read applab's log" and no
# cause. The Go test passed because it labelled its own fake pod with the value
# the selector wanted, which is exactly the shape of test that cannot catch this.
#
# Cross-checked against the Go constant and the rendered pod template, so the two
# are compared to each other rather than to a copy of one of them here.
self_selector="$(sed -n 's/^const selfSelector = "\(.*\)"$/\1/p' internal/observe/observe.go)"
[ -n "$self_selector" ] \
  || fail "observe.selfSelector is gone; this check has nothing to compare against"
selector_key="${self_selector%%=*}"
selector_val="${self_selector#*=}"
if ! render | python3 -c '
import sys, yaml
want_key, want_val = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
deploys = [d for d in docs if d.get("kind") == "Deployment"]
if not deploys:
    print("no Deployment rendered", file=sys.stderr)
    sys.exit(1)
labels = deploys[0]["spec"]["template"]["metadata"]["labels"]
if labels.get(want_key) != want_val:
    print(f"pod template labels are {labels}, which do not carry {want_key}={want_val}", file=sys.stderr)
    sys.exit(1)
' "$selector_key" "$selector_val"; then
  fail "AppLab's pods do not carry $self_selector, which is what observe.selfSelector lists them by — the platform log endpoint would find no pods and report every request as a failure"
fi

# RBAC is a Role, not a ClusterRole: AppLab keeps everything in one namespace,
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
# an Ingress AppLab created would be ignored by it.
grep -q 'APPLAB_DEPLOY_GATEWAY: "ops-system/gateway"' <<<"$out" \
  || fail "deploy.gateway does not reach the server; apps would have no route"

# The default gateway has to reach the server too. A default that stops in
# values.yaml and never renders is a deployment where every app is unreachable,
# discovered from a browser rather than at install. Rendered without setting
# deploy.gateway at all, which is the only way to observe the chart's default —
# `--set deploy.gateway=` blanks it rather than restoring it.
defaulted="$(helm template applab "$CHART" --namespace "$NS" \
  --set "auth.key=k" --set "build.registry=r.example.com/a" \
  --set "ingress.host=apps.example.com" \
  --set "objectStore.endpoint=http://minio:9000" --set "objectStore.bucket=applab")"
grep -q 'APPLAB_DEPLOY_GATEWAY: "istio-ingress/istio-ingress"' <<<"$defaulted" \
  || fail "the default deploy.gateway does not reach the server"

# The namespace defaults to ops-system, and the prefix model it replaced is gone.
grep -q 'APPLAB_NAMESPACE: "ops-system"' <<<"$out" || fail "the namespace is not ops-system"
if grep -q 'APPLAB_NAMESPACE_PREFIX' <<<"$out"; then
  fail "APPLAB_NAMESPACE_PREFIX is still set; the per-app namespace model is gone"
fi

# AppLab's own image is pulled always: a re-pushed tag must not be served from a
# node's cache.
grep -q 'imagePullPolicy: Always' <<<"$out" || fail "applab's own image is not pulled always"

# The default image tag has to be one an image is actually published under.
#
# This check is the one that was missing. The chart defaulted an unset
# image.tag to appVersion while the image was published under a chart version
# and a `sha-`-prefixed commit, so the two never met: every install that did not
# override the tag pulled a tag that does not exist, and the chart's own README
# taught exactly that install.
#
# It has to be checked on a *packaged* chart rather than the working copy.
# Chart.yaml has version and appVersion both at 0.1.0, so rendering the source
# produces the same string under either spelling and the assertion would pass
# while the bug was present — which is precisely why the bug survived. It is
# packaging that separates them, so packaging is what this renders.
#
# The version used here is the one CI would publish for this commit, and the
# app-version is a stand-in for the commit: divergent, which is the shape that
# ships.
if command -v helm >/dev/null 2>&1; then
  packaged="$RUNTIME_DIR/packaged"
  mkdir -p "$packaged"
  helm package "$CHART" --version "$(./hack/chart-version.sh)" \
    --app-version deadbeef --destination "$packaged" >/dev/null

  published="$(./hack/chart-version.sh)"
  pkg_tag="$(helm template applab "$packaged"/applab-*.tgz --namespace "$NS" \
    --set "auth.key=k" --set "build.registry=r.example.com/a" \
    --set "objectStore.endpoint=http://minio:9000" --set "objectStore.bucket=applab" 2>/dev/null \
    | awk '/^ *image: /{gsub(/"/, "", $2); split($2, a, ":"); print a[2]; exit}')"

  if [ "$pkg_tag" != "$published" ]; then
    fail "a packaged chart's default image tag is '$pkg_tag' but this commit publishes '$published'; an install that does not set image.tag would pull a tag nothing publishes"
  fi

  # And an explicit tag still wins, or the default would be the only
  # installable one.
  tagged="$(render --set image.tag=v9.9.9)"
  grep -q 'image: "docker.io/shaowenchen/applab:v9.9.9"' <<<"$tagged" \
    || fail "--set image.tag=... does not override the chart's own version"
fi

# A base domain with no gateway is a deployment where every app is unreachable
# from outside, so it has to be refused rather than rendered. That case is
# covered by `must_fail "blanked gateway"` above: deploy.gateway has a default
# now, so leaving it unset is no longer the way to reach the guard.

# A gateway that is not namespace/name would not resolve.
if helm template applab "$CHART" --namespace "$NS" \
  --set "auth.key=k" --set "build.registry=r.example.com/a" \
  --set "objectStore.endpoint=http://minio:9000" --set "objectStore.bucket=applab" \
  --set "ingress.host=apps.example.com" --set "deploy.gateway=just-a-name" >/dev/null 2>&1; then
  fail "a gateway without a namespace should be refused"
fi

# apps.pathPrefix has to reach the server, or every app would be given a host of
# its own while the gateway served them under a path.
#
# With ingress.enabled=false, because a prefix and an Ingress are alternatives:
# an app on a prefix is nested under ingress.path, and an Ingress routes that
# whole path to applab itself, so the app would be deployed and unreachable.
# That combination is refused outright, and the check for it is further down.
prefixed="$(render --set "apps.pathPrefix=/apps" --set ingress.enabled=false --set "deploy.gateway=istio-system/gw")"
grep -q 'APPLAB_PATH_PREFIX: "/apps"' <<<"$prefixed" \
  || fail "apps.pathPrefix does not reach the server; apps would be routed by subdomain"
# And a prefix with an Ingress is refused rather than rendered into an app that
# nothing can reach.
if helm template applab "$CHART" "${BASE[@]}" --set "apps.pathPrefix=/apps" >/dev/null 2>&1; then
  fail "apps.pathPrefix with an Ingress should be refused: the Ingress routes the app's own path to applab"
fi
# A prefix with no host cannot route: the prefix is the only thing telling one
# app from another on a shared host, so every app would be unreachable.
#
# The host has to be blanked explicitly. It defaults to a placeholder, because
# the Ingress needs one to render at all — so a release only has no host when
# someone sets it to nothing, which is the documented way to run internal-only.
if helm template applab "$CHART" --namespace "$NS" \
  --set "auth.key=k" --set "build.registry=r.example.com/a" \
  --set "objectStore.endpoint=http://minio:9000" --set "objectStore.bucket=applab" \
  --set "ingress.enabled=false" --set "ingress.host=" --set "apps.pathPrefix=/apps" >/dev/null 2>&1; then
  fail "a path prefix without a host should be refused"
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
# same gateway, so a request for "/" reaches AppLab rather than the gateway's
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
internal="$(render --set ingress.enabled=false --set ingress.host=)"
grep -q 'kind: Deployment' <<<"$internal" \
  || fail "an Ingress-less internal release does not render a Deployment"
if console_vs --set ingress.enabled=false --set ingress.host= | grep -q found; then
  fail "the console VirtualService is rendered with no base domain, so it would have no host to match"
fi

# The Ingress is one host and one path, as the object it produces — and the
# server is told the same path.
#
# Asserted rather than inferred from a successful render: an Ingress whose rule
# had no paths renders fine and is rejected by the API server, which is how this
# was found.
#
# The pairing is the point. An Ingress routes on a path but cannot strip one, so
# the server has to expect the prefix; a chart that set one without the other
# would produce an Ingress that looks right and a deployment that answers 404 to
# every request.
render > "$RUNTIME_DIR/ingress.yaml"
python3 - "$RUNTIME_DIR/ingress.yaml" <<'PY'
import sys, yaml

with open(sys.argv[1]) as fh:
    docs = [d for d in yaml.safe_load_all(fh) if d]
ingresses = [d for d in docs if d.get("kind") == "Ingress"]
if len(ingresses) != 1:
    print(f"expected one Ingress, found {len(ingresses)}", file=sys.stderr)
    sys.exit(1)

rules = ingresses[0]["spec"]["rules"]
if len(rules) != 1:
    print(f"expected one rule, found {len(rules)}", file=sys.stderr)
    sys.exit(1)
if rules[0]["host"] != "apps.example.com":
    print(f"host is {rules[0]['host']}, not the one the release was given", file=sys.stderr)
    sys.exit(1)

paths = rules[0]["http"]["paths"]
if len(paths) != 1:
    print(f"expected one path, found {len(paths)}: {paths}", file=sys.stderr)
    sys.exit(1)
p = paths[0]
if p["path"] != "/applab" or p["pathType"] != "Prefix":
    print(f"the path is {p['path']}/{p['pathType']}, want /applab + Prefix", file=sys.stderr)
    sys.exit(1)
if p["backend"]["service"]["name"] != "applab":
    print(f"the path points at {p['backend']['service']['name']}, not the release's Service", file=sys.stderr)
    sys.exit(1)
if p["backend"]["service"]["port"]["number"] != 80:
    print(f"the path points at port {p['backend']['service']['port']['number']}, not the Service's", file=sys.stderr)
    sys.exit(1)

# And the server is told, or the Ingress above serves nothing.
configmaps = [d for d in docs if d.get("kind") == "ConfigMap"]
if len(configmaps) != 1:
    print(f"expected one ConfigMap, found {len(configmaps)}", file=sys.stderr)
    sys.exit(1)
seen = configmaps[0].get("data", {}).get("APPLAB_BASE_PATH")
if seen != p["path"]:
    print(f"APPLAB_BASE_PATH is {seen!r} but the Ingress path is {p['path']!r}; the server would expect a different prefix than the Ingress sends, and every request would 404", file=sys.stderr)
    sys.exit(1)
PY

# Setting the path moves all three together: the Ingress route, the prefix the
# server expects, and the address in the URL it hands out.
setpath="$(render --set "ingress.path=/platform")"
grep -q 'path: "/platform"' <<<"$setpath" || fail "--set ingress.path=... does not set the Ingress path"
grep -q 'APPLAB_BASE_PATH: "/platform"' <<<"$setpath" \
  || fail "--set ingress.path=... does not reach the server's base_path"
grep -q "APPLAB_BASE_URL: \"http://applab.$NS.svc:80/platform\"" <<<"$setpath" \
  || fail "--set ingress.path=... does not reach the address a build clones from, so every build would ask for a path the server does not serve"

# "/" is the root, and the server must be told nothing rather than "/".
rootpath="$(render --set "ingress.path=/")"
grep -q 'APPLAB_BASE_PATH: ""' <<<"$rootpath" \
  || fail "ingress.path=/ does not clear the server's base_path; the server would look for every route under //"
grep -q "APPLAB_BASE_URL: \"http://applab.$NS.svc:80\"" <<<"$rootpath" \
  || fail "ingress.path=/ leaves a path on the address a build clones from"

# An explicit apps.baseURL is the operator saying what the whole address is, so
# nothing is appended to it — including this chart's own idea of the prefix.
seturl="$(render --set "apps.baseURL=https://applab.example.com" --set "ingress.path=/platform")"
grep -q 'APPLAB_BASE_URL: "https://applab.example.com"' <<<"$seturl" \
  || fail "an explicit apps.baseURL had the Ingress path appended to it"

# The base path applies with the gateway as well as with an Ingress.
#
# It used to be returned only when ingress.enabled was true, on the reasoning
# that the path was the Ingress's business. But the server serves the console,
# the API, git and the apps, and the apps are nested under this path — so an
# installation published through the gateway instead got a server at the root
# and apps written at "/apps/shop", a path the gateway never routes. The
# failure was an environment whose console worked and whose every app 404'd.
gatewayed="$(render --set ingress.enabled=false --set "deploy.gateway=istio-system/gw")"
grep -q 'APPLAB_BASE_PATH: "/applab"' <<<"$gatewayed" \
  || fail "the base path is not set when the console is served from the gateway; every app would be routed outside the prefix the gateway serves"
grep -q "APPLAB_BASE_URL: \"http://applab.$NS.svc:80/applab\"" <<<"$gatewayed" \
  || fail "the gateway deployment's clone address has no base path, so every build would ask for a path the server does not serve"

# The host is one value, so `--set ingress.host=...` is the whole of it — the
# spelling the README teaches, and the reason `hosts` stopped being a list.
sethost="$(render --set "ingress.host=only.example.com")"
grep -q 'host: "only.example.com"' <<<"$sethost" \
  || fail "--set ingress.host=... does not set the Ingress host"

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
