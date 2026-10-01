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
#   kind cluster
#     AppLab         the published image, installed with this repository's chart
#                    — twice, as two independent installations in two namespaces
#     istio-ingress  the gateway everything is published through (NodePort 30080)
#     metrics-server the resource metrics API, which the console reads usage from
#                    (ns kube-system; optional, and the console says so without it)
#
#   on the runner
#     minio          the object store both installations keep everything in
#     cloudflared    a tunnel, so the environment is reachable from anywhere
#
# Two installations rather than one, because the things worth testing here are
# the things that only happen when there are two: two releases in different
# namespaces, two consoles on one gateway, two key sets, and one bucket shared
# between them. Everything they must not share is separated by an ordinary
# Kubernetes boundary — a namespace, a release name, a Secret — except the object
# store, which is not per installation and cannot be. APPLAB_OBJECT_STORE_PREFIX
# is what keeps those two out of each other's keys, and it is the one setting in
# here whose absence is silent: without it both installations come up perfectly
# and then quietly overwrite one another.
#
# The domain and the key differ per installation; the address shape is shared,
# because it is the rig's shape and not an installation's. One gateway serves
# both halves of each installation, and that is the whole of the routing: each
# console and API at its base path (a VirtualService the chart installs), each
# app under "/apps/<app>" (a VirtualService AppLab writes at deploy time).
# Nothing sits in front of the gateway to tell any of them apart, because the
# hosts and the paths already do: Istio sorts a virtual host's catch-all route to
# the end and keeps the rest in order, "/apps/<app>/" is not a prefix any
# console's own paths share, and the two consoles are on two different hosts.
# See debugger/README.md.
#
# The order below is load-bearing and the reason it is a script rather than a
# list of workflow steps. Each hostname has to be settled first, because it
# becomes ingress.host and that cannot be set after AppLab is installed — but
# settling it is not the same as starting a tunnel, and the tunnel only has to be
# started early when it is the one thing that knows the name. Otherwise it comes
# up last, once there is something behind it to publish. The cluster and the
# gateway are the other half of the order: they are shared, so they are built
# once, before either installation exists.

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

# Two installations, one cluster, and one key each. Every per-installation
# variable below is numbered for exactly that reason: one number is the whole of
# what tells the two apart, so there is one code path and nothing that can be
# changed for one installation and forgotten for the other.
#
# The two keys are separate on purpose, and the environments verify it — an
# installation whose key is not its own is an installation anyone with the other
# one can delete apps from. See verify_instance.
: "${APPLAB_API_KEY:=}"
: "${APPLAB_API_KEY_2:=}"
: "${APPLAB_DOMAIN_1:=}"
: "${APPLAB_DOMAIN_2:=}"
: "${APPLAB_PUBLIC_HOST_1:=}"
: "${APPLAB_PUBLIC_HOST_2:=}"
: "${APPLAB_SESSION_HOURS:=0}"
: "${APPLAB_TUNNEL:=cloudflare}"
: "${APPLAB_GATEWAY_NODEPORT:=30080}"
# The object store's key prefix, one name per installation.
#
# This is the variable the two installations live or die by, and its absence was
# the trap: the object store is shared — the bucket, the endpoint and the
# credential are the runner's, not an installation's — so without a prefix both
# installations read and write the same "apps/<id>" keys and each one's apps,
# keys and build history collide with the other's. The prefix is AppLab's own
# answer to that (see objectstore.Prefixed); this is the rig supplying it.
#
# The bucket is deliberately *not* split in two. It is infrastructure, it is
# created once below, and a prefix already separates the two installations
# inside it — a second bucket would be a second thing to create, name and
# grant, for no separation the prefix does not already give.
: "${APPLAB_OBJECT_STORE_PREFIX:=applab}"
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
# Deployment names it to pull. One registry, one credential, one name — one per
# installation, which is why the name is derived in install_instance rather than
# defaulted here: two installations in one cluster cannot share it, because a
# Secret belongs to a namespace and names nothing outside its own.

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
# Kept as "one URL per line, in installation order" because it is the only thing
# a later reader has: the tunnel and the script both only ever write, and
# anything that wants to know where the environment ended up — a person, a
# script after the run — reads this rather than the log.
: > "$PUBLIC_URL_FILE"

# Every tunnel this environment ever starts, in one list. The record is kept
# because the wait loop at the bottom has to watch all of them: a dead tunnel is
# the only thing that makes the environment unreachable, and with two of them the
# second one dying is exactly as fatal as the first — and would otherwise only be
# noticed at the deadline.
tunnel_pids=()

# Set by resolve_hosts, below: the hostname and the URL each installation is
# served under, indexed 1 and 2. Bash has no portable way to return two values
# from a function, and these are read from a dozen places, so they are globals.
TUNNEL_HOST_1=""
TUNNEL_HOST_2=""
public_url_1=""
public_url_2=""

# The installations, as the loop variable n. Named here rather than written as
# "1 2" in the one loop that uses it, so a third installation is a change in one
# place — and so the check in .github/workflows/image.yml, which stats the files
# on disk, matches what this script means. The registry Secret is what makes a
# second set of files necessary: a Secret belongs to a namespace, so neither
# installation can hold the other's, and every hardcoded name here is one the
# second installation would otherwise have shared with the first.
APPLAB_INSTANCES="1 2"

# The registry Secret each installation creates, indexed the same way. Named
# after the first installation rather than numbered, because installation 1 is
# the one that used to be called just "applab" and there is no reason to make a
# new name for it.
registry_secret_name() { if [ "$1" = "1" ]; then printf 'applab-registry'; else printf 'applab-registry-%s' "$1"; fi; }
# Same for the auth Secret: installation 1 keeps the name it already had.
auth_secret_name() { if [ "$1" = "1" ]; then printf 'applab-keys'; else printf 'applab-keys-%s' "$1"; fi; }

# registry_host_of reads the registry's *address* out of APPLAB_REGISTRY, which is
# a description of where an image goes rather than an address on its own.
#
# The first segment is the host only if it reads like one — the same rule applab
# applies to the value itself (see pathSegments): a dot, a colon, or "localhost".
# A bare account name is Docker Hub, whose Secret server and login address are
# Docker's own fixed URL rather than the account.
#
# A function because it is needed in two places — the docker-registry Secret each
# installation creates, and the one login that checks the credential — and a rule
# this easy to get subtly wrong is one to have in one place.
registry_host_of() {
  local host
  host="$(printf '%s' "$APPLAB_REGISTRY" | cut -d/ -f1)"
  case "$host" in
    *.*|*:*|localhost) printf '%s' "$host" ;;
    *) printf '%s' "https://index.docker.io/v1/" ;;
  esac
}

