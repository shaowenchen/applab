#!/usr/bin/env bash
#
# Start a complete applab test environment on this machine and publish it.
#
# This is what the debugger/ action runs. It builds a throwaway Kubernetes
# cluster, a registry, an Istio gateway and an applab installation — everything
# a single app needs before `applab push` can build, deploy and serve it. The
# point is that none of it has to be assembled by hand first.
#
# What comes up:
#
#   kind cluster (ns ops-system)
#     applab         the published image, installed with this repository's chart
#     istio-ingress  the gateway apps are published through (NodePort 30080)
#     registry:2     where built images are pushed (kind-registry:5000)
#
#   on the runner
#     cloudflared    a quick tunnel, so the environment is reachable from anywhere
#     router.mjs     splits one hostname between applab and the apps
#     kubectl port-forward  applab's Service, which has no ingress here
#
# The order below is load-bearing and the reason it is a script rather than a
# list of workflow steps: the tunnel has to be up *first*, because the hostname
# it mints becomes apps.baseDomain, which cannot be set after applab is
# installed, and because the chart refuses a base domain with no gateway.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)

# ── inputs ──────────────────────────────────────────────────────────────────

# The published `latest`, which is what the release workflow produces alongside
# the version tags. A moving tag, deliberately: the point of this environment is
# to run the newest applab, and a version read from the chart would pin it to a
# release that has to exist first — which is the failure that put this here, since
# the chart's appVersion had never been published at all.
#
# imagePullPolicy is Always (set below), so a moving tag is re-resolved on every
# start rather than being served from a node's cache.
: "${APPLAB_VERSION:=latest}"

: "${APPLAB_API_KEY:=}"
: "${APPLAB_SESSION_HOURS:=0}"
: "${APPLAB_TUNNEL:=cloudflare}"
: "${APPLAB_NAMESPACE:=ops-system}"
: "${APPLAB_BUILD_ROOTLESS:=true}"
: "${APPLAB_GATEWAY_NODEPORT:=30080}"
: "${APPLAB_ROUTER_PORT:=3080}"
: "${APPLAB_CLUSTER_NAME:=applab-debugger}"
: "${APPLAB_PATH_PREFIX:=/apps}"
: "${APPLAB_IMAGE_REPOSITORY:=docker.io/shaowenchen/applab}"
# The registry runs as a container on the same docker network as the kind nodes,
# so both the nodes' containerd and the build Jobs' pods can resolve this name
# and reach it without any TLS or credential.
: "${APPLAB_REGISTRY_NAME:=kind-registry}"
: "${APPLAB_REGISTRY_PORT:=5000}"
: "${APPLAB_REGISTRY:=${APPLAB_REGISTRY_NAME}:${APPLAB_REGISTRY_PORT}}"

: "${CLOUDFLARE_TOKEN:=}"
: "${NGROK_TOKEN:=}"

# Set so `kind` and `kubectl` are found however this script is invoked; the
# action installs them into /usr/local/bin.
export PATH="/usr/local/bin:$PATH"

log()  { printf '\n\033[1;34m[applab-debugger]\033[0m %s\n' "$*"; }
warn() { printf '\n\033[1;33m[applab-debugger]\033[0m %s\n' "$*" >&2; }
die()  { printf '\n\033[1;31m[applab-debugger]\033[0m %s\n' "$*" >&2; exit 1; }
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

# ── 2. the tunnel, before anything that needs its hostname ──────────────────

tunnel_pid=""
TUNNEL_API=""

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

# resolve_tunnel settles on the domain the environment is served under, from a
# tunnel or from the caller.
#
# APPLAB_PUBLIC_HOST skips the tunnel entirely and uses the given hostname. It
# exists for CI, which must not depend on a public tunnel being granted: a quick
# tunnel's hostname is minted per connection, is rate-limited, and is aimed at
# trying things rather than at being a test dependency. CI sets it and drives the
# router over loopback; the tunnel itself is exercised by the debugger workflow,
# where a flaky link is a person's problem to re-run rather than a red build.
#
# APPLAB_DOMAIN names the domain apps are served under, for a tunnel that *is*
# started. It is not tunnel configuration — a named Cloudflare tunnel keeps its
# hostname in its ingress, and the connector is never told it, and nothing has to
# be passed to cloudflared. It is what applab needs: apps.baseDomain, and the
# host the router hands to Istio so a VirtualService matches. A named tunnel
# cannot report it, so it has to be supplied.
resolve_tunnel() {
  if [ -n "${APPLAB_PUBLIC_HOST:-}" ]; then
    TUNNEL_HOST="$APPLAB_PUBLIC_HOST"
    public_url="http://${TUNNEL_HOST}"
    printf '%s\n' "$public_url" > "$PUBLIC_URL_FILE"
    log "using the supplied hostname ${TUNNEL_HOST}; no tunnel is started"
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
  fi

  open_tunnel

  # Taken as given rather than discovered, because a named tunnel's hostname is
  # not discoverable — see above. The tunnel's ingress has to already point here;
  # nothing in this script can create or check it.
  if [ -n "${APPLAB_DOMAIN:-}" ]; then
    TUNNEL_HOST="$APPLAB_DOMAIN"
    public_url="https://${TUNNEL_HOST}"
    printf '%s\n' "$public_url" > "$PUBLIC_URL_FILE"
    log "apps will be served under ${TUNNEL_HOST}"
    log "  (the tunnel's ingress is configured in Cloudflare, not here)"
    return 0
  fi

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

  printf '%s\n' "$found" > "$PUBLIC_URL_FILE"
  TUNNEL_HOST="${found#https://}"
  TUNNEL_HOST="${TUNNEL_HOST#http://}"
  TUNNEL_HOST="${TUNNEL_HOST%%/*}"
  public_url="https://${TUNNEL_HOST}"
  log "the environment will be published at ${public_url}"
}

