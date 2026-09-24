# AppLab

Deploy an application to Kubernetes by uploading its source. You get back a URL.

There is no Dockerfile you must write by hand, no CI to configure, no registry to
push to and no manifest to apply. One HTTP call starts the path from a local
working directory to a running application; one call streams the build log; one
call tells you what is running.

Two things are enough to use it: **the base URL** and **an API key**.

```
upload source ──▶ build image ──▶ deploy ──▶ https://shop.apps.example.com
                                             or .../apps/shop
```

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

An app that needs configuration — a database URL, an API token — gets it with
`applab env`, and it takes effect on the next deploy:

```bash
applab env set myshop LOG_LEVEL=debug
applab env secret set myshop DATABASE_URL="$DATABASE_URL"
applab deploy myshop
```

Everything else answers a question:

```bash
applab overview           # the whole platform at a glance
applab list               # what exists
applab status myshop      # what is running, and where
applab logs myshop -f     # watch it
applab pods myshop        # the pods, and why one is not ready
applab events myshop      # Kubernetes events, warnings first
applab diagnose myshop    # why it is not working
applab build myshop       # build a commit without deploying it
applab update myshop --replicas 3
applab rollback myshop    # go back to an earlier upload
```

### The dashboard

The same deployment serves a web dashboard at its root, opening on a platform
overview: how many apps are running, how many need attention, how many builds
have failed, whether the cluster is reachable, and the most recent builds across
every app. From there, **Apps** lists everything and **Builds** is the
platform-wide build history.

It is a client of the same public API — every request it makes is one you could
make with `curl`, and the overview is `GET /api/v1/overview` — so it has no
privileged position and no separate backend. Sign in with the deployment's
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
# The key goes in the URL as the password. Any username works — git needs one to
# send a password at all, and applab reads only the password.
git clone "https://x:$APPLAB_KEY@${APPLAB_URL#http://}/git/shop.git"
```

If you would rather not put the key in the URL — it lands in shell history and in
the repository's `config` on disk — send it as a header instead:

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

Each build runs as a one-shot Kubernetes Job: AppLab operates no build service,
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

Every app runs in the same namespace as AppLab itself (default `ops-system`).
The trade is deliberate, and both halves of it matter.

**What it buys:** AppLab needs a namespaced `Role` and nothing else. It holds no
permission anywhere else in the cluster, so the reach of a bug in it, or of a
compromise, is the apps it manages rather than every workload you run. The chart
renders a Role and a RoleBinding; there is no ClusterRole to audit.

**What it costs:** apps are not isolated from each other by a namespace boundary.
One app taking a node's memory affects its neighbours, and `kubectl get pods`
shows you everything at once. There is no per-app `ResourceQuota` — the CPU and
memory limits on each app's container are the only bound, which is why
`deploy.appResources` exists and why its limits are not generous.

Objects are told apart by the `applab.io/app` label they carry. Deleting an app
removes its objects and leaves everything else alone, including AppLab's own
Deployment, which shares the namespace and carries no app label.

**Neither an app's pods nor a build's are given a Kubernetes API token**
(`automountServiceAccountToken: false`). A token can read every Secret in its
namespace, which here means the API keys and the registry credentials, and an
app is arbitrary code from whoever pushed the source. Nothing AppLab runs needs
to reach the API server: a build fetches its source over HTTP with a single-use
token, and an app just serves traffic.

### How apps are published

Apps are published through an **Istio gateway** as `VirtualService` resources.

The gateway is cluster infrastructure that already exists, holding the listeners
and the certificate for the whole domain. AppLab attaches to it by name
(`deploy.gateway`, default `istio-ingress/istio-ingress`) and never creates or
modifies it. TLS is therefore not per-app configuration: an app is served over
HTTPS when the gateway has an HTTPS listener.

An app is addressed one of two ways, and the deployment picks one:

**A hostname per app** — the default. `apps.baseDomain` is what apps are served
under, so an app with id `shop` is at `shop.apps.example.com`. This needs a
wildcard DNS record and a wildcard certificate.

**One host, one path per app** — set `apps.pathPrefix`. Every app then shares
`apps.baseDomain` and the path says which is meant, so `shop` is at
`apps.example.com/apps/shop`. One ordinary certificate covers any number of
apps, and nothing has to be reissued as the deployment grows.

AppLab strips the prefix before the request reaches the app, so an app sees the
paths it would see at a root and needs no change to work under one; it also sets
`X-Forwarded-Prefix`, which an app that honours it can use to build correct
absolute links. The cost is a shared origin: browser connection limits and
cookies are shared between apps, and two apps cannot both own `/`.

Two failure modes are designed around rather than left to be discovered:

- A base domain without a gateway is refused at startup and at render time,
  because it produces a `VirtualService` whose empty gateway list Istio reads as
  mesh-internal only — an app that deploys, reports healthy, and is unreachable
  from outside.
- The path an app is routed on always ends in a slash. Istio's prefix match is a
  plain string prefix rather than a path-segment match, so a route on
  `/apps/shop` would also claim `/apps/shop-2/anything` — and since the order
  between two VirtualServices on one host is undefined, which app won would not
  even be consistent. `/apps/shop/` cannot match `/apps/shop-2/`.

### AppLab's record versus the cluster

AppLab records what it last did. **The cluster is the source of truth for what is
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
auth refusals) so a scraper can watch AppLab itself. It needs no key — a scraper
holds one awkwardly and these numbers describe the platform rather than any app —
so restrict the path at the network edge where that matters.

## The API

**Start at `GET /api/v1/describe`.** One call answers everything needed to work
with a deployment: where it is, the full endpoint list with the credential each
route requires, what it is wired to, what the presented key may do, which apps
exist and where each is served, and the shortest call for each operation. It
needs no key, so it is also how you find out what a deployment is before you have
one — but the answer is fuller with a key, which is what lets it name the apps
that key reaches.

The endpoint list is generated from the route table in `internal/api/router.go`,
so the served API and the described API cannot drift: there is nothing to
regenerate and no committed copy to go stale. Two routes deliberately stay out of
it — they are destructive maintenance operations, and the list is read by agents
that act on what they find.

Two standing rules for the descriptions in that table:

- **Describe capability, not destruction.** Each entry says what the route does
  and what credential it needs, in enough detail to call it correctly.
- **Keep it true.** A changed parameter or a changed default is a change to the
  route's `Doc` in the same commit. A test asserts every served route appears in
  the list, so a route cannot be added and forgotten.

## Development

```bash
make build-server   # build the control plane
make run            # run with a development key
make check          # fmt, vet, tests — the gate CI runs
make test           # tests only
make coverage       # coverage summary
make docs           # render the documentation site into ./pages
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