# instance_number is the loop variable an installation is running as, which the
# functions below read from the environment rather than taking as an argument.
# It exists because the ERR trap interpolates BASH_COMMAND, and BASH_COMMAND is
# the *unexpanded* text of the command that failed — so "${APPLAB_API_KEY}" would
# reach the log as a variable reference and not as a key, while a value passed in
# as an argument would reach it expanded. Everything that touches a key reads
# this instead. See verify_instance.
instance_number() { printf '%s' "${APPLAB_INSTANCE_N:-}"; }
# Same, for the two settings that differ per installation and cannot be derived
# from the number.
instance_host() { if [ "$(instance_number)" = "2" ]; then printf '%s' "$TUNNEL_HOST_2"; else printf '%s' "$TUNNEL_HOST_1"; fi; }
instance_key()  { if [ "$(instance_number)" = "2" ]; then printf '%s' "$APPLAB_API_KEY_2"; else printf '%s' "$APPLAB_API_KEY"; fi; }
# The prefix this installation writes under in the shared bucket. Name-slugified
# rather than left as the bare number so a bucket listing says which rig it came
# from: several of these environments can share one bucket over time.
instance_slug() { printf '%s-%s' "$APPLAB_OBJECT_STORE_PREFIX" "$(instance_number)"; }

# ── 1. the API keys ─────────────────────────────────────────────────────────

# One per installation, generated when not supplied, and printed at the end. They
# are deliberately not masked: they are the deliverable, and a masked value could
# not be shown in the summary that exists to show it.
#
# And they are deliberately different from each other. Filling the second one in
# from the first when it is empty would be the friendlier default and the wrong
# one: it would make the check in verify_instance — that installation 1's key does
# not open installation 2 — pass for the wrong reason, and it would give two
# installations one credential while looking like two.
if [ -z "$APPLAB_API_KEY" ]; then
  APPLAB_API_KEY=$(openssl rand -hex 32)
fi
if [ -z "$APPLAB_API_KEY_2" ]; then
  APPLAB_API_KEY_2=$(openssl rand -hex 32)
fi
[ "$APPLAB_API_KEY" != "$APPLAB_API_KEY_2" ] \
  || die "both installations were given the same key; each one has to have its own, or a key that can destroy one installation's apps also destroys the other's"

# ── 2. the hostnames, before anything that needs them ───────────────────────

# Each installation is served on its own hostname — that is the whole point of
# naming two instead of one — so each gets its own tunnel. Everything below is
# per host: one agent, one log, one URL. What is *not* per host is the origin they
# all point at, which is the one gateway on the gateway's own node port.
#
# The URLs are recorded in installation order as they are settled, because the
# order is what makes the file readable: line 1 is installation 1.
record_url() {
  printf '%s\n' "$2" >> "$PUBLIC_URL_FILE"
  case "$1" in
    1) public_url_1="$2" ;;
    2) public_url_2="$2" ;;
  esac
}

tunnel_log() { printf '%s.%s.log' "$TUNNEL_LOG" "$(instance_number)"; }

# open_tunnel starts one agent, for the installation named by instance_number.
#
# One agent per installation, and not one agent with two hostnames: the two are
# separate public addresses by design, and a tunnel is what makes an address
# public. What they share is only the origin — both agents forward to the same
# gateway on the same node port, which is why the second one is a tunnel and not
# a second gateway.
#
# The pid is recorded in a list rather than a single variable, because both have
# to be watched afterwards: the environment is as unreachable as its deadest
# tunnel, and a monitor that watched only the last one started would sleep
# through the other's death.
open_tunnel() {
  case "$APPLAB_TUNNEL" in
    cloudflare) open_cloudflare_tunnel ;;
    ngrok)      open_ngrok_tunnel ;;
    *)          die "unknown APPLAB_TUNNEL '$APPLAB_TUNNEL'; expected 'cloudflare' or 'ngrok'" ;;
  esac
  local pid="${tunnel_pids[$(( $(instance_number) - 1 ))]:-}"

  # The agent's output is mirrored into the job log: when a tunnel fails, its own
  # words are the only thing that explains why, and a log nobody prints hides
  # exactly that. `-u` because the job log is not a tty and sed would
  # block-buffer, so the output would arrive in bursts instead of as it happens.
  #
  # Prefixed with the installation number, because two agents' logs interleaved
  # without a label is a log nobody can attribute a failure to.
  for _ in $(seq 1 50); do [ -f "$(tunnel_log)" ] && break; sleep 0.1; done
  tail -f "$(tunnel_log)" 2>/dev/null | sed -u "s/^/[${APPLAB_TUNNEL}-$(instance_number)] /" &
  printf '%s\n' "$!" >> "$RUNTIME_DIR/tail.pid"

  # A usage or credential error makes an agent exit instantly, and without this
  # check the only symptom is a missing link minutes later.
  sleep 3
  kill -0 "$pid" 2>/dev/null || {
    sed 's/^/    /' "$(tunnel_log)" 2>/dev/null || true
    die "tunnel $(instance_number): the ${APPLAB_TUNNEL} agent exited during startup; its output is above"
  }
}

