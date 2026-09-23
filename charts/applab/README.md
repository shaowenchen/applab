# applab

Deploy an application to Kubernetes by uploading its source.

This chart installs the control plane: it stores source in a git repository per
app, builds images in the cluster, deploys them and exposes them on a hostname. Give someone its URL and an API key, and they can ship
with one command.

## Quick start

```bash
helm repo add applab https://shaowenchen.github.io/applab
helm repo update

helm install applab applab/applab \
  --namespace ops-system --create-namespace \
  --set auth.keys[0]="$(openssl rand -hex 32)" \
  --set apps.baseDomain=apps.example.com \
  --set deploy.gateway=ops-system/gateway \
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

`apps.baseDomain` is what apps are served under: an app with id `shop` becomes
`shop.apps.example.com`. Everything under it needs to resolve to the gateway.

TLS is configured on the **gateway**, not here. The gateway holds the listeners
and the certificate for the whole domain, so an app is served over HTTPS when the
gateway has an HTTPS listener, and there is no per-app certificate setting to get
wrong.

Set `deploy.gateway` to the gateway apps are published through, as
`<namespace>/<name>`. applab attaches a `VirtualService` to it and never creates
or modifies it — the gateway is infrastructure you own.

Setting `apps.baseDomain` without a gateway is refused at render time: it would
produce a `VirtualService` whose empty gateway list Istio reads as mesh-internal
only, so the app would deploy, report healthy and be unreachable from outside.

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

That is the point of the single-namespace model. A platform like this is usually
bound to a `ClusterRole` (or worse, `cluster-admin`) because it manages resources
across namespaces; applab does not, so the worst a bug in it can do is what it
would have done anyway.

The rules in `role.yaml` are exactly what applab uses, each with a comment saying
why. Nothing is granted for future convenience. In particular there is no
permission on `namespaces` at all, and none to write pods.

**What this costs:** apps are not isolated from one another by a namespace
boundary. A resource-hungry app affects its neighbours, and an operator reading
`kubectl get pods` sees every app at once. The CPU and memory limits on each app
container are the only bound — which is why `deploy.appResources` exists.

## Values

See [`values.yaml`](values.yaml) for every option, each with a note on what it
does and why it defaults the way it does. The ones that matter most:

| Value | Default | Note |
|---|---|---|
| `auth.keys` | `[]` | **Required.** `openssl rand -hex 32`, one per caller |
| `auth.existingSecret` | `""` | Preferred over `auth.keys`: keeps keys out of the release |
| `apps.baseDomain` | `""` | Domain apps are served under |
| `deploy.gateway` | `""` | **Required with a base domain.** `<namespace>/<name>` |
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

## Upgrading

```bash
helm upgrade applab applab/applab --namespace ops-system -f my-values.yaml
```

The database schema migrates on start. A newer applab refuses to run against an
older one's schema rather than guessing, so roll the image back with the chart if
an upgrade needs reverting.

### Migrating from a per-app-namespace install

An earlier version gave each app its own namespace (`applab-<app>`). Since every
app now lives in the release namespace, objects left in those old namespaces are
**not** adopted: the applab record still names the app, but the Deployment the old
version created keeps running where it is, invisible to the new one.

Deploying an app recreates it in the release namespace, so the old copy has to be
removed by hand or you will have two of everything:

```bash
kubectl get namespaces -l applab.io/app            # what the old version created
kubectl -n applab-<app> delete deployment,service,ingress --all
kubectl delete namespace -l applab.io/app          # if nothing else lives there
```

Source repositories are unaffected: they live on the volume, not in a namespace.

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
