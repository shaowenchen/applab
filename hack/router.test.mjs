#!/usr/bin/env node
/**
 * The routing decision, tested directly.
 *
 * splitTarget is the whole of what decides whether a request reaches applab or
 * an app, and it is pure — so it is tested as a function rather than by starting
 * two stub servers and driving traffic through them. A routing bug here is the
 * one failure that makes the environment look like it works while sending
 * people to the wrong thing.
 *
 * No cluster, no network, no ports. Run: node hack/router.test.mjs
 */

import { splitTarget, parseAuthority, GATEWAY, APPLAB } from './router.mjs'

let failures = 0

function check(name, got, want) {
  if (got === want) {
    console.log(`  ok   ${name}`)
    return
  }
  console.error(`  FAIL ${name}\n       got  ${got}\n       want ${want}`)
  failures++
}

console.log('splitTarget — apps go to the gateway, everything else to applab')

// The app paths.
check('/apps itself is the gateway', splitTarget('/apps'), GATEWAY)
check('an app root', splitTarget('/apps/shop'), GATEWAY)
check('an app sub-path', splitTarget('/apps/shop/cart'), GATEWAY)
check('a deep app path', splitTarget('/apps/shop/a/b/c.js'), GATEWAY)
check('a trailing slash', splitTarget('/apps/shop/'), GATEWAY)

// The trap: a plain string prefix would match these, and the gateway has no
// VirtualService for them, so they would 404 for a reason nobody could see.
check('/appstore does not match the prefix', splitTarget('/appstore'), APPLAB)
check('/apps-other does not match the prefix', splitTarget('/apps-other/shop'), APPLAB)

// applab's own surface.
check('the console root', splitTarget('/'), APPLAB)
check('the API', splitTarget('/api/v1/apps'), APPLAB)
check('the overview endpoint', splitTarget('/api/v1/overview'), APPLAB)
check('git', splitTarget('/git/shop.git/info/refs'), APPLAB)
check('llms.txt', splitTarget('/llms.txt'), APPLAB)
check('metrics', splitTarget('/metrics'), APPLAB)
check('health', splitTarget('/health'), APPLAB)
// A console deep link — the SPA serves it, so it is applab's, not an app's.
check('a console deep link', splitTarget('/nonexistent-page'), APPLAB)

console.log('\nparseAuthority')

check('host and port', JSON.stringify(parseAuthority('127.0.0.1:30080')), '{"host":"127.0.0.1","port":30080}')
check('a hostname and port', JSON.stringify(parseAuthority('applab.example.com:8080')), '{"host":"applab.example.com","port":8080}')
check('a bare host defaults to 80', JSON.stringify(parseAuthority('example.com')), '{"host":"example.com","port":80}')

// A TLS-ish port is just a port; nothing here speaks TLS, but the parse must
// not split on the wrong colon in a value that has more than one.
check('only the last colon separates the port', JSON.stringify(parseAuthority('fe80::1:8080')), '{"host":"fe80::1","port":8080}')

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`)
  process.exit(1)
}
console.log('\nall checks passed')
