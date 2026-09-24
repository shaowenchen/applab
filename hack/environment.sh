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
#     istio-ingress  the gateway everything is published through (NodePort 30080)
#     registry:2     where built images are pushed (kind-registry:5000)
#
#   on the runner
#     cloudflared    a tunnel, so the environment is reachable from anywhere
#
# One gateway serves both halves, and that is the whole of the routing: the
# console and the API at "/" (a VirtualService the chart installs), each app
# under "/apps/<app>/" (a VirtualService applab writes at deploy time). Nothing
# sits in front of the gateway to tell the two apart, because the paths already
# do: Istio sorts a virtual host's catch-all route to the end and keeps the rest
# in order, and "/apps/<app>/" is not a prefix any of the console's own paths
# share. See debugger/README.md.
#
# The order below is load-bearing and the reason it is a script rather than a
# list of workflow steps. The hostname has to be settled first, because it
# becomes apps.baseDomain and that cannot be set after applab is installed — but
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
# to run the newest applab, and a version read from the chart would pin it to a
# release that has to exist first — which is the failure that put this here, since
# the chart's appVersion had never been published at all.
#
# imagePullPolicy is Always (set below), so a moving tag is re-resolved on every
# start rather than being served from a node's cache.
: "${APPLAB_VERSION:=latest}"

# APPLAB_LOCAL_IMAGE names an image already built on this machine, which is
# loaded into the kind nodes instead of being pulled. CI sets it: the point of
# running an environment there is to exercise the commit under test, and an image
# pulled from a registry is whatever was published last, which may be neither that
# commit nor in the registry at all.
#
# It also removes a race. A pull depends on someone else having pushed first; a
# load depends on nothing.
: "${APPLAB_LOCAL_IMAGE:=}"

# Always when pulling, because the tag is `latest` and a node that already has it
# would otherwise keep serving the previous build — the deploy reports success
# while running the old image, which is the hardest kind of failure to notice.
#
# Never when the image was loaded locally: there is nothing to pull it from, and
# `Always` would send the kubelet to a registry that has never heard of it.
# `IfNotPresent` is right there, because the image is already on the node.
if [ -n "$APPLAB_LOCAL_IMAGE" ]; then
  : "${APPLAB_IMAGE_PULL_POLICY:=IfNotPresent}"
else
  : "${APPLAB_IMAGE_PULL_POLICY:=Always}"
fi

: "${APPLAB_API_KEY:=}"
: "${APPLAB_SESSION_HOURS:=0}"
: "${APPLAB_TUNNEL:=cloudflare}"
: "${APPLAB_NAMESPACE:=ops-system}"
: "${APPLAB_BUILD_ROOTLESS:=true}"
: "${APPLAB_GATEWAY_NODEPORT:=30080}"
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

