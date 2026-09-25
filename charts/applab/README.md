# AppLab

Deploy an application to Kubernetes by uploading its source.

This chart installs the control plane: it stores source in a git repository per
app per branch, builds images in the cluster, deploys them and exposes them on a
hostname. Give someone its URL and an API key, and they can ship with one
command.

## Quick start

Three steps, and the order matters: the registry credential has to exist before
AppLab does, because a build reads it from the namespace AppLab runs in.

**1. The namespace, and the registry credential**

```bash
kubectl create namespace ops-system
```

Then the Secret, if the registry needs one — a cluster-local registry usually
does not, in which case skip to step 2 and set `--set build.secret=` there.

```bash
kubectl -n ops-system create secret docker-registry applab-registry \
  --docker-server=registry.example.com \
  --docker-username=<user> \
  --docker-password=<password>
```

The namespace is created here rather than by `--create-namespace` on the install,
because the Secret has to be in it first. The name matters: `applab-registry` is
what the chart expects, so call the Secret that and the install needs to say
nothing more about it. A Secret by another name is passed to the install in step
2.

**2. AppLab**

```bash
helm repo add applab https://www.chenshaowen.com/applab
helm repo update

helm install applab applab/applab \
  --version 0.1.0-dev \
  --namespace ops-system \
  --set auth.key="$(openssl rand -hex 32)" \
  --set objectStore.endpoint=https://s3.us-east-1.amazonaws.com \
  --set objectStore.bucket=applab \
  --set objectStore.accessKey=... --set objectStore.secretKey=... \
  --set ingress.host=applab.example.com \
  --set deploy.gateway=istio-system/istio-ingressgateway \
  --set build.registry=registry.example.com/apps \
  --set build.secret=applab-registry
```

`build.secret` is the registry credential — the one thing that reaches the
private registry in `build.registry`, used both to push the built image and for
the app to pull it. It is shown here at its default, so the line can simply be
deleted: the value is the name of the Secret from step 1, and `applab-registry`
is what
the chart looks for when nothing is set. Change it to reach a Secret by another
name, or set it to `""` when the registry needs no credentials at all.

The bucket is not optional and has no default. AppLab keeps everything in it —
every app, every repository, every commit and every key — so there is nothing for
a release without one to store anything in, and the chart refuses to render. The
server checks the same thing at startup, because a chart is not the only way this
runs. Create the bucket first; AppLab does not create one, because a bucket's
name, region and lifecycle policy belong to whoever runs the platform.

