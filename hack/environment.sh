#!/usr/bin/env bash
#
# Start a complete AppLab test environment on this machine and publish it.
#
# This is what the debugger/ action runs. It builds a throwaway Kubernetes
# cluster, an object store, an Istio gateway and an AppLab installation —
# everything a single app needs before `applab push` can build, deploy and serve
# it. The point is that none of it has to be assembled by hand first.
#
# What comes up:
#
#   kind cluster (ns ops-system)
#     AppLab         the published image, installed with this repository's chart
#     istio-ingress  the gateway everything is published through (NodePort 30080)
#
#   on the runner
#     minio          the object store AppLab keeps everything in
#     cloudflared    a tunnel, so the environment is reachable from anywhere
#
# Built images do not come up here: they are pushed to a real registry, which is
# the point — the push and the pull are both exercised, credential and all. See
# APPLAB_REGISTRY.
#
# One gateway serves both halves, and that is the whole of the routing: the
# console and the API at "/" (a VirtualService the chart installs), each app
# under "/apps/<app>/" (a VirtualService AppLab writes at deploy time). Nothing
# sits in front of the gateway to tell the two apart, because the paths already
# do: Istio sorts a virtual host's catch-all route to the end and keeps the rest
# in order, and "/apps/<app>/" is not a prefix any of the console's own paths
# share. See debugger/README.md.
#
# The order below is load-bearing and the reason it is a script rather than a
# list of workflow steps. The hostname has to be settled first, because it
# becomes ingress.host and that cannot be set after AppLab is installed — but
# settling it is not the same as starting a tunnel, and the tunnel only has to be
# started early when it is the one thing that knows the name. Otherwise it comes
# up last, once there is something behind it to publish.

# -E so the ERR trap below also fires for a failure inside a function. Without
# it the trap only sees failures at the top level, which is not where they
# happen: every step of this script is a function call or a subshell.
set -Eeuo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)

# ── inputs ──────────────────────────────────────────────────────────────────

# The published `latest`, which is what the release workflow produces alongside
# the version tags. A moving tag, deliberately: the point of this environment is
# to run the newest AppLab, and a version read from the chart would pin it to a
# release that has to exist first — which is the failure that put this here, since
# the chart's appVersion had never been published at all.
#
# The chart pulls with imagePullPolicy: Always, so a moving tag is re-resolved on
# every start rather than being served from a node's cache.
: "${APPLAB_VERSION:=latest}"

: "${APPLAB_API_KEY:=}"
: "${APPLAB_SESSION_HOURS:=0}"
: "${APPLAB_TUNNEL:=cloudflare}"
: "${APPLAB_NAMESPACE:=ops-system}"
: "${APPLAB_GATEWAY_NODEPORT:=30080}"
: "${APPLAB_CLUSTER_NAME:=applab-debugger}"
# Where the apps are served, nested under the installation's own base path:
# with the defaults below, an app called "myshop" is at "/applab/apps/myshop/".
: "${APPLAB_PATH_PREFIX:=/apps}"
# The path the whole installation is served under. It is the chart's
# ingress.path, which applies whether or not an Ingress exists — this
# environment sets ingress.enabled=false and serves the console from the gateway
# instead, and the server still has to expect the prefix because the gateway
# route is written for it.
#
# Everything this environment touches moves with it: the console, the API, git
# and the apps. That is why it is one variable here rather than a literal in the
# half-dozen places that build a URL.
: "${APPLAB_BASE_PATH:=/applab}"
: "${APPLAB_IMAGE_REPOSITORY:=docker.io/shaowenchen/applab}"
# How long AppLab's first rollout may take before the environment gives up. The
# script waits for the Deployment itself rather than letting helm block on it, so
# the deadline is here and it is one number rather than two that can drift.
#
# Generous, because the first pull of an image on a cold runner is genuinely
# slow. Raise it on a host that is slower than that; the failure names this
# variable, so there is nothing to guess.
: "${APPLAB_INSTALL_TIMEOUT_SECONDS:=300}"
# Where the apps this environment builds have their images pushed.
#
# A real registry rather than the cluster-local one this used to start. A local
# registry is faster and needs no credential, but it is also the one arrangement
# that exercises none of what a deployment actually does: an image that only ever
# travels inside one machine cannot fail to pull, and the pull half of the
# pipeline is exactly what a private registry and its credential are for.
#
# The three shapes build.registry can take are all one string, and applab reads
# which is meant from it — see imageRef. This is the third: a repository that
# already carries a tag, so apps are pushed under "<repo>:demo-<app>-<commit>".
# The demo- prefix is what keeps this environment's apps from colliding with the
# image tags the release workflow publishes to the same repository.
#
# Requires DOCKERHUB_USERNAME and DOCKERHUB_TOKEN in the environment. The token
# needs write access, because the build pushes; the same credential is what the
# nodes pull with, which is why it is created as a Secret below rather than only
# being handed to the build.
: "${APPLAB_REGISTRY:=shaowenchen/applab:demo}"
: "${APPLAB_REGISTRY_USERNAME:=${DOCKERHUB_USERNAME:-}}"
: "${APPLAB_REGISTRY_PASSWORD:=${DOCKERHUB_TOKEN:-}}"
# The Secret both halves read: the build Job mounts it to push, and every app's
# Deployment names it to pull. One registry, one credential, one name.
: "${APPLAB_REGISTRY_SECRET:=applab-registry}"

# The object store, in the same shape and for the same reason as the registry:
# a container on this host that the cluster reaches by name.
#
# The credential is generated here rather than fixed. It is a throwaway — the
# container is destroyed with the runner — and generating it means no credential
# is ever written into this repository, which is the rule the seed script's key
# follows too.
: "${APPLAB_OBJECT_STORE_NAME:=applab-object-store}"
: "${APPLAB_OBJECT_STORE_PORT:=9000}"
: "${APPLAB_OBJECT_STORE_BUCKET:=applab}"
: "${APPLAB_OBJECT_STORE_ENDPOINT:=${APPLAB_OBJECT_STORE_NAME}:${APPLAB_OBJECT_STORE_PORT}}"
: "${APPLAB_OBJECT_STORE_ACCESS_KEY:=applab}"
: "${APPLAB_OBJECT_STORE_SECRET_KEY:=$(openssl rand -hex 16)}"

# The object store's images, pinned to what bitnami/minio 17.0.21 declares — the
# chart's `annotations.images`, which is the list it validates its own defaults
# against. Pinned for the same reason kind and istio are: a moving tag would
# change this environment's behaviour without this repository changing.
#
# The repository is `bitnamilegacy` rather than `bitnami`. Bitnami moved its free
# images out of `bitnami/` during 2025 and the old paths now resolve to nothing —
# `bitnami/minio` has no tags at all — so the tag the chart names is only
# pullable from the legacy namespace. It is still the same image the chart
# selects, which is the point of naming the version rather than a tag alone.
#
# Both halves are here because both are needed: the server, and `mc` for creating
# the bucket. `mc` is not a dependency of the server image, so it is a second
# container that runs once and exits.
: "${APPLAB_OBJECT_STORE_IMAGE:=bitnamilegacy/minio:2025.7.23-debian-12-r3}"
: "${APPLAB_OBJECT_STORE_CLIENT_IMAGE:=bitnamilegacy/minio-client:2025.7.21-debian-12-r2}"