The cluster-backed pieces — app keys, app secrets, the deployer — are tested
against `k8s.io/client-go/kubernetes/fake`, which is a real object tracker rather
than a stub. It is close enough to take the label selectors and create/update
semantics seriously, with one divergence worth knowing: a real API server folds a
Secret's `StringData` into `Data` on the way in and the fake does not, so
anything writing a Secret writes `Data` directly and a test asserts that.

Two claims are asserted against the object the code produces rather than a
summary of it, because they are the ones a plausible-looking implementation can
still get wrong:

- **a secret's value never appears in the generated Deployment**, checked by
  searching the whole object, so a leak into a label or an annotation is caught
  as well as one into the env list;
- **the migration brings a version-1 database forward without losing it**, since
  every database already in the field is at version 1 and a fresh-database test
  would never exercise that.

### A whole platform on a runner

The build and deploy halves need real infrastructure, so they are exercised on
one that is built for the purpose and thrown away: [`debugger`](debugger) is a
GitHub Action that creates a `kind` cluster, a registry and an Istio gateway,
installs AppLab from this repository's own chart, and publishes the result
through a tunnel. It is started by hand — it holds a runner for the whole
session, which is not something to spend on every push. Open the run's summary
for a link, and `applab push` works against it.

```
hack/environment.sh   the whole environment, in order
```

One address serves everything, and the gateway is what serves it. Istio routes
one virtual host's catch-all route last while leaving the rest in order
(`route.SortVHostRoutes`), so the console — a catch-all the chart installs on the
base domain — is evaluated only after every app has declined the request, and the
apps match on `/<pathPrefix>/<app>/`, a prefix the console's own paths do not
share. That ordering is defined, so nothing has to sit in front of the gateway to
tell the two apart.

CI checks the commit and builds the image on every change; the environment above
is what a person starts when they want to push an app at something.

### The documentation site

`https://www.chenshaowen.com/applab` is both the Helm repository and these
documents. The site is generated from this repository's own markdown — this file,
the chart's README, and the debugger's — by `cmd/gendocs`, so the pages cannot
drift from the documents people actually edit.

A link in the markdown names a repository file, which is not where anything lives
on the site, so links are rewritten to a resolved target: another page, or GitHub
for a file like the license. A link that resolves to neither **fails the build**,
because a dead end in a document someone is reading is the markdown author's to
fix, not the reader's to discover.

The destination is shared with the chart repository, so the build removes exactly
what the previous one wrote, recorded in a manifest, rather than clearing the
directory. `make docs` renders it into a directory of your own.

## Configuration

See [`.env.example`](.env.example) for every variable. Precedence, lowest to
highest: built-in defaults, an optional YAML file named by `APPLAB_CONFIG`, then
the environment.

A deployment with no key configured refuses to start rather than serving the API
openly.

## Security notes

