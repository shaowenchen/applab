// Executes the console's own JavaScript against a small DOM stub.
//
// The console renders an app's address in two places, and with a shared path
// prefix the host is the deployment's rather than the app's — so a version that
// showed the host alone labelled every app with the same string. That is a
// display bug no Go test can see: the API was right, the page was wrong, and
// nothing in `make check` rendered the page.
//
// Running the real script rather than asserting on its text is the point. A
// regex for `app.hostname + app.path` would pass on code that computes the
// address and then never uses it, which is exactly the mistake worth catching.
//
// Exits non-zero on failure. Requires node; the Go test that runs it skips when
// node is absent, since the console is not worth a runtime dependency for
// everyone building the server.
"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const html = fs.readFileSync(path.join(__dirname, "static", "index.html"), "utf8");

// The single <script> block in the page, which is the whole console.
const match = html.match(/<script>([\s\S]*?)<\/script>/);
if (!match) {
  console.error("no <script> block found in index.html");
  process.exit(1);
}
const source = match[1];

// --- A DOM stub, only as complete as the console needs ----------------------

function makeElement(id = "") {
  const el = {
    id,
    textContent: "",
    value: "",
    href: "",
    target: "",
    rel: "",
    onclick: null,
    onkeydown: null,
    onchange: null,
    className: "",
    children: [],
    dataset: {},
    classList: {
      _set: new Set(),
      add(c) { this._set.add(c); },
      remove(c) { this._set.delete(c); },
      toggle(c, on) { on ? this._set.add(c) : this._set.delete(c); },
      contains(c) { return this._set.has(c); },
    },
    appendChild(child) { this.children.push(child); return child; },
    replaceChildren(...kids) { this.children = kids; },
    focus() {},
    addEventListener() {},
    querySelector() { return null; },
    // Enough for the code under test to read back what it rendered.
    allText() {
      return (this.textContent || "") + this.children.map((c) => c.allText()).join("");
    },
  };
  return el;
}