: "${CLOUDFLARE_TOKEN:=}"
: "${NGROK_TOKEN:=}"

# Set so `kind` and `kubectl` are found however this script is invoked; the
# action installs them into /usr/local/bin.
export PATH="/usr/local/bin:$PATH"

log()  { printf '\n\033[1;34m[AppLab-debugger]\033[0m %s\n' "$*"; }
warn() { printf '\n\033[1;33m[AppLab-debugger]\033[0m %s\n' "$*" >&2; }
die()  { printf '\n\033[1;31m[AppLab-debugger]\033[0m %s\n' "$*" >&2; exit 1; }

# show runs a query and prints it indented under a label.
#
# Every step that installs something is followed by this, rather than everything
# being dumped once at the end: when a component comes up wrong, the objects it
# made are what says so, and having them next to the step that produced them is
# what makes a log readable.
#
# A failure is printed rather than fatal. A query for something that is not there
# yet is a fact about the install, and stopping on it would hide the component's
# own output behind a shell error.
show() {
  local label="$1"; shift
  printf '\n\033[1;34m[AppLab-debugger]\033[0m %s\n' "$label"
  "$@" 2>&1 | sed 's/^/  /' || true
}

# A command that fails under `set -e` stops the script with no message at all,
# which is the worst way for a long install to end: the log simply stops, and
# where it stopped is the only clue. This says which command failed, on which
# line, and with what status.
#
# It is a diagnostic, not error handling — nothing is recovered from — so it
# reports and lets the non-zero status stand.
trap 'status=$?; printf "\n\033[1;31m[AppLab-debugger]\033[0m %s failed (exit %d)\n" "$BASH_COMMAND" "$status" >&2' ERR

RUNTIME_DIR="${APPLAB_RUNTIME_DIR:-$PWD/.applab-debugger}"
mkdir -p "$RUNTIME_DIR"
PUBLIC_URL_FILE="$RUNTIME_DIR/url.txt"
TUNNEL_LOG="$RUNTIME_DIR/tunnel.log"
: > "$PUBLIC_URL_FILE"

# ── 1. the API key ──────────────────────────────────────────────────────────

# Generated when not supplied, and printed at the end. It is deliberately not
# masked: it is the deliverable, and a masked value could not be shown in the
# summary that exists to show it.
if [ -z "$APPLAB_API_KEY" ]; then
  APPLAB_API_KEY=$(openssl rand -hex 32)
fi

# ── 2. the hostname, before anything that needs it ──────────────────────────

tunnel_pid=""

open_tunnel() {
  case "$APPLAB_TUNNEL" in
    cloudflare) open_cloudflare_tunnel ;;
    ngrok)      open_ngrok_tunnel ;;
    *)          die "unknown APPLAB_TUNNEL '$APPLAB_TUNNEL'; expected 'cloudflare' or 'ngrok'" ;;
  esac

  # The agent's output is mirrored into the job log: when a tunnel fails, its own
  # words are the only thing that explains why, and a log nobody prints hides
  # exactly that. `-u` because the job log is not a tty and sed would
  # block-buffer, so the output would arrive in bursts instead of as it happens.
  for _ in $(seq 1 50); do [ -f "$TUNNEL_LOG" ] && break; sleep 0.1; done
  tail -f "$TUNNEL_LOG" 2>/dev/null | sed -u "s/^/[${APPLAB_TUNNEL}] /" &
  echo $! > "$RUNTIME_DIR/tail.pid"

  # A usage or credential error makes an agent exit instantly, and without this
  # check the only symptom is a missing link minutes later.
  sleep 3
  kill -0 "$tunnel_pid" 2>/dev/null || {
    sed 's/^/    /' "$TUNNEL_LOG" 2>/dev/null || true
    die "the ${APPLAB_TUNNEL} agent exited during startup; its output is above"
  }
}

# find_public_host polls the agent for the hostname it was given.
#
# Both agents expose it at a local API, and both are read from there rather than
# parsed out of a log, because a log format is not a contract and an API is.
# Cloudflared's metrics port is not pinned when the agent starts, so a taken port
# makes it step to the next one — hence the short scan before giving up.
find_public_host() {
  local urls=()
  case "$APPLAB_TUNNEL" in
    cloudflare)
      for p in 20241 20242 20243 20244 20245; do urls+=("http://127.0.0.1:$p/quicktunnel"); done
      ;;
    ngrok)
      urls=("http://127.0.0.1:4040/api/tunnels")
      ;;
  esac

  for attempt in $(seq 1 90); do
    for u in "${urls[@]}"; do
      local body
      body=$(curl -s --max-time 5 "$u" 2>/dev/null || true)
      [ -n "$body" ] || continue
      case "$APPLAB_TUNNEL" in
        cloudflare)
          local host
          host=$(printf '%s' "$body" | grep -oE '"hostname":"[^"]+"' | head -1 | cut -d'"' -f4 || true)
          [ -n "$host" ] && { printf '%s' "$host"; return 0; }
          ;;
        ngrok)
          local url
          url=$(printf '%s' "$body" | grep -oE '"public_url":"https://[^"]+"' | head -1 | cut -d'"' -f4 || true)
          [ -n "$url" ] && { printf '%s' "$url"; return 0; }
          ;;
      esac
    done

    # A quick tunnel's hostname also appears in the agent's own log, which is the
    # fallback when its metrics endpoint is unreachable.
    local from_log
    case "$APPLAB_TUNNEL" in
      cloudflare) from_log=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$TUNNEL_LOG" 2>/dev/null | head -1 || true) ;;
      ngrok)      from_log=$(grep -oE 'https://[a-z0-9-]+\.ngrok-free\.app' "$TUNNEL_LOG" 2>/dev/null | head -1 || true) ;;
    esac
    [ -n "$from_log" ] && { printf '%s' "$from_log"; return 0; }

    kill -0 "$tunnel_pid" 2>/dev/null || return 1
    if [ $((attempt % 15)) -eq 0 ]; then log "  still waiting for the tunnel... ${attempt}s"; fi
    sleep 2
  done
  return 1
}