# A command that fails under `set -e` stops the script with no message at all,
# which is the worst way for a long install to end: the log simply stops, and
# where it stopped is the only clue. This says which command failed, on which
# line, and with what status.
#
# It is a diagnostic, not error handling — nothing is recovered from — so it
# reports and lets the non-zero status stand.
trap 'status=$?; printf "\n\033[1;31m[applab-debugger]\033[0m %s failed (exit %d)\n" "$BASH_COMMAND" "$status" >&2' ERR

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
# It does not start anything. The hostname has to be known before applab is
# installed, because it becomes apps.baseDomain — but knowing it is not the same
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
# is what applab needs: apps.baseDomain, which is both where the apps are served
# and the host the console's own route matches.
resolve_host() {
  # The one case with nothing to publish: the caller has a name, and there is no
  # tunnel to start. Recorded here and returned to by publish(), which does
  # nothing when APPLAB_PUBLIC_HOST is set.
  if [ -n "${APPLAB_PUBLIC_HOST:-}" ]; then
    TUNNEL_HOST="$APPLAB_PUBLIC_HOST"
    public_url="http://${TUNNEL_HOST}"
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
    public_url="https://${TUNNEL_HOST}"
    log "the environment will be served at ${public_url}"
    log "  (the tunnel's ingress is configured in Cloudflare, not here)"
    return 0
  fi

  # A quick tunnel, or ngrok: the name is whatever the agent is given, and the
  # only way to learn it is to start the agent and ask. So this is the one path
  # that has to run early — and it does, from here, before applab is installed.
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
  public_url="https://${TUNNEL_HOST}"
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

# An image built here goes straight into the nodes, so the cluster never has to
# reach a registry for it. `kind load` copies the layers into each node's
# containerd, which is why no pull secret and no network path are needed.
#
# The image keeps the name it was built with, and the chart is pointed at that
# same name below — a load matches on the reference, so retagging it here would
# only create a second name for the nodes to not find.
if [ -n "$APPLAB_LOCAL_IMAGE" ]; then
  log "loading the locally built image ${APPLAB_LOCAL_IMAGE} into the cluster"
  docker image inspect "$APPLAB_LOCAL_IMAGE" >/dev/null 2>&1 \
    || die "APPLAB_LOCAL_IMAGE is '${APPLAB_LOCAL_IMAGE}', which is not an image on this machine; build it first"
  kind load docker-image --name "$APPLAB_CLUSTER_NAME" "$APPLAB_LOCAL_IMAGE"

  # Everything before the tag is the repository, which is what the chart is told.
  #
  # A colon may be the tag separator or a registry's port — "registry:5000/apps"
  # has no tag — so the separator is a colon in the *last* path segment, and only
  # there. Splitting on the last colon anywhere would read that port as a tag and
  # hand the chart a repository of "registry".
  #
  # A reference with no tag at all is left as the repository, and APPLAB_VERSION
  # keeps the `latest` set above: an untagged name means `latest` to Docker, so
  # the two agree without this having to say so.
  last_segment="${APPLAB_LOCAL_IMAGE##*/}"
  case "$last_segment" in
    *:*)
      APPLAB_VERSION="${last_segment##*:}"
      APPLAB_IMAGE_REPOSITORY="${APPLAB_LOCAL_IMAGE%:*}"
      ;;
    *)
      APPLAB_IMAGE_REPOSITORY="$APPLAB_LOCAL_IMAGE"
      ;;
  esac
fi

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
# success the moment the objects are created, and the first symptom is a gateway
# that cannot reach applab ninety attempts later — which says nothing about the
# image being the problem. The timeout is the chart's own.
#
# ingress.enabled=false, because a kind cluster has no ingress controller and
# installing one would be a moving part added for nothing. The chart then
# publishes the console through the Istio gateway instead — one VirtualService on
# the base domain, which is the same host the apps are already served on, so the
# whole environment is reachable through the one address the tunnel publishes.
# `if !` rather than letting it fail: under `set -e` a failed install stops the
# script on the spot, and the one thing worth having then is the state of the
# cluster it left behind. helm's own message says what it waited for; only the
# cluster says why.
if ! helm install applab "$REPO_ROOT/charts/applab" \
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
  --set "image.pullPolicy=${APPLAB_IMAGE_PULL_POLICY}" \
  --timeout 10m
then
  warn "applab did not install; the state it left behind follows"
  kubectl -n "$APPLAB_NAMESPACE" get pods,deployment,replicaset,service,pvc 2>&1 | sed 's/^/    /' || true
  # The reason a pod is not Ready is almost always in these three, and they are
  # the things a person would otherwise have to guess at: an image that cannot be
  # pulled, a volume that cannot be mounted, an applab that started and refused
  # its own configuration.
  kubectl -n "$APPLAB_NAMESPACE" describe pods 2>&1 | tail -n 60 | sed 's/^/    /' || true
  kubectl -n "$APPLAB_NAMESPACE" logs deploy/applab --all-containers --tail=100 2>&1 | sed 's/^/    /' || true
  helm -n "$APPLAB_NAMESPACE" status applab 2>&1 | sed 's/^/    /' || true
  die "helm could not bring applab up: the message above is helm's, the rest is the cluster's"