# resolve_hosts settles the hostname each installation is served under.
#
# It does not start anything for the two cases that know their own name. A
# hostname has to be known before AppLab is installed, because it becomes
# ingress.host — but knowing it is not the same as publishing it, and for every
# case except a quick tunnel the name is settled without a tunnel running at all.
# The agents are started later, by publish().
#
# APPLAB_PUBLIC_HOST_1 / _2 skip the tunnel entirely and use the given hostname.
# They exist for CI, which must not depend on a public tunnel being granted: a
# quick tunnel's hostname is minted per connection, is rate-limited, and is aimed
# at trying things rather than at being a test dependency. CI sets them and
# drives the gateway over the node port; the tunnel itself is exercised by the
# debugger workflow, where a flaky link is a person's problem to re-run rather
# than a red build.
#
# APPLAB_DOMAIN_1 / _2 name the domain each installation is served under. They
# are not tunnel configuration — a named Cloudflare tunnel keeps its hostname in
# its ingress, and the connector is never told it, and nothing has to be passed
# to cloudflared. They are what AppLab needs: ingress.host, which is both where
# the apps are served and the host the console's own route matches.
resolve_hosts() {
  # The one case with nothing to publish: the caller has names, and there is no
  # tunnel to start. Returned to by publish(), which starts nothing when these are
  # set. Checked once here rather than per installation because it is one decision
  # about the whole environment.
  if [ -n "$APPLAB_PUBLIC_HOST_1" ] || [ -n "$APPLAB_PUBLIC_HOST_2" ]; then
    [ -n "$APPLAB_PUBLIC_HOST_1" ] && [ -n "$APPLAB_PUBLIC_HOST_2" ] \
      || die "APPLAB_PUBLIC_HOST_1 and APPLAB_PUBLIC_HOST_2 must be set together: the two installations are served on two hostnames, and supplying one leaves the other with nothing to answer on"
    TUNNEL_HOST_1="$APPLAB_PUBLIC_HOST_1"
    TUNNEL_HOST_2="$APPLAB_PUBLIC_HOST_2"
    record_url 1 "http://${TUNNEL_HOST_1}${APPLAB_BASE_PATH}"
    record_url 2 "http://${TUNNEL_HOST_2}${APPLAB_BASE_PATH}"
    log "using the supplied hostnames ${TUNNEL_HOST_1} and ${TUNNEL_HOST_2}; no tunnel will be started"
    return 0
  fi

  # A domain can only be named for a tunnel whose ingress was configured in
  # advance, which a quick tunnel's is not: Cloudflare assigns it a random name
  # and it cannot be given another, so a domain chosen here would not resolve.
  # The ngrok path does not pass the flag a reserved domain needs either. Both
  # are refused rather than silently ignored, which would publish a link to a
  # domain that serves nothing.
  #
  # Both domains are checked before either is used, because the failure is a
  # property of the run rather than of one installation, and a run that refused
  # after installing one installation and before the other would have spent five
  # minutes to say the same thing.
  local n domain
  for n in $APPLAB_INSTANCES; do
    if [ "$n" = "1" ]; then domain="$APPLAB_DOMAIN_1"; else domain="$APPLAB_DOMAIN_2"; fi
    if [ -n "$domain" ] && [ "$APPLAB_TUNNEL" != "cloudflare" ]; then
      die "APPLAB_DOMAIN_${n} is only supported with a named Cloudflare tunnel, not '${APPLAB_TUNNEL}'"
    fi
    if [ -n "$domain" ] && [ -z "$CLOUDFLARE_TOKEN" ]; then
      die "APPLAB_DOMAIN_${n} needs CLOUDFLARE_TOKEN: a quick tunnel is assigned a random hostname by Cloudflare, so apps could not be served under the one given here"
    fi
  done

  # A quick tunnel, or ngrok: the names are whatever the agents are given, and
  # the only way to learn them is to start the agents and ask. So this is the one
  # path that has to run early — and it does, here, before AppLab is installed.
  #
  # Both or neither, and refused rather than mixed: a named tunnel serves the
  # hostname in its own ingress, so it cannot also report the random one the other
  # installation would need, and one installation served under a name Cloudflare
  # does not answer for is a link that does not resolve.
  if [ -z "$APPLAB_DOMAIN_1" ] || [ -z "$APPLAB_DOMAIN_2" ]; then
    [ -z "$APPLAB_DOMAIN_1" ] && [ -z "$APPLAB_DOMAIN_2" ] \
      || die "APPLAB_DOMAIN_1 and APPLAB_DOMAIN_2 must be set together: the two installations are separate addresses by design, and one named tunnel cannot serve the other's random hostname"
    log "starting ${APPLAB_TUNNEL} twice, to find out which hostnames they will be given"
    for n in $APPLAB_INSTANCES; do
      APPLAB_INSTANCE_N="$n"
      open_tunnel
    done
    local found
    for n in $APPLAB_INSTANCES; do
      APPLAB_INSTANCE_N="$n"
      log "waiting for tunnel ${n} to report its public hostname"
      if ! found=$(find_public_host); then
        warn "this is a named tunnel: Cloudflare does not tell the connector its own"
        warn "hostname, so it cannot be discovered here. Name the domains instead:"
        warn "  APPLAB_DOMAIN_1=<your domain> APPLAB_DOMAIN_2=<other> ... hack/environment.sh"
        die "tunnel ${n} never reported a public hostname; see $(tunnel_log)"
      fi
      found="${found#https://}"; found="${found#http://}"; found="${found%%/*}"
      if [ "$n" = "1" ]; then TUNNEL_HOST_1="$found"; else TUNNEL_HOST_2="$found"; fi
      record_url "$n" "https://${found}${APPLAB_BASE_PATH}"
      log "installation ${n} will be published at https://${found}${APPLAB_BASE_PATH}"
    done
    return 0
  fi

  # Named tunnels: the names are taken as given rather than discovered, because a
  # named tunnel's hostname is not discoverable — see above. Each tunnel's
  # ingress has to already point here; nothing in this script can create or check
  # it.
  TUNNEL_HOST_1="$APPLAB_DOMAIN_1"
  TUNNEL_HOST_2="$APPLAB_DOMAIN_2"
  record_url 1 "https://${TUNNEL_HOST_1}${APPLAB_BASE_PATH}"
  record_url 2 "https://${TUNNEL_HOST_2}${APPLAB_BASE_PATH}"
  log "installation 1 will be served at https://${TUNNEL_HOST_1}${APPLAB_BASE_PATH}"
  log "installation 2 will be served at https://${TUNNEL_HOST_2}${APPLAB_BASE_PATH}"
  log "  (both hostnames must already point at the tunnel in Cloudflare's dashboard; that is not configured here)"
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
      cloudflare) from_log=$(grep -oE 'https://[a-z0-9-]+\.trycloudflare\.com' "$(tunnel_log)" 2>/dev/null | head -1 || true) ;;
      ngrok)      from_log=$(grep -oE 'https://[a-z0-9-]+\.ngrok-free\.app' "$(tunnel_log)" 2>/dev/null | head -1 || true) ;;
    esac
    [ -n "$from_log" ] && { printf '%s' "$from_log"; return 0; }

    kill -0 "${tunnel_pids[$(( $(instance_number) - 1 ))]:-}" 2>/dev/null || return 1
    if [ $((attempt % 15)) -eq 0 ]; then log "  still waiting for tunnel $(instance_number)... ${attempt}s"; fi
    sleep 2
  done
  return 1
}