# resolve_host settles on the hostname the environment is served under.
#
# It does not start anything. The hostname has to be known before AppLab is
# installed, because it becomes ingress.host — but knowing it is not the same
# as publishing it, and for every case except a quick tunnel the name is settled
# without a tunnel running at all. The agent is started later, by publish().
#
# APPLAB_PUBLIC_HOST skips the tunnel entirely and uses the given hostname. It
# exists for CI, which must not depend on a public tunnel being granted: a quick
# tunnel's hostname is minted per connection, is rate-limited, and is aimed at
# trying things rather than at being a test dependency. CI sets it and drives the
# gateway over the node port; the tunnel itself is exercised by the debugger
# workflow, where a flaky link is a person's problem to re-run rather than a red
# build.
#
# APPLAB_DOMAIN names the domain apps are served under. It is not tunnel
# configuration — a named Cloudflare tunnel keeps its hostname in its ingress, and
# the connector is never told it, and nothing has to be passed to cloudflared. It
# is what AppLab needs: ingress.host, which is both where the apps are served
# and the host the console's own route matches.
resolve_host() {
  # The one case with nothing to publish: the caller has a name, and there is no
  # tunnel to start. Recorded here and returned to by publish(), which does
  # nothing when APPLAB_PUBLIC_HOST is set.
  if [ -n "${APPLAB_PUBLIC_HOST:-}" ]; then
    TUNNEL_HOST="$APPLAB_PUBLIC_HOST"
    public_url="http://${TUNNEL_HOST}${APPLAB_BASE_PATH}"
    log "using the supplied hostname ${TUNNEL_HOST}; no tunnel will be started"
    return 0
  fi

  # A domain can only be named for a tunnel whose ingress was configured in
  # advance, which a quick tunnel's is not: Cloudflare assigns it a random name
  # and it cannot be given another, so a domain chosen here would not resolve.
  # The ngrok path does not pass the flag a reserved domain needs either. Both
  # are refused rather than silently ignored, which would publish a link to a
  # domain that serves nothing.
  if [ -n "${APPLAB_DOMAIN:-}" ]; then
    case "$APPLAB_TUNNEL" in
      cloudflare)
        [ -n "$CLOUDFLARE_TOKEN" ] \
          || die "APPLAB_DOMAIN needs CLOUDFLARE_TOKEN: a quick tunnel is assigned a random hostname by Cloudflare, so apps could not be served under the one given here"
        ;;
      *)
        die "APPLAB_DOMAIN is only supported with a named Cloudflare tunnel, not '${APPLAB_TUNNEL}'"
        ;;
    esac

    # Taken as given rather than discovered, because a named tunnel's hostname is
    # not discoverable — see above. The tunnel's ingress has to already point
    # here; nothing in this script can create or check it.
    TUNNEL_HOST="$APPLAB_DOMAIN"
    public_url="https://${TUNNEL_HOST}${APPLAB_BASE_PATH}"
    log "the environment will be served at ${public_url}"
    log "  (the tunnel's ingress is configured in Cloudflare, not here)"
    return 0
  fi

  # A quick tunnel, or ngrok: the name is whatever the agent is given, and the
  # only way to learn it is to start the agent and ask. So this is the one path
  # that has to run early — and it does, from here, before AppLab is installed.
  log "starting ${APPLAB_TUNNEL} to find out which hostname it will be given"
  open_tunnel

  log "waiting for the tunnel to report its public hostname"
  local found
  if ! found=$(find_public_host); then
    # Reached only by a named tunnel with no domain given: nothing else can
    # fail to report one. Say what to do about it, because the symptom is
    # otherwise an environment that looks fine and a link that never appears.
    if [ -n "$CLOUDFLARE_TOKEN" ]; then
      warn "this is a named tunnel: Cloudflare does not tell the connector its own"
      warn "hostname, so it cannot be discovered here. Name the domain instead:"
      warn "  APPLAB_DOMAIN=<your domain> ... hack/environment.sh"
    fi
    die "the tunnel never reported a public hostname; see ${TUNNEL_LOG}"
  fi

  TUNNEL_HOST="${found#https://}"
  TUNNEL_HOST="${TUNNEL_HOST#http://}"
  TUNNEL_HOST="${TUNNEL_HOST%%/*}"
  public_url="https://${TUNNEL_HOST}${APPLAB_BASE_PATH}"
  log "the environment will be published at ${public_url}"
}

# publish starts the tunnel, if one is needed and is not already up.
#
# It runs last, after the environment answers, so a tunnel that fails does so
# with the cluster already proven good — the failure is then the tunnel's alone
# and cannot be mistaken for the platform not coming up. resolve_host has already
# started one for the only case that needed the name in advance (a quick tunnel
# or ngrok), and this returns immediately then.
publish() {
  printf '%s\n' "$public_url" > "$PUBLIC_URL_FILE"

  if [ -n "${APPLAB_PUBLIC_HOST:-}" ]; then
    return 0
  fi
  if [ -n "$tunnel_pid" ]; then
    # Already up, because its hostname was what we had to wait for.
    return 0
  fi

  log "opening the ${APPLAB_TUNNEL} tunnel"
  open_tunnel
}

open_cloudflare_tunnel() {
  if [ -n "$CLOUDFLARE_TOKEN" ]; then
    log "opening a Cloudflare named tunnel"
    cloudflared tunnel --no-autoupdate run --token "$CLOUDFLARE_TOKEN" >"$TUNNEL_LOG" 2>&1 &
  else
    log "opening a Cloudflare quick tunnel (no account needed)"
    cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}" >"$TUNNEL_LOG" 2>&1 &
  fi
  tunnel_pid=$!
}

open_ngrok_tunnel() {
  [ -n "$NGROK_TOKEN" ] || die "APPLAB_TUNNEL=ngrok needs NGROK_TOKEN"
  log "opening an ngrok tunnel"
  ngrok config add-authtoken "$NGROK_TOKEN" >"$TUNNEL_LOG" 2>&1 || die "ngrok rejected the authtoken"
  ngrok http "$APPLAB_GATEWAY_NODEPORT" >>"$TUNNEL_LOG" 2>&1 &
  tunnel_pid=$!
}

resolve_host

# ── 3. cluster, registry, gateway ───────────────────────────────────────────

log "creating the kind cluster"

# The containerd patch is what makes the registry usable from the nodes: without
# a mirror entry, node containerd tries to speak HTTPS to a registry that serves
# plain HTTP and every app's image pull fails with an x509 error.
cat > "$RUNTIME_DIR/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${APPLAB_CLUSTER_NAME}
nodes:
  - role: control-plane
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: "ingress-ready=true"
    extraPortMappings:
      # The Istio ingress gateway, so the tunnel — and anything on this runner —
      # can reach it.
      #
      # One mapping is enough, and the port in it does not leak into routing: a
      # request to "http://host:30080/path" carries "Host: host:30080", but Istio
      # sets IgnorePortInHostMatching on the gateway's route configuration
      # (pilot/pkg/networking/core/gateway.go), so Envoy drops the port before
      # matching and the bare hostnames the VirtualServices carry are what match.
      - containerPort: ${APPLAB_GATEWAY_NODEPORT}
        hostPort: ${APPLAB_GATEWAY_NODEPORT}
        protocol: TCP
EOF

kind create cluster --config "$RUNTIME_DIR/kind.yaml" --wait 120s

show "the cluster" kubectl get nodes -o wide