// Every id the script touches at load time, so the wiring section does not throw.
const ids = new Set();
for (const m of source.matchAll(/\$\("([^"]+)"\)/g)) ids.add(m[1]);
for (const m of source.matchAll(/getElementById\("([^"]+)"\)/g)) ids.add(m[1]);

const elements = new Map();
for (const id of ids) elements.set(id, makeElement(id));

const store = new Map();
const sandbox = {
  console,
  document: {
    getElementById: (id) => {
      if (!elements.has(id)) elements.set(id, makeElement(id));
      return elements.get(id);
    },
    createElement: (tag) => {
      const el = makeElement();
      el.tagName = tag.toUpperCase();
      return el;
    },
  },
  // A browser always has both of these. Modelled here rather than left as
  // origin alone, because the console derives the address it offers from them —
  // and a deployment served under a path (ingress.path, the default) would be
  // offered the wrong one if only the origin were read.
  window: {
    location: { origin: "https://applab.example.com", pathname: "/applab/" },
  },
  localStorage: {
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, String(v)),
    removeItem: (k) => store.delete(k),
  },
  // The script fetches on boot; a rejection is caught and shown, which is fine
  // here — nothing under test depends on it.
  fetch: () => Promise.reject(new Error("no network in tests")),
  setTimeout,
  clearTimeout,
  Promise,
  JSON,
  Math,
  Date,
  Object,
  Array,
  String,
  Number,
  Boolean,
  Error,
  Set,
  Map,
  RegExp,
};
sandbox.globalThis = sandbox;

const context = vm.createContext(sandbox);
vm.runInContext(source, context, { filename: "console.js" });

// `loadApps` is declared with `async function`, which in the script's scope is a
// binding rather than a property of the sandbox — read it back by evaluating in
// the same context.
const loadApps = vm.runInContext("loadApps", context);
if (typeof loadApps !== "function") {
  console.error("loadApps is not defined; the console's script shape changed");
  process.exit(1);
}

// --- The checks ------------------------------------------------------------

let failures = 0;
function check(what, got, want) {
  if (got !== want) {
    console.error(`FAIL: ${what}\n  got:  ${JSON.stringify(got)}\n  want: ${JSON.stringify(want)}`);
    failures++;
  } else {
    console.log(`ok: ${what}`);
  }
}

// Drives one render with the given apps, against a stubbed API.
async function render(apps) {
  const appsBody = elements.get("apps");
  appsBody.replaceChildren();

  // A fresh context per render, so the boot fetch cannot interleave.
  const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
  ctx.globalThis = ctx;
  ctx.fetch = async () => ({
    ok: true,
    status: 200,
    statusText: "OK",
    headers: { get: () => "application/json" },
    // api() reads the body as text and parses it itself, so that a non-JSON
    // answer from a proxy is reported as text rather than as a parse error.
    text: async () => JSON.stringify({ data: apps }),
  });
  vm.runInContext(source, ctx, { filename: "console.js" });
  const fn = vm.runInContext("loadApps", ctx);
  await fn();

  // The rows are built in the context's own document stub, which shares the
  // elements map.
  return elements.get("apps");
}

(async () => {
  // A shared path prefix: one host, the app distinguished by path.
  {
    const body = await render([
      { id: "shop", status: "running", hostname: "www.example.com", path: "/apps/shop", url: "https://www.example.com/apps/shop" },
      { id: "blog", status: "running", hostname: "www.example.com", path: "/apps/blog", url: "https://www.example.com/apps/blog" },
    ]);
    const text = body.allText();
    check("path-prefixed apps show their own path", text.includes("/apps/shop"), true);
    check("and the other app's too", text.includes("/apps/blog"), true);
    check("neither row shows a bare host", /www\.example\.com(?!\/apps)/.test(text), false);
  }

  // A subdomain per app: no path, and the host is the whole address.
  {
    const body = await render([
      { id: "shop", status: "running", hostname: "shop.apps.example.com", url: "https://shop.apps.example.com" },
    ]);
    const text = body.allText();
    check("subdomain apps show their host", text.includes("shop.apps.example.com"), true);
    check("and no stray path", text.includes("undefined"), false);
  }

  // Not deployed: no url, but the address is still known and is what the row
  // should name.
  {
    const body = await render([
      { id: "shop", status: "created", hostname: "www.example.com", path: "/apps/shop" },
    ]);
    const text = body.allText();
    check("an undeployed app shows its address", text.includes("www.example.com/apps/shop"), true);
    check("marked as not deployed", text.includes("not deployed"), true);
  }

  // No domain configured at all: the row must not render "null" or "undefined".
  {
    const body = await render([{ id: "shop", status: "created" }]);
    const text = body.allText();
    check("a deployment with no domain shows a dash", text.includes("—"), true);
    check("and never the string undefined", text.includes("undefined"), false);
  }

  // The address the console talks to.
  //
  // It has to carry the path the page was served from, not just its origin: a
  // deployment under a path (ingress.path, which the chart defaults to /applab)
  // is reached at https://host/applab, and every API call built from a bare
  // origin would miss the server. The stub's pathname is "/applab/", so the
  // trailing slash being trimmed is part of what this checks.
  //
  // Asserted through the sign-in card's own text, which is where the address is
  // now shown rather than typed. That keeps the check on the same derived value
  // as before — boot() sets state.url from the page and renders it here — while
  // the field it used to read no longer exists.
  check(
    "the console reports the address it was served from, trailing slash trimmed",
    elements.get("signin-where").textContent,
    "https://applab.example.com/applab"
  );

  // There must be no address input left to fill in: the key is the only thing
  // anyone should have to bring.
  check(
    "the sign-in form asks for nothing but a key",
    html.includes("signin-url"),
    false
  );

  // And the key is the only field in it. Counted from the rendered form rather
  // than asserted on the absence of one id, so a second field added under any
  // name is caught: "type the URL and the key" is the shape this replaced.
  {
    const form = html.match(/<form id="signin-form"[\s\S]*?<\/form>/);
    check("the sign-in form exists", form !== null, true);
    if (form) {
      const fields = form[0].match(/<input\b/g) || [];
      check("and holds exactly one field", fields.length, 1);
      check("which is the key", form[0].includes('id="signin-key"'), true);
    }
  }

  if (failures > 0) {
    console.error(`\n${failures} check(s) failed`);
    process.exit(1);
  }
  console.log("\nall console rendering checks passed");
})();
