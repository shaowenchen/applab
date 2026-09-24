#!/usr/bin/env bash
#
# Start the image and ask it to work, as the container that runs it.
#
# The gap this closes: CI built and pushed an image on every change and never ran
# it, so everything that differs between a built image and a running one was
# unverified. Two defects reached a cluster that way —
#
#   - the data directory was chmod'ed at boot, which fails for a process that
#     does not own the mounted volume, and the container exited before serving;
#   - `git-http-backend` lives in Alpine's separate `git-daemon` package, so the
#     image started and then refused the first clone or push.
#
# Both are invisible to every other check in this repository. The Go tests run
# against a working copy on a developer's machine, where the data directory is
# owned by the developer and git is a full install; `helm-check.sh` asserts what
# the chart renders, not what the image contains. Only running the image can tell
# you the image is complete.
#
# It runs the container with a volume mounted and no capability overrides, and
# the checks go through the server's own HTTP API rather than inspecting the
# filesystem — those are the two properties that make it able to see what the
# other checks cannot.
#
# Needs docker. Skips loudly without it, since docker is not required to build or
# test the Go code.
set -euo pipefail

cd "$(dirname "$0")/.."

image="${1:-applab-smoke:local}"

if ! command -v docker >/dev/null 2>&1; then
  echo "SKIP: no docker, so the image cannot be run"
  exit 0
fi

if ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "building $image for the smoke test"
  docker build -q -t "$image" . >/dev/null
fi

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "smoke test: $image"

# ── One container, run the way a user runs it ───────────────────────────────
#
# The data volume is the part that matters for the permission half. A docker
# named volume is created root-owned, which is the arrangement the chart's
# fsGroup produces: the process writes inside it and is not its owner. The image's
# own USER applies, so this is uid 1000, not root.
#
# The port is published to a random host port rather than 8080, so a run cannot
# collide with anything already listening on the host.
volume="applab-smoke-data-$$"
name="applab-smoke-$$"
docker volume create "$volume" >/dev/null
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The CLI runs inside the container, against the container's own server: no host
# networking, and no second copy of the client to drift from the one shipping in
# the image. The address is localhost because it is the same container.
docker run -d --name "$name" \
  -v "$volume:/data" \
  -e APPLAB_KEY=smoke-test-key \
  -e APPLAB_URL=http://127.0.0.1:8080 \
  "$image" >/dev/null

# The container's own log is the evidence, so it is what a failure prints: the
# two defects above announced themselves there and nowhere else.
logs() {
  echo "--- container log ---" >&2
  docker logs "$name" >&2 2>&1 || true
}

# ── It comes up ─────────────────────────────────────────────────────────────
#
# `applab config` is the check rather than a /health probe: it authenticates,
# which proves the key reached the server through the Secret path the chart uses,
# and it reports which capabilities the deployment believes it has. It is the
# same call the CLI's own documentation tells an operator to make first.
deadline=$((SECONDS + 60))
until docker exec "$name" /usr/local/bin/applab-cli config >/dev/null 2>&1; do
  if ! docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null | grep -q true; then
    logs
    fail "the container exited before answering; see the log above"
  fi
  if [ "$SECONDS" -ge "$deadline" ]; then
    logs
    fail "applab did not answer applab config within 60s"
  fi
  sleep 1
done
echo "  ok: it boots against a root-owned /data and authenticates over HTTP"

# ── It reports itself able to serve git ─────────────────────────────────────
#
# A missing git-http-backend does not get this far: applab resolves it at boot
# and exits when it cannot, which the wait above catches with the log. This check
# is the other half of the same fact, stated directly rather than inferred from a
# process that stayed up — the capability flag is what the server believes about
# itself, and a deployment that answered HTTP while believing it could not serve
# repositories would be one nothing else here would notice.
#
# It also pins the flag itself, which is cheap and is the kind of thing a
# refactor drops silently.
if ! docker exec "$name" /usr/local/bin/applab-cli config 2>&1 | grep -q 'git=yes'; then
  docker exec "$name" /usr/local/bin/applab-cli config 2>&1 | sed 's/^/    /' >&2 || true
  logs
  fail "the deployment does not report the git capability, so the server believes it cannot serve repositories"
fi
echo "  ok: it reports the git capability"

# ── A repository is actually created and served ─────────────────────────────
#
# The capability flag above is the server's own opinion. This makes an app — which
# creates its repository — and clones it through git's HTTP transport, so the
# opinion is checked against the thing it is about.
#
# A freshly created repository is empty, so the clone succeeds with git's "you
# appear to have cloned an empty repository" warning. That is the expected
# outcome here and not a silent skip: what is under test is that the request
# reaches git-http-backend and is answered, which is exactly what fails when the
# backend is absent, and the empty-clone path exercises the same CGI invocation a
# clone with content does. Cloning is used rather than a push because it needs no
# credentials beyond the key already in play, and no local commit.
docker exec "$name" /usr/local/bin/applab-cli create smoke --port 8080 >/dev/null \
  || fail "the API refused to create an app, so no repository was made"

if ! docker exec -w /tmp "$name" git \
      -c http.extraHeader="Authorization: Bearer smoke-test-key" \
      clone http://127.0.0.1:8080/git/smoke.git cloned 2>&1; then
  logs
  fail "cloning an app's repository over HTTP failed; this is the path git-http-backend serves"
fi
echo "  ok: an app's repository clones over HTTP inside the image"

echo "smoke test passed: $image"