# An object store, for the same reason and in the same shape as the registry: a
# container on this host, joined to the kind network, so AppLab reaches it by
# name from inside the cluster.
#
# AppLab keeps everything it persists in a bucket — the apps, their history and
# their source repositories — and holds nothing on a replica. There is no volume
# in the release at all, which is the point of the change: this environment used
# to prove that a PersistentVolume survived a restart, and now proves the
# opposite.
#
# The image is the one bitnami/minio 17.0.21 declares, so this runs what the
# chart would run if the object store were an object in the cluster rather than a
# container here. Three things about that image shape the invocation below, and
# all three are the chart's own settings rather than choices made here:
#
#   - its data directory is /bitnami/minio/data, not /data, and that is what
#     MINIO_DATA_DIR names. The chart sets it to its persistence.mountPath, which
#     defaults to the same path, so this is the chart's value written out.
#   - it runs as uid 1001, not root. The image declares /bitnami/minio/data as a
#     volume, so docker initialises that directory with the image's own ownership
#     and the server can write to it. A bind mount in its place would be
#     root-owned and would fail on the first write.
#   - MINIO_SKIP_CLIENT is what the chart sets when there are no default buckets,
#     which is this case: the bucket is created below, and the image's own client
#     step would be a second thing trying to do it. It also keeps the server from
#     needing $HOME/.mc, which it cannot write with HOME=/ and uid 1001.
#
# No command or arguments are passed, which is deliberate: the chart passes none
# either, so the image's own entrypoint and run script are what starts the
# server. Handing it `server /data` — what the previous image took — would
# replace that entrypoint's default command with something it cannot exec.
log "starting the object store at ${APPLAB_OBJECT_STORE_ENDPOINT}"
if [ "$(docker inspect -f '{{.State.Running}}' "$APPLAB_OBJECT_STORE_NAME" 2>/dev/null || echo false)" != "true" ]; then
  docker run -d --restart=always -p "127.0.0.1:${APPLAB_OBJECT_STORE_PORT}:9000" \
    --name "$APPLAB_OBJECT_STORE_NAME" \
    -e "MINIO_ROOT_USER=${APPLAB_OBJECT_STORE_ACCESS_KEY}" \
    -e "MINIO_ROOT_PASSWORD=${APPLAB_OBJECT_STORE_SECRET_KEY}" \
    -e "MINIO_DATA_DIR=/bitnami/minio/data" \
    -e "MINIO_SKIP_CLIENT=yes" \
    "$APPLAB_OBJECT_STORE_IMAGE" >/dev/null
fi
docker network connect "kind" "$APPLAB_OBJECT_STORE_NAME" 2>/dev/null || true

# The bucket has to exist before AppLab starts. AppLab does not create one: a
# bucket is infrastructure, its name and region and lifecycle policy belong to
# whoever runs the platform, and a service that made one on its own would make it
# in whatever region it happened to be configured with.
#
# `mc` is MinIO's own client, run from this host against the published port, so
# it is one throwaway container rather than a dependency of the server image.
# The client image is the one the chart declares beside the server's, and it is
# driven the way the chart drives it — `mc alias set`, then `mc mb` with
# --ignore-existing — so what creates the bucket here is what would create it in
# a cluster.
#
# HOME is set to /tmp because the image ships HOME=/ and `mc` keeps its config in
# $HOME/.mc. The chart mounts /.mc as a writable volume for exactly that reason;
# with no volume here, / is root-owned and uid 1001 cannot write to it, so `mc`
# would fail before reaching the server. /tmp is world-writable, and nothing
# outside this one-shot container reads what it writes.
log "creating the bucket ${APPLAB_OBJECT_STORE_BUCKET}"
for _ in $(seq 1 30); do
  if docker run --rm --network "container:${APPLAB_OBJECT_STORE_NAME}" \
      -e "HOME=/tmp" \
      --entrypoint /bin/bash \
      "$APPLAB_OBJECT_STORE_CLIENT_IMAGE" -c \
      "mc alias set local http://127.0.0.1:9000 '${APPLAB_OBJECT_STORE_ACCESS_KEY}' '${APPLAB_OBJECT_STORE_SECRET_KEY}' && mc mb --ignore-existing local/${APPLAB_OBJECT_STORE_BUCKET}" \
      >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

show "the object store (a container on this host)" \
  docker inspect --format '{{.State.Status}} {{.Config.Image}}' "$APPLAB_OBJECT_STORE_NAME"

log "installing Istio (this is the slow step)"
# The community default profile, into the community default namespace, producing
# the community default gateway name. Nothing is overridden for kind, because
# nothing needs to be: Istio's platform profiles carry only CNI paths, and kind
# uses the standard containerd layout, so there is no profile for it — passing
# `--set values.global.platform=kind` does not select a kind-specific profile,
# it fails the render with "unknown platform kind".
#
# The gateway's Service is patched afterwards rather than configured here:
# istioctl's `components.ingressGateways[0].k8s.service.ports` is a positional
# list whose first entry is the *status* port (15021), not HTTP, so addressing
# the HTTP port by index is a bug waiting for an Istio release that reorders it.
# See the patch below, which selects the port by number instead.
istioctl install --set profile=default -y

kubectl -n istio-system wait --for=condition=available --timeout=300s \
  deployment/istio-ingressgateway

# The `Gateway` resource, which istioctl deliberately does not install.
#
# `istioctl install` produces the Deployment, the Service and the RBAC, and
# stops: a Gateway resource is the user's to write, because it is what says which
# ports and hosts this proxy serves. Without one, every VirtualService in this
# environment names a gateway that does not exist.
#
# That is not a formality. A Gateway's `selector` is the only thing binding a
# route to the ingress proxy's pods — it is how istiod knows which Envoy gets the
# listener and the routes. A VirtualService referencing a Gateway that is not
# there has nothing to be programmed into, so the proxy has no route for the host
# and answers 404. The symptom is a request through the gateway that fails while
# the AppLab pod behind it is healthy and serving.
#
# The chart does not create this and should not: the gateway is cluster
# infrastructure the installation attaches to, and `charts/applab/README.md` says
# so. This environment assembles that infrastructure, so it belongs here.
#
# The selector is read from the deployment rather than written as the customary
# `istio: ingressgateway`, because it has to match whatever Istio actually
# installed and that is the definition of the match. The jsonpath returns a JSON
# object, which is also valid YAML flow-mapping syntax, so it interpolates
# straight into the manifest below.
gateway_selector=$(kubectl -n istio-system get deployment istio-ingressgateway \
  -o jsonpath='{.spec.selector.matchLabels}')
[ -n "$gateway_selector" ] \
  || die "the ingress gateway deployment has no selector, so a Gateway resource cannot be bound to it"

log "creating the Gateway resource istioctl does not install"

