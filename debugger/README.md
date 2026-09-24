# applab debugger environment

Start a complete applab platform on a GitHub runner — a Kubernetes cluster, a
registry, an Istio gateway and applab itself — hand yourself a link, and push an
app. It is built, deployed and served before you open the console.

The point is that applab needs real infrastructure before it can do anything: a
cluster to deploy into, a registry to push to, and a gateway to publish through.
This action assembles all of it on a throwaway kind cluster, so the first thing
you have to do is not "install a cluster" but "push an app".

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
```

That is the whole workflow. Open the run's **Summary** for the console link and
the API key, then push something at it:

```bash
export APPLAB_URL='https://<the link>'
export APPLAB_KEY='<the key>'

cd any-project-with-a-Dockerfile
applab push myshop
```

The app is served at `<the link>/apps/myshop/` and appears in the console. The
`applab` CLI is the binary from [the applab repository](https://github.com/shaowenchen/applab);
see [the overview](../README.md) for how to install it, or drive the API directly —
[the API reference](../api/llms.txt) is the contract.

## What it starts

| | What it is |
|---|---|
| **kind cluster** | A throwaway Kubernetes cluster, created for this run and deleted with it. |
| **applab** | The published image, installed with this repository's [Helm chart](../charts/applab/README.md). |
| **Istio** | The ingress gateway apps are published through. Install it yourself in a real deployment; here it is part of the environment. |
| **registry:2** | Where built images are pushed, as `kind-registry:5000` — a cluster-local registry with no TLS and no credentials. |
| **cloudflared** | A quick tunnel, so the environment is reachable from anywhere. Set `tunnel: ngrok` to use ngrok instead. |

One hostname serves everything. Requests under `/apps/<app>/` reach the app,
published through the Istio gateway; everything else reaches applab — the
console at `/`, the API under `/api/v1/`, and the git endpoints under `/git/`.

## Inputs

| Input | Default | Description |
|---|---|---|
| `api_key` | generated | API key. Printed in the summary either way, because it is the deliverable. |
| `session_hours` | `4` | How long the environment may run. `0` means no self-imposed limit, bounded by the job's timeout. |
| `tunnel` | `cloudflare` | `cloudflare` (no account needed) or `ngrok`. |
| `cloudflare_token` | — | Token of a named Cloudflare tunnel; empty starts a quick tunnel. |
| `domain` | — | The domain apps are served under. Needed with a named tunnel, and explained below. |
| `ngrok_token` | — | ngrok authtoken; required when `tunnel` is `ngrok`. |

Only `api_key` is worth passing from a secret: it is generated when left empty,
so the common case needs no configuration at all.

The applab image tag is not an input. It is the published `latest`, so the
environment runs the newest applab — the same tag the release workflow publishes
alongside the version tags, re-resolved on every start because the chart pulls
with `imagePullPolicy: Always`.

### A named tunnel needs the domain named

With `cloudflare_token` set, the environment runs a **named** tunnel — the one
whose hostname and ingress live in your Cloudflare dashboard. Cloudflare never
tells the connector its own name, so the environment cannot discover the address
apps should be served under, and a run without `domain` stops with:

```
this is a named tunnel: Cloudflare does not tell the connector its own hostname
```

Pass the domain and it is used as given:

```
domain: applab.example.com
```

This is app configuration, not tunnel configuration. Nothing is passed to
`cloudflared` — a named tunnel already knows its ingress, because you configured
it. What needs the value is applab: it becomes `apps.baseDomain`, and the host the
router hands to Istio so a `VirtualService` matches. That is why the input is
named for the domain rather than for the tunnel.

One thing this does **not** do: create the ingress. Point the hostname at the
tunnel in the Cloudflare dashboard first, or the address will resolve to a tunnel
that routes nothing. Nothing here can make or check that.

Nor can it be used with a quick tunnel or with ngrok. A quick tunnel is assigned
a random hostname by Cloudflare and cannot be given another, so apps could not be
served under the domain you named; the ngrok path here does not pass the flag a
reserved domain needs. Both combinations are refused when the run starts, rather
than after a wait for a hostname that was never coming.

Leaving `cloudflare_token` empty is the simpler path, and the default: a quick
tunnel is assigned a random `trycloudflare.com` hostname, needs no account, and
that hostname is both where the console lives and the domain apps are served
under. The link appears in the summary.

## The two things most likely to go wrong

**A named Cloudflare tunnel cannot report its own hostname.** Cloudflare never
tells the connector its name, so the environment cannot discover the domain to
serve apps under — and without one it stops rather than publishing nothing. Pass
`domain` (see above), or leave `cloudflare_token` empty and use a quick tunnel,
whose hostname it *is* told.

**A quick tunnel is for trying things.** It carries no SLA and its hostname is
minted per connection, so a restart gives a different link. That is exactly right
for a session you open now and discard, and wrong for anything you keep.

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
| [hack/router.mjs](../hack/router.mjs) | Splits one hostname between applab and the apps it publishes. |
| [hack/summary.sh](../hack/summary.sh) | Publishes the link and the key to the job summary. |
| [hack/demo-app/Dockerfile](../hack/demo-app/Dockerfile) | A minimal app, used by CI to prove push, build, deploy and serve work. |

## License

See [LICENSE](../LICENSE).
