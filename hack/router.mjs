#!/usr/bin/env node
/**
 * router.mjs — put applab and the apps it deploys behind one address.
 *
 * The debugger environment exposes a single tunnel, but two different things
 * have to answer on it:
 *
 *   /apps/<app>/...   an app, published by applab through the Istio gateway
 *   everything else   applab itself — the console, the API, /git, /llms.txt
 *
 * Routing this inside the cluster is not possible. applab publishes each app as
 * its own Istio VirtualService whose host is the deployment's base domain, and a
 * request for applab itself would need a *third* route on that same host. Istio
 * gives no defined order between two VirtualServices matching one host, so which
 * won would depend on apply order — the same undefined-ordering hazard applab
 * already avoids by making its own routes end in a slash (see model.Address
 * .RoutePath). A thirty-line proxy in front is deterministic where a
 * cross-VirtualService match is not.
 *
 * Both destinations are on the runner, reached over loopback:
 *
 *   cloudflared ──▶ router :3080 ──┬─ /apps/*  ──▶ gateway  :30080 (kind NodePort)
 *                                  └─ /*       ──▶ applab   :8080  (port-forward)
 *
 * Zero dependencies: the runner has Node for the tunnel agent's sake, and a
 * proxy this small does not warrant anything else.
 */

import { createServer, request as httpRequest } from 'node:http'
import { pathToFileURL } from 'node:url'

// Where each half lives. Overridable so the router can be unit-tested against
// stubs without a cluster.
const LISTEN_PORT = Number(process.env.APPLAB_ROUTER_PORT ?? '3080')
const APPS_PREFIX = process.env.APPLAB_ROUTER_PREFIX ?? '/apps'

/** The Istio ingress gateway, as host:port on the runner. */
export const GATEWAY = process.env.APPLAB_ROUTER_GATEWAY ?? '127.0.0.1:30080'

/** applab itself, as host:port on the runner. */
export const APPLAB = process.env.APPLAB_ROUTER_APPLAB ?? '127.0.0.1:8080'

/**
 * The hostname the tunnel publishes.
 *
 * It is carried through to both destinations unchanged, and that is
 * load-bearing rather than incidental: Istio matches a VirtualService by the
 * request's Host, so a gateway request forwarded with a rewritten Host matches
 * no route and comes back 404 from the gateway's own default handler.
 *
 * Empty means "forward whatever Host arrived", which is what a caller with no
 * tunnel (a loopback test, or a runner serving only locally) wants.
 */
const PUBLIC_HOST = process.env.APPLAB_ROUTER_HOST ?? ''

/**
 * splitTarget decides where one request goes.
 *
 * Exported for the unit test: this is the whole decision, and it is worth
 * testing directly rather than by starting proxies.
 *
 * The prefix is matched as a path *segment* — "/apps" or "/apps/..." — never as
 * a bare string prefix. A plain startsWith would send "/appstore" to the
 * gateway, where no VirtualService claims it. applab guards its own routes the
 * same way for the same reason.
 */
export function splitTarget(pathname) {
  if (pathname === APPS_PREFIX || pathname.startsWith(APPS_PREFIX + '/')) {
    return GATEWAY
  }
  return APPLAB
}

/** parseAuthority splits a "host:port" pair. */
export function parseAuthority(authority) {
  const i = authority.lastIndexOf(':')
  if (i < 0) return { host: authority, port: 80 }
  return { host: authority.slice(0, i), port: Number(authority.slice(i + 1)) }
}

/**
 * proxy forwards one request to target, preserving method, path, headers and
 * body.
 *
 * Host is preserved deliberately — see PUBLIC_HOST above. Hop-by-hop headers are
 * dropped, since forwarding them would describe a connection the downstream
 * server does not have.
 */
function proxy(req, res, target) {
  const { host, port } = parseAuthority(target)

  const headers = { ...req.headers }
  // Set by this proxy, and wrong downstream: the downstream connection is to
  // loopback, not to whatever the client reached us on.
  delete headers['connection']
  delete headers['keep-alive']
  delete headers['transfer-encoding']
  delete headers['upgrade']
  if (PUBLIC_HOST) headers['host'] = PUBLIC_HOST

  const upstream = httpRequest(
    { host, port, method: req.method, path: req.url, headers },
    (up) => {
      res.writeHead(up.statusCode ?? 502, up.headers)
      up.pipe(res)
    }
  )

  upstream.on('error', (err) => {
    // A 502 with the reason in the body, rather than a dropped connection: the
    // reason is what tells a person which half is down, and a browser shows the
    // body where it shows nothing for a reset.
    console.error(`[router] ${req.method} ${req.url} -> ${target} failed: ${err.message}`)
    if (!res.headersSent) {
      res.writeHead(502, { 'content-type': 'text/plain; charset=utf-8' })
    }
    res.end(`the router could not reach ${target.split(':')[0]}: ${err.message}\n`)
  })

  req.pipe(upstream)
}

function main() {
  const server = createServer((req, res) => {
    const pathname = (req.url ?? '/').split('?')[0]
    const target = splitTarget(pathname)
    proxy(req, res, target)
  })

  server.listen(LISTEN_PORT, '127.0.0.1', () => {
    console.log(`[router] listening on 127.0.0.1:${LISTEN_PORT}`)
    console.log(`[router]   ${APPS_PREFIX}/* -> ${GATEWAY}   (apps, via the Istio gateway)`)
    console.log(`[router]   /*        -> ${APPLAB}   (applab: console, API, git)`)
    if (PUBLIC_HOST) console.log(`[router]   host ${PUBLIC_HOST} preserved for Istio's VirtualService match`)
  })
}

// Only listen when run directly, so the unit test can import the pure helpers
// without binding a port. Comparing resolved file URLs rather than string
// suffixes, which would match any path ending in the same basename.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main()
}