- **There are two tiers of key.** An **admin key** — what `APPLAB_KEYS` holds —
  can do everything, including delete. Treat it as a credential that can destroy
  data. An **app key** belongs to one app and reaches only that app: it can push,
  build, deploy, roll back and read logs, but it cannot delete the app and cannot
  see any other. Hand the admin key to operators and an app key to whoever
  deploys that app. See [Keys](#keys).
- **Keys are read only from the `Authorization` header** — never a query
  parameter or a form field, both of which are logged and leaked by default.
- **Uploaded archives are untrusted input.** Extraction refuses absolute paths and
  `..` components, and writes through an `os.Root` so a symlink cannot be used to
  escape the destination even though the path string looks clean.
- **Repositories are never public.** Every git request carries a key, like every
  other request, and an app key reaches only its own repository — a repository is
  where the secrets in a project live.
- **App keys are kept in the cluster**, one Secret per app, and nothing is
  cached. Two consequences worth knowing: rotating a key takes effect
  immediately, and **if the API server is unreachable then nothing can
  authenticate, the admin key included.** A deployment with no cluster at all
  has no app keys and works on the admin tier alone; set `APPLAB_KEY` and manage
  apps with it.
- **Configuration is split by sensitivity.** Environment variables are not
  secrets: they are stored with the app, returned by the API, and visible to
  anyone who can read the app's Deployment. Secrets are kept in a Kubernetes
  Secret, are never written into the Deployment — it references the Secret and
  the kubelet substitutes the values inside the container — and **no route
  returns a value**, only the names. That is structural rather than a promise:
  the store the API holds has no method that returns a value. See
  [Configuring an app](#configuring-an-app).
- **A secret's value still reaches the cluster as API traffic** and is stored in
  `etcd` like any Kubernetes Secret. Encryption at rest is the cluster's job.

## Configuring an app

An app gets two kinds of configuration, and which one you want matters.

```bash
# Plain variables — visible in the Deployment, readable back.
applab env set myshop LOG_LEVEL=debug FEATURE_X=on
applab env unset myshop FEATURE_X

# Secrets — never in the Deployment, never readable back.
applab env secret set myshop DATABASE_URL="$DATABASE_URL"
applab env secret unset myshop DATABASE_URL

applab env myshop              # show both
```

The distinction is the whole design. A password belongs in a secret; a log level
does not, and putting it there would make it unreadable for no gain.

| | Variables | Secrets |
|---|---|---|
| Stored in | The database, with the app | A Kubernetes Secret, `applab-env-<app>` |
| In the Deployment | Yes, as env vars | No — referenced by `envFrom` |
| Readable back | Yes, including through the API and console | **Never.** No route returns a value |
| Needs a cluster | No | Yes |

**Changes take effect on the next deploy.** Configuration travels the same path
as code: `applab env set myshop A=1 && applab deploy myshop`. The deployer puts a
hash of the whole configuration into the pod template, which is what makes a
deploy that changed only a secret value actually roll the pods — Kubernetes does
not restart pods for a changed Secret on its own, so without that a rotated
password would be written to the cluster and never reach the running app.

`PORT` cannot be set: it comes from the app's `port` setting, which is also what
the Service targets. Set that instead.

There is no route that returns a secret's value, and none is planned. If one is
lost, set it again — recovery is the same operation.

## Keys

Two tiers, for the two kinds of person who use AppLab: whoever operates the
platform, and whoever deploys one app.

| | Admin key | App key |
|---|---|---|
| Where it comes from | `APPLAB_KEYS`, or `auth.key` in the chart | Created with the app, in a Secret |
| Reaches | Everything | One app |
| Delete that app | Yes | No |
| Clone its repository | Any | Its own |
| Rotate | Restart with a new value | `applab keys rotate <app>`, live |

```bash
applab keys myshop              # read the app's key
applab keys rotate myshop       # replace it; the old one stops working at once
```

An app key is minted when the app is created, so it always exists. Its value can
be read back at any time rather than shown once — deliberately, since a key
nobody can recover is one that has to be rotated the moment it is mislaid.

The same key works everywhere: `Authorization: Bearer <key>` on the API, the
`APPLAB_KEY` environment variable for the CLI, and the sign-in form on the
console. The console reads the tier from the API and shows an app key its own
app only, with no overview, no app list and no delete button.

## Uninstalling

What it takes depends on which of the two ways above you started it, and the two
leave different things behind.

**A local run** is a process and a directory. Stopping `applab` ends the service;
`APPLAB_DATA_DIR` — `./data` in the quick start — is everything else: the
database, and a git repository per app. That directory is the only copy of every
app's source, so deleting it is the point of no return for all of them. Anything
already deployed to a cluster keeps running, because AppLab put it there and does
not own it.

**A chart install** is one command:

```bash
helm uninstall applab --namespace ops-system
```

and the surprise is what it leaves. **The apps are not removed** — they are
Deployments, Services and VirtualServices carrying `applab.io/app` rather than
helm's release labels, so an uninstall stops AppLab and leaves every app it
deployed running. That is usually what you want and occasionally a surprise. The
PersistentVolumeClaim is not removed either, and it holds every app's source.

[The chart's README](charts/applab) has the full teardown, including how to
remove the apps and the volume along with the installation.

## License

See [LICENSE](LICENSE).