# Port 80 and every host, because that is what this environment serves: the
# tunnel terminates TLS at Cloudflare and forwards plain HTTP to the node port, so
# the gateway side is HTTP only. The host list is `*` rather than the tunnel's
# hostname because a quick tunnel's hostname is not known until the agent reports
# it, and the VirtualServices that name specific hosts match against a `*` server
# either way.
kubectl apply -f - <<EOF
apiVersion: networking.istio.io/v1
kind: Gateway
metadata:
  name: istio-ingressgateway
  namespace: istio-system
spec:
  selector: ${gateway_selector}
  servers:
    - port:
        number: 80
        name: http
        protocol: HTTP
      hosts:
        - "*"
EOF

# What Istio installed, and what was added to it. istiod is what programs every
# VirtualService in the environment, so it is worth seeing alongside the gateway:
# a control plane that is not running looks exactly like a VirtualService that
# does not route.
#
# The Gateway is listed because it is the thing this script had to add — see
# above. A VirtualService is inert without it.
show "istio (the control plane, the gateway deployment and the Gateway)" \
  kubectl -n istio-system get deployment,service,gateway.networking.istio.io

# Expose the gateway's HTTP port on the node port kind already maps to the host.
#
# The Service is a LoadBalancer by default, which never gets an address on kind,
# so it has to become a NodePort for anything outside the cluster to reach it.
#
# `--type=strategic` is required, not stylistic. The gateway's port list carries
# `patchMergeKey: port`, which is a *strategic* merge instruction — a plain JSON
# merge patch (`--type=merge`) ignores it and replaces the whole list. That would
# delete the status port (15021) and HTTPS (443) along with their names, leaving a
# gateway that cannot report its own health and drops TLS. It would also survive
# the check below, since port 80's nodePort is correct either way.
log "exposing the gateway on node port ${APPLAB_GATEWAY_NODEPORT}"
kubectl -n istio-system patch svc istio-ingressgateway --type=strategic -p "$(cat <<EOF
spec:
  type: NodePort
  ports:
    - port: 80
      nodePort: ${APPLAB_GATEWAY_NODEPORT}
EOF
)"

# Confirm the port took, rather than assuming the merge matched: a patch that
# silently changes nothing leaves a gateway nothing can reach, and the symptom
# would be a 404 from a deploy ten minutes later.
gateway_nodeport=$(kubectl -n istio-system get svc istio-ingressgateway \
  -o jsonpath='{.spec.ports[?(@.port==80)].nodePort}')
[ "$gateway_nodeport" = "$APPLAB_GATEWAY_NODEPORT" ] \
  || die "the gateway's HTTP port is on node port '${gateway_nodeport}', expected ${APPLAB_GATEWAY_NODEPORT}"

# And confirm the other ports survived. The check above passes even when the
# patch replaced the whole list, because port 80 is the one it looks at — so the
# two ports that would be lost silently are asserted separately. 15021 is the
# gateway's own health endpoint and 443 is what serves TLS through it.
gateway_status_port=$(kubectl -n istio-system get svc istio-ingressgateway \
  -o jsonpath='{.spec.ports[?(@.name=="status-port")].port}')
gateway_https_port=$(kubectl -n istio-system get svc istio-ingressgateway \
  -o jsonpath='{.spec.ports[?(@.name=="https")].port}')
[ "$gateway_status_port" = "15021" ] \
  || die "the gateway's status port is '${gateway_status_port}', expected 15021 — the port list was replaced rather than merged"
[ "$gateway_https_port" = "443" ] \
  || die "the gateway's HTTPS port is '${gateway_https_port}', expected 443 — the port list was replaced rather than merged"

show "the gateway's ports after the patch" \
  kubectl -n istio-system get svc istio-ingressgateway -o wide

# ── 4. AppLab ───────────────────────────────────────────────────────────────

log "installing AppLab in namespace ${APPLAB_NAMESPACE}"

kubectl create namespace "$APPLAB_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The registry credential, which both halves of the build pipeline read: the
# build Job mounts it to push, and every app's Deployment names it to pull. One
# registry, one credential, one name — see build.secret in the chart.
#
# Created here rather than by the chart because the chart carries no registry
# credential: it would have to be a value, and a value is plain text in the
# release's history and in this repository. The environment supplies it.
#
# The server is the registry's host, which is what a docker-registry Secret
# stores: Docker Hub's own address, or whatever the value names. A tag on the
# registry is part of the image path and not part of its address, so it is not
# what goes here.
# The first segment is the host only if it reads like one — the same rule
# applab applies to the value itself (see pathSegments): a dot, a colon, or
# "localhost". A bare account name is Docker Hub, whose Secret server is
# Docker's own fixed address rather than the account.
registry_host="$(printf '%s' "$APPLAB_REGISTRY" | cut -d/ -f1)"
case "$registry_host" in
  *.*|*:*|localhost) ;;
  *) registry_host="https://index.docker.io/v1/" ;;
esac
kubectl -n "$APPLAB_NAMESPACE" create secret docker-registry "$APPLAB_REGISTRY_SECRET" \
  --docker-server="$registry_host" \
  --docker-username="$APPLAB_REGISTRY_USERNAME" \
  --docker-password="$APPLAB_REGISTRY_PASSWORD" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The key goes in through a Secret rather than --set auth.key=..., which
# would write it into the release's stored values where anyone with read on the
# namespace can recover it.
kubectl -n "$APPLAB_NAMESPACE" create secret generic applab-keys \
  --from-literal=APPLAB_KEYS="$APPLAB_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# install, not upgrade: this script assumes a fresh cluster, and an upgrade
# against a half-installed release would hide a first-install failure.
#
# No --wait. It is the obvious flag and the wrong one here: with it helm blocks
# silently until every object is ready, so an image that cannot be pulled looks
# exactly like a slow start — ten minutes of nothing, then a timeout that says
# what it waited for and not why. Without it the install returns as soon as the
# objects exist, and the waiting is done below, where it can be bounded, printed
# and diagnosed.
#
# ingress.enabled=false, because a kind cluster has no ingress controller and
# installing one would be a moving part added for nothing. The chart then
# publishes the console through the Istio gateway instead — one VirtualService on
# the same host the apps are already served on, so the whole environment is
# reachable through the one address the tunnel publishes.
#
# build.secret names the Secret created above, which is what makes the build
# mount it to push and every app's Deployment reference it to pull. One
# credential for both halves, which is what the registry expects: the same token
# authenticates a push and a pull.
#
# insecureRegistry is left at its default of false. Docker Hub is reached over
# TLS, and a credential sent in clear text is the one thing that flag turns on —
# it is for a cluster-local registry serving plain HTTP, which this no longer is.
#
# ingress.path is set explicitly rather than left to the chart's default,
# because this script builds URLs from it: the console, the API and every app
# live under this path, and a chart default that drifted would leave the printed
# links pointing at nothing.
if ! helm install applab "$REPO_ROOT/charts/applab" \
  --namespace "$APPLAB_NAMESPACE" \
  --set auth.existingSecret=applab-keys \
  --set "ingress.host=${TUNNEL_HOST}" \
  --set "ingress.path=${APPLAB_BASE_PATH}" \
  --set "apps.pathPrefix=${APPLAB_PATH_PREFIX}" \
  --set deploy.gateway=istio-system/istio-ingressgateway \
  --set "build.registry=${APPLAB_REGISTRY}" \
  --set "build.secret=${APPLAB_REGISTRY_SECRET}" \
  --set ingress.enabled=false \
  --set "image.repository=${APPLAB_IMAGE_REPOSITORY}" \
  --set "image.tag=${APPLAB_VERSION}" \
  --set "objectStore.endpoint=http://${APPLAB_OBJECT_STORE_ENDPOINT}" \
  --set "objectStore.bucket=${APPLAB_OBJECT_STORE_BUCKET}" \
  --set "objectStore.accessKey=${APPLAB_OBJECT_STORE_ACCESS_KEY}" \
  --set "objectStore.secretKey=${APPLAB_OBJECT_STORE_SECRET_KEY}" \
  --set objectStore.pathStyle=true \
  --set objectStore.insecure=true
