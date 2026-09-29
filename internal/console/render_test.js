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

// The markup above the script, which is where every id and class comes from.
// Split out rather than scanning the whole file: the script is full of `<` and
// `>` in comparisons and in the SVG strings it builds, and a tag regex run over
// it would invent elements that do not exist.
const markup = html.slice(0, html.indexOf("<script>"));

// The base path the server would inject into this page.
//
// The server writes it into the head as it serves the page — see console.go —
// and the console reads it from there rather than from its own location, so a
// harness that left the tag out would be testing a page the server never
// serves. Keeping it a constant here rather than deriving it from the stub's
// location is the point: the two differing is exactly the bug this covers.
const injectedBasePath = "/applab";


// --- A DOM stub, only as complete as the console needs ----------------------

function makeElement(id = "") {
  const el = {
    id,
    // A browser always reports one, and the console reads it to decide how to
    // mask a value. Defaulted to a span, which is the text half of that branch;
    // the harness overrides it from the markup for the ids that are something
    // else.
    tagName: "SPAN",
    type: "",
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
    // Variadic, unlike appendChild. The KPI tiles are built with it, and a
    // refresh is what reaches that code — a check that only loaded the overview
    // once would never have called it.
    append(...kids) { for (const k of kids) this.children.push(k); },
    // A real element always reports one, and the instances table reads it back
    // after appending: it builds a pill, copies it, appends the copy and then
    // restyles the cell's first child.
    get firstChild() { return this.children[0]; },
    // The instances table builds a pill and copies its classes onto the row's
    // own cell, so the stub has to answer. A shallow copy is enough: what the
    // code under test reads back is the class and the text.
    cloneNode() {
      const copy = makeElement(this.id);
      copy.tagName = this.tagName;
      copy.className = this.className;
      copy.textContent = this.textContent;
      for (const c of this.classList._set) copy.classList.add(c);
      return copy;
    },
    replaceChildren(...kids) { this.children = kids; },
    focus() {},
    addEventListener() {},
    querySelector() { return null; },
    // The app page's section watcher asks an element for the cards it holds, so
    // the stub has to answer. Empty rather than null-or-undefined: a real
    // element always returns an iterable, and code that spreads the result is
    // correct against a browser — failing here would be the stub reporting its
    // own gap as a bug in the console.
    querySelectorAll() { return []; },
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
//
// The markup's own ids are included as well: some are reached through a computed
// name — `$(view + "-view")` in the code that switches views — which a scan of
// the script cannot see, and a check that asked for one of those by hand would
// be reading an element the harness never made.
const ids = new Set();
for (const m of source.matchAll(/\$\("([^"]+)"\)/g)) ids.add(m[1]);
for (const m of source.matchAll(/getElementById\("([^"]+)"\)/g)) ids.add(m[1]);

// And which element each id is, read from the markup. It matters: the console
// masks a credential in one of two ways depending on what it is looking at — a
// real `<input>` gets its type switched, anything else gets its text replaced —
// so a stub that reported every element as the same thing would exercise one
// branch and silently never run the other.
//
// The starting classes come from the markup for the same reason: every section
// is born `hidden` and shown by the script, so a harness that began with them
// all visible would report a console that greets a signed-out visitor with the
// overview, the apps list and the nav.
const tags = new Map();
const classes = new Map();
for (const m of markup.matchAll(/<([a-z]+)\b([^>]*)>/g)) {
  const id = /\bid="([^"]+)"/.exec(m[2]);
  if (!id) continue;
  tags.set(id[1], m[1].toUpperCase());
  ids.add(id[1]);
  // Attribute order in the markup is whatever reads best — `id` first on some
  // elements, `class` first on others — so both are read from the tag as a
  // whole rather than from what follows the id.
  const cls = /\bclass="([^"]*)"/.exec(m[2]);
  if (cls) classes.set(id[1], cls[1].split(/\s+/).filter(Boolean));
}

const elements = new Map();
for (const id of ids) {
  const el = makeElement(id);
  if (tags.has(id)) el.tagName = tags.get(id);
  for (const c of classes.get(id) || []) el.classList.add(c);
  elements.set(id, el);
}

const store = new Map();