See [A registry the cluster can push to and pull
from](#1-a-registry-the-cluster-can-push-to-and-pull-from) for what happens when
the Secret is named and missing.

`--version` is not optional yet, and leaving it out fails with `chart "applab"
matching  not found in applab index` — which reads like a typo or a stale index
and is neither. See [Installing a development
build](#installing-a-development-build) for why, and for what changes when a
release is tagged.

**3. Use it**, from any project:

```bash
export APPLAB_URL=https://applab.example.com
export APPLAB_KEY=<the key from the release's Secret>

applab push myshop
```

## Before you install

A bucket and three decisions. The bucket is required and has no default, and each
decision is much easier to make now than after, because it changes what the
cluster has to be able to do.

### 0. A bucket, which is where everything lives

AppLab is stateless: every app, every repository, every commit, every build and
every key are objects in a bucket you point it at. Create the bucket before
installing — AppLab does not create one, because a bucket's name, region and
lifecycle policy belong to whoever runs the platform — and give it a credential
that can read, write, delete and list:

```bash
--set objectStore.endpoint=https://s3.us-east-1.amazonaws.com \
--set objectStore.bucket=applab \
--set objectStore.accessKey=... --set objectStore.secretKey=...
```

AWS needs no `endpoint` beyond the region's address and no `pathStyle`. A
self-hosted service such as MinIO is the opposite on both counts:

```bash
--set objectStore.endpoint=http://minio.ops-system:9000 \
--set objectStore.pathStyle=true --set objectStore.insecure=true
```

`insecure` is separate from the scheme on purpose: allowing an http endpoint means
sending the credential and every app's source in the clear, which is a decision to
make on purpose rather than one to infer from a typo.

Two things worth knowing before you put production data in it:

- **Back the bucket up.** It is the only copy of every app's source, and an
  uninstall deliberately does not touch it.
- **The bucket's credential reaches every app's API key**, which live under
  `apps/<id>/key.json`. Before, those were in the cluster with a different
  credential; now they are in the same place as the source.

The layout inside the bucket is one directory per app, and its source is one
repository per branch:

```
apps/<id>/app.json                    the app: name, port, env, status, deployed commit
apps/<id>/key.json                    the app's API key
apps/<id>/commits/<sha>.json          one recorded commit
apps/<id>/builds/<id>.json            one build attempt
apps/<id>/repo/branches/<branch>/     the repository for one branch
```

**`branches/` is where storage multiplies.** Each branch is a repository of its
own, holding a full copy of everything reachable from it — so an app with three
live branches occupies roughly three times one branch's repository. Measured: a
repository of 40 commits of 1 MB files is 39 MB with one branch and 78 MB once a
second branch is pushed, even when that second branch held one extra commit. This
is a deliberate trade, not an oversight: it is what makes a branch a directory you
can list and delete on its own, and git offers no way to share an object database
between branches that could be split this way. Budget for it if apps are large or
branches are many.

Put more than one AppLab in one bucket by setting `objectStore.prefix` to a
per-installation key prefix.

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
namespace AppLab runs in** and name it. Do this before installing — see step 1 of
the quick start:

```bash
kubectl -n ops-system create secret docker-registry applab-registry \
  --docker-server=registry.example.com \
  --docker-username=<user> \
  --docker-password=<password>
```

That name is the default, so an install needs nothing for it; use
`--set build.secret=<name>` for a Secret by another name. One credential for one
registry — the build pushes with it and the app pulls with it. It lives in the
release namespace, where apps run too, so there is no boundary for the credential
to cross and nothing is copied.

A Secret named here and **missing** is refused before anything is created: the
build or the deploy fails and says which Secret and namespace it looked in,
rather than starting a pod that cannot come up. The reason that matters is what
the pod-level failure looks like — a container whose Secret is not there never
starts, so the build sits at `pending` with an empty log, and the app reports
`ImagePullBackOff` naming the image rather than the credential. Both readings
point away from the actual cause.

A registry that is only reachable over plain HTTP, or with a self-signed
certificate, needs `build.insecureRegistry=true` — which is a deliberate weakening
of the guarantee that the image that arrived is the image that was pushed, so it
is off by default.

### 2. A domain, a gateway, and a certificate

`ingress.host` is the one hostname this installation lives on, and it does two
jobs at once: AppLab's own console and API are served under it, and the apps are
published under it too. There is no second domain setting — setting
`ingress.host=applab.example.com` is the whole of the address.

An app with id `shop` is published in one of two shapes:

**A subdomain per app** — the default. `shop` becomes `shop.applab.example.com`.
The certificate has to cover every host under the domain, which in practice means
a wildcard, and a wildcard DNS record to go with it.

**One host, one path per app** — set `apps.pathPrefix`. Every app then shares
`ingress.host` and the path says which is meant:

```bash
--set ingress.host=applab.example.com --set apps.pathPrefix=/apps
# shop is served at https://applab.example.com/apps/shop
```

The reason to want this is the certificate. One host needs one ordinary
certificate, not a wildcard, and nothing has to be reissued as apps are added.
AppLab strips the prefix before the request reaches the app, so an app sees the
paths it would see at a root and needs no change to work under one; it also sets
`X-Forwarded-Prefix` for an app that builds absolute links. The cost is a shared
origin — browser connection limits and cookies are shared between apps, and two
apps cannot both own `/`.

Set `ingress.host=` (empty) to keep everything inside the cluster: no
`VirtualService` is created for the apps and there is no host for the console
either, so the release is reachable by `port-forward` only. The install notes
say so, and print the command.

#### The gateway

Apps are published by attaching a `VirtualService` to an **Istio gateway**. The
gateway is infrastructure you own — it holds the listeners and the certificate
for the domain — and AppLab attaches to it by name and never creates or modifies
it.

```bash
--set deploy.gateway=istio-system/istio-ingressgateway
```

The value is `<namespace>/<name>`, which is how Istio resolves a gateway. The
default is `istio-ingress/istio-ingress`, the naming the official `istio/gateway`
Helm chart produces:

```bash
helm repo add istio https://istio-release.storage.googleapis.com/charts
helm install istio-ingress istio/gateway -n istio-ingress --create-namespace
```

A cluster installed with `istioctl install` instead names it
`istio-system/istio-ingressgateway`, which is the form the quick start above
uses. Either way, **the gateway has to exist before AppLab is installed** and
must already have a listener for `ingress.host`; a missing listener shows up as
an app that deploys, reports healthy and cannot be reached from outside.

TLS is configured on the gateway, not here — there is no per-app certificate
setting to get wrong. An app is served over HTTPS when the gateway has an HTTPS
listener whose certificate covers the host. For a subdomain-per-app install that
certificate must be a wildcard (`*.applab.example.com`); for a `pathPrefix`
install a single-host certificate is enough, which is the reason to prefer it.

#### When the console has its own Ingress

The console and API are reached through a plain Kubernetes Ingress by default
(`ingress.path`, `/applab`), on whatever ingress controller the cluster runs.
Apps are unaffected: they are always published through the gateway above.

A cluster with no ingress controller sets `ingress.enabled=false` and gets the
console on the gateway instead — a `VirtualService` on `ingress.host`, beside the
apps' own. That is why this is one hostname rather than two: the console's
fallback route and the apps' route are the same host, so the gateway only needs
one listener and one certificate for both.

#### Configuration that is refused

The chart fails at render time rather than letting these reach a cluster:

- `ingress.host` set with `deploy.gateway` empty — apps would get hostnames that
  nothing serves, and with no Ingress the console would have no route either.
- `deploy.gateway` not in `<namespace>/<name>` form.
- `apps.pathPrefix` set with `ingress.host` empty — the prefix is the only thing
  telling one app from another on that shared host.

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

**Or** set `build.enabled=false` and run AppLab without the build pipeline. Source
storage, the API and the console all still work, and you can deploy images built
elsewhere. `applab push` will say clearly that this deployment cannot build.

With the pipeline off there is no registry to authenticate to either, so the
registry credential is ignored: `build.secret` is emptied along with
`build.registry`, and no Secret has to exist for an app to deploy.

## What gets installed

| Resource | Why |
|---|---|
| Deployment, Service | the control plane |
| Secret | the API keys |
| Secret | the bucket credential |
| ConfigMap | everything else |
| Role, RoleBinding | AppLab keeps everything in one namespace, and this is all it needs |
| Ingress | how a person reaches the console and the API |
| ServiceMonitor | optional, for `/metrics` |

### Replicas and the bucket

AppLab holds nothing on a replica. Every app, every repository and all of the
history are in the bucket you point `objectStore` at, so `replicaCount` can be
raised for availability and a pod can be replaced at any moment.

The one thing that is not coordinated across replicas is a write to one branch's
repository: the lock that serialises those is per process and per branch, so two
pushes arriving at the same instant on two replicas can lose one of the two —
while two pushes to *different* branches of one app never contend at all, since
each branch is a repository of its own. A push is a rare event and the next build
reads what git actually has, so this is a note rather than a warning — but it is
why one replica is still the default.

**Back up the bucket.** It is the only copy of every app's source.

### The Role

AppLab runs in one namespace and deploys every app into it too, so a namespaced
`Role` and `RoleBinding` are enough — one namespace, one binding, no cluster-wide
grant.

A platform like this is usually bound to a `ClusterRole`, or worse to
`cluster-admin`, because it manages resources across namespaces. AppLab does not,
so the reach of a bug in it is the apps it manages.

The rules in `role.yaml` are exactly what AppLab uses, each with a comment saying
why. Nothing is granted for future convenience. In particular there is no
permission on `namespaces` at all, and none to write pods.

**What this costs:** apps are not isolated from one another by a namespace
boundary. A resource-hungry app affects its neighbours, and an operator reading
`kubectl get pods` sees every app at once. The CPU and memory limits on each app
container are the only bound — which is why `deploy.appResources` exists.

One thing the chart cannot do for you: **an app's pods and a build's are given no
Kubernetes API token** (`automountServiceAccountToken: false`). A token reads
every Secret in its namespace, so an app holding one could reach the API keys and
the registry credentials. AppLab sets this itself, so there is nothing to
configure — but an app deployed into this namespace by hand needs the same.

## Values

See [`values.yaml`](values.yaml) for every option, each with a note on what it
does and why it defaults the way it does. The ones that matter most:

| Value | Default | Note |
|---|---|---|
| `auth.key` | `""` | **Required.** The admin key: `openssl rand -hex 32`. One is enough; see [Keys](#keys) for more |
| `auth.existingSecret` | `""` | Preferred over `auth.key`: keeps the key out of the release, and carries more than one |
| `apps.pathPrefix` | `""` | Serves every app under one path on that host; needs no wildcard certificate |
| `deploy.gateway` | `istio-ingress/istio-ingress` | **Required with a host.** `<namespace>/<name>` |
| `build.enabled` | `true` | `false` runs AppLab without building |
| `build.registry` | `""` | Required when `build.enabled` |
| `build.rootless` | `true` | See the prerequisites above |
| `build.cacheRepoPrefix` | `""` | Registry-side layer cache; a Job has no persistent disk |
| `build.secret` | `applab-registry` | The registry credential: pushes the image and pulls it. `""` for a registry needing none, and ignored when `build.enabled=false` |
| `deploy.appResources` | 2 CPU / 2Gi | Applied to every app AppLab deploys |
| `ingress.host` | `applab.example.com` | **The whole address.** The console, the API and every app. Empty runs internal-only |
| `ingress.path` | `/applab` | The path under it; the server is told the same one |
| `objectStore.endpoint` | `""` | **Required.** The bucket AppLab keeps everything in |
| `objectStore.bucket` | `""` | **Required.** Created by you, not by the chart |
| `objectStore.accessKey` / `secretKey` | `""` | Or `objectStore.existingSecret` |
| `objectStore.pathStyle` | `false` | Most self-hosted services need `true` |
| `scratchSizeLimit` | `2Gi` | Scratch only; nothing durable is written there |

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

**App keys** are created by AppLab itself, one per app, as apps are created. An
app key reaches only its app: it can push, build, deploy, roll back and read
logs, but cannot delete the app and cannot see any other app. That is the
credential to hand to whoever deploys an app, so they never hold one that can
destroy the installation. Read or rotate one with `applab keys <app>` (or
`GET /api/v1/apps/<app>/key`).

An app key is stored in the bucket, under the app's own directory, so nothing is
cached and a rotation takes effect immediately. Two consequences worth knowing:
the bucket credential now reaches every app's key as well as the source, and a
deployment whose object storage is unreachable cannot authenticate at all, admin
keys included. Nothing in the chart has to be configured for this.

### App configuration

Both halves of an app's configuration live in the app's own record in the
bucket, and both reach the container the same way: AppLab writes them into the
Deployment's `env` as plain values.

**Environment variables** — `LOG_LEVEL`, `FEATURE_X` — are the plain half.

**Secrets** — passwords, tokens, connection strings — are the other. No endpoint
returns a value, so a secret cannot be read back through the API, the CLI or the
console; only its name. But **the value is in the Deployment's spec in the
clear**, because that is where a container's environment comes from — anyone who
can read a Deployment in this namespace can read every app's secrets:

```bash
kubectl -n ops-system get deploy <app> -o yaml   # shows every env value
```

That is the cost of an AppLab with no Secret objects, and it is worth knowing
before you put a production credential in one. Kubernetes is where a value can
be kept out of a pod spec, and the two objects that do it — Secret and
`valueFrom.secretKeyRef` — are exactly what this design gives up. The Role's
`secrets` permission remains only so that deleting an app still cleans up Secret
objects left by an older version.

Both halves are managed with `applab env` (or `PUT /api/v1/apps/<app>/secrets`),
and **take effect on the next deploy**. Nothing to configure in the chart.

## After installing

```bash
# The key the chart generated, if you used auth.key
kubectl -n ops-system get secret applab-auth -o jsonpath='{.data.APPLAB_KEYS}' | base64 -d

# What this deployment is, and every endpoint it serves — read this first
curl -s https://applab.example.com/api/v1/describe

# What the deployment can do
applab config
```

## Your first app

Everything below runs from a terminal with no cluster access — that is the whole
point of installing AppLab. The only credential needed up front is the **admin
key** from the step above; the app gets one of its own along the way.

```bash
export APPLAB_URL=https://applab.example.com
export APPLAB_KEY=<the admin key from the release's Secret>
```

`applab` here is the CLI. [Build it from the repository](https://github.com/shaowenchen/applab#development)
(`make build` puts it in `bin/`), or drive the same endpoints with `curl` — the
last part of this section shows that instead.

### 1. Create the app

```bash
applab create shop --port 8080
```

| Flag | Meaning |
|---|---|
| `--port` | the port the app listens on in its container; 8080 if omitted |
| `--dockerfile` | path to the Dockerfile within the source; `Dockerfile` if omitted |
| `--replicas` | how many copies to run |
| `--domain` | an explicit hostname, overriding the one derived from `ingress.host` |

### 2. Take the app's own key

```bash
applab keys shop
```

An app has a key separate from the admin one, and it is the one to use from here
on. It reaches this app and nothing else: it can push, build, deploy and roll
back, but cannot delete the app and cannot see any other. That is what makes it
safe to keep on the machine doing the work, where the admin key — which can
delete every app this installation manages — should not be.

```bash
export APPLAB_APP_KEY=<the key the command printed>
```

Both keys authenticate; the difference is reach. See [Keys](#keys) for the full
comparison.

### 3. Clone the app's repository

Every app *is* a git repository from the moment it is created, so it can be
cloned before it has any content:

```bash
git clone "https://x:$APPLAB_APP_KEY@applab.example.com/git/shop.git"
cd shop
```

Three things about that URL:

- **The username is a placeholder.** `x` can be anything — `git`, the app id.
  Git needs *some* username to send a password at all, and AppLab reads only the
  password.
- **The key is the password.** Which is why the admin key should not be used
  here: a URL lands in shell history and in the repository's own `config` on
  disk.
- **The branch is in the URL.** `/git/shop.git` is the app's active branch;
  another is `/git/shop@dev.git`. Pushing to a branch that does not exist yet
  creates it.

If you would rather the key never enter the URL:

```bash
git -c http.extraHeader="Authorization: Bearer $APPLAB_APP_KEY" \
  clone https://applab.example.com/git/shop.git
```

### 4. Push

The clone is empty, so add something to build. AppLab needs a Dockerfile; the
app listens on the port given at creation:

```bash
cat > Dockerfile <<'EOF'
FROM nginx:alpine
COPY . /usr/share/nginx/html
EOF
echo "hello from shop" > index.html

git add .
git commit -m "first"
git push origin main
```

A push stores the source and nothing else — it does not build. That is deliberate
(`git push` is how you get code in, not how you ask for a deploy), so the next
step is explicit.

### 5. Build and deploy

```bash
applab build shop          # build the newest commit into an image
applab deploy shop         # run that image

# Or both, following the log until it is serving:
applab deploy shop --build
```

When it finishes, `applab deploy` prints the URL the app is served at. If it does
not come up, `applab status shop` says what AppLab recorded and what the cluster
actually has — the two side by side, because the difference between them is the
information — and `applab diagnose shop` is the first thing to read: it walks the
same checks in order and reports the first that fails.

### The same thing with curl

No CLI required. Each numbered step above is one request:

```bash
# 1. Create — the admin key does this.
curl -sS -X POST "$APPLAB_URL/api/v1/apps" \
  -H "Authorization: Bearer $APPLAB_KEY" \
  -H "Content-Type: application/json" \
  -d '{"id":"shop","port":8080}'

# 2. The app's own key, to use from here on.
curl -sS "$APPLAB_URL/api/v1/apps/shop/key" \
  -H "Authorization: Bearer $APPLAB_KEY"
```

Steps 3 and 4 are git itself, unchanged — the clone URL above works the same
either way. Step 5, as one call rather than two:

```bash
curl -sS -X POST "$APPLAB_URL/api/v1/apps/shop/deploy" \
  -H "Authorization: Bearer $APPLAB_APP_KEY" \
  -H "Content-Type: application/json" \
  -d '{"build":true}'
```

`build:true` because a deploy with no image to deploy is refused, and names this
flag as the fix: building is a separate operation, so a deploy that silently
started one would make the response time unpredictable and hide a build failure
behind a deploy. Leaving it out is only correct when the image already exists.

`GET /api/v1/describe` lists every endpoint with what it needs and what it
returns, which is where to look for anything not shown here.

## Installing a development build

Every push to the default branch publishes a chart versioned `<Chart.yaml
version>-dev`, replacing the previous one, and an image tagged with that same
version — so the chart's default `image.tag` is the image it needs, and an
install with nothing overridden runs the build that chart was packaged from.

`appVersion` is the commit the chart was built from, which is what
`app.kubernetes.io/version` carries on every object AppLab creates, so a release
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

An upgrade is a rollout, and that is the whole of it. AppLab holds no state:
every app, every repository, every commit, every build and every key is an object
in the bucket you pointed it at. There is nothing inside the deployment to carry
forward, no schema to migrate, and rolling the image back with the chart is the
whole of a revert.

That is also why the chart mounts no volume. Replacing a pod, losing a node, or
running three replicas changes nothing, because no replica is the only copy of
anything — the bucket is, which is what makes backing it up the one piece of
upkeep this installation needs.

Nothing is released yet: the chart is published only as a prerelease built from
each commit, and the bucket's layout has changed more than once across those
builds. Those changes are not migrated on the way in — each is read by the
version that wrote it, and an app whose history predates one simply reads as
having no commits until it is pushed again. That is the ordinary cost of running
a pre-release, and it is why the shape of the bucket is not something this
document walks you through: it is not part of the contract until there is a
release to break.

## Uninstalling

```bash
helm uninstall applab --namespace ops-system
```

**The apps are not removed.** They are Deployments, Services and
VirtualServices in the release namespace, and the release does not own them —
they carry `applab.io/app`, not helm's release labels. So an uninstall stops
AppLab and leaves every app it deployed running, which is usually what you want
and occasionally a surprise.

**The bucket is not touched.** Everything AppLab remembers — the apps, their
history and their source — is in object storage, which is not part of the
release. An uninstall that emptied the bucket would be an uninstall that deleted
every app's source, so it does not; the bucket is yours to keep or remove.

To remove an installation completely:

```bash
kubectl -n ops-system get deployments,services -l applab.io/app   # what it deployed
kubectl -n ops-system delete deployments,services,jobs -l applab.io/app
kubectl -n ops-system delete virtualservices.networking.istio.io -l applab.io/app
# and the source, which is whatever you pointed objectStore at
```

There are no Secrets to remove with them: AppLab keeps keys and configuration in
the bucket, so an app owns no Secret of its own. The chart's own
`applab-auth` and object-store Secrets belong to the release and go with the
uninstall.

The namespace itself is yours rather than AppLab's — it is where AppLab was
installed, and it may hold other things. AppLab never deletes it.
