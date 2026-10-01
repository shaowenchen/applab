# AppLab debugger environment

Start a complete AppLab platform on a GitHub runner — a Kubernetes cluster, an
object store, an Istio gateway and AppLab itself — hand yourself a link, and push
an app. It is built, deployed and served before you open the console.

Two AppLab installations come up, not one: two consoles, two keys, two
namespaces, and one shared object store, on a single throwaway cluster. The
things worth trying here are mostly the things that only happen when there are
two, and the environment is arranged so that they can be.

The point is that AppLab needs real infrastructure before it can do anything: a
cluster to deploy into, a registry to push to, and a gateway to publish through.
This action assembles all of it on a throwaway kind cluster, so the first thing
you have to do is not "install a cluster" but "push an app".

The registry is a real one you supply credentials for, not a container on the
runner. An image that only ever travels inside one machine cannot fail to pull —
and the pull half, with a credential behind it, is exactly what a real deployment
has to get right.

## Using it from another repository

```yaml
name: applab
on:
  workflow_dispatch:

jobs:
  applab:
    runs-on: ubuntu-latest
    # A little longer than the environment's own lifetime, so it stops itself
    # and shuts down cleanly rather than being killed at the runner's ceiling.
    timeout-minutes: 280
    steps:
      - uses: shaowenchen/applab/debugger@master
        with:
          session_hours: '4'
          registry_username: ${{ secrets.DOCKERHUB_USERNAME }}
          registry_password: ${{ secrets.DOCKERHUB_TOKEN }}
          # Optional. Both default to applab-1.chenshaowen.com and
          # applab-2.chenshaowen.com; see "The domains" below, which is the part
          # that has to be arranged outside this repository.
          domain_1: applab-1.example.com
          domain_2: applab-2.example.com
```

That is the whole workflow. Open the run's **Summary** for the two console links
and the two keys, then push something at one of them:

```bash
export APPLAB_URL='https://<installation 1>/applab'
export APPLAB_KEY='<installation 1 key>'

cd any-project-with-a-Dockerfile
applab push myshop
```

The app is served at `<installation 1>/applab/apps/myshop/` and appears in that
console. Point `APPLAB_URL` and `APPLAB_KEY` at installation 2 instead and the
same push creates a second, independent app of the same name.

