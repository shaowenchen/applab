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
| P3 | Deploy, one namespace, Istio VirtualService | done |
| P4 | Observability: pods, events, logs, metrics | done |
| P5 | Console and CLI | done |
| P6 | Helm chart | done — see [`charts/applab`](charts/applab) |

The chart is verified by `make helm-check`, which renders it and asserts the
things that were wrong: that every setting the server reads reaches it, that
`build.enabled=false` actually disables the pipeline, and that a malformed
configuration is refused at render time rather than deployed.

## Quick start

```bash
make build          # builds bin/applab and bin/applab-cli

export APPLAB_KEY="$(openssl rand -hex 32)"
export APPLAB_DATA_DIR=./data
export APPLAB_BASE_DOMAIN=apps.example.com

./bin/applab
```

Then, from the project you want to deploy:

```bash
export APPLAB_URL=http://localhost:8080
export APPLAB_KEY=<the key above>

applab push myshop        # uploads, builds and deploys this directory
```

That is the whole workflow. `push` creates the app if it is new, packages the
directory, uploads it, follows the build and waits for the rollout, and prints the
URL. Build output and version-control directories are left out automatically.

Everything else answers a question:

```bash
applab list               # what exists
applab status myshop      # what is running, and where
applab logs myshop -f     # watch it
applab diagnose myshop    # why it is not working
applab rollback myshop    # go back to an earlier upload
```

### The console

The same deployment serves a web console at its root. It is a client of the same
public API — every request it makes is one you could make with `curl` — so it has
no privileged position and no separate backend. Sign in with the deployment's
address and a key; both are kept in your browser.

## Or by hand

If you would rather not install the CLI:

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

### One namespace, for everything

Every app is deployed into the same namespace applab itself runs in (default
`ops-system`). That is a deliberate trade, and it is worth being clear about both
halves.

**What it buys:** applab needs a namespaced `Role` and nothing else. It holds no
permission anywhere else in the cluster, so a bug here — or a compromise — reaches
the apps it manages rather than every workload you run. The chart renders a Role
and a RoleBinding, and there is no ClusterRole to audit.

**What it costs:** apps are not isolated from each other by a namespace boundary.
One app taking a node's memory affects its neighbours, and `kubectl get pods`
shows you everything at once. There is no per-app `ResourceQuota`; the CPU and
memory limits on each app's container are the only bound, which is why
`deploy.appResources` exists and why its limits are not generous.

Objects are told apart by their `applab.io/app` label rather than by where they
live. Deleting an app removes its objects and leaves the rest alone — including
applab's own Deployment, which sits in the same namespace with no app label.

Because the namespace no longer separates an app from applab's own credentials,
**neither an app's pods nor a build's are given a Kubernetes API token**
(`automountServiceAccountToken: false`). Every pod gets one by default, and in
this namespace that token can read every Secret — the API keys, the registry
credentials. An app is arbitrary code from whoever pushed the source, so it has
no business holding a credential that reaches applab itself.

### How apps are published

Through an **Istio gateway**, not an Ingress: the routing in front of these apps
is Istio, and an Ingress applab created would be ignored by it — the app would
deploy, report healthy, and be unreachable.

The gateway is cluster infrastructure that already exists. It holds the listeners
and the certificate for the whole domain, and applab attaches a `VirtualService`
to it by name (`deploy.gateway`, e.g. `ops-system/gateway`) without ever creating
or modifying it. TLS is therefore not applab's business and has no per-app
setting: an app is served over HTTPS when the gateway has an HTTPS listener.

Setting a base domain without a gateway is refused at render time, because the
result would be a `VirtualService` whose empty gateway list Istio reads as
mesh-internal only.

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

`make helm-check` needs `helm`, which is not required to build or test the Go
code, only to check the chart. It renders the chart and asserts what the
rendering has to contain — a chart whose templates are wrong still renders, so
"it rendered" is not evidence of anything.

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