fi

# The install reported success, which with --wait means every object it created
# was ready. Asserted anyway, because "ready" is what helm inferred from the
# objects it knows about, and the ones that matter here are the two applab writes
# itself: the console's VirtualService from the chart, and the Service behind it.
# Both are checked before anything is published, so a broken install fails here
# rather than at a browser.
kubectl -n "$APPLAB_NAMESPACE" rollout status deploy/applab --timeout=120s
kubectl -n "$APPLAB_NAMESPACE" get virtualservice applab-console -o name >/dev/null 2>&1 \
  || die "the console has no VirtualService, so the gateway would answer 404 at \"/\": the chart rendered one only when ingress.enabled is false, and this install did not produce it"

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
log "waiting for the gateway to serve applab"
for attempt in $(seq 1 90); do
  if [ "$(gateway_code /health)" = "200" ]; then break; fi
  if [ $((attempt % 15)) -eq 0 ]; then log "  still waiting... (attempt ${attempt})"; fi
  sleep 2
done
if [ "$(gateway_code /health)" != "200" ]; then
  kubectl -n "$APPLAB_NAMESPACE" get pods
  kubectl -n "$APPLAB_NAMESPACE" logs deploy/applab --tail=50 2>/dev/null || true
  kubectl -n "$APPLAB_NAMESPACE" get virtualservices 2>/dev/null || true
  die "the gateway is not serving applab at /health"
fi

# What is actually running. Printed rather than assumed: when something is wrong
# this is the first thing anyone asks for, and it is worth having in the log of a
# run that succeeded too — it is the only record of what the environment was.
log "the cluster, as it came up"
kubectl get nodes -o wide
kubectl -n istio-system get deployment,service
kubectl -n "$APPLAB_NAMESPACE" get deployment,service,pod,secret,pvc
# VirtualServices are Istio's rather than Kubernetes', so `get all` does not
# include them — and they are the objects that decide whether anything is
# reachable at all.
kubectl -n "$APPLAB_NAMESPACE" get virtualservices

# Every endpoint a person or a client uses, and the status each one answers.
#
# Reported and asserted in one pass, because they are the same question: these
# are all served by one process behind one route, so anything but a 200 is a bug
# rather than a slow start, and a table of green is the evidence that the gateway,
# the two VirtualServices, the Service and the deployment all line up.
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
check_endpoint "/health"                /health
check_endpoint "/api/v1/config"         /api/v1/config
check_endpoint "/api/v1/version"        /api/v1/version
check_endpoint "/llms.txt"              /llms.txt
check_endpoint "/metrics"               /metrics
check_endpoint "/ (the console)"        /
# The one thing that proves the admin key works through the gateway, not only
# that the route exists.
check_endpoint "/api/v1/overview (key)" /api/v1/overview -H "Authorization: Bearer ${APPLAB_API_KEY}"

[ -z "$failed" ] || die "these did not answer 200 through the gateway:${failed}"

# ── 6. publish ──────────────────────────────────────────────────────────────

# The tunnel comes up now rather than at the start. Everything above is the
# cluster's own business and is already proven — the pods, the Service, both
# VirtualServices and every endpoint answered — so a tunnel that fails here fails
# on its own, and cannot be mistaken for the platform not having come up.
#
# The exception is a quick tunnel or ngrok, whose hostname had to be known before
# applab was installed; resolve_host started that one already, and publish()
# notices and does nothing.
publish

APPLAB_PUBLIC_URL="$public_url" \
APPLAB_API_KEY_SHOWN="$APPLAB_API_KEY" \
APPLAB_VERSION_SHOWN="$APPLAB_VERSION" \
APPLAB_NAMESPACE_SHOWN="$APPLAB_NAMESPACE" \
APPLAB_PATH_PREFIX_SHOWN="$APPLAB_PATH_PREFIX" \
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