# publish starts the tunnel for the installation named by instance_number, if one
# is needed and is not already up.
#
# It runs last, after the environment answers, so a tunnel that fails does so
# with both installations already proven good — the failure is then the tunnel's
# alone and cannot be mistaken for the platform not coming up. resolve_hosts has
# already started the agents for the only case that needed the names in advance
# (a quick tunnel or ngrok), and this returns immediately then.
publish() {
  local n; n="$(instance_number)"

  if [ -n "$APPLAB_PUBLIC_HOST_1" ]; then
    return 0
  fi
  [ -z "${tunnel_pids[$((n - 1))]:-}" ] || {
    # Already up, because its hostname was what we had to wait for.
    return 0
  }

  log "opening the ${APPLAB_TUNNEL} tunnel for installation ${n}"
  open_tunnel
}

open_cloudflare_tunnel() {
  if [ -n "$CLOUDFLARE_TOKEN" ]; then
    log "opening a Cloudflare named tunnel"
    cloudflared tunnel --no-autoupdate run --token "$CLOUDFLARE_TOKEN" >"$(tunnel_log)" 2>&1 &
  else
    log "opening a Cloudflare quick tunnel (no account needed)"
    cloudflared tunnel --no-autoupdate --url "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}" >"$(tunnel_log)" 2>&1 &
  fi
  tunnel_pids[$(( $(instance_number) - 1 ))]=$!
}

open_ngrok_tunnel() {
  [ -n "$NGROK_TOKEN" ] || die "APPLAB_TUNNEL=ngrok needs NGROK_TOKEN"
  log "opening an ngrok tunnel"
  ngrok config add-authtoken "$NGROK_TOKEN" >"$(tunnel_log)" 2>&1 || die "ngrok rejected the authtoken"
  ngrok http "$APPLAB_GATEWAY_NODEPORT" >>"$(tunnel_log)" 2>&1 &
  tunnel_pids[$(( $(instance_number) - 1 ))]=$!
}

resolve_hosts

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

log "installing metrics-server, so the console can show resource usage"
# The Kubernetes resource metrics API, which is what the console's monitoring
# panels and `applab resources` read. It is not part of Kubernetes: the API group
# exists only when a server provides it, and a cluster without one answers 404
# for the whole group.
#
# AppLab works either way — the panels say usage is unavailable and every limit
# still applies — but then the one feature this environment exists to exercise
# cannot be seen at all, so it is installed here rather than left to whoever
# notices two em dashes in the console.
#
# Every step below is non-fatal, and deliberately so: this is an optional
# component, and ending the whole environment because it did not come up would
# trade a missing readout for a missing everything. Each failure warns with what
# it costs and the script carries on. The alternative is also how this block
# first shipped — a typo in it killed the run before AppLab was ever installed,
# which is exactly the trade being avoided.
#
# The whole block is guarded on the apply, so a cluster that cannot reach GitHub
# skips the rest rather than warning four times about the same cause.
#
# The upstream components.yaml, unmodified except for the kubelet TLS flag below.
# The version is pinned, like kind and istio above: a moving tag would install
# something different tomorrow than it did today.
metrics_server_version="v0.7.2"

if kubectl apply -f "https://github.com/kubernetes-sigs/metrics-server/releases/download/${metrics_server_version}/components.yaml"; then
  # kind's kubelet serves its own self-signed certificate, and metrics-server
  # verifies it by default — so without this every scrape fails with an x509
  # error and the Deployment sits unavailable with nothing in the console
  # explaining why. This is the flag the project's own kind documentation gives;
  # it is safe here because the kubelet is reached over the cluster's own network.
  #
  # Tested before it is added rather than appended outright: `--type=json` with
  # an `add` on `args/-` appends unconditionally, so a second run of this script
  # against a cluster that already has the flag would add a second copy of it.
  # Harmless for a boolean, and still not something to leave lying around.
  if kubectl -n kube-system get deployment metrics-server \
       -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null \
       | grep -q -- '--kubelet-insecure-tls'; then
    log "metrics-server already trusts the kubelet's certificate"
  elif ! kubectl -n kube-system patch deployment metrics-server --type=json \
         -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'; then
    warn "could not tell metrics-server to trust kind's kubelet certificate; its scrapes will fail with an x509 error and the console will show no usage"
  fi

  if ! kubectl -n kube-system wait --for=condition=available --timeout=300s deployment/metrics-server; then
    warn "metrics-server did not become available; the console will report usage as unavailable"
  fi
  show "metrics-server" kubectl -n kube-system get deployment metrics-server

  # Waited for a reading rather than for the Deployment, because those are not
  # the same moment: metrics-server becomes available when it starts and answers
  # only after its first scrape of the kubelet — up to a minute later. A check
  # that stopped at "available" would let the AppLab install proceed against an
  # API that still 404s, which is the failure this wait exists to rule out.
  #
  # `top nodes` needs one row, and until one arrives the CLI reports "no metrics
  # known". Sixty tries at two seconds is two minutes, about the interval
  # metrics-server takes to serve its first sample.
  for i in $(seq 1 60); do
    if kubectl top nodes >/dev/null 2>&1; then
      break
    fi
    if [ "$i" = 60 ]; then
      warn "metrics-server is up but reports no readings; usage will be unavailable in the console"
    fi
    sleep 2
  done
else
  warn "metrics-server could not be installed; the console will report usage as unavailable and the rest of the environment is unaffected"
fi

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

# ── 4. AppLab, once per installation ────────────────────────────────────────

