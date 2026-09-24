# applab

Deploy an application to Kubernetes by uploading its source.

This chart installs the control plane: it stores source in a git repository per
app, builds images in the cluster, deploys them and exposes them on a hostname. Give someone its URL and an API key, and they can ship
with one command.

## Quick start

```bash
helm repo add applab https://www.chenshaowen.com/applab
helm repo update

helm install applab applab/applab \
  --version 0.1.0-dev \
  --namespace ops-system --create-namespace \
  --set auth.key="$(openssl rand -hex 32)" \
  --set apps.baseDomain=apps.example.com \
  --set ingress.host=applab.example.com \
  --set build.registry=registry.example.com/apps
```

`--version` is not optional yet, and leaving it out fails with `chart "applab"
matching  not found in applab index` — which reads like a typo or a stale index
and is neither. See [Installing a development
build](#installing-a-development-build) for why, and for what changes when a
release is tagged.

Then, from any project:

```bash
export APPLAB_URL=https://applab.example.com
export APPLAB_KEY=<the key from the release's Secret>

applab push myshop
```

## Before you install

Three decisions are much easier to make now than after, because each one changes
what the cluster has to be able to do.

### 1. A registry the cluster can push to and pull from

`build.registry` is where built images go. How an app's image is named depends on
how much path the registry has — a repository can only be extended so far before
the registry rejects it, so a deep prefix puts the app in the tag instead:

| `build.registry` | the image for app `shop` |
|---|---|
| `registry.example.com/apps` | `registry.example.com/apps/shop:<commit>` |
| `registry.example.com` | `registry.example.com/shop:<commit>` |
| `shaowenchen` | `shaowenchen/shop:<commit>` |
| `shaowenchen/applab` | `shaowenchen/applab:shop-<commit>` |

Either way the tag names the commit, which is what lets a rollback reuse an image
rather than rebuild it.

If the registry needs credentials, create a `docker-registry` Secret **in the
namespace applab runs in** and name it:

```bash
kubectl -n ops-system create secret docker-registry regcred \
  --docker-server=registry.example.com \
  --docker-username=<user> \
  --docker-password=<password>
```

```bash
--set build.pushSecret=regcred --set deploy.imagePullSecret=regcred
```

Two settings rather than one because pushing and pulling can need different
credentials, and a registry that is open to pull but not to push only needs the
build one.

Both names refer to a Secret in the release namespace. Apps run in that same
namespace, so there is no boundary for the credential to cross and nothing is
copied. A Secret named here and missing is discovered at the first build or
deploy rather than at install.

A registry that is only reachable over plain HTTP, or with a self-signed
certificate, needs `build.insecureRegistry=true` — which is a deliberate weakening
of the guarantee that the image that arrived is the image that was pushed, so it
is off by default.

### 2. A domain, and a certificate for it

`apps.baseDomain` is what apps are served under. Everything under it needs to
resolve to the gateway.

There are two ways to put an app on it:

**A subdomain per app** — the default. An app with id `shop` becomes
`shop.apps.example.com`. The certificate has to cover every host under the
domain, which in practice means a wildcard, and a wildcard DNS record to go with
it.

**One host, one path per app** — set `apps.pathPrefix`. Every app then shares
`apps.baseDomain` and the path says which is meant:

```bash
--set apps.baseDomain=apps.example.com --set apps.pathPrefix=/apps
# shop is served at https://apps.example.com/apps/shop
```

The reason to want this is the certificate. One host needs one ordinary
certificate, not a wildcard, and nothing has to be reissued as apps are added.
applab strips the prefix before the request reaches the app, so an app sees the
paths it would see at a root and needs no change to work under one; it also sets
`X-Forwarded-Prefix` for an app that builds absolute links. The cost is a shared
origin — browser connection limits and cookies are shared between apps, and two
apps cannot both own `/`.

TLS is configured on the **gateway**, not here. The gateway holds the listeners
and the certificate for the whole domain, so an app is served over HTTPS when the
gateway has an HTTPS listener, and there is no per-app certificate setting to get
wrong.

Set `deploy.gateway` to the gateway apps are published through, as
`<namespace>/<name>`. It defaults to `istio-ingress/istio-ingress` — the naming
the official `istio/gateway` chart produces. A cluster installed with
`istioctl install` names it `istio-system/istio-ingressgateway` instead, and has
to say so:

```bash
--set deploy.gateway=istio-system/istio-ingressgateway
```

applab attaches a `VirtualService` to that gateway and never creates or modifies
it — the gateway is infrastructure you own.

Setting `apps.baseDomain` without a gateway is refused at render time: it would
produce a `VirtualService` whose empty gateway list Istio reads as mesh-internal
only, so the app would deploy, report healthy and be unreachable from outside.
So is setting `apps.pathPrefix` without a base domain, since the prefix is the
only thing telling one app from another on that shared host.

### 3. Whether the cluster can build

This is the one to check before installing, because a build that cannot run fails
in a way that looks like nothing is happening.

Builds run as a Kubernetes Job per build, with **BuildKit in rootless mode**. That
is the default because a build executes code from whoever pushed the source, and a
privileged build container is a container breakout away from the node.

Rootless BuildKit needs the nodes to permit **unprivileged user namespaces**. The
requirements, from BuildKit's own documentation:

| Requirement | Why |
|---|---|
| `user.max_user_namespaces` > 0 on the node | RootlessKit creates a user namespace; with this at 0 it cannot |
| seccomp **unconfined** for the build container | The default seccomp profile blocks the `unshare` and `mount` syscalls the namespace needs |
| AppArmor **unconfined** for the build container | AppArmor blocks the mounts RootlessKit performs |
| Kernel ≥ 5.11, or `fuse-overlayfs` + `/dev/fuse` | Overlayfs in a user namespace needs a recent kernel; older ones fall back |
| On Ubuntu 24.04+: `kernel.apparmor_restrict_unprivileged_userns=0` | Ubuntu restricts unprivileged user namespaces by default |

The chart sets the seccomp and AppArmor profiles already. The node-level settings
are yours.

**To check before installing**, on any node:

```bash
sysctl user.max_user_namespaces
# 0 means rootless builds cannot work on this node
```

**If the cluster cannot satisfy this**, set `build.rootless=false`. Build
containers then run privileged. It works everywhere, and it means a build — which
is arbitrary code from whoever pushed the source — has the run of the node. Prefer
fixing the node setting.

**Or** set `build.enabled=false` and run applab without the build pipeline. Source
storage, the API and the console all still work, and you can deploy images built
elsewhere. `applab push` will say clearly that this deployment cannot build.

## What gets installed

| Resource | Why |
|---|---|
| Deployment, Service | the control plane. **One replica**, see below |
| PersistentVolumeClaim | the database and every app's git repository |
| Secret | the API keys |
| ConfigMap | everything else |
| Role, RoleBinding | applab keeps everything in one namespace, and this is all it needs |
| Ingress | how a person reaches the console and the API |
| ServiceMonitor | optional, for `/metrics` |

### One replica, deliberately

`replicaCount` must be 1, and the chart refuses anything else.

applab keeps its state in SQLite on a ReadWriteOnce volume. SQLite cannot be
shared between processes over a network filesystem — two pods writing one file
across it corrupts the file — so a second replica would not merely be wasteful,
it would be unsafe. Scaling out means moving to a database built for it; until
then, one replica is the honest answer rather than a limitation to work around.

The volume is the only copy of every app's source. **Back it up.**

### The Role

applab runs in one namespace and deploys every app into it too, so a namespaced
`Role` and `RoleBinding` are enough — one namespace, one binding, no cluster-wide
grant.

A platform like this is usually bound to a `ClusterRole`, or worse to
`cluster-admin`, because it manages resources across namespaces. applab does not,
so the reach of a bug in it is the apps it manages.

The rules in `role.yaml` are exactly what applab uses, each with a comment saying
why. Nothing is granted for future convenience. In particular there is no
permission on `namespaces` at all, and none to write pods.

**What this costs:** apps are not isolated from one another by a namespace
boundary. A resource-hungry app affects its neighbours, and an operator reading
`kubectl get pods` sees every app at once. The CPU and memory limits on each app
container are the only bound — which is why `deploy.appResources` exists.

One thing the chart cannot do for you: **an app's pods and a build's are given no
Kubernetes API token** (`automountServiceAccountToken: false`). A token reads
every Secret in its namespace, so an app holding one could reach the API keys and
the registry credentials. applab sets this itself, so there is nothing to
configure — but an app deployed into this namespace by hand needs the same.

## Values

See [`values.yaml`](values.yaml) for every option, each with a note on what it
does and why it defaults the way it does. The ones that matter most:

| Value | Default | Note |
|---|---|---|
| `auth.key` | `""` | **Required.** The admin key: `openssl rand -hex 32`. One is enough; see [Keys](#keys) for more |
| `auth.existingSecret` | `""` | Preferred over `auth.key`: keeps the key out of the release, and carries more than one |
| `apps.baseDomain` | `""` | Domain apps are served under |
| `apps.pathPrefix` | `""` | Serves every app under one path on that host; needs no wildcard certificate |
| `deploy.gateway` | `istio-ingress/istio-ingress` | **Required with a base domain.** `<namespace>/<name>` |
| `build.enabled` | `true` | `false` runs applab without building |
| `build.registry` | `""` | Required when `build.enabled` |
| `build.rootless` | `true` | See the prerequisites above |
| `build.cacheRepoPrefix` | `""` | Registry-side layer cache; a Job has no persistent disk |
| `build.pushSecret` | `""` | Registry credentials for the build Job to push with |
| `deploy.imagePullSecret` | `""` | Registry credentials for the app to pull with |
| `deploy.appResources` | 2 CPU / 2Gi | Applied to every app applab deploys |
| `ingress.host` | `applab.example.com` | The host the console and API are reached at |
| `ingress.path` | `/applab` | The path under it; the server is told the same one |
| `persistence.size` | `50Gi` | Holds every app's source |

### Keys

There are two tiers, and the difference is reach.

**Admin keys** are what this chart configures. A key that authenticates can also
delete — every app this installation manages — so give it to nobody you would not
give the installation to.

`auth.key` holds one. That is enough to install with, and one is the honest
default: it is a single credential, and a second copy in the release's values is
a second place to leak from rather than a second identity. To give several people
their own key, so one can be revoked without disturbing the others, point
`auth.existingSecret` at a Secret you manage, carrying an `APPLAB_KEYS` key whose
value is the comma-separated list. Removing an entry from it and upgrading is the
whole revocation.

That is also why `auth.existingSecret` is worth using even with one key. Release
values are stored in plain text in the cluster and are frequently committed.

**App keys** are created by applab itself, one Secret per app, as apps are
created. An app key reaches only its app: it can push, build, deploy, roll back
and read logs, but cannot delete the app and cannot see any other app. That is
the credential to hand to whoever deploys an app, so they never hold one that can
destroy the installation. Read or rotate one with `applab keys <app>` (or
`GET /api/v1/apps/<app>/key`).

App keys live in the cluster, so nothing is cached and a rotation takes effect
immediately — and a deployment whose API server is unreachable cannot
authenticate at all, admin keys included. They need the `secrets` permission the
Role already grants; nothing here has to be widened.

### App configuration

An app's configuration is split by sensitivity, and only one half is yours to
configure here.

**Environment variables** — `LOG_LEVEL`, `FEATURE_X` — are stored in applab's
database and are visible in an app's Deployment to anyone who can read it. They
need no setting.

**Secrets** — passwords, tokens, connection strings — are created by applab, one
Secret per app named `applab-env-<app>`, as they are set. They are never written
into the Deployment: it references the Secret through `envFrom` and the kubelet
substitutes the values inside the container. No endpoint returns a value, so a
secret cannot be read back through the API, the CLI or the console — only its
name.

Both halves are managed with `applab env` (or `PUT /api/v1/apps/<app>/secrets`),
and **take effect on the next deploy**. Nothing to configure in the chart.

Worth knowing: secrets reach the cluster as API traffic and are stored in `etcd`
like any Kubernetes Secret. Encryption at rest is the cluster's job, not
applab's.

## After installing

```bash
# The key the chart generated, if you used auth.key
kubectl -n ops-system get secret applab-auth -o jsonpath='{.data.APPLAB_KEYS}' | base64 -d

# The API contract — what to read before calling anything
curl -s https://applab.example.com/llms.txt

# What the deployment can do
applab config
```

## Installing a development build

Every push to the default branch publishes a chart versioned `<Chart.yaml
version>-dev`, replacing the previous one, and an image tagged with that same
version — so the chart's default `image.tag` is the image it needs, and an
install with nothing overridden runs the build that chart was packaged from.

`appVersion` is the commit the chart was built from, which is what
`app.kubernetes.io/version` carries on every object applab creates, so a release
that is installed is traceable back to its code. It is deliberately **not** the
image tag: it names a commit, and no image is published under a bare commit.

```bash
helm repo add applab https://www.chenshaowen.com/applab
helm repo update

helm upgrade --install applab applab/applab \
  --namespace ops-system --set ... \
  --version 0.1.0-dev
```

**`--version` is required, and this is why.** `0.1.0-dev` is a prerelease — a
version with a hyphen and a suffix — and Helm leaves prereleases out of version
resolution. It does not rank them below releases and fall back to one; it does
not see them at all. So with only dev builds published, `helm install applab
applab/applab` resolves no version and fails with:

```
Error: INSTALLATION FAILED: chart "applab" matching  not found in applab index.
(try 'helm repo update'): no chart version found for applab-
```

The `helm repo update` in that message is a red herring: the index is fine, and
running it changes nothing. The `matching` and `applab-` with nothing after them
are the version constraint coming out empty, which is the tell.

Once a release is tagged the picture changes, because a tagged version has no
hyphen and is therefore visible. Then the plain form works and takes the highest
release — `helm install applab applab/applab` — while `--version 0.1.0-dev`
still asks for the dev build specifically. Until then, there is nothing to fall
back to.

Tagged releases (`v0.2.0` → chart `0.2.0`) are published alongside and never
pruned. Only the `-dev` package is replaced, so a development build can never
remove a release.

## Upgrading

```bash
helm upgrade applab applab/applab \
  --namespace ops-system -f my-values.yaml \
  --version 0.1.0-dev
```

`--version` is needed here for the same reason it is on install, while only dev
builds are published: Helm does not resolve a prerelease unless it is asked for
by name. An upgrade without it fails the same way an install does.

The database schema migrates on start. A newer applab refuses to run against an
older one's schema rather than guessing, so roll the image back with the chart if
an upgrade needs reverting.

## Uninstalling

```bash
helm uninstall applab --namespace ops-system
```

**The apps are not removed.** They are Deployments, Services and
VirtualServices in the release namespace, and the release does not own them —
they carry `applab.io/app`, not helm's release labels. So an uninstall stops
applab and leaves every app it deployed running, which is usually what you want
and occasionally a surprise.

The PersistentVolumeClaim is not removed either, and it holds the only copy of
every app's source.

To remove an installation completely:

```bash
kubectl -n ops-system get deployments,services -l applab.io/app   # what it deployed
kubectl -n ops-system delete deployments,services,jobs,secrets -l applab.io/app
kubectl -n ops-system delete virtualservices.networking.istio.io -l applab.io/app
kubectl -n ops-system delete pvc applab                           # and the source with it
```

The namespace itself is yours rather than applab's — it is where applab was
installed, and it may hold other things. applab never deletes it.
