# applab

Deploy an application to Kubernetes by uploading its source. You get back a URL.

There is no Dockerfile you must write by hand, no CI to configure, no registry to
push to and no manifest to apply. One HTTP call starts the path from a local
working directory to a running application; one call streams the build log; one
call tells you what is running.

Two things are enough to use it: **the base URL** and **an API key**.

```
upload source ──▶ build image ──▶ deploy ──▶ https://<app>.<domain>
```

## Status

| Phase | Scope | State |
|---|---|---|
| P0 | Skeleton, auth, contract (`llms.txt`), app CRUD | done |
| P1 | Source storage: git repositories, tarball ingest, chunked upload, git over HTTP | done |
| P2 | BuildKit build pipeline | done |
| P3 | Deploy, namespaces, ingress | done |
| P4 | Observability: pods, events, logs, metrics | done |
| P5 | Console and CLI | not started |
| P6 | Helm chart | not started |

## Quick start

```bash
make build-server

export APPLAB_KEY="$(openssl rand -hex 32)"
export APPLAB_DATA_DIR=./data
export APPLAB_BASE_DOMAIN=apps.example.com

./bin/applab
```

Then, from the project you want to deploy:

```bash
# Create the app (once).
curl -sS -X POST "$APPLAB_URL/api/v1/apps" \
  -H "Authorization: Bearer $APPLAB_KEY" \
  -H "Content-Type: application/json" \
  -d '{"id":"shop","port":8080}'

# Push the source.
tar czf - . | curl -sS -X POST "$APPLAB_URL/api/v1/apps/shop/source?message=first" \
  -H "Authorization: Bearer $APPLAB_KEY" \
  -H "Content-Type: application/gzip" \
  --data-binary @-
```

Clone what you pushed:

```bash
git -c http.extraHeader="Authorization: Bearer $APPLAB_KEY" \
  clone "$APPLAB_URL/git/shop.git"
```

## How it works

### Source is a git repository

Every app's source lives in its own real git repository, hosted here. An upload
arrives as a tarball — because a `curl` works from anywhere, including an agent
or a container with no git credential — but what it leaves behind is a genuine
commit. You can clone it, diff two uploads, browse the history, and check out any
earlier revision with ordinary git tools.

The tarball is how the bytes travel. The commit is what is recorded.

### A build is a Job, not a daemon

Each build runs as a one-shot Kubernetes Job: applab operates no build service,
and a build's resources are released the moment it ends. The Job has two
containers — an init container that downloads one commit's source, and a builder
that runs BuildKit rootless over it — and **the builder never sees a credential**.
The fetch uses a single-use token scoped to one commit, because a build runs
arbitrary code from the uploaded Dockerfile and must not hold anything worth
stealing.

Layers are cached in the registry (`<cache-prefix>/<app>:buildcache`). A Job has
no persistent disk, so without a registry-side cache every rebuild would start
from nothing.

### Commits are what you deploy

A deploy names a commit. Rolling back means deploying an earlier one, and if that
commit was built before, its image is reused rather than rebuilt — which is what
makes a rollback fast and unable to fail for a reason the original build did not.

`POST /apps/{app}/deploy` deploys a commit; `POST /apps/{app}/rollback` returns to
an earlier one. A deploy of a commit that has never been built is refused rather
than quietly building, so a caller always knows which operation is running and a
build failure is never reported as a deploy failure.

### One namespace per app

Each app gets its own namespace, named with the deployment's prefix. Isolation is
the point, and it makes deletion honest: removing an app removes its namespace, so
nothing is left behind.

### applab's record versus the cluster

applab records what it last did. **The cluster is the source of truth for what is
running.** When the two disagree, believe the cluster — `/apps/{app}/status`
reports both side by side rather than picking one.

### Finding out what went wrong

`GET /apps/{app}/diagnose` answers "why is my app down" in one call: the pods and
their per-container state, the Kubernetes events, and the log that explains it,
ordered so the most likely cause comes first.

Two details make it work in practice. A crash loop's reason is read from the
**previous** container instance, because the current one is merely "waiting" and
says nothing. And warnings are listed before routine notices — a namespace's
events are mostly image pulls and scheduling notes, and burying the one that
explains the failure among them is the difference between a useful answer and a
list to dig through.

`/metrics` publishes the platform's own counters (requests, builds, deploys,
auth refusals) so a scraper can watch applab itself. It needs no key — a scraper
holds one awkwardly and these numbers describe the platform rather than any app —
so restrict the path at the network edge where that matters.

## The API

[`api/llms.txt`](api/llms.txt) is the authoritative, endpoint-by-endpoint
reference — query parameters, request bodies, response shapes and side effects.
It is served at `/llms.txt`, generated from the route table in
`internal/api/router.go` so the served API and the documented API cannot drift.

Two standing rules for its content:

- **Describe capability, not destruction.** It is read by agents that act on what
  they find, so it documents the functional endpoints in enough detail to call
  them correctly.
- **Keep it true.** A new route, a changed parameter or a changed default is an
  `llms.txt` change in the same commit. Run `make llms` and commit the result;
  `make check` fails if you forget.

## Development

```bash
make build-server   # build the control plane
make run            # run with a development key
make check          # fmt, vet, llms.txt consistency, tests — the gate CI runs
make test           # tests only
make coverage       # coverage summary
make llms           # regenerate api/llms.txt from the route table
```

Requires Go 1.24+ and `git` on `PATH`. Nothing else: the SQLite driver is pure Go,
so `CGO_ENABLED=0` and the binary is static.

### Verification without a cluster

Everything except the build and deploy halves runs and is tested locally. The
source layer is exercised with the real `git` binary over a real HTTP server —
`internal/gitx` clones and pushes in its tests — and the API is driven through its
own handler with `httptest`.

`helm template` needs `helm`, which is not required to build or test the Go code,
only to check the chart.

## Configuration

See [`.env.example`](.env.example) for every variable. Precedence, lowest to
highest: built-in defaults, an optional YAML file named by `APPLAB_CONFIG`, then
the environment.

A deployment with no key configured refuses to start rather than serving the API
openly.

## Security notes

- **A key is the whole identity, and there is one tier.** A key that
  authenticates can also delete. Treat it as a credential that can destroy data.
- **Keys are read only from the `Authorization` header** — never a query
  parameter or a form field, both of which are logged and leaked by default.
- **Uploaded archives are untrusted input.** Extraction refuses absolute paths and
  `..` components, and writes through an `os.Root` so a symlink cannot be used to
  escape the destination even though the path string looks clean.
- **Repositories are never public.** Every git request carries a key, like every
  other request.

## License

See [LICENSE](LICENSE).