then
  warn "AppLab could not be installed at all; the state it left behind follows"
  kubectl -n "$APPLAB_NAMESPACE" get pods,deployment,replicaset,service 2>&1 | sed 's/^/    /' || true
  helm -n "$APPLAB_NAMESPACE" status applab 2>&1 | sed 's/^/    /' || true
  die "helm rejected the release: the message above is helm's, the rest is the cluster's"
fi

# The rollout, waited for here rather than by helm.
#
# The deadline is generous, because the first pull of an image on a cold runner
# is genuinely slow. What matters is that it is bounded and that the wait is
# visible: the pod's own state is printed as it changes, so a stuck install shows
# ImagePullBackOff or a CrashLoopBackOff in the log while it is stuck, rather
# than ten minutes later as a timeout.
#
# `rollout status` is not used for the same reason `--wait` is not: it blocks
# silently and reports a condition, where the interesting thing is the reason.
# It is only polled with a one-second timeout to ask whether the wait is over.
applab_wait_seconds="$APPLAB_INSTALL_TIMEOUT_SECONDS"
log "waiting up to ${applab_wait_seconds}s for applab to roll out"
last_state=""
rolled_out=""
crash_grabbed=""
for attempt in $(seq 1 "$applab_wait_seconds"); do
  # A single line per pod, in a stable order, so the same state does not print
  # every second: only a change is worth a line.
  state=$(kubectl -n "$APPLAB_NAMESPACE" get pods \
    -o 'custom-columns=NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,STATUS:.status.phase,REASON:.status.containerStatuses[*].state.waiting.reason' \
    --no-headers 2>/dev/null | sort || true)
  if [ "$state" != "$last_state" ]; then
    printf '%s\n' "$state" | sed 's/^/    /'
    last_state="$state"
  fi

  # Some states will not resolve by waiting. A container that cannot start — a
  # crash loop, an image that cannot be pulled — is already failing, and the
  # remaining minutes add nothing but a delay before the same answer. Grab the
  # container's own last words while it is still there to ask, and stop.
  #
  # ImagePullBackOff is excluded deliberately: a first pull on a cold runner is
  # slow, and pulling is not failing. It is ErrImagePull's settled form and it
  # does eventually resolve.
  case "$state" in
    *CrashLoopBackOff*|*CreateContainerConfigError*|*RunContainerError*|*InvalidImageName*)
      crash_grabbed="yes" ;;
  esac
  [ -z "$crash_grabbed" ] || break

  if kubectl -n "$APPLAB_NAMESPACE" rollout status deploy/applab --timeout=1s >/dev/null 2>&1; then
    rolled_out="yes"
    break
  fi
  sleep 1
done

if [ -z "$rolled_out" ]; then
  if [ -n "$crash_grabbed" ]; then
    warn "AppLab cannot start — its container is not coming up, and waiting would not change that"
  else
    warn "AppLab did not become ready within ${applab_wait_seconds}s; the state it is in follows"
  fi
  kubectl -n "$APPLAB_NAMESPACE" get pods,deployment,replicaset,service 2>&1 | sed 's/^/    /' || true
  # The reason a container cannot start is in these, and they are the things a
  # person would otherwise have to guess at: an image that cannot be pulled, a
  # volume that cannot be mounted, an AppLab that started and refused its own
  # configuration. The log is what the process itself said before it died, which
  # is the one thing no amount of waiting produces.
  kubectl -n "$APPLAB_NAMESPACE" describe pods 2>&1 | tail -n 60 | sed 's/^/    /' || true
  kubectl -n "$APPLAB_NAMESPACE" logs deploy/applab --all-containers --tail=100 2>&1 | sed 's/^/    /' || true
  # --previous, because a crash loop's current container may have produced
  # nothing yet — the reason is in the attempt that already died.
  kubectl -n "$APPLAB_NAMESPACE" logs deploy/applab --all-containers --previous --tail=100 2>&1 | sed 's/^/    /' || true
  if [ -n "$crash_grabbed" ]; then
    die "AppLab exited on startup: its output is above. Nothing about waiting changes this"
  fi
  die "AppLab never became ready — if this is a slow first pull, raise APPLAB_INSTALL_TIMEOUT_SECONDS"
fi

# Ready is what the Deployment reports. The console's route is a separate object
# that AppLab only has if the chart rendered it, and without it the gateway
# answers 404 at "/" — so it is checked here rather than discovered at a browser.
kubectl -n "$APPLAB_NAMESPACE" get virtualservice applab-console -o name >/dev/null 2>&1 \
  || die "the console has no VirtualService, so the gateway would answer 404 at \"/\": the chart rendered one only when ingress.enabled is false, and this install did not produce it"

# Everything the release made, right after it made it. The VirtualServices are
# listed with the rest rather than separately: one is the console's, from the
# chart, and an app's appears here too the moment something is deployed — which
# is exactly the object to look at when a deploy succeeds and nothing is
# reachable.
show "AppLab" \
  kubectl -n "$APPLAB_NAMESPACE" get deployment,replicaset,pod,service,secret
show "AppLab's routes (VirtualServices)" \
  kubectl -n "$APPLAB_NAMESPACE" get virtualservices