The environment's base path is part of the address, so `APPLAB_URL` carries it:
a URL without it reaches the host rather than this deployment. The
`applab` CLI is the binary from [the AppLab repository](https://github.com/shaowenchen/applab);
see [the overview](../README.md) for how to install it, or drive the API directly —
`GET /api/v1/describe` is the contract, and it needs no key.

## What it starts

| | What it is |
|---|---|
| **kind cluster** | A throwaway Kubernetes cluster, created for this run and deleted with it. |
| **AppLab** | The published image, installed with this repository's [Helm chart](../charts/applab/README.md) — twice, as two releases in two namespaces (`applab-1`, `applab-2`). |
| **Istio** | The ingress gateway both installations are published through. Install it yourself in a real deployment; here it is part of the environment. |
| **your registry** | Where built images are pushed and pulled from, set by `registry` — `shaowenchen/applab:demo` by default, so images land as `shaowenchen/applab:demo-<app>-<commit>`. The credential is installed as a Secret both halves read: the build pushes with it, each app's Deployment pulls with it. One Secret per namespace, holding the same credential. |
| **MinIO** | The object store, as `applab-object-store:9000` — a container on the host the cluster reaches by name. The image `bitnami/minio` 17.0.21 declares, so what runs here is what a real install of that chart would run. **Shared by both installations**, which is why each one is given a different `objectStore.prefix` — see "Two installations, one object store". |
| **cloudflared** | Two connectors, one per installation, both running the same named Cloudflare tunnel and publishing its two hostnames. Set both `domain_1` and `domain_2` to empty for quick tunnels instead, or `tunnel: ngrok` to use ngrok. |

The Istio objects here come from two places, and only one of them from
`istioctl`. `istioctl install --profile=default` creates the gateway
**Deployment**, its Service and the RBAC, and stops there — a `Gateway` resource
says which ports and hosts that proxy serves, so writing one is the operator's
job. `hack/environment.sh` writes it, with the selector read from the deployment
it has to bind to. Without it every VirtualService here names a gateway that does
not exist: the API server accepts it, the chart renders it, and the proxy serves
nothing.

The `Gateway` carries `hosts: ["*"]`, so **adding a second installation does not
touch it**: each release's console is a VirtualService the chart installs on the
same gateway, matching its own host. That is also why there is still exactly one
NodePort — one `extraPortMappings` entry in the kind config, one port on the
gateway's Service. Istio sets `IgnorePortInHostMatching` on the gateway's route
configuration, so Envoy drops the port from the `Host` header before matching and
the bare hostnames are what match; a second mapping would add a port nothing
routes on.

One hostname serves everything in an installation, and the Istio gateway is what
serves it. AppLab itself — the console, the API under `/api/v1/`, the git
endpoints under `/git/` — is a route the chart installs on that gateway under the
installation's own base path, `/applab` by default, and an app is published under
`/applab/apps/<app>/`: nested inside it, because that is the path the gateway
routes on.

There is nothing in front of the gateway to tell the two apart, because the paths
and the hosts already do. Istio sorts a virtual host's catch-all route to the end
while leaving the rest in order, so the console — which matches everything — is
evaluated only after every app has declined the request, and an app's route is
under a path none of the console's own routes claim.

## Two installations, one object store

Each installation is one Helm release in its own namespace, and everything that
separates them is an ordinary Kubernetes boundary:

| | Installation 1 | Installation 2 |
|---|---|---|
| Namespace | `applab-1` | `applab-2` |
| Helm release | `applab-1` | `applab-2` |
| `fullnameOverride` | `applab-1` | `applab-2` |
| Auth Secret | `applab-keys` | `applab-keys-2` |
| Registry Secret | `applab-registry` | `applab-registry-2` |
| `objectStore.prefix` | `applab-1` | `applab-2` |
| Domain | `domain_1` | `domain_2` |
| API key | `api_key` | `api_key_2` |

The registry Secret is duplicated rather than shared because a Secret belongs to a
namespace, and the two installations are in two of them. Both copies hold the same
credential; the duplication is the API's, not a choice.

**The object store is the one thing that is not per installation**, and it is the
one thing whose misconfiguration is silent. There is a single MinIO container and
a single bucket, because a bucket is infrastructure and one bucket is what a real
deployment has. What keeps the two apart inside it is `objectStore.prefix`: with
it set, AppLab writes under `<prefix>/` and reads only from there. Leave it unset
and both installations write `apps/<id>/…` to the same place — they both come up
healthy, both serve, and each one's apps, keys and build history appear in the
other's console.

That failure is why the environment checks it rather than trusting the `helm`
arguments: after both installations are up, `hack/environment.sh` reads the
prefix back out of installation 1's ConfigMap and fails if it is empty or wrong.

The two keys are likewise checked from the outside, and for the same reason: an
admin key can delete every app its installation manages, so an installation whose
Secret was built from the other's key looks perfectly healthy and hands one
installation's owner the other's data. Nothing an installation reports about
itself would show that, so the script asks installation 2 a question with
installation 1's key and insists on a refusal (in both directions, and with each
key on its own installation, so that "refused everywhere" cannot pass).

## Inputs

| Input | Default | Description |
|---|---|---|
| `api_key` | generated | API key for installation 1. Printed in the summary either way, because it is the deliverable. |
| `api_key_2` | generated | API key for installation 2. Never filled in from `api_key`: the two are separate on purpose, and the values being equal is refused. |
| `session_hours` | `4` | How long the environment may run. `0` means no self-imposed limit, bounded by the job's timeout. |
| `tunnel` | `cloudflare` | `cloudflare` (no account needed) or `ngrok`. |
| `cloudflare_token` | — | Token of the named Cloudflare tunnel that carries both domains; empty starts a quick tunnel per installation instead. |
| `domain_1` | `applab-1.chenshaowen.com` | The domain installation 1 is served under. Named by default, and explained below. |
| `domain_2` | `applab-2.chenshaowen.com` | The domain installation 2 is served under. Set with `domain_1` or not at all. |
| `ngrok_token` | — | ngrok authtoken; required when `tunnel` is `ngrok`. |

Only the keys and `cloudflare_token` are worth passing from a secret: the keys are
generated when left empty, so they need no configuration unless you want
particular ones, and the token is a credential and never a plain input.

The AppLab image tag is not an input. It is the published `latest`, so the
environment runs the newest AppLab — the same tag the release workflow publishes
alongside the version tags, re-resolved on every start because the chart pulls
with `imagePullPolicy: Always`.

### The domains, and the named tunnel they need

`domain_1` and `domain_2` default to `applab-1.chenshaowen.com` and
`applab-2.chenshaowen.com`, and each is used as given: it becomes that
installation's `ingress.host`, so the apps are served under it and the console's
own route matches it too.

