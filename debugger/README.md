# AppLab debugger environment

Start a complete AppLab platform on a GitHub runner — a Kubernetes cluster, an
object store, an Istio gateway and AppLab itself — hand yourself a link, and push
an app. It is built, deployed and served before you open the console.

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
```

That is the whole workflow. Open the run's **Summary** for the console link and
the API key, then push something at it:

```bash
export APPLAB_URL='https://<the link>/applab'
export APPLAB_KEY='<the key>'

cd any-project-with-a-Dockerfile
applab push myshop
```

The app is served at `<the link>/applab/apps/myshop/` and appears in the console.
The environment's base path is part of the address, so `APPLAB_URL` carries it:
a URL without it reaches the host rather than this deployment. The
`applab` CLI is the binary from [the AppLab repository](https://github.com/shaowenchen/applab);
see [the overview](../README.md) for how to install it, or drive the API directly —
`GET /api/v1/describe` is the contract, and it needs no key.

## What it starts

| | What it is |
|---|---|
| **kind cluster** | A throwaway Kubernetes cluster, created for this run and deleted with it. |
| **AppLab** | The published image, installed with this repository's [Helm chart](../charts/applab/README.md). |
| **Istio** | The ingress gateway apps are published through. Install it yourself in a real deployment; here it is part of the environment. |
| **your registry** | Where built images are pushed and pulled from, set by `registry` — `shaowenchen/applab:demo` by default, so images land as `shaowenchen/applab:demo-<app>-<commit>`. The credential is installed as a Secret both halves read: the build pushes with it, each app's Deployment pulls with it. |
| **MinIO** | The object store, as `applab-object-store:9000` — a container on the host the cluster reaches by name. The image `bitnami/minio` 17.0.21 declares, so what runs here is what a real install of that chart would run. |
| **cloudflared** | A named tunnel, published at `domain`. Set `domain` to empty for a quick tunnel instead, or `tunnel: ngrok` to use ngrok. |

Two Istio objects are involved here and only one comes from `istioctl`.
`istioctl install --profile=default` creates the gateway **Deployment**, its
Service and the RBAC, and stops there — a `Gateway` resource says which ports and
hosts that proxy serves, so writing one is the operator's job. `hack/environment.sh`
writes it, with the selector read from the deployment it has to bind to. Without
it every VirtualService here names a gateway that does not exist: the API server
accepts it, the chart renders it, and the proxy serves nothing.

One hostname serves everything, and the Istio gateway is what serves it. AppLab
itself — the console, the API under `/api/v1/`, the git endpoints under `/git/` —
is a route the chart installs on that gateway under the installation's own base
path, `/applab` by default, and an app is published under `/applab/apps/<app>/`:
nested inside it, because that is the path the gateway routes on.

There is nothing in front of the gateway to tell the two apart, because the paths
already do. Istio sorts a virtual host's catch-all route to the end while leaving
the rest in order, so the console — which matches everything — is evaluated only
after every app has declined the request, and an app's route is under a path none
of the console's own routes claim.

## Inputs

| Input | Default | Description |
|---|---|---|
| `api_key` | generated | API key. Printed in the summary either way, because it is the deliverable. |
| `session_hours` | `4` | How long the environment may run. `0` means no self-imposed limit, bounded by the job's timeout. |
| `tunnel` | `cloudflare` | `cloudflare` (no account needed) or `ngrok`. |
| `cloudflare_token` | — | Token of a named Cloudflare tunnel; empty starts a quick tunnel. |
| `domain` | `applab.chenshaowen.com` | The domain apps are served under. Named by default, and explained below. |
| `ngrok_token` | — | ngrok authtoken; required when `tunnel` is `ngrok`. |

Only `api_key` and `cloudflare_token` are worth passing from a secret: the key is
generated when left empty, so it needs no configuration unless you want a
particular one, and the token is a credential and never a plain input.

The AppLab image tag is not an input. It is the published `latest`, so the
environment runs the newest AppLab — the same tag the release workflow publishes
alongside the version tags, re-resolved on every start because the chart pulls
with `imagePullPolicy: Always`.

### The domain, and the named tunnel it needs

`domain` defaults to `applab.chenshaowen.com`, and it is used as given: it becomes
`ingress.host`, so the apps are served under it and the console's own route
matches it too.

This is app configuration, not tunnel configuration. Nothing is passed to
`cloudflared` — a named tunnel already knows its ingress, because you configured
it. That is why the input is named for the domain rather than for the tunnel.

It only works with a **named** tunnel — the one whose hostname and ingress live in
your Cloudflare dashboard. A quick tunnel is assigned a random `trycloudflare.com`
hostname by Cloudflare and cannot be given another, so apps could not be served
under the domain you named; the ngrok path here does not pass the flag a reserved
domain needs. Both combinations are refused when the run starts, rather than after
a wait for a hostname that was never coming — which means **a default run needs
`cloudflare_token` set**. Without it the run stops and says so.

A named tunnel keeps its hostname in its ingress, and Cloudflare never tells the
connector its own name, so the environment cannot discover the address apps should
be served under:

```
this is a named tunnel: Cloudflare does not tell the connector its own hostname
```

That is why the domain is declared rather than found, and why it has a default at
all: the value has to come from a person, and naming it once is better than
naming it on every run.

One thing this does **not** do: create the ingress. Point the hostname at the
tunnel in the Cloudflare dashboard first, or the address will resolve to a tunnel
that routes nothing. Nothing here can make or check that.

Setting `domain` to empty goes back to a quick tunnel: one is assigned a random
`trycloudflare.com` hostname, needs no account, and that hostname is both where
the console lives and the domain apps are served under. The link appears in the
summary. It is the simpler path and the flakier one — see below.

## The two things most likely to go wrong

**A run with no `cloudflare_token` stops.** The default `domain` needs a named
tunnel, and a quick tunnel cannot be given another hostname — so the run refuses
the combination rather than publishing an address that does not match the domain
it was told to serve. Set `cloudflare_token`, or set `domain` to empty.

**A quick tunnel is for trying things.** It carries no SLA and its hostname is
minted per connection, so a restart gives a different link. That is exactly right
for a session you open now and discard, and wrong for anything you keep — which is
why the default is a named domain rather than one of these.

## What it costs

Roughly ten minutes to come up — most of it Istio — and about two more for the
first app's build. Everything is deleted when the run ends: the cluster, the
images, the app's source. Nothing survives, which is the point.

## Implementing it yourself

Nothing here is specific to GitHub Actions except the workflow file. The pieces
are ordinary scripts, and they are documented where they are:

| Path | What it does |
|---|---|
| [debugger/action.yml](action.yml) | The composite action: installs kind, kubectl, istioctl, helm and a tunnel agent, then runs the script. |
| [hack/environment.sh](../hack/environment.sh) | The whole environment, in order. Set `APPLAB_PUBLIC_HOST` to skip the tunnel and use a hostname you already have, or `APPLAB_DOMAIN` to name the domain a named tunnel serves apps under. |
| [hack/summary.sh](../hack/summary.sh) | Publishes the link and the key to the job summary. |

## License

See [LICENSE](../LICENSE).
