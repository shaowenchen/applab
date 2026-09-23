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
  --namespace ops-system --create-namespace \
  --set auth.keys[0]="$(openssl rand -hex 32)" \
  --set apps.baseDomain=apps.example.com \
  --set ingress.hosts[0].host=applab.example.com \
  --set build.registry=registry.example.com/apps
```

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

`build.registry` is where built images go. The image for an app is
`<registry>/<app>:<commit>`, so applab needs to create a repository per app under
that prefix.

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
| `auth.keys` | `[]` | **Required.** `openssl rand -hex 32`, one per caller |
| `auth.existingSecret` | `""` | Preferred over `auth.keys`: keeps keys out of the release |
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
| `ingress.hosts` | `applab.example.com` | Change this |
| `persistence.size` | `50Gi` | Holds every app's source |

### Keys

An API key is the whole identity: there is no user store. **A key that
authenticates can also delete** — every app this installation manages. Give each
caller its own key so one can be revoked without disturbing the others; removing
it from the list and upgrading is the whole revocation.

`auth.existingSecret` is worth using. Release values are stored in plain text in
the cluster and are frequently committed.

## After installing

```bash
# The key the chart generated, if you used auth.keys
kubectl -n ops-system get secret applab-auth -o jsonpath='{.data.APPLAB_KEYS}' | base64 -d

# The API contract — what to read before calling anything
curl -s https://applab.example.com/llms.txt

# What the deployment can do
applab config
```

## Installing a development build

Every push to the default branch publishes a chart versioned `<Chart.yaml
version>-dev`, replacing the previous one. Its `appVersion` is the commit it was
built from, so a release that is installed is traceable back to its code.

```bash
helm repo add applab https://www.chenshaowen.com/applab
helm repo update

helm upgrade --install applab applab/applab \
  --namespace ops-system --set ... \
  --version 0.1.0-dev
```

Pin `--version` when you want the dev build. Without it, `helm install` takes the
highest version in the repository, which is whatever release was tagged last —
dev builds sort *below* releases, deliberately, so an install that does not ask
for one never gets one.

Tagged releases (`v0.2.0` → chart `0.2.0`) are published alongside and never
pruned. Only the `-dev` package is replaced, so a development build can never
remove a release.

## Upgrading

```bash
helm upgrade applab applab/applab --namespace ops-system -f my-values.yaml
```

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