// The timers the console starts, recorded rather than run. Declared up here
// because the sandbox closes over it — see setInterval below.
const intervals = [];
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
    //
    // A class selector is answered, because the document's role cards are
    // hidden that way: `renderDocs` shows or hides every `.admin-only` by class,
    // and a stub that returned nothing would leave the two cards in whatever
    // state the markup gave them — so a console that showed the admin's card to
    // an app key would pass. Answered from the same class list the markup
    // seeded, which is what a browser would match on.
    querySelectorAll: (selector) => {
      if (typeof selector === "string" && selector.startsWith(".")) {
        const want = selector.slice(1);
        return [...elements.values()].filter((el) => el.classList.contains(want));
      }
      return [];
    },
    // The console reads the deployment's base path from the meta tag the server
    // injects, and this is that tag. Answered rather than left null because a
    // null here is the "not served by applab" case, which has its own check —
    // every other check in this file is about a deployment under a path, and
    // they would all silently become tests of a root deployment without it.
    querySelector: (selector) => {
      if (selector !== 'meta[name="applab-base-path"]') return null;
      return {
        getAttribute: (name) => (name === "content" ? injectedBasePath : null),
      };
    },
    createRange: () => ({ selectNodeContents() {} }),
  },
  // A browser always has both of these. Modelled here rather than left as
  // origin alone, because the console derives the address it offers from them —
  // and a deployment served under a path (ingress.path, the default) would be
  // offered the wrong one if only the origin were read.
  //
  // The pathname is deliberately a *deep link* under the base path, not the
  // base path itself. The page is served for every non-asset address, so this
  // is what a console opened at an app's own address sees — and reading the
  // API base from it is the bug that shipped: the console asked
  // /applab/apps/shop/api/v1/apps, which the server answered with this page, so
  // sign-in failed with "200 OK: <!doctype html>". A stub whose pathname was
  // the base path could never have caught it.
  window: {
    // protocol is included because the console reads it: an app's address is
    // offered over the scheme the page was loaded with. A stub without it would
    // make the console look like it mangles every URL, which is the harness
    // missing a field rather than the console having a bug.
    location: { protocol: "https:", origin: "https://applab.example.com", pathname: injectedBasePath + "/apps/shop" },
    // The clipboard fallback selects the value in the document, so the selection
    // API has to exist for that path to run at all.
    getSelection: () => ({ removeAllRanges() {}, addRange(r) { this._range = r; } }),
  },
  localStorage: {
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, String(v)),
    removeItem: (k) => store.delete(k),
  },
  // The script fetches on boot; a rejection is caught and shown, which is fine
  // here — nothing under test depends on it.
  fetch: () => Promise.reject(new Error("no network in tests")),
  // The console reads navigator for the language default and for the clipboard.
  // Modelled rather than left absent: a missing navigator is a real case the
  // script guards, but it is not the case these checks are about, and without it
  // the copy path would silently take the fallback branch every time.
  navigator: { language: "en", clipboard: null },
  // Used by the log panels, which are the only part of the console that streams.
  // A browser has all three; leaving them out makes startLogs throw before it
  // sends anything, so a check of it would fail for a reason that has nothing to
  // do with what it is checking.
  AbortController,
  TextDecoder,
  URLSearchParams,
  setTimeout,
  clearTimeout,
  // The console reloads the current view on a timer. Recorded rather than run:
  // a real interval would keep the process alive and fire during unrelated
  // checks. What the timer *does* is reachable directly through refreshCurrent,
  // which is what the checks exercise.
  // The console reloads the current view on a timer. Recorded rather than run:
  // a real interval would keep the process alive and fire during unrelated
  // checks. What the timer *does* is reachable directly through refreshCurrent,
  // which is what the checks exercise.
  // `this` inside a method would not be the sandbox — the console's script is in
  // strict mode, so a bare call has no receiver. The array is closed over.
  setInterval: (fn, ms) => {
    intervals.push({ fn, ms });
    return intervals.length;
  },
  clearInterval() {},
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
      { id: "shop", status: "running", url: "https://www.example.com/applab/apps/shop" },
      { id: "blog", status: "running", url: "https://www.example.com/applab/apps/blog" },
    ]);
    const text = body.allText();
    check("path-prefixed apps show their own path", text.includes("/applab/apps/shop"), true);
    check("and the other app's too", text.includes("/applab/apps/blog"), true);
    check("neither row shows a bare host", /www\.example\.com(?!\/applab)/.test(text), false);
    // The scheme is part of the address shown, not just of the link target. The
    // column is read as an address — it is what someone copies — and the app
    // detail's own row shows the full URL, so a list that showed the two without
    // a scheme would disagree with the page it links to about what an app's
    // address is.
    check("and the address is shown with its scheme", text.includes("https://www.example.com/applab/apps/shop"), true);
  }

  // A subdomain per app: no path, and the host is the whole address.
  {
    const body = await render([
      { id: "shop", status: "running", url: "https://shop.apps.example.com" },
    ]);
    const text = body.allText();
    check("subdomain apps show their host", text.includes("shop.apps.example.com"), true);
    check("and no stray path", text.includes("undefined"), false);
    check("with their scheme", text.includes("https://shop.apps.example.com"), true);
  }

  // The app id is the only way into an app from the list, and it is reachable
  // without a mouse.
  //
  // A click handler on a <td> is enough to open an app with a pointer and
  // nothing else: no tab stop, so the row cannot be reached by keyboard at all,
  // and no role, so a screen reader announces it as an ordinary empty cell. The
  // cell carries tabindex, role=button and the two keys a real button answers
  // to; this checks all four, because dropping any one of them silently removes
  // the row from keyboard use again.
  //
  // Built directly rather than through render(), and openApp is replaced in this
  // context first: the real handler navigates the page, and the elements here
  // are shared with every other check, so pressing a key would leave the app
  // view open for the checks that follow.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const opened = [];
    vm.runInContext("openApp = (id) => opened.push(id)", ctx);
    ctx.opened = opened;

    const cell = vm.runInContext("appIDCell", ctx)("shop");
    check("the app id cell is focusable", cell.getAttribute("tabindex"), "0");
    check("and announced as a control", cell.getAttribute("role"), "button");
    check("and clicking it opens that app", (() => { cell.onclick(); return opened.join(","); })(), "shop");

    // Space must not also scroll the page, which is what it does by default on
    // a div — and the row would jump before the app opened.
    let prevented = false;
    cell.onkeydown({ key: " ", preventDefault: () => { prevented = true; } });
    check("space opens the app", opened[opened.length - 1], "shop");
    check("and does not scroll the page on the way", prevented, true);

    // Enter is the other key a real button answers to.
    cell.onkeydown({ key: "Enter", preventDefault: () => {} });
    check("enter opens it too", opened[opened.length - 1], "shop");
    check("and both keys opened it once each", opened.length, 3);
  }

  // Not deployed: the app has no `url` from the API yet, but its address is
  // still known and is still what the row should offer. A plain-text address
  // that becomes a link on first deploy teaches nothing, and "where will this
  // be" is exactly the question someone has before deploying.
  {
    const body = await render([
      { id: "shop", status: "created", url: "https://www.example.com/applab/apps/shop" },
    ]);
    const text = body.allText();
    check("an undeployed app shows its address", text.includes("www.example.com/applab/apps/shop"), true);
    // And nothing else in the cell. "(not deployed)" used to qualify the link,
    // and it is gone: the column is the address, and the row's own status
    // column is what says whether the app is serving.
    check("with nothing appended to it", text.includes("not deployed"), false);

    const link = body.children[0].children[3].children.find((c) => c.tagName === "A");
    check("and the address is a link even before it is serving", link !== undefined, true);
    check(
      "pointing at where the app will be",
      link && link.href,
      "https://www.example.com/applab/apps/shop"
    );
    // The scheme comes from the page: a deployment served over https serves its
    // apps over https, through the same gateway.
    check("over the same scheme as the console", link && link.href.startsWith("https://"), true);

    // Four cells, and no commit among them.
    //
    // The deployed commit is a fact about one app, and it is on that app's page
    // next to the history it belongs to. Repeating it down a list whose job is
    // to say which apps exist and what state they are in made the row wide
    // enough to wrap and told nobody anything they could act on.
    const cells = body.children[0].children;
    check("an app row has four cells", cells.length, 4);
    check(
      "and none of them is a commit",
      cells.map((c) => c.textContent).join(" | ").includes("1234567890ab"),
      false
    );
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
  // It has to carry the path the deployment is served under, not just the
  // origin: a deployment under a path (ingress.path, which the chart defaults to
  // /applab) is reached at https://host/applab, and every API call built from a
  // bare origin would miss the server.
  //
  // The path comes from the tag the server injects, and the stub's pathname is
  // deliberately *not* it — it is a deep link under it. Deriving the base from
  // the location is the bug that shipped: a console opened at an app's own
  // address asked /applab/apps/shop/api/v1/apps, the server answered with this
  // page, and sign-in reported "200 OK: <!doctype html>". If baseURL ever reads
  // the location again, this returns the deep link and fails.
  check(
    "the console reports the deployment's address, from the injected path",
    vm.runInContext("baseURL()", context),
    "https://applab.example.com/applab"
  );

  // And the injected path is what it says, so the check above cannot pass by
  // both values happening to agree.
  check(
    "which is the injected base path, not the address the page was opened at",
    vm.runInContext("basePath()", context) !== "/applab/apps/shop",
    true
  );

  // Nothing on the sign-in screen names the deployment's address. It is in the
  // page's own location bar, and repeating it was one more thing to read on the
  // one screen that should say as little as possible.
  //
  // Asserted over the whole card rather than on one id, so an address reintroduced
  // under another name is still caught — the point is that the screen is quiet,
  // not that a particular element is gone.
  {
    const card = markup.match(/<section id="signin"[\s\S]*?<\/section>/);
    check("the sign-in card exists", card !== null, true);
    if (card) {
      check(
        "and prints no deployment address",
        /https?:\/\/|signin-where|state\.url/.test(card[0]),
        false
      );
    }
  }

  // The app detail page's clone command lives in the State card, with the rest
  // of the app's facts, rather than in a card of its own. The command is the one
  // thing about the repository a reader needs — the bare address was a line
  // nobody reads, since the command contains it.
  {
    const card = markup.match(/<h2 data-i18n="State"[\s\S]*?<\/div>\n    <\/div>/);
    check("the State card exists", card !== null, true);
    if (card) {
      check(
        "and holds the clone command",
        card[0].includes('id="app-git-hint"'),
        true
      );
      check("and no bare repository address", card[0].includes("app-git-url"), false);
    }
  }

  // The app's key is not printed anywhere on the page. It was a card of its own,
  // which put a credential on screen for every visit — including the many where
  // nobody needed it. It is still what fills the clone command above, and it is
  // still readable on demand through the API and the CLI.
  check(
    "the app's key has no card of its own",
    markup.includes('id="app-key-value"'),
    false
  );
  check(
    "and no card heading offers it",
    /<h2 data-i18n="API key">/.test(markup),
    false
  );

  // There must be no address input left to fill in: the key is the only thing
  // anyone should have to bring.
  check(
    "the sign-in form asks for nothing but a key",
    markup.includes("signin-url"),
    false
  );

  // And the key is the only field in it. Counted from the rendered form rather
  // than asserted on the absence of one id, so a second field added under any
  // name is caught: "type the URL and the key" is the shape this replaced.
  {
    const form = markup.match(/<form id="signin-form"[\s\S]*?<\/form>/);
    check("the sign-in form exists", form !== null, true);
    if (form) {
      const fields = form[0].match(/<input\b/g) || [];
      check("and holds exactly one field", fields.length, 1);
      check("which is the key", form[0].includes('id="signin-key"'), true);
    }
  }

  // Signed out, the console shows the sign-in card and nothing else but the
  // document: no data view is left on screen behind it, and the header keeps
  // only what is useful before a key exists. This is the state a first visit
  // lands in, and the one the page has to get right without any JavaScript
  // having run a view.
  //
  // Docs is the deliberate exception — see the block below, which is where the
  // rule that used to read "and neither is the nav" now lives.
  {
    check("signed out, the sign-in card is up", elements.get("signin").classList.contains("hidden"), false);
    for (const view of ["overview", "apps", "app"]) {
      check(
        `signed out, the ${view} view is not on screen`,
        elements.get(view + "-view").classList.contains("hidden"),
        true
      );
    }
    check("and neither is the docs view", elements.get("docs-view").classList.contains("hidden"), true);
    // The nav is up, offering the one destination that works without a key.
    check("the nav is up for the document", elements.get("nav").classList.contains("hidden"), false);
    check("with the data views not offered", elements.get("nav-overview").classList.contains("hidden"), true);
    check("nor the app list", elements.get("nav-apps").classList.contains("hidden"), true);
    check("and the document offered", elements.get("nav-docs").classList.contains("hidden"), false);
    // The header itself stays, because the theme and language controls live in
    // it and they are the whole of what this screen offers besides the card.
    check("the header stays up for its preference controls", elements.get("app-header").classList.contains("hidden"), false);
  }

  // The eye on the sign-in field. It is the one control that exists before any
  // credential does, so it is the one that has to work from a cold start: a real
  // input, whose type the button toggles.
  {
    const key = elements.get("signin-key");
    const eye = elements.get("signin-reveal");
    check("the sign-in key is a real field", key.tagName, "INPUT");
    check("masked to begin with", key.type, "password");
    check("with an eye beside it", (eye.textContent || eye.innerHTML || "").length > 0, true);
    check("labelled as what it will do", eye.getAttribute("aria-label"), "Show");

    eye.onclick();
    check("clicking it reveals the key", key.type, "text");
    check("and offers the reverse", eye.getAttribute("aria-label"), "Hide");

    eye.onclick();
    check("clicking again masks it", key.type, "password");
  }

  // A saved key the server no longer accepts has to land on the sign-in form.
  //
  // This is a bug that shipped: boot called connect(), the call was refused,
  // connect set an error and returned, and boot returned with it — so the page
  // was a bare "401 Unauthorized: present an API key as ..." over nothing. The
  // form was never shown, and the only way out was clearing site data.
  //
  // Driven through boot itself rather than by calling connect(), because the
  // bug was in boot's return, not in connect's failure.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;

    // A stored key, which is what makes boot take the saved-credentials path.
    const saved = new Map([["applab.key", "stale-key"]]);
    ctx.localStorage = {
      getItem: (k) => (saved.has(k) ? saved.get(k) : null),
      setItem: (k, v) => saved.set(k, String(v)),
      removeItem: (k) => saved.delete(k),
    };

    // Every request is refused, which is what a rotated or foreign key gets.
    ctx.fetch = async () => ({
      ok: false,
      status: 401,
      statusText: "Unauthorized",
      headers: { get: () => "application/json" },
      text: async () => JSON.stringify({
        error: 'unauthorized: present an API key as "Authorization: Bearer <key>"',
      }),
    });

    // The element map is shared across renders, and an earlier one has already
    // un-hidden the form. Reset it, or "the form is shown" asserts a state this
    // boot did not produce and passes even when boot never shows it.
    elements.get("signin").classList.add("hidden");
    elements.get("error").classList.add("hidden");
    elements.get("error").textContent = "";

    vm.runInContext(source, ctx, { filename: "console.js" });
    // boot is an async IIFE, so this lets its microtasks finish.
    await new Promise((r) => setTimeout(r, 0));

    const form = elements.get("signin");
    const error = elements.get("error");
    check("a refused saved key shows the sign-in form", form.classList.contains("hidden"), false);
    check("and does not leave the raw 401 on screen", error.classList.contains("hidden"), true);
    check("with the stale key forgotten", saved.has("applab.key"), false);
  }

  // The same refusal typed into the form is said in the form's own terms rather
  // than by repeating the server's instruction, which is written for a client.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.fetch = async () => ({
      ok: false,
      status: 401,
      statusText: "Unauthorized",
      headers: { get: () => "application/json" },
      text: async () => JSON.stringify({
        error: 'unauthorized: present an API key as "Authorization: Bearer <key>"',
      }),
    });
    vm.runInContext(source, ctx, { filename: "console.js" });
    const connect = vm.runInContext("connect", ctx);
    await connect("wrong-key");

    const shown = elements.get("error").textContent || "";
    check("a refused key does not quote the server's instruction", /Authorization|Bearer/.test(shown), false);
    check("it says what to do instead", /not accepted/i.test(shown), true);
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

    // The tab's title follows the language, which is why it is set in setLang
    // rather than written into the markup: the file has one language and the
    // reader may want the other.
    check(
      "and the tab's title is set",
      sandbox.document.title,
      "AppLab - an application platform better suited to VibeCoding"
    );
    {
      const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
      ctx.globalThis = ctx;
      vm.runInContext(source, ctx, { filename: "console.js" });
      vm.runInContext('setLang("zh")', ctx);
      check(
        "and it is translated when the language is",
        vm.runInContext("document.title", ctx),
        "AppLab - 更适合 VibeCoding 的应用开发平台"
      );
      // Put it back. setLang remembers the choice in localStorage, and the store
      // is shared with every later check in this file — one that reads the
      // language at boot would otherwise come up in Chinese and fail on its
      // English strings, which is a confusing way to learn about a leak.
      vm.runInContext('setLang("en")', ctx);
    }
  }

  // Every control the header carries has to label itself: two are text buttons
  // the theme and language functions write into, and the rest are markup. The
  // ids are the ones the console actually uses, so a control that stopped being
  // labelled fails here rather than silently rendering blank.
  for (const id of ["theme-toggle", "lang-toggle"]) {
    check(
      `the ${id} control is labelled`,
      (elements.get(id).textContent || "").length > 0,
      true
    );
  }

  // The header's name is the way back to the landing view, and it has to start
  // out unable to do that.
  //
  // The header is up on the sign-in screen — its two preference controls are the
  // whole of what that screen offers — so a name that were enabled from the
  // start would be a control that loads the overview before there is a key to
  // load it with. Disabled in the markup and re-derived by navTo is the pair
  // that makes it correct on both sides: right before anything is rendered, and
  // right after.
  {
    check(
      "the header's name is a control, not static text",
      /<h1>\s*<button id="home"[^>]*>/.test(markup),
      true
    );
    // Read from the markup rather than the harness: `disabled` is a property
    // there, and what this asserts is that the page ships it that way, since the
    // header is visible on the sign-in screen before any script has run.
    check(
      "and it ships disabled",
      /<button id="home"[^>]*\bdisabled\b/.test(markup),
      true
    );

    // It has to be wired, and wired to a function that exists — an onclick
    // naming nothing renders identically and does nothing when pressed. Read
    // from the script, since the wiring is not in the markup above it.
    const wired = /\$\("home"\)\s*\.onclick\s*=\s*(\w+)/.exec(source);
    check("and it is wired to a handler", wired !== null, true);
    if (wired) {
      check(
        `and that handler (${wired[1]}) is defined`,
        vm.runInContext(`typeof ${wired[1]} === "function"`, context),
        true
      );
    }

    // And navTo has to decide its enabled state, because that is what makes it
    // right once a view is showing.
    check(
      "and navTo derives its enabled state",
      /\$\("home"\)\.disabled\s*=/.test(source),
      true
    );

    // What it does, driven through the real view functions rather than asserted
    // from the source. An admin's console opens on the overview; from an app's
    // page it goes back to the overview, and on the overview itself there is
    // nowhere up, so it is disabled rather than re-rendering what is showing.
    {
      const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
      ctx.globalThis = ctx;
      const calls = [];
      ctx.fetch = async (url) => {
        calls.push(String(url));
        return {
          ok: true,
          status: 200,
          statusText: "OK",
          headers: { get: () => "application/json" },
          text: async () => JSON.stringify({ data: [] }),
        };
      };
      vm.runInContext(source, ctx, { filename: "console.js" });
      vm.runInContext('state.scope = "admin"; state.signedIn = true;', ctx);

      const navTo = vm.runInContext("navTo", ctx);
      const goHome = vm.runInContext("goHome", ctx);

      navTo("app");
      check("the name is usable from an app's page", elements.get("home").disabled, false);

      navTo("overview");
      check("and never on the landing view, where it would reload what is showing", elements.get("home").disabled, true);

      // An app key's console has one app and no overview, so its home is that
      // app — enabled, because a detail view is somewhere it can be pressed.
      vm.runInContext('state.scope = "app"; state.app = "shop";', ctx);
      navTo("app");
      check("an app key can still reach its own app", elements.get("home").disabled, false);

      // Pressed on the landing view it does nothing at all, rather than firing a
      // request for a view nobody asked for. Back to an admin, because that is
      // the tier that has an overview to land on — an app key's home is its app,
      // which the check above covers.
      vm.runInContext('state.scope = "admin"; state.app = "";', ctx);
      calls.length = 0;
      navTo("overview");
      await goHome();
      check("pressing it on the landing view does nothing", calls.length, 0);
    }
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
    const fromMarkup = [...markup.matchAll(/data-i18n(?:-placeholder|-title)?="([^"]+)"/g)].map((m) => m[1]);

    // Literal arguments only. A value passed through a variable — t(status) —
    // cannot be read statically, and is covered by the runtime checks below.
    const fromJS = [];
    for (const m of source.matchAll(/\btf?\(\s*"((?:[^"\\]|\\.){2,})"/g)) fromJS.push(m[1]);
    // The i18n title attributes are the same kind of string reached the same
    // way, and one of them with no entry is a marker that renders its own key.
    for (const m of markup.matchAll(/data-i18n-title="([^"]+)"/g)) fromJS.push(m[1]);

    // This design has no key space: the English text *is* the key, so a marker
    // can be checked for being text rather than for existing. A dotted,
    // separator-free marker like "apps.new.id" is a key that leaked in from a
    // dictionary mindset — t() returns it unchanged in English, so the reader
    // sees the key itself, and the coverage checks above pass because it is in
    // ZH as well. Two rounds of that shipped before this check existed:
    // "nav.overview" in the navigation, and "apps.new.id" as a placeholder.
    const keyLike = [...fromMarkup, ...fromJS].filter((s) => /^[a-z][a-z0-9]*(?:\.[a-z0-9_]+)+$/.test(s));
    check(
      "no string marker is a dotted key rather than the text it stands for",
      keyLike.join(" | "),
      ""
    );

    // A key declared twice is silently the last one. Object literals allow it
    // and JavaScript keeps only the final value, so the dictionary still parses,
    // still covers every marker, and quietly translates the earlier uses with the
    // wrong word.
    //
    // That is not hypothetical: "Build" was declared three times — once as the
    // noun for a build, once for the table column, once as "build status" — so
    // t("Build") returned "构建状态" and both the builds table's header and the
    // button that starts one read as "build status". The runtime checks above
    // could not see it, because reading ZH through the VM gives the collapsed
    // object.
    //
    // Scanned from the source text rather than from the parsed object for exactly
    // that reason. The ZH block is a simple run of `"key": value,` lines, so a
    // line-level scan finds every declaration including the ones that lost.
    const zhSource = source.slice(source.indexOf("const ZH = {"), source.indexOf("\n};", source.indexOf("const ZH = {")));
    const declared = [...zhSource.matchAll(/^\s{2}"((?:[^"\\]|\\.)*)":/gm)].map((m) => m[1]);
    const seen = new Set();
    const repeated = new Set();
    for (const k of declared) {
      if (seen.has(k)) repeated.add(k);
      seen.add(k);
    }
    check(
      "no translation is declared twice, silently discarding one of them",
      [...repeated].join(" | "),
      ""
    );

    const wanted = new Set([...fromMarkup, ...fromJS]);
    const missing = [...wanted].filter((k) => !(k in zh));
    check(
      "every string the interface shows is translated",
      missing.join(" | "),
      ""
    );

    // A translation that is present but empty is worse than a missing one: the
    // coverage check above finds the key and reports the sentence translated,
    // while t() — `ZH[s] || s` — treats the empty string as absent and returns
    // the English. So the only person who sees the bug is the one reading the
    // page, in the one language where it is wrong.
    //
    // "or create one here." shipped that way, as the tail of the apps-list empty
    // sentence; because that sentence is assembled from three markup fragments it
    // was also mistranslated as a fragment, and the empty value hid it.
    const emptyTranslation = Object.keys(zh).filter((k) => !zh[k]);
    check(
      "no translation is empty, which t() treats as none at all",
      emptyTranslation.join(" | "),
      ""
    );

    // The dictionary side. Values reached through a variable are listed here
    // because the static scan cannot see them; each is asserted reachable by the
    // pill and status checks elsewhere in this file.
    const viaVariable = new Set([
      "nothing broken", "failed or build-failed",
      "running", "failed", "build-failed", "building", "deploying", "created",
      "succeeded", "pending", "ready", "not ready", "deployed",
      // Reached as t(shown ? "Hide" : "Show") and t(copied ? "Copied" : "Copy"),
      // which the static scan cannot read — the argument is an expression.
      // Exercised by the eye checks above.
      "Show", "Hide", "Copy", "Copied",
      // A build status rendered by buildPill as t(status), where the status is
      // whatever the API returned. Every value of model.BuildStatus has to be
      // here or a build shows its raw API word.
      "cancelled",
      // The log dialog's heading, reached as t(panel.title) from the LOG_PANELS
      // declaration. "Platform log" is still found statically — it is also the
      // card's own heading — but "Build log" is only ever a dialog title now
      // that the build's output moved into one.
      "Build log",
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

  // What can be changed about a running app, on the page that reports it. The
  // controls belong next to the thing they act on, so this asserts they are in
  // the right card rather than only that they exist somewhere.
  //
  // Branch and Port moved from State to Instances. They were rows among facts
  // that are only read, and both are what someone comes to set when the
  // instances are the problem: the wrong branch is the wrong code running, and
  // the wrong port leaves every copy unreachable.
  {
    const state = markup.match(/<h2 data-i18n="State"[\s\S]*?<h2 data-i18n="Instances"/);
    const instances = markup.match(/<h2 data-i18n="Instances"[\s\S]*?<h2 data-i18n="Configuration"/);
    check("the State card exists", state !== null, true);
    check("and the Instances card", instances !== null, true);

    if (state && instances) {
      for (const id of ["app-build", "app-deploy", "app-auto-deploy"]) {
        check(`the State card carries ${id}`, state[0].includes(`id="${id}"`), true);
      }
      for (const id of ["app-replicas", "app-branch", "app-branch-use", "app-port", "app-port-set"]) {
        check(`the Instances card carries ${id}`, instances[0].includes(`id="${id}"`), true);
      }
      // And they are not in both, which is how a move becomes a copy.
      for (const id of ["app-branch", "app-port"]) {
        check(`and the State card no longer carries ${id}`, state[0].includes(`id="${id}"`), false);
      }

      // Auto-deploy is a setting like the others — and unlike them it applies
      // immediately, which is why it must not sit under the note about the next
      // deploy.
      check(
        "and auto-deploy is not under the next-deploy note",
        /app-auto-deploy[\s\S]{0,400}?takes effect on the next deploy/.test(state[0]),
        false
      );
    }

    // Every card's Save is in the card's head, at the top right.
    //
    // A control that acts on a whole card belongs beside the card's name rather
    // than under its content: below a table of inputs it is read last, after the
    // reader has already scrolled past what they came to change, and on a long
    // card it can be off screen while the fields it saves are not.
    // The Resources card is not in this list any more: it is gone, and its
    // bounds are set through the API and the CLI. See the check below for what
    // replaced it.
    for (const [card, button] of [
      ["State", "app-deploy"],
      ["Configuration", "config-env-add"],
    ]) {
      const head = markup.match(new RegExp('<h2 data-i18n="' + card + '"[\\s\\S]{0,900}?</div>'));
      check(
        `the ${card} card's control is in its head`,
        head !== null && head[0].includes(`id="${button}"`),
        true
      );
    }
  }

  // The Resources card is gone.
  //
  // Its four inputs were the only place in the console the bounds could be set,
  // and they were removed deliberately: they are set through the API and the
  // CLI, and the metrics dialog shows what they resolve to beside what the pod
  // is using — which is the pairing that makes a bound legible. Asserted so the
  // card cannot come back as a second, drifting answer to the same question.
  {
    check("the Resources card is gone", markup.includes('id="card-resources"'), false);
    check("and its inputs with it", markup.includes("resources-cpu-request"), false);
    check("and it is not in the app page's sections", markup.includes('{ id: "card-resources"'), false);
  }

  // The overview does not carry a Deployment card.
  //
  // It reported the version, the API version, the namespace, the address
  // template and whether the cluster answered — which is what `applab config`
  // is for, and none of it is what someone opens a dashboard to find out. On a
  // healthy deployment it was a card saying nothing was wrong.
  //
  // Asserted as absence rather than by deleting the check, because the way it
  // would come back is the markup being restored while the render code stays
  // gone: that renders an empty card and passes every other check here. The
  // capabilities row below is asserted the same way and for the same reason.
  {
    for (const gone of ['overview-deployment', 'overview-cluster', 'overview-caps']) {
      check(`the Deployment card's ${gone} is gone from the page`, markup.includes(gone), false);
    }
    check(
      "and so is the card's heading",
      markup.includes('data-i18n="Deployment"') || markup.includes('data-i18n="Capabilities"'),
      false
    );
    // The rows it rendered are gone from the script too, so nothing writes into
    // elements that no longer exist.
    check(
      "and nothing renders into it",
      /overview-deployment|overview-cluster/.test(source),
      false
    );
  }

  // Replicas sits with the instances it counts, which is the card that lists
  // them. Asserted as a placement rather than as existence: the control works
  // wherever it is, and what makes it findable is being next to its subject.
  {
    const instances = markup.match(/<h2 data-i18n="Instances"[\s\S]*?<\/div>\s*<\/div>/);
    check("the Instances card exists", instances !== null, true);
    if (instances) {
      check("the Instances card carries app-replicas", instances[0].includes('id="app-replicas"'), true);
      check("and app-replicas-set", instances[0].includes('id="app-replicas-set"'), true);
    }
  }

  // The branch control, driven through the app's real loader against a stubbed
  // API. What matters is what the reader ends up able to do: with several
  // branches the control is usable, and with one it reports which is running
  // without offering a switch that cannot change anything.
  {
    const branchesContext = async (payload) => {
      const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
      ctx.globalThis = ctx;
      ctx.fetch = async () => ({
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: payload }),
      });
      vm.runInContext(source, ctx, { filename: "console.js" });
      await vm.runInContext("loadBranches", ctx)();
      return ctx;
    };

    {
      await branchesContext({ app_id: "shop", branches: ["main", "dev"], active: "dev" });
      const select = elements.get("app-branch");
      const options = select.children.map((o) => o.value);
      check("the branch control lists every branch", options.join(","), "main,dev");
      check(
        "and selects the one that is running",
        select.children.filter((o) => o.selected).map((o) => o.value).join(","),
        "dev"
      );
      check("several branches make the switch usable", select.disabled, false);
      check("and the button with it", elements.get("app-branch-use").disabled, false);
    }

    {
      await branchesContext({ app_id: "shop", branches: ["main"], active: "main" });
      const select = elements.get("app-branch");
      check("a single branch still reports which one", select.children.map((o) => o.value).join(","), "main");
      check("but there is nothing to switch to", select.disabled, true);
      check("so the button is disabled too", elements.get("app-branch-use").disabled, true);
    }

    {
      // No source storage: the API answers 501, which api() raises. The control
      // goes rather than sitting there enabled and failing on every click.
      const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
      ctx.globalThis = ctx;
      ctx.fetch = async () => ({
        ok: false,
        status: 501,
        statusText: "Not Implemented",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ error: "this deployment has no source storage configured" }),
      });
      vm.runInContext(source, ctx, { filename: "console.js" });
      await vm.runInContext("loadBranches", ctx)();
      check("no source storage hides the branch control", elements.get("app-branch").classList.contains("hidden"), true);
    }
  }

  // Replicas are validated before the request is made. The server refuses a bad
  // value too, but a field that is right here can say what is wrong with it
  // instead of surfacing a 400 that names a parameter.
  {
    const setReplicas = vm.runInContext("setReplicas", context);
    check("setReplicas is defined", typeof setReplicas, "function");
    if (typeof setReplicas === "function") {
      elements.get("app-replicas").value = "0";
      await setReplicas();
      const message = elements.get("error").textContent || "";
      check("a replica count below one is refused", message.includes("1"), true);
      check("and the refusal is shown", elements.get("error").classList.contains("hidden"), false);
    }
  }

  // The app's repository address.
  //
  // Every app has a git repository from the moment it is created, and the page
  // shows the command that clones it. The address is the API's to compose and
  // the page's to display: an earlier version built it here as
  // state.url + "/git/" + app + ".git", which silently named the app with no
  // branch — so an app on `dev` was described everywhere on the page as running
  // dev while the command in front of it cloned main.
  //
  // Driven through loadRepository rather than by calling setCloneHint, so the
  // check covers the fetch and the field it reads as well as the display.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.fetch = async () => ({
      ok: true,
      status: 200,
      statusText: "OK",
      headers: { get: () => "application/json" },
      text: async () =>
        JSON.stringify({
          data: {
            app_id: "shop",
            key: "s3cret",
            git_url: "https://applab.example.com/applab/git/shop@dev.git",
            git_url_with_key: "https://x:s3cret@applab.example.com/applab/git/shop@dev.git",
          },
        }),
    });
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com/applab"; state.app = "shop"', ctx);
    await vm.runInContext("loadRepository", ctx)();

    // Read from dataset.value: the element is concealed, so its textContent is
    // the mask. What is on the clipboard is what the reveal hands over, and that
    // is this.
    const shown = vm.runInContext('document.getElementById("app-git-hint").dataset.value', ctx);
    check(
      "the clone command is the address the API reported, branch and all",
      shown,
      "git clone https://x:s3cret@applab.example.com/applab/git/shop@dev.git"
    );
    check(
      "and is not reassembled from the page's own address",
      shown.includes("/git/shop.git"),
      false
    );
    // And it is the full command, not the bare URL: what a reader pastes has to
    // run on its own.
    check("and is a whole git clone command", shown.startsWith("git clone https://"), true);
  }

  // The platform log panel.
  //
  // AppLab's own log is served by a different route from an app's — no app in the
  // path, because it is not about one — and the two panels share one reader
  // keyed by name. So the thing worth checking is that the name reaches the right
  // URL: a copy-paste that left the app route in place would ask for the log of
  // whichever app happened to be open, or of no app at all, and the panel would
  // look like it worked while showing the wrong container's output.
  {
    // A context whose fetch records what it was asked for and then ends the
    // stream immediately, so startLogs returns rather than following forever.
    const logContext = async (status, payload) => {
      const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
      ctx.globalThis = ctx;
      ctx.requests = [];
      ctx.fetch = async (url) => {
        ctx.requests.push(url);
        if (status !== 200) {
          return {
            ok: false,
            status,
            statusText: status === 501 ? "Not Implemented" : "Error",
            headers: { get: () => "application/json" },
            text: async () => JSON.stringify(payload || {}),
          };
        }
        return {
          ok: true,
          status: 200,
          statusText: "OK",
          headers: { get: () => "text/plain" },
          body: {
            getReader: () => ({
              read: async () => ({ done: true, value: undefined }),
            }),
          },
        };
      };
      // The script first, because `state` is a const at its top level and only
      // exists in this context once the script has run. The key and address are
      // normally set at sign-in; startLogs sends both.
      vm.runInContext(source, ctx, { filename: "console.js" });
      vm.runInContext('state.url = "https://applab.example.com"; state.key = "k"', ctx);
      await vm.runInContext("startLogs", ctx)("platform");
      return ctx;
    };

    {
      const ctx = await logContext(200);
      const asked = ctx.requests[0] || "";
      check(
        "the platform panel reads the platform's log",
        asked.startsWith("https://applab.example.com/api/v1/platform/logs?"),
        true
      );
      check("and does not name an app", asked.includes("/apps/"), false);
    }

    {
      // The dialog streams, and the request has to say so.
      //
      // The response is read through body.getReader() and appended to as chunks
      // arrive, and the Follow button toggles between Follow and Stop to abort
      // that read — so the whole design is the streaming one. It asked for
      // follow=false anyway, which returns a single snapshot and closes: the
      // reader hit done on the first read, the finally block reset the button to
      // "Follow", and output written after that never appeared. The panel looked
      // like a log viewer and behaved like a screenshot.
      const ctx = await logContext(200);
      const asked = ctx.requests[0] || "";
      check(
        "the log request asks the server to follow",
        asked.includes("follow=true"),
        true
      );
      check(
        "and never asks for a static snapshot while streaming the body",
        asked.includes("follow=false"),
        false
      );
    }

    {
      // No cluster client: a legitimate way to run, not a fault, so the panel
      // says which it is instead of sending someone after a bug that is not
      // there. Same 501 the branch control treats as "not configured".
      await logContext(501, { error: "this deployment cannot observe" });
      const text = elements.get("platform-logs").textContent || "";
      check("no cluster client is reported as such", text.includes("no cluster client"), true);
      check("and not as a failed read", text.includes("Could not read the log"), false);
    }

    {
      // Any other failure is a real one and keeps the generic wording, so the
      // 501 branch above is a distinction rather than a blanket rewrite.
      await logContext(500, { error: "boom" });
      const text = elements.get("platform-logs").textContent || "";
      check("any other failure is reported as a failed read", text.includes("Could not read the log"), true);
    }
  }

  // A build's pod belongs to the build, not to the app.
  //
  // A build Job's pod carries the app's label — it is how the uninstall sweep
  // finds it — so it used to appear in the app's Instances list as a pod that is
  // running but not ready. It is filtered out there by a label selector and
  // reported here instead, on the build it belongs to. Both halves are checked
  // because either one alone leaves the pod either hidden or duplicated.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.fetch = async (url) => ({
      ok: true,
      status: 200,
      statusText: "OK",
      headers: { get: () => "application/json" },
      text: async () =>
        JSON.stringify({
          data: [
            { id: "abcdef1234567890", commit_sha: "1234567890abcdef", status: "running", created_at: "2026-01-01T00:00:00Z", pod: { name: "applab-build-shop-abc" } },
            { id: "fedcba0987654321", commit_sha: "0987654321fedcba", status: "succeeded", created_at: "2026-01-01T00:00:00Z" },
            {
              id: "1111222233334444",
              commit_sha: "4444333322221111",
              status: "failed",
              created_at: "2026-01-01T00:00:00Z",
              reason: "error building image: getting stage builder for stage 0: failed to resolve source metadata for docker.io/library/golang:1.24-alpine",
              // A pod, because the events column is built from it. A Job that
              // failed before its pod existed carries none, which is the case
              // the check below covers with the row above.
              pod: { name: "applab-build-shop-def" },
            },
          ],
        }),
    });
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.app = "shop";', ctx);
    await vm.runInContext("loadAppBuilds", ctx)();

    const rows = elements.get("app-builds").children;
    const running = (rows[0] && rows[0].children[3] && rows[0].children[3].textContent) || "";
    const done = (rows[1] && rows[1].children[3] && rows[1].children[3].textContent) || "";

    check("a build in flight shows the pod it is running in", running, "applab-build-shop-abc");
    // Absent, not blank: a finished build's Job has been collected by its TTL, so
    // there is no pod to name and a caller must not read the column as one.
    check("and a build with no pod says so rather than showing nothing", done, "—");

    // The Events column, which replaced the reason text.
    //
    // The reason was a sentence from kaniko — the layer, the command, the exit
    // status — capped and truncated to keep the table on one line, which put the
    // useful half of a failure behind an ellipsis. Events carry the same
    // information with the rest of it: why the pod landed where it did, why the
    // pull was retried, what the container exited with.
    //
    // A button rather than text, because they open in the shared dialog, where
    // they are refreshed while it is up.
    const eventsCell = rows[2] && rows[2].children[5];
    const eventsButton = eventsCell && eventsCell.children[0];
    check("a finished build offers its pod's events", eventsButton && eventsButton.textContent, "Events");

    // Offered only when there is a pod to have events about. The second row is
    // the finished build with no pod, and a button that opened an empty panel
    // would read as a failure rather than as an absence.
    const noPodCell = rows[1] && rows[1].children[5];
    check("and a build with no pod offers none", (noPodCell && noPodCell.children.length) || 0, 0);

    // The deploy button is gone from this table. Deploying a build's commit is
    // what the History card and the State card's Deploy latest are for, and a
    // third copy of it here was a button on every successful row.
    const labels = rows.map((r) => r.children.map((c) => c.allText()).join(" ")).join(" | ");
    check("a build row no longer offers a deploy", labels.includes("Deploy"), false);

    // And no em dash on a finished build's stop cell. A column of dashes down a
    // table of completed builds is noise that reads as missing data; the cell is
    // simply empty. The running build is the exception, and it is the one that
    // has something to stop.
    const lastOf = (r) => (r.children[r.children.length - 1] || {}).allText();
    check("a finished build's stop cell is empty rather than a dash", lastOf(rows[1]) + lastOf(rows[2]), "");
    check("and the build still running keeps its stop", lastOf(rows[0]), "Stop");

    // The capped-cell rule is still in the stylesheet, because the instances
    // table's reason column still uses it. The cap has to be on a block child
    // rather than on the <td>: a table cell ignores max-width, so the obvious
    // version of this fix silently does nothing in a browser while reading as
    // though it did.
    check(
      "the capped-cell class is a real rule, not just a name",
      /\.report-cell\s*\{[^}]*max-width/.test(markup),
      true
    );
  }

  // The resource units.
  //
  // A reading is shown in cores and GiB while the API reports Kubernetes
  // quantities, so every figure crosses that boundary once. A mistake is silent
  // and expensive: memory written as a bare number means *bytes*, so 128Mi shown
  // as "134217728" beside a limit of "2" is not a comparison anyone can make —
  // and one shown as "0.13" beside a limit of "0.125" is a wrong one.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const conv = (name, arg) => vm.runInContext(name, ctx)(arg);

    // Read only, now that the form is gone: an app's bounds are set through the
    // API and the CLI, and the console's job is to show a reading in a unit a
    // person can compare against them. The round trip is still worth checking
    // here, because the two ends of it are still both in this file — a reading
    // is shown beside a bound, and a wrong conversion makes the pair look wrong
    // rather than making anything fail.
    for (const c of [
      { q: "500m", cores: "0.5" },
      { q: "1500m", cores: "1.5" },
      { q: "2", cores: "2" },
      { q: "100m", cores: "0.1" },
    ]) {
      check(`cpu ${c.q} reads as ${c.cores} cores`, conv("toCores", c.q), c.cores);
    }

    for (const m of [
      { q: "134217728", gi: "0.125" },
      { q: "512Mi", gi: "0.5" },
      { q: "1Gi", gi: "1" },
      { q: "2Gi", gi: "2" },
    ]) {
      check(`memory ${m.q} reads as ${m.gi} GiB`, conv("toGi", m.q), m.gi);
    }

    // An absent quantity reads as nothing rather than zero. This is the one
    // that matters most: the metrics API sends no field for a pod it has not
    // sampled, and rendering that as "0 cores" would show a busy pod as idle.
    check("and an absent quantity reads as nothing rather than zero", conv("toGi", ""), "");
    check("and an absent cpu quantity likewise", conv("toCores", ""), "");

    // Six decimals, checked because the precision is load-bearing rather than
    // cosmetic: 128Mi is 0.125 GiB and is also this deployment's own default, so
    // rounding it to 0.13 would change the value a save wrote back.
    check("128Mi reads as exactly 0.125 GiB", conv("toGi", "134217728"), "0.125");
    check("and the smallest sensible memory limit is distinguishable", conv("toGi", "1048576"), "0.000977");

    // A large limit is not rendered in scientific notation, which a number input
    // would refuse to hold.
    check("a 64 GiB limit renders as a plain number", conv("toGi", "68719476736"), "64");
  }

  // An app's address is offered over the scheme this page was loaded with.
  //
  // The API infers the scheme from the request, which is right when the
  // deployment is reached directly and a guess when something terminates TLS in
  // front of it. The page is by definition loaded over the scheme a person
  // reached the deployment with, so it is the better of the two — and a link
  // that opens a plain-http app from an https page is a mixed-content browser
  // warning rather than a working link.
  //
  // Only the scheme moves: the host is the API's, because with a path prefix
  // every app shares the deployment's host and only the server knows which app
  // got which.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const url = (u) => vm.runInContext("withPageScheme", ctx)(u);

    check(
      "an app address reported as http is offered as https on an https page",
      url("http://shop.apps.example.com"),
      "https://shop.apps.example.com"
    );
    check(
      "and the host and path are left alone",
      url("http://applab.example.com/applab/apps/shop"),
      "https://applab.example.com/applab/apps/shop"
    );
    check(
      "an address that is already https is unchanged",
      url("https://shop.apps.example.com"),
      "https://shop.apps.example.com"
    );
    // An installation with no base domain reports no address, and inventing a
    // scheme for an empty string would turn "nothing" into a link that resolves
    // nowhere.
    check("an empty address stays empty", url(""), "");
    check("and a relative path is not given a scheme", url("/apps/shop"), "/apps/shop");
  }

  // The instances list asks for the app's pods, and reports an empty answer as
  // "nothing is running".
  //
  // There used to be a label filter here, sent to the server as ?label=, and
  // this checked that the selector went out and that a filtered empty result was
  // worded differently from an empty app. The input is gone from the card — the
  // API and the CLI still take a selector, which is where it is useful — so what
  // is left to assert is that the request is the plain one and that an empty
  // list reads as an empty app rather than as a failure.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: { app_id: "shop", pods: [], count: 0 } }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.app = "shop";', ctx);

    await vm.runInContext("loadPods", ctx)();

    check(
      "the instances list asks for the app's pods, unfiltered",
      ctx.requests[0],
      "https://applab.example.com/api/v1/apps/shop/pods"
    );
    check(
      "and an empty answer reads as nothing running",
      elements.get("pods-empty").textContent,
      "Nothing is running."
    );
  }

  // Each instance row carries the two things a person comes to that table for.
  //
  // A pod's log and its events are about *that* pod, so they are on its row.
  // The card at the bottom of the page could only ever say "the app's", which is
  // the newest pod's — and a crash loop is usually an older one, which is
  // exactly when someone is looking.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    // Answered per URL, because loadPods makes two reads: the rows and the
    // reading that is a column of them. One stub for both would put the pod list
    // where the usage was expected, and every check below would pass while
    // testing nothing.
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      const body = String(url).endsWith("/resources")
        ? {
            app_id: "shop",
            available: true,
            cpu: "150m",
            memory: "96Mi",
            pods: { "applab-shop-abc": { cpu: "150m", memory: "96Mi" } },
          }
        : {
            app_id: "shop",
            count: 1,
            pods: [
              { name: "applab-shop-abc", phase: "Running", ready: false, restarts: 5, reason: "CrashLoopBackOff" },
            ],
          };
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: body }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.app = "shop";', ctx);
    await vm.runInContext("loadPods", ctx)();

    const row = elements.get("pods").children[0];
    // Found by class rather than by position: the row's cells have changed more
    // than once, and an index that has to be re-counted every time a column is
    // added is a check that quietly starts reading the wrong cell instead of
    // failing.
    const actions = row && row.children.find((c) => c.className === "actions");
    const buttons = (actions && actions.children) || [];
    const labels = buttons.map((b) => b.textContent).join(" | ");
    check("an instance row offers its own log", labels.includes("Log"), true);
    check("and its own events", labels.includes("Events"), true);

    // The status cell says the pod's phase, not "not ready". A pod that is
    // Running with a failing probe and one that is Pending are different
    // situations, and one word for both flattened them.
    //
    // Read through allText rather than textContent: the value is inside a pill
    // the cell contains, and the cell's own textContent is empty.
    //
    // Coloured by readiness rather than by phase, because the colour answers
    // "healthy": this pod is not ready, so it is the bad colour even though its
    // phase is a word that sounds fine.
    const cells = row.children.map((c) => c.allText());
    check("the status column reads the pod's phase", cells.includes("Running"), true);
    check("and not the old readiness wording", cells.includes("not ready"), false);
    const statusCell = row.children.find((c) => c.allText() === "Running");
    check("and it is coloured by readiness", statusCell.children[0].className, "pill bad");

    // The reason is still here, as the cell's title. It is the most useful single
    // thing about a pod that will not run, and hiding it in a tooltip is the
    // trade that keeps a long message from squeezing every other column.
    check("the reason is the status cell's title", statusCell.title, "CrashLoopBackOff");

    // And the usage is not a column any more: the dialog behind the row shows
    // the pod's own history, which one figure per replica never did.
    check("no cpu cell is left on the row", cells.includes("0.15 cores"), false);
    check("nor a memory cell", cells.includes("0.09 GiB"), false);

    // Clicking Log names the pod. Without it the endpoint answers with the
    // newest pod's log, so the button on a crashing row would show a different
    // container's output while looking like it worked.
    ctx.requests.length = 0;
    buttons[0].onclick();
    const asked = ctx.requests[0] || "";
    check("and the log it asks for names that pod", asked.includes("pod=applab-shop-abc"), true);
  }

  // What creating an app asks for.
  //
  // The dialog asks for the id and nothing else. Everything else an app is — its
  // port, its replicas, its branch, its bounds — has a default that is right for
  // a first deploy and is editable on the app's own page, beside what it
  // produces. Asking for a port at create time meant answering a question about
  // a container that did not exist yet.
  //
  // So the assertion is twofold: the request carries the id alone, and the
  // dialog is a dialog rather than a row that appears above the list.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.bodies = [];
    ctx.fetch = async (url, options) => {
      ctx.bodies.push({ url: String(url), body: options && options.body });
      return {
        ok: true,
        status: 201,
        statusText: "Created",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: { id: "shop", port: 80 } }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.key = "k";', ctx);

    // The dialog has no port field.
    check(
      "the create dialog asks for an id and no port",
      /id="apps-new-port"/.test(markup),
      false
    );
    check(
      "and the id field is in the markup it is looked up from",
      /id="apps-new-id"/.test(markup),
      true
    );

    // The request carries the id alone, so the server's own default is what a
    // new app gets rather than a number this page invented.
    elements.get("apps-new-id").value = "shop";
    try {
      await vm.runInContext("createApp", ctx)();
    } catch {
      /* the re-render after the create is not what this checks */
    }

    const posted = ctx.bodies.find((b) => b.url.endsWith("/api/v1/apps"));
    if (!posted) {
      check("creating an app posts to the apps collection", false, true);
    } else {
      const body = JSON.parse(posted.body);
      check("and sends the id", body.id, "shop");
      check(
        "and leaves the port to the server's default",
        Object.prototype.hasOwnProperty.call(body, "port"),
        false
      );
    }
  }

  // The app page's side navigation.
  //
  // Every link points at a section of the page, so the thing worth asserting is
  // that the ids exist — a typo is a link that does nothing, which looks like a
  // broken console rather than a missing section. Read from APP_SECTIONS, the
  // list the nav is built from, so a section added there without its markup is
  // caught too.
  {
    const sections = vm.runInContext("APP_SECTIONS", context);
    check("the app page declares its sections", Array.isArray(sections) && sections.length > 0, true);

    const body = markup.slice(markup.indexOf('id="app-view"'));
    const missing = (sections || []).filter((s) => !body.includes(`id="${s.id}"`));
    check(
      "and every section it lists is on the page",
      missing.map((s) => s.id).join(" | "),
      ""
    );

    // The nav sits in the page rather than being generated into it, and the list
    // is built from the declaration above.
    check("the nav is in the markup", markup.includes('id="app-nav"'), true);
    check(
      "and the renderer builds it from that declaration",
      /renderAppNav[\s\S]{0,400}?APP_SECTIONS/.test(source),
      true
    );

    // Every label has to be translatable: the nav is rebuilt on a language
    // switch, so an untranslated label would sit in English on a Chinese page.
    const labels = (sections || []).map((s) => s.label);
    const untranslated = labels.filter((l) => !(/\bt\(/.test(source) && source.includes(`"${l}":`)));
    check("and every section's label is a translated string", untranslated.join(" | "), "");

    // The sticky offset and the anchor's scroll margin have to agree, or a jump
    // puts the heading under the header.
    check("the nav sticks", /\.app-nav\s*\{[^}]*position:\s*sticky/.test(markup), true);
    check("and an anchored card clears the header", /scroll-margin-top/.test(markup), true);
  }

  // One failing panel must not leave the rest of a view half-built.
  //
  // Every loader on the app view used to be awaited with Promise.all, which
  // rejects on the first failure — so a single failing endpoint skipped every
  // line after it. The app view has moved on since (its log opens from a button
  // rather than on load), but the loaders are still awaited together, and a
  // generic loader failing is still the difference between a page with one
  // broken panel and a page that stopped drawing.
  //
  // Asserted on what rendered rather than on how: the app's own fields come from
  // loadAppState, and they have to be there even though loadPods blew up.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url) => {
      const target = String(url);
      ctx.requests.push(target);
      // One panel fails; everything else answers.
      if (target.includes("/pods")) {
        return {
          ok: false,
          status: 500,
          statusText: "Server Error",
          headers: { get: () => "application/json" },
          text: async () => JSON.stringify({ error: "boom" }),
        };
      }
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: { id: "shop", port: 8080, replicas: 2 } }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.key = "k";', ctx);

    let threw = "";
    try {
      await vm.runInContext("openApp", ctx)("shop");
    } catch (err) {
      threw = String(err && err.message);
    }
    await new Promise((r) => setTimeout(r, 0));

    check("a failing panel does not abort the app view", threw, "");
    check(
      "and the panels that did answer are still read",
      ctx.requests.some((u) => u.includes("/api/v1/apps/shop/commits")),
      true
    );
  }

  // One app's configuration and history must not survive into another's page.
  //
  // Both cards are filled per app, and neither read clears the table before it
  // has an answer — so opening a second app whose read fails left the first
  // app's rows on screen under the new app's title. In the configuration table
  // that is worse than a stale display: each row carries the value in
  // `dataset.value` and a Remove button aimed at `state.app`, so the button
  // under the new app deleted the *old* app's variable name from the new app —
  // and the name shown was not one the new app had. The history card has the
  // same shape with a Rollback button in it.
  //
  // Driven through the real loaders rather than by calling renderEnv directly,
  // because the bug is the failure path not reaching it.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];

    // The second app's config and commits reads fail; everything else answers.
    let second = false;
    ctx.fetch = async (url) => {
      const target = String(url);
      ctx.requests.push(target);
      if (second && (target.includes("/config") || target.includes("/commits"))) {
        return {
          ok: false,
          status: 500,
          statusText: "Server Error",
          headers: { get: () => "application/json" },
          text: async () => JSON.stringify({ error: "boom" }),
        };
      }
      if (target.includes("/config")) {
        return {
          ok: true, status: 200, statusText: "OK",
          headers: { get: () => "application/json" },
          text: async () => JSON.stringify({ data: { env: { SHOP_ONLY: "s3cret" } } }),
        };
      }
      if (target.includes("/commits")) {
        return {
          ok: true, status: 200, statusText: "OK",
          headers: { get: () => "application/json" },
          text: async () => JSON.stringify({ data: { commits: [{ sha: "a".repeat(40), created_at: "2026-01-01T00:00:00Z", message: "shop only" }] } }),
        };
      }
      // The build list is iterated, so it has to be a list rather than the
      // generic object the other paths get away with.
      if (target.includes("/builds")) {
        return {
          ok: true, status: 200, statusText: "OK",
          headers: { get: () => "application/json" },
          text: async () => JSON.stringify({ data: [] }),
        };
      }
      return {
        ok: true, status: 200, statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: { id: "x", port: 80, replicas: 1 } }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.key = "k";', ctx);

    await vm.runInContext("openApp", ctx)("shop");
    const envBody = elements.get("config-env");
    const commitsBody = elements.get("commits");
    check("the first app's config is on the page", envBody.children.length, 1);
    check("and its history", commitsBody.children.length, 1);

    second = true;
    await vm.runInContext("openApp", ctx)("blog");

    check(
      "opening an app whose config cannot be read shows no rows rather than another app's",
      envBody.children.length,
      0
    );
    check(
      "and the same for its history",
      commitsBody.children.length,
      0
    );
  }

  // A failed auto-deploy change says so.
  //
  // The handler puts the checkbox back from the server, which means reloading
  // the app's state — and loadAppState starts by clearing the error banner. With
  // the message set before that reload, the reload wiped it and the toggle
  // reverted in complete silence: the page appeared to disagree with the click
  // for no stated reason. The failure has to be reported after the restore.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url, options) => {
      const target = String(url);
      ctx.requests.push(target);
      if (options && options.method === "PATCH") {
        return {
          ok: false,
          status: 500,
          statusText: "Server Error",
          headers: { get: () => "application/json" },
          text: async () => JSON.stringify({ error: "nope" }),
        };
      }
      return {
        ok: true, status: 200, statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: { id: "shop", auto_deploy: true, port: 80, replicas: 1 } }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.key = "k"; state.app = "shop";', ctx);

    // The reader has just turned it off, so the box differs from the server.
    elements.get("app-auto-deploy").checked = false;
    await vm.runInContext("setAutoDeploy", ctx)();

    check(
      "a failed auto-deploy change is reported",
      elements.get("error").textContent.includes("Could not set auto-deploy"),
      true
    );
    check("and it is visible rather than hidden", elements.get("error").classList.contains("hidden"), false);
    check("while the box goes back to what the app is set to", elements.get("app-auto-deploy").checked, true);
  }

  // The log dialog is opened by a button, and it is the only thing that starts a
  // stream.
  //
  // This is the shape the console settled on after the log was moved out of the
  // page: a log is read by scrolling a long way and then closed, which is what a
  // dialog is for, and an always-present panel pushed everything below it off
  // the screen. What has to hold is that opening the dialog reads, and that
  // nothing reads before it — a stream started on view entry would open a dialog
  // nobody clicked.
  //
  // Both halves matter because the ids moved when the dialog was introduced:
  // LOG_PANELS still pointed at the old in-page elements after they were
  // removed, and because the panel description is read before the fetch, a
  // missing element threw and the log came up empty with nothing to say why.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: { id: "shop", pod: "shop-1" } }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.key = "k";', ctx);

    await vm.runInContext("openApp", ctx)("shop");
    await new Promise((r) => setTimeout(r, 0));

    check(
      "opening the app view does not read a log",
      ctx.requests.filter((u) => u.includes("/logs")).length,
      0
    );

    // Every panel's output element has to exist, or the stream throws before it
    // writes anything — which presents as an empty log rather than as an error.
    const described = vm.runInContext("Object.values(LOG_PANELS).map((p) => p.output)", ctx);
    const missing = described.filter((id) => !markup.includes(`id="${id}"`));
    check("every log panel points at an element that is in the page", missing.join(", "), "");

    // A row's Log button is what reads one, and it names the pod it was pressed
    // on — the platform log route answers with the newest replica's output when
    // no pod is given, which is not the row that was clicked.
    //
    // Opened with the platform scope, which is how the row above it passes what
    // its own table knows: the same opener serves an app's rows and these, and
    // the scope is what picks the endpoint.
    const opened = vm.runInContext("openPodLog", ctx);
    opened("applab-6b9f7-abc", "platform");
    await new Promise((r) => setTimeout(r, 0));

    const asked = ctx.requests.find((u) => u.includes("/api/v1/platform/logs")) || "";
    check("and a row's log button is what reads one", asked.length > 0, true);
    check("naming the pod the row was about", asked.includes("pod=applab-6b9f7-abc"), true);
  }

  // The document, and the thing that makes it worth having: the commands carry
  // the reader's own key when there is one and a placeholder when there is not.
  //
  // That is the whole reason it is rendered rather than written into the markup.
  // The page has to be correct in two states — signed out, which is when it is
  // most useful, and signed in, where a copyable command is — and a document
  // with a hardcoded `<key>` in it would be wrong in the second one for the
  // reader who is most likely to run it.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const docs = vm.runInContext("showDocs", ctx);
    const read = (id) => elements.get(id).textContent;

    // Signed out: no key, no app, so the commands have placeholders in both.
    vm.runInContext('state.url = "https://applab.example.com"; state.key = ""; state.scope = ""; state.app = "";', ctx);
    await docs();
    // Signed out is the case the document exists for, and someone who has not
    // signed in has no app either — so the role cards are exactly what they
    // need. Only an app key is a reader who already has their app.
    check("a signed-out reader is shown the role cards", elements.get("doc-admin").classList.contains("hidden"), false);

    vm.runInContext('state.url = "https://applab.example.com"; state.key = ""; state.scope = "admin"; state.app = "";', ctx);
    await docs();
    check("the document names the deployment", read("docs-push-cmd").includes("APPLAB_URL=https://applab.example.com"), true);
    check("and leaves the key for the reader to fill in", read("docs-push-cmd").includes("APPLAB_KEY=<your key>"), true);
    check("and does not put a real key in the commands", read("docs-push-cmd").includes("undefined"), false);
    // Signed out, the sign-in card is what the page shows and the document is
    // behind it — but the card has to step aside or the two overlap.
    check("the sign-in card steps aside for the document", elements.get("signin").classList.contains("hidden"), true);
    check("and the document is on screen", elements.get("docs-view").classList.contains("hidden"), false);

    // Signed in as an admin: the real key goes in, and the examples still name
    // no app, because an admin has none in particular.
    vm.runInContext('state.key = "sk-secret"; state.signedIn = true; state.scope = "admin"; state.app = "";', ctx);
    await docs();
    check("signed in, the commands carry the key", read("docs-push-cmd").includes("APPLAB_KEY=sk-secret"), true);
    check("and an admin's examples name no single app", read("docs-push-cmd").includes("applab push <app>"), true);
    // Both role cards are for this reader, and they have to actually be on
    // screen: the checks below read their commands out of the elements, which
    // is true whether or not the card is hidden. Paired with the app-key case
    // further down, so neither "always show" nor "always hide" passes.
    check("an admin is shown the make-an-app card", elements.get("doc-admin").classList.contains("hidden"), false);
    check("and the handover card", elements.get("doc-developer").classList.contains("hidden"), false);

    // How to get applab.sh in the first place, which is the admin's section and
    // the thing this page was missing: it opened with `cd <app> && ./applab.sh
    // status`, assuming a script the reader may have no way to obtain — it lives
    // in the repository, and someone who has not cloned has no repository.
    {
      const admin = read("docs-admin-cmd");
      check("the admin section says where to fetch applab.sh from", admin.includes("https://applab.example.com/bootstrap/applab.sh"), true);
      check("and makes it executable", admin.includes("chmod +x applab.sh"), true);
      check("and carries the reader's key", admin.includes("APPLAB_KEY=sk-secret"), true);
      check("and makes an app with create", admin.includes("./applab.sh create <app>"), true);
      check("and not a use of an app they did not name", admin.includes("./applab.sh use"), false);
      // What the admin actually does next is hand the app over, so the second
      // role card says what to give the developer — and reading the key back is
      // what makes the handover possible, since it is not shown anywhere else.
      const developer = read("docs-developer-cmd");
      check("the developer section hands over the clone address", developer.includes("git clone https://applab.example.com/git/<app>.git"), true);
      check("and says how to read the app's key back for handover", developer.includes("applab keys <app>"), true);
    }

    // Signed in with an app key: one app, so the examples name it, and the two
    // role cards step aside — this reader has their app already.
    vm.runInContext('state.key = "app-secret"; state.scope = "app"; state.app = "shop";', ctx);
    await docs();
    check("an app key's examples name its own app", read("docs-push-cmd").includes("applab push shop"), true);
    check("and its clone command does too", read("docs-clone-cmd").includes("/git/shop.git"), true);
    check("an app key is not shown the admin's make-an-app card", elements.get("doc-admin").classList.contains("hidden"), true);
    check("nor the handover card", elements.get("doc-developer").classList.contains("hidden"), true);

    // Configure: the running app's environment, secrets, bounds and count.
    {
      const configure = read("docs-configure-cmd");
      check("the configure section sets env", configure.includes("./applab.sh env set"), true);
      check("sets secrets", configure.includes("./applab.sh secret set"), true);
      check("sets resource bounds", configure.includes("./applab.sh resources set"), true);
      check("and reads back what is set", configure.includes("./applab.sh config"), true);
    }

    // Back to the app key for the sign-out check below.
    vm.runInContext('state.key = "app-secret"; state.scope = "app"; state.app = "shop";', ctx);
    await docs();

    // Signing out must not leave the discarded key sitting in the commands.
    vm.runInContext("signOut", ctx)();
    check("signing out drops the key from the commands", read("docs-push-cmd").includes("sk-secret"), false);
    check("and leaves the placeholder", read("docs-push-cmd").includes("APPLAB_KEY=<your key>"), true);
    check("while the sign-in card comes back", elements.get("signin").classList.contains("hidden"), false);
  }


  // The document is the one view reachable without a key, and it is a view like
  // any other: it has a table of contents, every entry in it points at a section
  // that exists, and the header's name is a way back out.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    await vm.runInContext("showDocs", ctx)();
    const nav = elements.get("docs-nav");
    check("the document has a table of contents", nav.children.length > 0, true);

    const sections = vm.runInContext("DOC_SECTIONS", ctx);
    const missing = sections.filter((s) => !markup.includes('id="' + s.id + '"'));
    check("and every entry points at a section that is in the page", missing.map((s) => s.id).join(", "), "");

    // Back to the sign-in card. Without this, opening the document before
    // signing in would be a one-way trip: the header's name is the only control
    // left, and it was disabled in the state.
    vm.runInContext("goHome", ctx)();
    check("the header's name returns to the sign-in card", elements.get("signin").classList.contains("hidden"), false);
    check("and leaves the document behind", elements.get("docs-view").classList.contains("hidden"), true);
  }

  // The dialogs are outside every view section.
  //
  // This is the bug that made all three log views unusable. Both dialogs used to
  // sit inside #app-view — contradicting the comment above them, which says they
  // are placed outside the scrolling column — and #app-view is hidden by navTo
  // whenever another view is showing. So the platform log's button, which lives
  // on the overview, opened a dialog inside a section with `display: none`, and
  // nothing appeared at all.
  //
  // Asserted structurally, because that is the whole of the failure: the markup
  // and the handlers were all present and correct, and the element was simply not
  // reachable from where the button was.
  {
    const idx = (needle) => markup.indexOf(needle);
    const mainOpen = idx("<main>");
    const mainClose = idx("</main>");
    // The three view sections, and where each ends. The last one runs to the
    // final </section> before </main>, not to </main> itself — the dialogs sit
    // between the two, which is exactly the distinction being asserted.
    const bounds = [
      ["overview-view", idx('id="apps-view"')],
      ["apps-view", idx('id="app-view"')],
      ["app-view", markup.lastIndexOf("</section>")],
    ]
      .map(([id, end]) => [idx('id="' + id + '"'), end])
      .filter(([a]) => a >= 0);

    for (const dialog of ["log-modal", "apps-new-modal"]) {
      const at = idx('id="' + dialog + '"');
      check(
        `the ${dialog} dialog exists in the markup`,
        at > mainOpen && at < mainClose,
        true
      );
      if (!(at > mainOpen && at < mainClose)) continue;

      const inside = bounds.find(([a, b]) => at > a && at < b);
      check(
        `and the ${dialog} dialog is not inside a view section`,
        inside ? "inside the section containing offset " + inside[0] : "",
        ""
      );
    }

  }

  // The platform instances card, which replaced the monitoring panel and the
  // platform log card.
  //
  // Two cards answering half a question each became one list: the monitoring
  // panel showed a single pod's CPU and memory behind a picker, and the log card
  // was a button opening the newest pod's log. Neither said which pod was the odd
  // one out, and the picker existed only because a total cannot show an outlier —
  // a list shows every pod at once, so there is nothing left to pick between.
  //
  // Asserted on the markup because the stub creates an element for any id the
  // script mentions, so a check that only looked one up would pass with the card
  // deleted from the page.
  {
    const idx = (needle) => markup.indexOf(needle);

    check("the overview carries a platform instances card", markup.includes('id="card-platform"'), true);
    check("with a table to fill", markup.includes('id="platform-instances"'), true);
    check("and a placeholder for an empty cluster", markup.includes('id="platform-instances-empty"'), true);

    // It is the overview's, so it is inside that section and not the app view's.
    const card = idx('id="card-platform"');
    check(
      "inside the overview view",
      card > idx('id="overview-view"') && card < idx('id="apps-view"'),
      true
    );

    // And the app page's side nav lists cards of the *app* view, so an entry for
    // this one would be a link to a card that page never renders.
    check("and it is not one of the app page's sections", markup.includes('{ id: "card-platform"'), false);

    // The two cards it replaced are gone. A leftover heading would be a second
    // answer to the same question, which is how the two drifted apart to begin
    // with.
    check("the monitoring panel is gone", markup.includes('id="card-monitor"'), false);
    check("and the platform log card with it", markup.includes('id="platform-logs-open"'), false);
  }

  // What the platform instances table renders: one row per pod, with its own
  // reading, and a Log button that names that pod.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url) => {
      const target = String(url);
      ctx.requests.push(target);

      // Answered per endpoint, because this block presses all three of a row's
      // buttons and each one reads something different. A single stub that
      // answered everything the same way would let a button read the wrong
      // endpoint and still look like it worked.
      const data = target.includes("/resources")
        ? { available: true, limited: {}, pods: {} }
        : target.includes("/events")
          ? { events: [] }
          : {};

      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        // A stream of one chunk, so startLogs finishes rather than waiting.
        body: { getReader: () => ({ read: async () => ({ done: true, value: undefined }) }) },
        text: async () => JSON.stringify({ data }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });

    const render = vm.runInContext("renderInstances", ctx);
    const body = elements.get("platform-instances");
    render({
      body,
      empty: elements.get("platform-instances-empty"),
      note: elements.get("platform-usage-note"),
      pods: [
        { name: "applab-6b9f7-abc", phase: "Running", ready: true, restarts: 0 },
        { name: "applab-6b9f7-def", phase: "Running", ready: false, restarts: 3, reason: "CrashLoopBackOff" },
      ],
      usage: {
        available: true,
        pods: {
          "applab-6b9f7-abc": { cpu: "12m", memory: "80Mi" },
          "applab-6b9f7-def": { cpu: "4m", memory: "20Mi" },
        },
      },
      failure: "",
      scope: "platform",
    });

    check("one row per pod", body.children.length, 2);
    check("and an empty cluster is not reported", elements.get("platform-instances-empty").classList.contains("hidden"), true);

    const first = body.children[0];
    check("the pod is named", first.children[0].textContent, "applab-6b9f7-abc");
    check("with its phase", first.children[1].allText(), "Running");
    check("and the healthy colour", first.children[1].children[0].className, "pill ok");
    check("and its restart count", first.children[2].textContent, 0);

    // A pod that is Running but not ready is the case the colour has to get
    // right: its phase reads fine and it is not. The reason that explains it is
    // the cell's title rather than a column of its own.
    const second = body.children[1];
    check("a not-ready pod is coloured as bad", second.children[1].children[0].className, "pill bad");
    check("and its reason is the title", second.children[1].title, "CrashLoopBackOff");


    // And its Log button names *that* pod. Without it the endpoint answers with
    // the newest replica's output, which is not the row that was clicked.
    //
    // Found by class rather than by index: the row's cells have changed more than
    // once, and a count that has to be redone whenever a column moves is a check
    // that silently reads the wrong cell instead of failing.
    const actions = second.children.find((c) => c.className === "actions");
    if (!actions) {
      console.error("FAIL: the row has no actions cell\n  row cells: " + second.children.map((c) => c.className || c.textContent).join(", "));
      process.exit(1);
    }
    check("every row offers its own log", actions.children[0].textContent, "Log");
    check("and its own metrics", actions.children[1].textContent, "Metrics");
    // Events used to be absent here, and their absence was asserted: an app's
    // events route matches on that app's label and there was no platform
    // equivalent. GET /api/v1/platform/events is that equivalent, so the button
    // belongs on these rows as much as on an app's.
    check("and its own events", actions.children[2].textContent, "Events");

    ctx.requests.length = 0;
    actions.children[0].onclick();
    await new Promise((r) => setTimeout(r, 0));
    const asked = ctx.requests.find((u) => u.includes("/platform/logs")) || "";
    check("and the log it reads names that pod", asked.includes("pod=applab-6b9f7-def"), true);

    // The readings read the platform's own route. They used to read the app's,
    // which on the overview meant `/api/v1/apps//resources` — an empty app id,
    // because this table is not about an app. This is the check that pins it.
    ctx.requests.length = 0;
    actions.children[1].onclick();
    await new Promise((r) => setTimeout(r, 0));
    const usageAsked = ctx.requests.find((u) => u.includes("/resources")) || "";
    check("and the readings it reads are the platform's", usageAsked.includes("/api/v1/platform/resources"), true);
    check("not an app's, which has no app to name here", usageAsked.includes("/api/v1/apps//"), false);
    vm.runInContext("closePodMetrics", ctx)();

    // And the events, likewise.
    ctx.requests.length = 0;
    actions.children[2].onclick();
    await new Promise((r) => setTimeout(r, 0));
    const eventsAsked = ctx.requests.find((u) => u.includes("/events")) || "";
    check("and the events it reads are the platform's", eventsAsked.includes("/api/v1/platform/events"), true);
    check("not an app's", eventsAsked.includes("/api/v1/apps//"), false);
    vm.runInContext("closeLogDialog", ctx)();

  }

  // A cluster with no metrics API.
  //
  // The table says so rather than showing zeros. A control plane being starved of
  // CPU and an idle one look identical at zero, and only one of them is a problem
  // — so the distinction is the whole reason the available flag exists.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const render = vm.runInContext("renderInstances", ctx);
    render({
      body: elements.get("platform-instances"),
      empty: elements.get("platform-instances-empty"),
      note: elements.get("platform-usage-note"),
      pods: [{ name: "applab-6b9f7-abc", phase: "Running", ready: true, restarts: 0 }],
      usage: { available: false },
      failure: "",
      scope: "platform",
    });

    const note = elements.get("platform-usage-note");
    check("with no metrics the table says so", note.classList.contains("hidden"), false);

    // The rows are still there. A reading that could not be taken must not empty
    // a table whose other columns were read fine — the pods and their restarts
    // are as useful without it, and the row's Metrics button is where the
    // absence is explained.
    const row = elements.get("platform-instances").children[0];
    check("and the pod is still listed", row.children[0].textContent, "applab-6b9f7-abc");
    check("with its state, which needs no metrics", row.children[1].allText(), "Running");
  }

  // Building is triggered from where the builds are.
  //
  // It used to be reachable only from the State card's "Build latest" — a
  // control on a card about something else, so someone looking at a build
  // history to start a build had to know to look a card up. Both are wired now,
  // and the History card's "no image" dead end became a Build button, since a
  // commit with no image is precisely the one that needs building.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.bodies = [];
    ctx.fetch = async (url, options) => {
      ctx.requests.push(String(url));
      if (options && options.body) ctx.bodies.push({ url: String(url), body: options.body });
      const u = String(url);
      // The History card reads three things: the commits, what has an image, and
      // the app's own record for which commit is deployed.
      let data = { commits: [] };
      if (u.includes("/commits")) {
        // `head` is what buildLatest reads, from the commits endpoint rather
        // than from the app record — so a stub that only filled `commits` would
        // leave the button with nothing to build and the check passing on an
        // empty body.
        data = {
          head: "aaaa1111",
          commits: [{ sha: "aaaa1111", created_at: "2026-01-01T00:00:00Z", message: "second" }],
        };
      } else if (u.includes("/builds")) {
        data = [];
      } else if (u.includes("/api/v1/apps/shop")) {
        // Deliberately a *different* commit from the one listed: the app record
        // is what says which commit is deployed, and if it named the same one
        // the row would render the "deployed" pill and never reach the branch
        // under test.
        data = { id: "shop", commit_sha: "beef9999" };
      }
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.app = "shop";', ctx);

    // The Builds card's own trigger, which is the thing this change adds.
    //
    // Asserted on the markup rather than on the element: the stub creates one
    // for any id the script mentions, so a check that only looked it up would
    // pass with the button deleted from the page — which is exactly the mistake
    // worth catching, since the whole change is that the control is *there*.
    check("the build module has its own trigger", markup.includes('id="app-builds-build"'), true);

    const buildsBuild = elements.get("app-builds-build");
    check("and it is wired", !!buildsBuild.onclick, true);

    // Pressing it builds the newest commit.
    ctx.bodies.length = 0;
    await buildsBuild.onclick();
    const posted = ctx.bodies.find((b) => b.url.endsWith("/builds")) || {};
    check("and it builds the newest commit", posted.body, JSON.stringify({ commit_sha: "aaaa1111" }));

    // The History card's action column, for a commit with no image.
    await vm.runInContext("loadCommits", ctx)();
    const actionCell = elements.get("commits").children[0].children[2];
    const actionButton = actionCell && actionCell.children[0];
    check("a commit with no image offers to build it", actionButton && actionButton.textContent, "Build");

    // And pressing that builds *that* commit rather than the tip — the whole
    // point of it being on a row.
    ctx.bodies.length = 0;
    await actionButton.onclick();
    const named = ctx.bodies.find((b) => b.url.endsWith("/builds")) || {};
    check("and it builds the commit on its own row", named.body, JSON.stringify({ commit_sha: "aaaa1111" }));

    // A commit that already has an image still offers the rollback instead. The
    // build button is for the commit that cannot be deployed yet, and offering
    // both would make the column mean two things.
    ctx.fetch = async (url) => {
      const u = String(url);
      let data = { commits: [] };
      if (u.includes("/commits")) {
        data = { head: "bbbb2222", commits: [{ sha: "bbbb2222", created_at: "2026-01-01T00:00:00Z", message: "x" }] };
      } else if (u.includes("/builds")) {
        data = [{ commit_sha: "bbbb2222", status: "succeeded", image: "registry/shop:bbbb" }];
      } else if (u.includes("/api/v1/apps/shop")) {
        data = { id: "shop", commit_sha: "beef9999" };
      }
      return {
        ok: true, status: 200, statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data }),
      };
    };
    await vm.runInContext("loadCommits", ctx)();
    const builtCell = elements.get("commits").children[0].children[2];
    check(
      "a commit that has an image offers a rollback, not a build",
      builtCell && builtCell.children[0] && builtCell.children[0].textContent,
      "Roll back"
    );
  }

  // The periodic refresh, which replaced the per-card Refresh buttons.
  //
  // What is asserted is the part that would be a bug: the timer exists, it is
  // not so eager that it hammers the API, and it declines to fire in the three
  // cases where reloading would be wrong — signed out, a hidden tab, and a
  // reader mid-edit. The last is the one that would actually lose work: the
  // loaders write form fields from the server, so a tick between two keystrokes
  // would replace what someone was typing with the stored value.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({ data: { apps: {}, builds: {} } }),
      };
    };

    // The elements and the interval list are shared by every block in this file
    // and earlier ones have opened dialogs, so the starting state is set rather
    // than assumed — otherwise a card left showing by an unrelated check would
    // suppress the refresh and every assertion below would pass for a reason
    // that has nothing to do with what it is testing.
    elements.get("log-modal").classList.add("hidden");
    elements.get("apps-new-modal").classList.add("hidden");
    ctx.document.activeElement = null;
    const before = intervals.length;

    vm.runInContext(source, ctx, { filename: "console.js" });
    await new Promise((r) => setTimeout(r, 0));

    // Boot starts it, so a page that is never touched still keeps itself true.
    const started = intervals.slice(before);
    check("boot starts a refresh timer", started.length, 1);
    // Conservative on purpose. A dashboard that polls every second is a
    // dashboard that costs more than it is looked at.
    check("and the interval is not aggressive", started[0].ms >= 5000, true);

    const refresh = vm.runInContext("refreshCurrent", ctx);

    // Signed out: nothing to refresh and nothing to refresh it with.
    vm.runInContext("state.signedIn = false", ctx);
    ctx.requests.length = 0;
    await refresh();
    check("a signed-out console refreshes nothing", ctx.requests.length, 0);

    // Signed in on the overview: the read the view is made of.
    vm.runInContext('state.signedIn = true; state.view = "overview";', ctx);
    ctx.requests.length = 0;
    await refresh();
    check("signed in, the overview is re-read", ctx.requests.some((u) => u.includes("/api/v1/overview")), true);

    // Mid-edit: the case that would overwrite what someone is typing.
    vm.runInContext("state.view = 'app'", ctx);
    ctx.document.activeElement = { tagName: "INPUT", type: "number" };
    ctx.requests.length = 0;
    await refresh();
    check("a tick while someone is typing is skipped", ctx.requests.length, 0);

    // But a checkbox being focused is not someone typing, and must not block it.
    ctx.document.activeElement = { tagName: "INPUT", type: "checkbox" };
    ctx.requests.length = 0;
    await refresh();
    check("and a focused checkbox does not block it", ctx.requests.length > 0, true);

    // Nothing focused at all — the ordinary case.
    ctx.document.activeElement = null;
    ctx.requests.length = 0;
    await refresh();
    check("and with nothing focused the app view is re-read", ctx.requests.some((u) => u.includes("/pods")), true);
  }

  // The events panel is live.
  //
  // Events are the answer to "why is this not starting", and that answer arrives
  // *after* the dialog is opened — a pod still pulling its image has one event
  // when it is opened and three a minute later. A panel that showed the snapshot
  // from when it opened would be stale exactly when it mattered.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({
          data: {
            // "Kind/Name", which is what the API sends. This stub used to send
            // the bare pod name, which matched the comparison the console made
            // and hid the fact that the server's answer never did — so the panel
            // reported "no recent events" for every pod in the real product while
            // this test passed.
            events: [
              { object: "Pod/applab-build-shop-abc", type: "Warning", reason: "FailedScheduling", message: "no nodes", count: 3 },
              { object: "Pod/someone-elses-pod", type: "Normal", reason: "Pulled", message: "a different pod entirely" },
            ],
          },
        }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.app = "shop";', ctx);

    const before = intervals.length;
    const pageIntervalMs = intervals.find((i) => i.ms >= 5000).ms;
    await vm.runInContext("showPodEvents", ctx)("applab-build-shop-abc");

    const el = elements.get("events-output");
    const text = el.allText();
    check("the events panel reads the pod when it opens", ctx.requests.length, 1);
    // Filtered to the pod. The endpoint answers per app, and every app's pods
    // share one namespace, so an unfiltered render would show another pod's
    // events under this one's name.
    check("and shows only that pod's", text.includes("FailedScheduling"), true);
    // Matched on the other event's message rather than on its object name: the
    // rendered lines carry the reason and the message, not the object, so a
    // check looking for the name would pass on an unfiltered render — which is
    // exactly the bug it is here to catch.
    check("not another pod's", text.includes("a different pod entirely"), false);
    check("with the count when there is one", text.includes("x3"), true);

    // And it keeps reading: one more timer, on its own interval.
    check("and it starts its own timer", intervals.length, before + 1);
    check("at a shorter interval than the page", intervals[before].ms < pageIntervalMs, true);

    // Opening another panel stops it. A timer left running behind a log the
    // reader has moved on to is a request every five seconds for output nobody
    // will render.
    vm.runInContext("stopEvents", ctx)();
    check("and switching away stops it", vm.runInContext("state.eventsTimer", ctx), null);
    check("and clears the panel's timer handle", intervals.length - before, 1);
  }

  // Closing a dialog while its first read is still in flight must not leave a
  // timer behind.
  //
  // The events panel and the metrics dialog both used to create their interval
  // *after* awaiting the first read. A close arriving during that read therefore
  // found the timer slot still empty, cleared nothing, and then the read's
  // continuation assigned an interval nothing would ever clear — the dialog was
  // closed, so no close path runs again. The panel went on polling until the
  // page was reloaded.
  //
  // The window is the duration of one request, which is small but not
  // theoretical: against a deployment with no cluster the metrics read is a 501
  // that takes as long as the server takes to say so. Driven here by holding
  // the fetch open until the test says to let it go.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];

    let release;
    const held = new Promise((resolve) => { release = resolve; });
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      await held;
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({
          data: { available: true, limited: {}, pods: { "applab-shop-abc": { cpu: "250m", memory: "128Mi" } }, events: [] },
        }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.app = "shop";', ctx);

    const before = intervals.length;

    // Events first.
    const events = vm.runInContext("showPodEvents", ctx)("applab-shop-abc");
    vm.runInContext("closeLogDialog", ctx)();
    release();
    await events;
    check(
      "a dialog closed during its first read leaves no events timer",
      vm.runInContext("state.eventsTimer", ctx),
      null
    );

    // And the metrics dialog, which has the same shape.
    const metrics = vm.runInContext("openPodMetrics", ctx)("applab-shop-abc");
    vm.runInContext("closePodMetrics", ctx)();
    release();
    await metrics;
    check(
      "a metrics dialog closed during its first read leaves no sample timer",
      vm.runInContext("state.metricsTimer", ctx),
      null
    );
    check("and neither one grew a timer after the close", intervals.length - before <= 2, true);
  }

  // The instances table distinguishes "no metrics here" from "the read failed".
  //
  // They are different problems with different remedies, and conflating them
  // sends whoever is debugging to the wrong place: a 401 from a stale key, a 500,
  // or a proxy with no route all used to render as "this cluster reports no
  // metrics", which points at metrics-server and away from the answer, which was
  // in the response body.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const render = vm.runInContext("renderInstances", ctx);
    const note = elements.get("platform-usage-note");
    const body = elements.get("platform-instances");

    const show = (usage, failure) => render({
      body,
      empty: elements.get("platform-instances-empty"),
      note,
      pods: [{ name: "applab-6b9f7-abc", phase: "Running", ready: true, restarts: 0 }],
      usage,
      failure,
      scope: "platform",
    });

    // No metrics-server: a fact about the installation, and the message says so.
    show({ available: false }, "");
    check(
      "with no metrics API the table names that",
      note.textContent,
      "Resource usage is not available: this cluster reports no metrics."
    );

    // A request that failed: the message names what the server said instead.
    show(null, "401 Unauthorized");
    check(
      "and a failed read reports the failure rather than blaming the cluster",
      note.textContent,
      "Could not read resource usage: 401 Unauthorized"
    );

    // A cluster that answers but has sampled nothing yet is a third case, and it
    // is not an error: the row still renders from what the pod list said, and the
    // empty readings simply do not appear anywhere — they are the dialog's now,
    // and it draws an em dash for one it has not sampled yet.
    show({ available: true, cpu: "", memory: "", pods: null }, "");
    check("an unsampled cluster still shows the row", body.children[0].children[0].textContent, "applab-6b9f7-abc");
    check("and does not claim metrics are missing", note.classList.contains("hidden"), true);
    check("and does not claim metrics are missing", note.classList.contains("hidden"), true);
  }

  // The metrics dialog: a reading per pod, sampled while it is open.
  //
  // Opened from a row, so it names that pod rather than the newest — which is the
  // same reason the log button does, and the same mistake if it did not: a crash
  // loop is usually not the newest replica.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    ctx.requests = [];
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      return {
        ok: true, status: 200, statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({
          data: {
            available: true,
            limited: { cpu: "1", memory: "512Mi" },
            pods: { "applab-shop-abc": { cpu: "250m", memory: "256Mi" } },
          },
        }),
      };
    };
    vm.runInContext(source, ctx, { filename: "console.js" });
    vm.runInContext('state.url = "https://applab.example.com"; state.app = "shop";', ctx);

    await vm.runInContext("openPodMetrics", ctx)("applab-shop-abc");

    // It names the pod, and reads that app's endpoint.
    // The pod is a control now rather than a label, so what it names is its
    // value — and the options are the table's pods, which is what lets the
    // dialog move to another replica without being closed.
    check("the dialog names the pod it was opened on", elements.get("metrics-pod-select").value, "applab-shop-abc");
    check("and it is shown", elements.get("metrics-modal").classList.contains("hidden"), false);
    check(
      "reading the app's usage",
      ctx.requests.some((u) => u.includes("/api/v1/apps/shop/resources")),
      true
    );

    // The newest reading is the headline, in the units the rest of the page
    // uses — 250m is a quarter of a core.
    check("with the cpu reading", elements.get("metrics-cpu-value").textContent, "0.25 cores");
    check("and the memory reading", elements.get("metrics-memory-value").textContent, "0.25 GiB");

    // And the line is drawn from it: a path, not an empty box.
    check("and a line is drawn", elements.get("metrics-cpu-plot").innerHTML.includes("<path"), true);
    // Scaled to the limit by default, which is what makes the line's height mean
    // "how much of the allowance is in use".
    check("scaled against the limit", elements.get("metrics-cpu-range").textContent, "limit 1 cores");

    // The timer it put up actually samples. Asserting only that a timer exists
    // is not enough: the interval used to be created before `metricsPod` was
    // set, so every fired tick took the "no pod" early return and the chart
    // stayed a single point forever. Firing it here is what tells the two
    // apart, and the sample count is the thing that changes.
    const samplesBefore = vm.runInContext("state.metricsSamples.cpu.length", ctx);
    const tick = intervals[intervals.length - 1];
    await tick.fn();
    check(
      "and each tick adds a reading rather than returning early",
      vm.runInContext("state.metricsSamples.cpu.length", ctx),
      samplesBefore + 1
    );

    // A pod the response does not mention is said so, not charted from another
    // pod's numbers. A pod is replaced by a deploy or a crash loop, and the
    // dialog stays on the name it was opened with — showing the new pod's
    // reading under the old pod's name is the one answer that is worse than
    // saying nothing.
    const beforeReopen = intervals.length;
    elements.get("metrics-modal").classList.add("hidden");
    ctx.fetch = async (url) => {
      ctx.requests.push(String(url));
      return {
        ok: true, status: 200, statusText: "OK",
        headers: { get: () => "application/json" },
        text: async () => JSON.stringify({
          data: {
            available: true,
            limited: {},
            pods: { "applab-shop-OTHER": { cpu: "900m", memory: "900Mi" } },
          },
        }),
      };
    };
    await vm.runInContext("openPodMetrics", ctx)("applab-shop-abc");
    check(
      "a pod that stopped reporting is said so rather than charted from another",
      elements.get("metrics-note").textContent.includes("no longer reporting"),
      true
    );
    check(
      "and its reading is not another pod's",
      elements.get("metrics-cpu-value").textContent,
      "–"
    );
    vm.runInContext("closePodMetrics", ctx)();

    // Sampling is a timer, and it stops when the dialog closes.
    check("and it samples on a timer", intervals.length, beforeReopen + 1);
    vm.runInContext("closePodMetrics", ctx)();
    check("closing stops it", vm.runInContext("state.metricsTimer", ctx), null);
    check("and hides the dialog", elements.get("metrics-modal").classList.contains("hidden"), true);
    // The history goes with it: one pod's samples under another's name would be
    // a line that is wrong for as long as the next reading takes.
    check("and forgets the samples", vm.runInContext("state.metricsSamples.cpu.length", ctx), 0);

    // The pod is a control: the dialog moves to another replica without being
    // closed. The list is the table's, so the names offered are the ones the
    // reader was looking at.
    {
      const pods = [{ name: "applab-shop-abc" }, { name: "applab-shop-def" }];
      const select = elements.get("metrics-pod-select");
      await vm.runInContext("openPodMetrics", ctx)("applab-shop-abc", "app", pods);
      // The stub has no real <select>, so the options are whatever was appended.
      check("the picker offers the table's pods", select.children.length, 2);
      check("starting on the one that was clicked", select.value, "applab-shop-abc");

      // And the pod it was opened on is always there, even when the list has
      // moved on — a replacement during a rollout is the ordinary case.
      await vm.runInContext("openPodMetrics", ctx)("applab-shop-gone", "app", pods);
      check(
        "and carries the opened pod even when the list no longer has it",
        select.children.length,
        3
      );
      check("selected rather than lost", select.value, "applab-shop-gone");
    }
  }

  // What the chart does with the awkward answers.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const draw = vm.runInContext("drawChart", ctx);
    const plot = elements.get("metrics-cpu-plot");
    const valueEl = elements.get("metrics-cpu-value");
    const rangeEl = elements.get("metrics-cpu-range");

    // Nothing sampled yet: an em dash, not a zero. A pod that has just started
    // and a cluster that cannot measure look identical at zero.
    draw({ plot, valueEl, rangeEl, samples: [], ceiling: 1, unit: "cores", format: vm.runInContext("formatCores", ctx), limitLabel: "1" });
    check("with no samples the reading is unknown rather than zero", valueEl.textContent, "–");

    // Automatic scale: the axis follows the data and says so, because there is
    // no limit to be a fraction of.
    const formatCores = vm.runInContext("formatCores", ctx);
    draw({
      plot, valueEl, rangeEl,
      samples: [{ at: 1, value: 0.1 }, { at: 2, value: 0.2 }],
      ceiling: null, unit: "cores", format: formatCores, limitLabel: "",
    });
    check("without a limit the axis says it is the peak", rangeEl.textContent, "peak 0.22 cores");

    // A reading above its own limit is drawn past the ceiling rather than
    // clamped to it — the crossing is the message.
    draw({
      plot, valueEl, rangeEl,
      samples: [{ at: 1, value: 0.5 }, { at: 2, value: 2 }],
      ceiling: 1, unit: "cores", format: formatCores, limitLabel: "1",
    });
    const ys = [...plot.innerHTML.matchAll(/L?([\d.]+) ([\d.]+)/g)].map((m) => Number(m[2]));
    check("a reading over its limit is drawn above the ceiling line", Math.min(...ys) < 50, true);
  }

  // The clone command's mask is the width of the app's address above it.
  //
  // Two values in one column, one of them a run of bullets of its own length,
  // read as a ragged column — the mask is the taller of the two and the address
  // looks cut short beside it. The mask asks the address how wide it is.
  //
  // Exercised through the registration the console actually makes rather than
  // through mask() alone: the width comes from a callback that reads another
  // element, and a check on the pure function would pass while the callback
  // returned a constant.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });
    await new Promise((r) => setTimeout(r, 0));

    const address = vm.runInContext('$("app-address")', ctx);
    const hint = vm.runInContext('$("app-git-hint")', ctx);
    const reveal = vm.runInContext("appGitReveal", ctx);

    address.textContent = "https://shop.apps.example.com/with/a/long/path";
    // 200 characters of key against a 47-character address: without the width
    // the mask would be capped at the constant, so a "matches" assertion here is
    // only meaningful because the two numbers differ.
    reveal.set("x".repeat(200));
    check(
      "the clone command's mask matches the address's width",
      hint.textContent.length,
      address.textContent.length
    );

    // And the floor: a short address does not produce a short mask, which reads
    // as a short value rather than as a withheld one.
    address.textContent = "https://a.io";
    reveal.refresh();
    check("and never drops below the readable minimum", hint.textContent.length, 32);

    // Masked, not shown. The whole point of the eye.
    check("and the key itself is not on the page", hint.textContent.includes("x"), false);
  }

  if (failures > 0) {
    console.error(`\n${failures} check(s) failed`);
    process.exit(1);
  }
  console.log("\nall console rendering checks passed");
})();