# Ask Istio's own analyzer whether the objects above can actually be programmed.
#
# This is the check whose absence let a whole class of failure through: a
# VirtualService naming a Gateway resource that does not exist is accepted by the
# API server, renders fine, passes every chart check, and then serves nothing —
# the objects all look correct and the proxy has no route. `istioctl analyze`
# reports exactly that by name, so the failure arrives as a sentence about the
# dangling reference rather than as a 404 minutes later.
#
# It runs before the requests below rather than after, because it answers a
# different question: those ask whether the environment serves, this asks whether
# the configuration is one that could ever serve. A configuration Istio cannot
# program is a bug whether or not the poll happens to pass.
#
# The exit status is what decides, plus the summary line Istio prints when it
# finds anything ("Analyzers found issues when analyzing namespace: ..."). Not the
# severity words in the body: the analyzer IDs and the tool's own verdict are the
# stable parts, and I have not confirmed which severities map to a non-zero exit,
# so the summary is matched as well rather than trusting the status alone.
#
# Both are checked because a missed issue here is silent: the poll further down
# would still catch a request that does not route, but it would catch it as a 404
# with nothing said about why — which is the failure this whole check exists to
# replace.
# Every namespace, not just AppLab's. The reference this check exists for crosses
# one — a VirtualService in the AppLab namespace naming a Gateway in
# istio-system — and the analyzer resolves references cluster-wide, so scoping it
# to one namespace risks missing the very case. The cost is that a problem
# anywhere fails the environment, which is the right trade on a kind cluster this
# script created moments ago and has nothing else in.
analyze_exit=0
analyze_output=$(istioctl analyze --all-namespaces 2>&1) || analyze_exit=$?

if [ "$analyze_exit" -ne 0 ] || printf '%s' "$analyze_output" | grep -q 'Analyzers found issues'; then
  printf '%s\n' "$analyze_output" | sed 's/^/    /' >&2
  die "istioctl analyze reports a configuration Istio cannot program; the detail above names what is wrong"
fi
show "istioctl analyze (the configuration is programmable)" \
  printf '%s\n' "$analyze_output"

# ── 5. what came up, and whether it answers ─────────────────────────────────

# Everything below reaches the gateway the way a browser does: over loopback on
# the node port, with the environment's own hostname as the Host header — which
# is what a request arriving through the tunnel carries.
#
# The port in that header is harmless. Istio sets IgnorePortInHostMatching on the
# gateway's route configuration (pilot/pkg/networking/core/gateway.go), so Envoy
# drops it before matching, and the bare hostnames the VirtualServices carry are
# what match.
gateway_code() {
  local path="$1"; shift
  # `|| true` so a refused connection reports as 000 rather than killing the
  # script: `set -e` sees curl's non-zero exit inside the command substitution
  # and stops with no message at all, which is the least useful way for a
  # gateway that is not listening to fail.
  curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Host: ${TUNNEL_HOST}" "$@" \
    "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}${path}" 2>/dev/null || true
}

# The gateway is the only thing the tunnel points at, so it is what has to
# answer: a ready Service behind an unprogrammed gateway is still an environment
# nobody can open.
log "waiting for the gateway to serve AppLab"
for attempt in $(seq 1 90); do
  if [ "$(gateway_code /health)" = "200" ]; then break; fi
  if [ $((attempt % 15)) -eq 0 ]; then log "  still waiting... (attempt ${attempt})"; fi
  sleep 2
done
if [ "$(gateway_code /health)" != "200" ]; then
  kubectl -n "$APPLAB_NAMESPACE" get pods
  kubectl -n "$APPLAB_NAMESPACE" logs deploy/applab --tail=50 2>/dev/null || true
  kubectl -n "$APPLAB_NAMESPACE" get virtualservices 2>/dev/null || true
  die "the gateway is not serving AppLab at /health (/health and /metrics are the two paths the cluster reaches the pod on; they are served at the root, outside the base path)"
fi

# Every endpoint a person or a client uses, and the status each one answers.
#
# Reported and asserted in one pass, because they are the same question: these
# are all served by one process behind one route, so anything but a 200 is a bug
# rather than a slow start, and a table of green is the evidence that the gateway,
# the two VirtualServices, the Service and the deployment all line up.
#
# The paths are written with the base path in front, except for the two the
# cluster itself uses. /health and /metrics are reached by the kubelet and by
# Prometheus on the container port, neither of which knows what path the
# deployment is published under — so the server answers them at the root and
# applies its prefix to everything else. Writing both kinds out here is what
# makes that exemption visible rather than a rule someone has to know.
log "the endpoints, through the gateway on ${APPLAB_GATEWAY_NODEPORT}"
printf '  %-32s %s\n' "PATH" "STATUS"

failed=""
check_endpoint() {
  local label="$1" path="$2"; shift 2
  local code
  code=$(gateway_code "$path" "$@")
  printf '  %-32s %s\n' "$label" "$code"
  [ "$code" = "200" ] || failed="${failed} ${label}=${code}"
}

# Open by design: a probe cannot hold a key, and the console is a page a browser
# fetches before anyone has signed in.
#
# /api/v1/describe is the one that matters most for an agent: it is where a
# caller with no key learns what this deployment is, and it serves the endpoint
# list. Asserting it here is what proves the front door opens — it is the route
# whose absence nothing else would catch, because everything else a client uses
# needs a key first.
check_endpoint "/health"                /health
check_endpoint "/metrics"               /metrics
check_endpoint "/ (the console)"        "${APPLAB_BASE_PATH}"
check_endpoint "${APPLAB_BASE_PATH}/api/v1/config"   "${APPLAB_BASE_PATH}/api/v1/config"
check_endpoint "${APPLAB_BASE_PATH}/api/v1/version"  "${APPLAB_BASE_PATH}/api/v1/version"
check_endpoint "${APPLAB_BASE_PATH}/api/v1/describe" "${APPLAB_BASE_PATH}/api/v1/describe"
# The one thing that proves the admin key works through the gateway, not only
# that the route exists.
check_endpoint "${APPLAB_BASE_PATH}/api/v1/overview (key)" "${APPLAB_BASE_PATH}/api/v1/overview" -H "Authorization: Bearer ${APPLAB_API_KEY}"
check_endpoint "${APPLAB_BASE_PATH}/api/v1/describe (key)" "${APPLAB_BASE_PATH}/api/v1/describe" -H "Authorization: Bearer ${APPLAB_API_KEY}"

# And the base path is a prefix, not an alias: the same service must not answer
# outside it. A server that ignored its own prefix would serve every route twice
# and shadow whatever else owns the root — which is the whole reason the setting
# exists.
root_code=$(gateway_code /api/v1/config)
printf '  %-32s %s\n' "/api/v1/config (outside the base path)" "$root_code"
[ "$root_code" != "200" ] || failed="${failed} the-api-answers-outside-its-base-path"

[ -z "$failed" ] || die "these did not answer as expected through the gateway:${failed}"

# The chart's settings have to reach the *server*, not only the render.
#
# This is the check whose absence let a whole class of failure through once
# already: the chart passed one thing, the server kept a default, every chart
# check passed, and the failure appeared only when someone pushed an app.
#
# Only what the API reports can be asserted from here, so these are the settings
# it does report: the address convention, which decides whether the console and
# the apps are reachable at all, and the build capability, which is what this
# environment now depends on a real registry for.
log "confirming the settings the chart passed actually arrived"
config_json=$(curl -s --max-time 5 -H "Host: ${TUNNEL_HOST}" \
  "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}${APPLAB_BASE_PATH}/api/v1/config" 2>/dev/null || true)

# jq is not assumed present on a runner; the raw JSON is matched instead.
config_problems=""
expect_config() {
  local label="$1" want="$2"
  printf '%s' "$config_json" | grep -qF -- "$want" \
    || config_problems="${config_problems} ${label}"
}