These are app configuration, not tunnel configuration. Nothing is passed to
`cloudflared` — a named tunnel already knows its ingress, because you configured
it. That is why the inputs are named for the domains rather than for the tunnel.

It works only with a **named** tunnel — the one whose hostname and ingress
live in your Cloudflare dashboard. A quick tunnel is assigned a random
`trycloudflare.com` hostname by Cloudflare and cannot be given another, so apps
could not be served under the domain you named; the ngrok path here does not pass
the flag a reserved domain needs. Both combinations are refused when the run
starts, rather than after a wait for a hostname that was never coming — which
means **a default run needs `cloudflare_token` set**. Without it the run stops and
says so.

A named tunnel keeps its hostname in its ingress, and Cloudflare never tells the
connector its own name, so the environment cannot discover the address apps should
be served under:

```
this is a named tunnel: Cloudflare does not tell the connector its own hostname
```

That is why the domains are declared rather than found, and why they have defaults
at all: the values have to come from a person, and naming them once is better than
naming them on every run.

The two are one decision rather than two: set both or neither. One named tunnel
cannot also report the random hostname the other installation would need, and an
installation served under a name Cloudflare does not answer for is a link that
does not resolve. The run refuses the mixture.

### Setting up the Cloudflare side — done by hand, not by this repository

**Nothing here can create or check any of this**, and a run will otherwise publish
addresses that resolve to nothing. Before running with the default domains:

1. In the Cloudflare dashboard, create (or reuse) **one named tunnel**.
2. Add **two public hostnames** to that tunnel's ingress, one per domain — e.g.
   `applab-1.chenshaowen.com` and `applab-2.chenshaowen.com`.
3. Point **both** of them at the same origin: `http://127.0.0.1:30080`.

The same origin for both is correct and is the whole arrangement: the two
hostnames are two tunnels to the same gateway on the runner, and the gateway picks
which installation answers from the `Host` header. One tunnel with two hostnames —
and one `cloudflare_token` — is all this needs.

Copy that tunnel's token into the `CLOUDFLARE_TOKEN` secret of the repository
running the workflow. The token is a token of *a* tunnel, not of a hostname, and
since the environment starts one `cloudflared` per installation, both connectors
run the same tunnel.

If the hostnames are not arranged, the run still comes up and its summary still
prints the addresses; they simply will not resolve. Setting both `domain_1` and
`domain_2` to empty is the way to test without any of this — quick tunnels need no
account and their addresses are printed in the summary, though they are minted per
connection and are for trying things rather than keeping.

Setting the domains to empty goes back to quick tunnels: Cloudflare assigns each a
random `trycloudflare.com` hostname, needs no account, and that hostname is both
where the console lives and the domain apps are served under. It is the simpler
path and the flakier one — see below.

## The two things most likely to go wrong

**A run with no `cloudflare_token` stops.** The default domains need a named
tunnel, and a quick tunnel cannot be given others — so the run refuses the
combination rather than publishing addresses that do not match the domains it was
told to serve. Set `cloudflare_token`, or set both domains to empty.

**A quick tunnel is for trying things.** It carries no SLA and its hostname is
minted per connection, so a restart gives different links. That is exactly right
for a session you open now and discard, and wrong for anything you keep — which is
why the defaults are named domains rather than these.

## What it costs

Roughly ten minutes to come up — most of it Istio — and the second installation
adds about as long as the first takes to roll out its pod, which is short: the
image is already in the node's cache by then. About two more minutes for the first
app's build. Everything is deleted when the run ends: the cluster, the images, the
app's source. Nothing survives, which is the point.

## Implementing it yourself

Nothing here is specific to GitHub Actions except the workflow file. The pieces
are ordinary scripts, and they are documented where they are:

| Path | What it does |
|---|---|
| [debugger/action.yml](action.yml) | The composite action: installs kind, kubectl, istioctl, helm and a tunnel agent, then runs the script. |
| [hack/environment.sh](../hack/environment.sh) | The whole environment, in order. Set `APPLAB_PUBLIC_HOST_1` and `APPLAB_PUBLIC_HOST_2` (together) to skip the tunnels and use hostnames you already have, or `APPLAB_DOMAIN_1` and `APPLAB_DOMAIN_2` to name the domains a named tunnel serves apps under. `APPLAB_OBJECT_STORE_PREFIX` is the base of the two per-installation prefixes. |
| [hack/summary.sh](../hack/summary.sh) | Publishes the links and the keys to the job summary. |

## License

See [LICENSE](../LICENSE).