# install_instance stands one installation up: its namespace, its two Secrets,
# its release, and the wait for its rollout.
#
# Everything that differs between the two is derived here from the one thing that
# does — the installation number — rather than passed in, and deliberately: a
# parameter is something a call site can get wrong, and the failure that produces
# is the one this whole arrangement exists to prevent. Two installations that
# share a namespace, a release name, a Secret or an object store prefix do not
# fail loudly; they come up and then interfere, which is much harder to see.
#
# The namespaces are `applab-1` and `applab-2` — not one namespace with two
# releases in it. A namespace is the boundary that makes the two independent
# installations rather than two Releases: it is what gives each its own copy of
# every namespaced object, its own Role, its own Service and its own Secrets, and
# it is why one installation's apps cannot see the other's. Two releases in one
# namespace would share the key Secret's name, the registry Secret's name, and
# every app's objects.
#
# The registry Secret is the one that cannot be shared at all, which is why it is
# the one name that has to differ per installation rather than being a constant
# here: a Secret lives in a namespace, and both installations use the same
# registry with the same credential, so each namespace needs its own copy of it.
install_instance() {
  local n; n="$(instance_number)"
  local ns="applab-${n}"
  # The release name is the namespace name, and fullnameOverride makes it the
  # resource prefix too. Set explicitly rather than left to the release: the
  # chart's resources are named after the release only when the release name
  # contains the chart name, and a namespace-scoped name is what makes an object
  # in `kubectl get -A` output say which installation it belongs to.
  local release="applab-${n}"
  local fullname="applab-${n}"
  local auth_secret; auth_secret="$(auth_secret_name "$n")"
  local registry_secret; registry_secret="$(registry_secret_name "$n")"
  # The object store prefix: one name per installation inside one shared bucket.
  # This is the line that keeps the two installations from reading and writing
  # each other's apps — see APPLAB_OBJECT_STORE_PREFIX.
  local store_prefix; store_prefix="$(instance_slug)"
  local host; host="$(instance_host)"
  local key; key="$(instance_key)"

  log "installing AppLab ${n} in namespace ${ns}, served at ${host}${APPLAB_BASE_PATH}"

  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # The registry credential, which both halves of the build pipeline read: the
  # build Job mounts it to push, and every app's Deployment names it to pull. One
  # registry, one credential, one name — see build.secret in the chart.
  #
  # Created here rather than by the chart because the chart carries no registry
  # credential: it would have to be a value, and a value is plain text in the
  # release's history and in this repository. The environment supplies it.
  #
  # Created per installation rather than once, because a Secret belongs to a
  # namespace and the two installations are in two of them. Both copies hold the
  # same credential; the duplication is the API's, not a choice.
  local registry_host
  registry_host="$(registry_host_of)"
  kubectl -n "$ns" create secret docker-registry "$registry_secret" \
    --docker-server="$registry_host" \
    --docker-username="$APPLAB_REGISTRY_USERNAME" \
    --docker-password="$APPLAB_REGISTRY_PASSWORD" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # The key goes in through a Secret rather than --set auth.key=..., which
  # would write it into the release's stored values where anyone with read on the
  # namespace can recover it.
  #
  # One key per installation, and this is where that becomes true: the Secret is
  # per namespace, and each carries only its own installation's key. The
  # cross-installation check in verify_instance is what proves it stayed true.
  kubectl -n "$ns" create secret generic "$auth_secret" \
    --from-literal=APPLAB_KEYS="$key" \
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
  # The object store prefix is the one setting that keeps the two installations
  # out of each other's way inside the one bucket, and leaving it unset is silent:
  # the release comes up, serves, and then shares its apps with the other one. Set
  # from the same slug the check later asserts against.
  if ! helm install "$release" "$REPO_ROOT/charts/applab" \
    --namespace "$ns" \
    --set "fullnameOverride=${fullname}" \
    --set auth.existingSecret="$auth_secret" \
    --set "ingress.host=${host}" \
    --set "ingress.path=${APPLAB_BASE_PATH}" \
    --set "apps.pathPrefix=${APPLAB_PATH_PREFIX}" \
    --set deploy.gateway=istio-system/istio-ingressgateway \
    --set "build.registry=${APPLAB_REGISTRY}" \
    --set "build.secret=${registry_secret}" \
    --set ingress.enabled=false \
    --set "image.repository=${APPLAB_IMAGE_REPOSITORY}" \
    --set "image.tag=${APPLAB_VERSION}" \
    --set "objectStore.endpoint=http://${APPLAB_OBJECT_STORE_ENDPOINT}" \
    --set "objectStore.bucket=${APPLAB_OBJECT_STORE_BUCKET}" \
    --set "objectStore.prefix=${store_prefix}" \
    --set "objectStore.accessKey=${APPLAB_OBJECT_STORE_ACCESS_KEY}" \
    --set "objectStore.secretKey=${APPLAB_OBJECT_STORE_SECRET_KEY}" \
    --set objectStore.pathStyle=true \
    --set objectStore.insecure=true
  then
    warn "AppLab ${n} could not be installed at all; the state it left behind follows"
    kubectl -n "$ns" get pods,deployment,replicaset,service 2>&1 | sed 's/^/    /' || true
    helm -n "$ns" status "$release" 2>&1 | sed 's/^/    /' || true
    die "helm rejected release ${release}: the message above is helm's, the rest is the cluster's"
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
  #
  # The two installations are waited for one at a time rather than together, and
  # that is fine rather than wasteful: the image is pulled on the first one and is
  # in the node's cache by the time the second asks for it, so the second's wait
  # is a few seconds. It is also the honest one — a failure is reported against
  # the installation it happened to.
  local applab_wait_seconds="$APPLAB_INSTALL_TIMEOUT_SECONDS"
  log "waiting up to ${applab_wait_seconds}s for AppLab ${n} to roll out"
  local last_state="" rolled_out="" crash_grabbed=""
  local attempt state
  for attempt in $(seq 1 "$applab_wait_seconds"); do
    # A single line per pod, in a stable order, so the same state does not print
    # every second: only a change is worth a line.
    state=$(kubectl -n "$ns" get pods \
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

    if kubectl -n "$ns" rollout status "deploy/${fullname}" --timeout=1s >/dev/null 2>&1; then
      rolled_out="yes"
      break
    fi
    sleep 1
  done

  if [ -z "$rolled_out" ]; then
    if [ -n "$crash_grabbed" ]; then
      warn "AppLab ${n} cannot start — its container is not coming up, and waiting would not change that"
    else
      warn "AppLab ${n} did not become ready within ${applab_wait_seconds}s; the state it is in follows"
    fi
    kubectl -n "$ns" get pods,deployment,replicaset,service 2>&1 | sed 's/^/    /' || true
    # The reason a container cannot start is in these, and they are the things a
    # person would otherwise have to guess at: an image that cannot be pulled, a
    # volume that cannot be mounted, an AppLab that started and refused its own
    # configuration. The log is what the process itself said before it died, which
    # is the one thing no amount of waiting produces.
    kubectl -n "$ns" describe pods 2>&1 | tail -n 60 | sed 's/^/    /' || true
    kubectl -n "$ns" logs "deploy/${fullname}" --all-containers --tail=100 2>&1 | sed 's/^/    /' || true
    # --previous, because a crash loop's current container may have produced
    # nothing yet — the reason is in the attempt that already died.
    kubectl -n "$ns" logs "deploy/${fullname}" --all-containers --previous --tail=100 2>&1 | sed 's/^/    /' || true
    if [ -n "$crash_grabbed" ]; then
      die "AppLab ${n} exited on startup: its output is above. Nothing about waiting changes this"
    fi
    die "AppLab ${n} never became ready — if this is a slow first pull, raise APPLAB_INSTALL_TIMEOUT_SECONDS"
  fi

  # Ready is what the Deployment reports. The console's route is a separate object
  # that AppLab only has if the chart rendered it, and without it the gateway
  # answers 404 at "/" — so it is checked here rather than discovered at a browser.
  #
  # The name carries this installation's fullname, so the check also asserts the
  # thing the second installation makes easy to get wrong: an object named for the
  # wrong release would be found here as a missing route and not as a VirtualService
  # in the other installation's namespace, which is the failure this check is for.
  kubectl -n "$ns" get virtualservice "${fullname}-console" -o name >/dev/null 2>&1 \
    || die "AppLab ${n} has no console VirtualService (${fullname}-console), so the gateway would answer 404 at \"/\": the chart rendered one only when ingress.enabled is false, and this install did not produce it"

  # Everything the release made, right after it made it. The VirtualServices are
  # listed with the rest rather than separately: one is the console's, from the
  # chart, and an app's appears here too the moment something is deployed — which
  # is exactly the object to look at when a deploy succeeds and nothing is
  # reachable.
  show "AppLab ${n}" \
    kubectl -n "$ns" get deployment,replicaset,pod,service,secret
  show "AppLab ${n}'s routes (VirtualServices)" \
    kubectl -n "$ns" get virtualservices
}

# ── 5. what came up, and whether it answers ─────────────────────────────────

# Everything below reaches the gateway the way a browser does: over loopback on
# the node port, with the installation's own hostname as the Host header — which
# is what a request arriving through the tunnel carries.
#
# The port in that header is harmless. Istio sets IgnorePortInHostMatching on the
# gateway's route configuration (pilot/pkg/networking/core/gateway.go), so Envoy
# drops it before matching, and the bare hostnames the VirtualServices carry are
# what match.
# gateway_code asks the gateway for one path and prints the status it answers.
#
# The host is an argument rather than a global because there are two of them now,
# and this is the function that has to be able to ask about either: the whole
# isolation story is "the same request with the other Host header", and a
# function that read one global could not make it. The caller is left holding the
# names, not the expansion — see verify_instance for why.
gateway_code() {
  local host="$1" path="$2"; shift 2
  # `|| true` so a refused connection reports as 000 rather than killing the
  # script: `set -e` sees curl's non-zero exit inside the command substitution
  # and stops with no message at all, which is the least useful way for a
  # gateway that is not listening to fail.
  curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Host: ${host}" "$@" \
    "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}${path}" 2>/dev/null || true
}