# The host and the path prefix are what this script sets, and a chart default
# surviving in either would put the apps somewhere nobody is looking.
expect_config "the-host" "${TUNNEL_HOST}"
[ -z "$APPLAB_PATH_PREFIX" ] || expect_config "the-path-prefix" "${APPLAB_PATH_PREFIX}"
expect_config "the-namespace" "${APPLAB_NAMESPACE}"

# The base path reaches the server too. Asserted against the address template
# rather than against a bare path, because that is where it is observable: with
# the two paths configured the template is "<host>/applab/apps/<app>", and a
# server that had taken the prefix but not the installation's own path — or the
# other way round — would publish an app at a path the gateway does not serve.
#
# Matched up to the app id and not through it. Go's JSON encoder escapes angle
# brackets into their unicode escape form, because that is what keeps a JSON
# document safe to embed in HTML — so the server reports the template correctly
# while the raw body holds something other than the characters the template was
# built from. A grep for the literal placeholder therefore matches nothing
# however right the deployment is, which is what this check did on its first run:
# it failed an environment whose every setting had arrived.
#
# The host and both paths are the part that can be compared, and they are also
# the part that distinguishes a server that took the base path from one that did
# not.
expect_config "the-base-path-in-the-address-template" "\"domain_template\":\"${TUNNEL_HOST}${APPLAB_BASE_PATH}${APPLAB_PATH_PREFIX}/"

# And the build pipeline has to be usable, since this environment sets a registry
# for it. A deployment whose build half silently came up disabled still serves
# every endpoint above, and fails only when someone pushes — which is exactly the
# class of failure this check exists to move forward.
printf '%s' "$config_json" | grep -q '"build":true' \
  || config_problems="${config_problems} the-build-pipeline-is-not-enabled"

# The credential has to be one the registry accepts, and that is a question only
# the registry can answer. Checked here rather than left to the first push,
# because a push is minutes of build before it fails, and the failure it would
# report — a denied push — names neither the Secret nor which of its fields is
# wrong. This is one request and it says so directly.
#
# It authenticates rather than pushing: a token with read access would pass this
# and fail the push, so it is not the whole answer, but it catches the failure
# that is actually likely — a token that is mistyped, expired, or for another
# account entirely.
log "confirming the registry credential is accepted"
if [ -n "$APPLAB_REGISTRY_USERNAME" ] && [ -n "$APPLAB_REGISTRY_PASSWORD" ]; then
  if ! printf '%s' "$APPLAB_REGISTRY_PASSWORD" \
    | docker login "$registry_host" --username "$APPLAB_REGISTRY_USERNAME" --password-stdin >/dev/null 2>&1; then
    config_problems="${config_problems} the-registry-credential-was-refused"
  fi
else
  config_problems="${config_problems} no-registry-credential-was-supplied"
fi

[ -z "$config_problems" ] || {
  printf '%s\n' "$config_json" | head -40 | sed 's/^/    /' >&2
  die "the deployment's own settings did not reach the server:${config_problems} — the values above are what it reports, and the chart's defaults are what it should not"
}

# ── 6. publish ──────────────────────────────────────────────────────────────

# The tunnel comes up now rather than at the start. Everything above is the
# cluster's own business and is already proven — the pods, the Service, both
# VirtualServices and every endpoint answered — so a tunnel that fails here fails
# on its own, and cannot be mistaken for the platform not having come up.
#
# The exception is a quick tunnel or ngrok, whose hostname had to be known before
# AppLab was installed; resolve_host started that one already, and publish()
# notices and does nothing.
publish

APPLAB_PUBLIC_URL="$public_url" \
APPLAB_API_KEY_SHOWN="$APPLAB_API_KEY" \
APPLAB_VERSION_SHOWN="$APPLAB_VERSION" \
APPLAB_NAMESPACE_SHOWN="$APPLAB_NAMESPACE" \
APPLAB_PATH_PREFIX_SHOWN="$APPLAB_PATH_PREFIX" \
APPLAB_BASE_PATH_SHOWN="$APPLAB_BASE_PATH" \
APPLAB_TUNNEL_SHOWN="$APPLAB_TUNNEL" \
APPLAB_REGISTRY_SHOWN="$APPLAB_REGISTRY" \
APPLAB_CLUSTER_SHOWN="$APPLAB_CLUSTER_NAME" \
APPLAB_RUNTIME_DIR_SHOWN="$RUNTIME_DIR" \
  bash "$SCRIPT_DIR/summary.sh"

cat <<EOF

=====================================================================
 applab is ready

   Console:  ${public_url}
   API key:  ${APPLAB_API_KEY}

   The console asks for this address and key; both are kept in your
   browser. Deployed apps are served under ${public_url}${APPLAB_PATH_PREFIX}/<app>/.

   Push an app from a local directory:

     export APPLAB_URL='${public_url}'
     export APPLAB_KEY='${APPLAB_API_KEY}'
     applab push myshop

=====================================================================
EOF

# ── 7. stay alive ───────────────────────────────────────────────────────────

cleanup() {
  log "ending the environment"
  [ -f "$RUNTIME_DIR/tail.pid" ] && kill "$(cat "$RUNTIME_DIR/tail.pid")" 2>/dev/null || true
  [ -n "$tunnel_pid" ] && kill "$tunnel_pid" 2>/dev/null || true
  kind delete cluster --name "$APPLAB_CLUSTER_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

DEADLINE=0
if [[ "$APPLAB_SESSION_HOURS" =~ ^[0-9]+$ ]] && [ "$APPLAB_SESSION_HOURS" -gt 0 ]; then
  DEADLINE=$(( $(date +%s) + APPLAB_SESSION_HOURS * 3600 ))
  log "the environment runs for ${APPLAB_SESSION_HOURS}h, or until the job times out"
else
  log "the environment runs until the job times out or the workflow is cancelled"
fi

# A deadline of 0 means "no self-imposed limit": the loop runs until the job is
# cancelled, which is the only kind of no-limit a runner that kills the job
# anyway can offer.
while [ "$DEADLINE" -eq 0 ] || [ "$(date +%s)" -lt "$DEADLINE" ]; do
  # A dead tunnel is worth reporting now rather than at the deadline: it is the
  # only way in, so nobody can reach the environment, which is a fact the person
  # watching needs before the run ends. Nothing else is watched — everything
  # after the tunnel is the cluster's own health, which Kubernetes reports.
  #
  # Only when there is one: APPLAB_PUBLIC_HOST means no tunnel was started, and
  # an absent agent is not a stopped one. `kill -0 ""` fails, so without this the
  # loop would end the environment on its first pass — which is exactly the case
  # CI runs in.
  if [ -n "$tunnel_pid" ]; then
    kill -0 "$tunnel_pid" 2>/dev/null || { warn "the tunnel agent stopped"; break; }
  fi
  sleep 15
done

log "environment over"
