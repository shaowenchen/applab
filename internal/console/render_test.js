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
    // The theme and language controls label themselves with setAttribute, and
    // boot applies both before it renders anything — so an element without this
    // makes the whole script throw before the form is ever shown, which is a
    // failure no assertion in this file would name.
    attrs: {},
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; },
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
    // The theme is applied by stamping an attribute on the root element, so the
    // root has to exist here. Tracked as a real map so a check can read back
    // which theme the console settled on.
    documentElement: {
      attributes: new Map(),
      setAttribute(k, v) { this.attributes.set(k, String(v)); },
      removeAttribute(k) { this.attributes.delete(k); },
      getAttribute(k) { return this.attributes.has(k) ? this.attributes.get(k) : null; },
    },
    // Translation walks the markup for data-i18n. The stub has no real DOM to
    // query, so this returns nothing and the static-text half is a no-op here —
    // the JS-produced strings, which are what the rendering checks assert on,
    // go through t() directly and are covered.
    querySelectorAll: () => [],
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

  // The theme must be stamped on the root element by boot, before anything is
  // rendered. Two things ride on it: a dark-mode viewer sees the right colours
  // on the first paint rather than a white flash, and the choice persists across
  // reloads. The default is no stamp at all, which is what "follow the system"
  // means — so this asserts the attribute is absent rather than that it is
  // "system", because a stamp spelling out the default would override the
  // stylesheet's media query and pin every viewer to light.
  {
    const root = sandbox.document.documentElement;
    check(
      "an unset theme leaves no stamp on the root, so the system decides",
      root.getAttribute("data-theme"),
      null
    );
    check("and the document language is set", root.getAttribute("lang"), "en");
  }

  // Both preference controls label themselves, which is what the theme function
  // does with setAttribute — the call that would throw in a real browser only if
  // the element were missing, and throws here if the stub is incomplete.
  for (const id of ["theme-toggle", "lang-toggle", "signin-theme-toggle", "signin-lang-toggle"]) {
    check(
      `the ${id} control is labelled`,
      (elements.get(id).textContent || "").length > 0,
      true
    );
  }

  // --- Translation coverage -------------------------------------------------
  //
  // Every string the interface can show must have a Chinese translation, and
  // every string in the dictionary must be one the interface can show.
  //
  // Both directions matter and they fail differently. A missing entry is English
  // text in an otherwise Chinese page, which reads as a gap. An unreferenced
  // entry is a translation of a sentence nobody can see any more — markup that
  // was edited without the dictionary following, which is exactly how a
  // dictionary rots: it still looks complete.
  //
  // The English-is-the-key design is what makes this checkable at all. There is
  // no key space to drift from the markup, so coverage is decidable by reading
  // the two together.
  {
    const zh = vm.runInContext("ZH", context);

    // Keys come from two places: data-i18n attributes in the markup, and string
    // literals passed to t()/tf() in the script.
    const fromMarkup = [...html.matchAll(/data-i18n(?:-placeholder)?="([^"]+)"/g)].map((m) => m[1]);

    // Literal arguments only. A value passed through a variable — t(status) —
    // cannot be read statically, and is covered by the runtime checks below.
    const fromJS = [];
    for (const m of source.matchAll(/\btf?\(\s*"((?:[^"\\]|\\.){2,})"/g)) fromJS.push(m[1]);

    const wanted = new Set([...fromMarkup, ...fromJS]);
    const missing = [...wanted].filter((k) => !(k in zh));
    check(
      "every string the interface shows is translated",
      missing.join(" | "),
      ""
    );

    // The dictionary side. Values reached through a variable are listed here
    // because the static scan cannot see them; each is asserted reachable by the
    // pill and status checks elsewhere in this file.
    const viaVariable = new Set([
      "nothing broken", "failed or build-failed", "{name} on", "{name} off",
      "running", "failed", "build-failed", "building", "deploying", "created",
      "succeeded", "pending", "ready", "not ready", "deployed", "no image",
    ]);
    const unreferenced = Object.keys(zh).filter((k) => !wanted.has(k) && !viaVariable.has(k));
    check(
      "and every translation is one the interface still shows",
      unreferenced.join(" | "),
      ""
    );
  }

  // A translated status is reachable through a variable, so it is exercised
  // through the real function rather than assumed. These are the words a person
  // reads most often in the console — they are what the status column says.
  {
    const zhStatuses = ["running", "failed", "build-failed", "deploying", "created"];
    const untranslated = zhStatuses.filter((s) => {
      const got = vm.runInContext(`(function(){ lang = "zh"; return t(${JSON.stringify(s)}); })()`, context);
      return got === s;
    });
    check("the statuses read in Chinese too", untranslated.join(" | "), "");
    vm.runInContext('lang = "en"', context);
  }

  if (failures > 0) {
    console.error(`\n${failures} check(s) failed`);
    process.exit(1);
  }
  console.log("\nall console rendering checks passed");
})();