# verify_instance checks one installation end to end: that the gateway serves it,
# that every endpoint answers, that the settings reached the server, and that its
# key is its own.
#
# It reads the host and the key through instance_host and instance_key, which is
# not only for brevity. The ERR trap prints BASH_COMMAND, and BASH_COMMAND is the
# unexpanded text of the command that failed — so a key written into a command as
# a variable reference reaches the log as a reference, while one passed as an
# argument or built by string concatenation would reach it as the key. Every
# command below that carries the key therefore names a variable, and the
# functions it calls do the same.
verify_instance() {
  local n="$1"; APPLAB_INSTANCE_N="$n"
  local ns="applab-${n}"
  local host; host="$(instance_host)"
  local key; key="$(instance_key)"
  local failed="" config_problems="" code body attempt

  # The gateway is the only thing the tunnel points at, so it is what has to
  # answer: a ready Service behind an unprogrammed gateway is still an environment
  # nobody can open.
  log "waiting for the gateway to serve AppLab ${n} on ${host}"
  for attempt in $(seq 1 90); do
    if [ "$(gateway_code "$host" /health)" = "200" ]; then break; fi
    if [ $((attempt % 15)) -eq 0 ]; then log "  still waiting... (attempt ${attempt})"; fi
    sleep 2
  done
  if [ "$(gateway_code "$host" /health)" != "200" ]; then
    kubectl -n "$ns" get pods
    kubectl -n "$ns" logs "deploy/applab-${n}" --tail=50 2>/dev/null || true
    kubectl -n "$ns" get virtualservices 2>/dev/null || true
    die "the gateway is not serving AppLab ${n} at /health (/health and /metrics are the two paths the cluster reaches the pod on; they are served at the root, outside the base path)"
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
  log "the endpoints of AppLab ${n}, through the gateway on ${APPLAB_GATEWAY_NODEPORT}"
  printf '  %-32s %s\n' "PATH" "STATUS"

  check_endpoint() {
    local label="$1" path="$2"; shift 2
    local code
    code=$(gateway_code "$host" "$path" "$@")
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
  check_endpoint "${APPLAB_BASE_PATH}/api/v1/overview (key)" "${APPLAB_BASE_PATH}/api/v1/overview" -H "Authorization: Bearer ${key}"
  check_endpoint "${APPLAB_BASE_PATH}/api/v1/describe (key)" "${APPLAB_BASE_PATH}/api/v1/describe" -H "Authorization: Bearer ${key}"

  # And the base path is a prefix, not an alias: the same service must not answer
  # outside it. A server that ignored its own prefix would serve every route twice
  # and shadow whatever else owns the root — which is the whole reason the setting
  # exists.
  code=$(gateway_code "$host" /api/v1/config)
  printf '  %-32s %s\n' "/api/v1/config (outside the base path)" "$code"
  [ "$code" != "200" ] || failed="${failed} the-api-answers-outside-its-base-path"

  [ -z "$failed" ] || die "AppLab ${n}: these did not answer as expected through the gateway:${failed}"

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
  log "confirming the settings the chart passed to AppLab ${n} actually arrived"
  body=$(curl -s --max-time 5 -H "Host: ${host}" \
    "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}${APPLAB_BASE_PATH}/api/v1/config" 2>/dev/null || true)

  # jq is not assumed present on a runner; the raw JSON is matched instead.
  expect_config() {
    local label="$1" want="$2"
    printf '%s' "$body" | grep -qF -- "$want" \
      || config_problems="${config_problems} ${label}"
  }

  # The host and the path prefix are what this script sets, and a chart default
  # surviving in either would put the apps somewhere nobody is looking.
  expect_config "the-host" "${host}"
  [ -z "$APPLAB_PATH_PREFIX" ] || expect_config "the-path-prefix" "${APPLAB_PATH_PREFIX}"
  expect_config "the-namespace" "${ns}"

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
  expect_config "the-base-path-in-the-address-template" "\"domain_template\":\"${host}${APPLAB_BASE_PATH}${APPLAB_PATH_PREFIX}/"

  # And the build pipeline has to be usable, since this environment sets a registry
  # for it. A deployment whose build half silently came up disabled still serves
  # every endpoint above, and fails only when someone pushes — which is exactly the
  # class of failure this check exists to move forward.
  printf '%s' "$body" | grep -q '"build":true' \
    || config_problems="${config_problems} the-build-pipeline-is-not-enabled"

  [ -z "$config_problems" ] || {
    printf '%s\n' "$body" | head -40 | sed 's/^/    /' >&2
    die "AppLab ${n}: the deployment's own settings did not reach the server:${config_problems} — the values above are what it reports, and the chart's defaults are what it should not"
  }
}

# verify_isolation is the check the second installation exists for.
#
# Each installation has an admin key, and an admin key can do anything its
# installation can — including delete every app in it. So the one arrangement
# that must not happen is the two sharing a key set: an installation whose Secret
# was built from the other's key looks completely healthy from the outside and
# hands one installation's owner the other's data.
#
# Nothing about either installation's own checks would catch that. Every endpoint
# answers 200 on both, both tables are green, and the two consoles are on two
# hosts with two keys — one of which simply also opens the other. It is caught
# here, by asking one installation's question of the other and insisting on a
# refusal. A 200 means the keys are not separate, which is worse than any single
# installation being broken.
verify_isolation() {
  local code body
  APPLAB_INSTANCE_N=1
  local host_1; host_1="$(instance_host)"
  local key_1;  key_1="$(instance_key)"
  APPLAB_INSTANCE_N=2
  local host_2; host_2="$(instance_host)"

  log "confirming the two installations do not share a key"
  code=$(gateway_code "$host_2" "${APPLAB_BASE_PATH}/api/v1/overview" -H "Authorization: Bearer ${key_1}")
  printf '  %-32s %s\n' "key 1 on AppLab 2" "$code"
  [ "$code" != "200" ] \
    || die "installation 1's key is accepted by installation 2: the two are one installation with two addresses, and either key can destroy both. Check that the two auth Secrets were built from different APPLAB_API_KEY values — the shared thing would be the Secret, since a Secret lives in a namespace and the two Secrets are separate objects"

  # And the reverse, because a one-way check would pass if installation 2's key
  # set happened to be a superset of installation 1's — which is exactly what a
  # Secret built from a comma-joined pair would look like.
  APPLAB_INSTANCE_N=2
  local key_2; key_2="$(instance_key)"
  code=$(gateway_code "$host_1" "${APPLAB_BASE_PATH}/api/v1/overview" -H "Authorization: Bearer ${key_2}")
  printf '  %-32s %s\n' "key 2 on AppLab 1" "$code"
  [ "$code" != "200" ] \
    || die "installation 2's key is accepted by installation 1: the two are one installation with two addresses, and either key can destroy both"

  # Each key on its own installation, so that "refused everywhere" cannot pass
  # this. Without it, two installations with no usable key at all would look
  # isolated.
  APPLAB_INSTANCE_N=1
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Host: ${host_1}" -H "Authorization: Bearer ${key_1}" \
    "http://127.0.0.1:${APPLAB_GATEWAY_NODEPORT}${APPLAB_BASE_PATH}/api/v1/overview" 2>/dev/null || true)
  printf '  %-32s %s\n' "key 1 on AppLab 1" "$code"
  [ "$code" = "200" ] || die "installation 1's key was refused by installation 1, so the check above proved nothing"

  # The object store prefix, which is the one separation a key check cannot see.
  #
  # This is read out of the running configuration rather than asserted against
  # the helm command line, because the command line is what was asked for and this
  # is what arrived — and the failure mode is a values file that drops it, which
  # is exactly the kind of thing that renders fine and changes nothing.
  #
  # The two are compared to each other as well as to what was requested. Equal
  # prefixes are the collision: both installations come up, both serve, and each
  # one's apps appear in the other's console and overwrite each other's keys.
  APPLAB_INSTANCE_N=1
  local prefix_1; prefix_1="$(instance_slug)"
  APPLAB_INSTANCE_N=2
  local prefix_2; prefix_2="$(instance_slug)"
  [ "$prefix_1" != "$prefix_2" ] \
    || die "both installations were given the object store prefix '${prefix_1}': they share one bucket, so they would read and write each other's apps. Set APPLAB_OBJECT_STORE_PREFIX to something that differs per installation"

  # The configmap is `<fullname>-config` — not the bare fullname, which is the
  # Service and the Deployment. Read from the rendered name rather than guessed:
  # a query against a name that does not exist returns empty, and empty is also
  # what a genuinely unset prefix returns, so a wrong name here would make this
  # check pass for the wrong reason — the one failure mode a check must not have.
  local configured
  configured=$(kubectl -n "applab-1" get configmap "applab-1-config" \
    -o jsonpath='{.data.APPLAB_OBJECT_STORE_PREFIX}' 2>/dev/null || true)
  [ "$configured" = "$prefix_1" ] \
    || die "installation 1's object store prefix is '${configured}', expected '${prefix_1}': with it empty both installations write the same keys in the one bucket, which nothing else here would notice until an app appeared in the wrong console"
}

# ── 6. both installations, in order ─────────────────────────────────────────

# The registry credential is shared by both installations — one registry, one
# credential — so it is checked once here rather than once per installation.
#
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
#
# Created twice above and checked once here, deliberately: the Secret is what is
# per installation, the credential is not, so two logins would ask the registry
# the same question twice and report the same answer.
log "confirming the registry credential is accepted"
if [ -n "$APPLAB_REGISTRY_USERNAME" ] && [ -n "$APPLAB_REGISTRY_PASSWORD" ]; then
  printf '%s' "$APPLAB_REGISTRY_PASSWORD" \
    | docker login "$(registry_host_of)" --username "$APPLAB_REGISTRY_USERNAME" --password-stdin >/dev/null 2>&1 \
    || die "the registry refused the credential: the build would push with it and every app would fail to pull. Check APPLAB_REGISTRY_USERNAME and APPLAB_REGISTRY_PASSWORD"
else
  die "no registry credential was supplied: this environment pushes its images to a real registry, so it needs one. Set APPLAB_REGISTRY_USERNAME and APPLAB_REGISTRY_PASSWORD"
fi

for n in $APPLAB_INSTANCES; do
  APPLAB_INSTANCE_N="$n"
  install_instance
done

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
# one — a VirtualService in an AppLab namespace naming a Gateway in istio-system —
# and the analyzer resolves references cluster-wide, so scoping it to one
# namespace risks missing the very case. The cost is that a problem anywhere fails
# the environment, which is the right trade on a kind cluster this script created
# moments ago and has nothing else in.
#
# It runs once, after both installations, rather than inside install_instance:
# it is a question about the cluster's whole configuration, and the answer it
# would give halfway through installing the second one is not one worth acting
# on. It is also the check that would catch the two releases interfering — a
# route bound to a Gateway that is not there is exactly what a second
# installation typo'd into the first one's namespace would look like.
analyze_exit=0
analyze_output=$(istioctl analyze --all-namespaces 2>&1) || analyze_exit=$?

if [ "$analyze_exit" -ne 0 ] || printf '%s' "$analyze_output" | grep -q 'Analyzers found issues'; then
  printf '%s\n' "$analyze_output" | sed 's/^/    /' >&2
  die "istioctl analyze reports a configuration Istio cannot program; the detail above names what is wrong"
fi
show "istioctl analyze (the configuration is programmable)" \
  printf '%s\n' "$analyze_output"

# One installation at a time, and fully, before the next is asked about: the
# tables of two installations interleaved would be two tables nobody can read, and
# a failure is worth reporting against the installation that produced it.
for n in $APPLAB_INSTANCES; do
  APPLAB_INSTANCE_N="$n"
  verify_instance "$n"
done
verify_isolation

# ── 7. publish ──────────────────────────────────────────────────────────────

# The tunnels come up now rather than at the start. Everything above is the
# cluster's own business and is already proven — the pods, the Services, all four
# VirtualServices and every endpoint answered on both hosts — so a tunnel that
# fails here fails on its own, and cannot be mistaken for the platform not having
# come up.
#
# The exception is a quick tunnel or ngrok, whose hostnames had to be known before
# AppLab was installed; resolve_hosts started those already, and publish() notices
# and does nothing for them.
for n in $APPLAB_INSTANCES; do
  APPLAB_INSTANCE_N="$n"
  publish
done

APPLAB_PUBLIC_URL_SHOWN="$(cat "$PUBLIC_URL_FILE")" \
APPLAB_API_KEY_SHOWN="$APPLAB_API_KEY" \
APPLAB_API_KEY_2_SHOWN="$APPLAB_API_KEY_2" \
APPLAB_VERSION_SHOWN="$APPLAB_VERSION" \
APPLAB_PATH_PREFIX_SHOWN="$APPLAB_PATH_PREFIX" \
APPLAB_BASE_PATH_SHOWN="$APPLAB_BASE_PATH" \
APPLAB_TUNNEL_SHOWN="$APPLAB_TUNNEL" \
APPLAB_REGISTRY_SHOWN="$APPLAB_REGISTRY" \
APPLAB_CLUSTER_SHOWN="$APPLAB_CLUSTER_NAME" \
APPLAB_RUNTIME_DIR_SHOWN="$RUNTIME_DIR" \
  bash "$SCRIPT_DIR/summary.sh"

cat <<EOF

=====================================================================
 applab is ready — two installations on one cluster

   ── Installation 1 (namespace applab-1) ──
   Console:  ${public_url_1}
   API key:  ${APPLAB_API_KEY}

   ── Installation 2 (namespace applab-2) ──
   Console:  ${public_url_2}
   API key:  ${APPLAB_API_KEY_2}

   Each console asks for its own address and key; both are kept in your
   browser. Deployed apps are served under
   ${public_url_1}${APPLAB_PATH_PREFIX}/<app>/ and
   ${public_url_2}${APPLAB_PATH_PREFIX}/<app>/.

   The two share nothing but the cluster: separate namespaces, separate
   releases, separate keys, and separate prefixes in the one object store.
   An app created in one does not appear in the other.

   Push an app from a local directory:

     export APPLAB_URL='${public_url_1}'
     export APPLAB_KEY='${APPLAB_API_KEY}'
     applab push myshop

   ...and the same against ${public_url_2} with key ${APPLAB_API_KEY_2},
   which creates a second, independent app of the same name.

=====================================================================
EOF

# ── 8. stay alive ───────────────────────────────────────────────────────────

cleanup() {
  log "ending the environment"
  # Every tail and every agent, not just the last one started: a leaked process
  # here is a leaked process on a runner that is about to be reclaimed anyway,
  # but a tunnel agent left running holds its hostname until it is killed and the
  # next run then argues with it.
  if [ -f "$RUNTIME_DIR/tail.pid" ]; then
    while read -r pid; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done < "$RUNTIME_DIR/tail.pid"
  fi
  for pid in "${tunnel_pids[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
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
  # Every tunnel, because with two of them the second one dying is exactly as
  # fatal as the first: it is one installation's only way in, and a loop that
  # watched a single agent would sleep through the other's death until the
  # deadline. Only when there are any: APPLAB_PUBLIC_HOST means no tunnel was
  # started, and an absent agent is not a stopped one. `kill -0 ""` fails, so
  # without the emptiness test the loop would end the environment on its first
  # pass — which is exactly the case CI runs in.
  for pid in "${tunnel_pids[@]:-}"; do
    [ -z "$pid" ] || kill -0 "$pid" 2>/dev/null \
      || { warn "a tunnel agent stopped; one of the installations is no longer reachable from outside"; break 2; }
  done
  sleep 15
done

log "environment over"