open_cloudflare_tunnel() {
  TUNNEL_API="http://127.0.0.1:20241"
  if [ -n "$CLOUDFLARE_TOKEN" ]; then
    log "opening a Cloudflare named tunnel"
    cloudflared tunnel --no-autoupdate run --token "$CLOUDFLARE_TOKEN" >"$TUNNEL_LOG" 2>&1 &
  else
    log "opening a Cloudflare quick tunnel (no account needed)"
    cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:${APPLAB_ROUTER_PORT}" >"$TUNNEL_LOG" 2>&1 &
  fi
  tunnel_pid=$!
}

open_ngrok_tunnel() {
  TUNNEL_API="http://127.0.0.1:4040"
  [ -n "$NGROK_TOKEN" ] || die "APPLAB_TUNNEL=ngrok needs NGROK_TOKEN"
  log "opening an ngrok tunnel"
  ngrok config add-authtoken "$NGROK_TOKEN" >"$TUNNEL_LOG" 2>&1 || die "ngrok rejected the authtoken"
  ngrok http "$APPLAB_ROUTER_PORT" >>"$TUNNEL_LOG" 2>&1 &
  tunnel_pid=$!
}

resolve_tunnel

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
      # The Istio ingress gateway, so the runner (and the router) can reach it.
      - containerPort: ${APPLAB_GATEWAY_NODEPORT}
        hostPort: ${APPLAB_GATEWAY_NODEPORT}
        protocol: TCP
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry.mirrors."${APPLAB_REGISTRY}"]
      endpoint = ["http://${APPLAB_REGISTRY}"]
EOF

kind create cluster --config "$RUNTIME_DIR/kind.yaml" --wait 120s

# The registry lives on the kind docker network with a stable alias, so it is
# reachable by the name the images carry from both the nodes' containerd and any
# pod in the cluster.
log "starting the registry at ${APPLAB_REGISTRY}"
if [ "$(docker inspect -f '{{.State.Running}}' "$APPLAB_REGISTRY_NAME" 2>/dev/null || echo false)" != "true" ]; then
  docker run -d --restart=always -p "127.0.0.1:${APPLAB_REGISTRY_PORT}:5000" \
    --name "$APPLAB_REGISTRY_NAME" registry:2 >/dev/null
fi
docker network connect "kind" "$APPLAB_REGISTRY_NAME" 2>/dev/null || true

# Advertise the registry to the cluster so a discovery-aware runtime (and
# anything reading the convention) finds the same answer the mirror gives.
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "${APPLAB_REGISTRY}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF

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

# ── 4. applab ───────────────────────────────────────────────────────────────

log "installing applab in namespace ${APPLAB_NAMESPACE}"

kubectl create namespace "$APPLAB_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The key goes in through a Secret rather than --set auth.keys[0]=..., which
# would write it into the release's stored values where anyone with read on the
# namespace can recover it.
kubectl -n "$APPLAB_NAMESPACE" create secret generic applab-keys \
  --from-literal=APPLAB_KEYS="$APPLAB_API_KEY" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# install, not upgrade: this script assumes a fresh cluster, and an upgrade
# against a half-installed release would hide a first-install failure.
#
# --wait, so an image that cannot be pulled fails here. Without it helm reports
# success the moment the objects are created, and the first symptom is a router
# that cannot connect ninety attempts later — which says nothing about the image
# being the problem. The timeout is the chart's own.
helm install applab "$REPO_ROOT/charts/applab" \
  --namespace "$APPLAB_NAMESPACE" \
  --wait \
  --set auth.existingSecret=applab-keys \
  --set "apps.baseDomain=${TUNNEL_HOST}" \
  --set "apps.pathPrefix=${APPLAB_PATH_PREFIX}" \
  --set deploy.gateway=istio-system/istio-ingressgateway \
  --set "build.registry=${APPLAB_REGISTRY}" \
  --set build.insecureRegistry=true \
  --set "build.rootless=${APPLAB_BUILD_ROOTLESS}" \
  --set ingress.enabled=false \
  --set "image.repository=${APPLAB_IMAGE_REPOSITORY}" \
  --set "image.tag=${APPLAB_VERSION}" \
  --set "image.pullPolicy=Always" \
  --timeout 10m

# applab is reached by port-forward: the chart's Ingress is disabled because a
# kind cluster has no ingress controller, and installing one would add a moving
# part for the sake of reaching a Service the runner can already reach directly.
log "forwarding applab's Service to 127.0.0.1:8080"
kubectl -n "$APPLAB_NAMESPACE" port-forward svc/applab 8080:80 \
  > "$RUNTIME_DIR/port-forward.log" 2>&1 &
port_forward_pid=$!
echo "$port_forward_pid" > "$RUNTIME_DIR/port-forward.pid"

# ── 5. the router ───────────────────────────────────────────────────────────

log "starting the router on 127.0.0.1:${APPLAB_ROUTER_PORT}"
APPLAB_ROUTER_PORT="$APPLAB_ROUTER_PORT" \
APPLAB_ROUTER_PREFIX="$APPLAB_PATH_PREFIX" \
APPLAB_ROUTER_GATEWAY="127.0.0.1:${APPLAB_GATEWAY_NODEPORT}" \
APPLAB_ROUTER_APPLAB="127.0.0.1:8080" \
APPLAB_ROUTER_HOST="$TUNNEL_HOST" \
  node "$SCRIPT_DIR/router.mjs" > "$RUNTIME_DIR/router.log" 2>&1 &
router_pid=$!
echo "$router_pid" > "$RUNTIME_DIR/router.pid"

# ── 6. wait until both halves answer ────────────────────────────────────────

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null; }

log "waiting for the environment to become reachable"
applab_ready() { [ "$(http_code "http://127.0.0.1:${APPLAB_ROUTER_PORT}/health")" = "200" ]; }

for attempt in $(seq 1 90); do
  if applab_ready; then break; fi
  if [ $((attempt % 15)) -eq 0 ]; then
    log "  still waiting... (attempt ${attempt})"
    tail -n 5 "$RUNTIME_DIR/router.log" 2>/dev/null | sed 's/^/    /' || true
  fi
  sleep 2
done
applab_ready || {
  kubectl -n "$APPLAB_NAMESPACE" get pods
  kubectl -n "$APPLAB_NAMESPACE" logs deploy/applab --tail=50 2>/dev/null || true
  die "applab is not answering through the router"
}

# The gateway is only exercised by a real deploy, so its readiness is reported
# rather than waited on: failing the whole environment because Istio is slow to
# program its first route would block the console and the API, which work.
gateway_code=$(http_code "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}/")
log "the ingress gateway answers ${gateway_code} on ${APPLAB_GATEWAY_NODEPORT}"

# ── 7. publish ──────────────────────────────────────────────────────────────

APPLAB_PUBLIC_URL="$public_url" \
APPLAB_API_KEY_SHOWN="$APPLAB_API_KEY" \
APPLAB_VERSION_SHOWN="$APPLAB_VERSION" \
APPLAB_NAMESPACE_SHOWN="$APPLAB_NAMESPACE" \
APPLAB_PATH_PREFIX_SHOWN="$APPLAB_PATH_PREFIX" \
APPLAB_TUNNEL_SHOWN="$APPLAB_TUNNEL" \
APPLAB_REGISTRY_SHOWN="$APPLAB_REGISTRY" \
APPLAB_CLUSTER_SHOWN="$APPLAB_CLUSTER_NAME" \
APPLAB_RUNTIME_DIR_SHOWN="$RUNTIME_DIR" \
  "$SCRIPT_DIR/summary.sh"

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

# ── 8. stay alive ───────────────────────────────────────────────────────────

cleanup() {
  log "ending the environment"
  for pidfile in router.pid port-forward.pid tail.pid; do
    [ -f "$RUNTIME_DIR/$pidfile" ] && kill "$(cat "$RUNTIME_DIR/$pidfile")" 2>/dev/null || true
  done
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
  # A dead half is worth reporting now rather than at the deadline: the tunnel
  # or the router dying means nobody can reach the environment, which is a fact
  # the person watching needs before the run ends.
  kill -0 "$(cat "$RUNTIME_DIR/router.pid" 2>/dev/null)" 2>/dev/null || { warn "the router stopped"; break; }
  kill -0 "$tunnel_pid" 2>/dev/null || { warn "the tunnel agent stopped"; break; }
  sleep 15
done

log "environment over"
