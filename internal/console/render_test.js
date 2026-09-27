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
    querySelectorAll: () => [],
    createRange: () => ({ selectNodeContents() {} }),
  },
  // A browser always has both of these. Modelled here rather than left as
  // origin alone, because the console derives the address it offers from them —
  // and a deployment served under a path (ingress.path, the default) would be
  // offered the wrong one if only the origin were read.
  window: {
    // protocol is included because the console reads it: an app's address is
    // offered over the scheme the page was loaded with. A stub without it would
    // make the console look like it mangles every URL, which is the harness
    // missing a field rather than the console having a bug.
    location: { protocol: "https:", origin: "https://applab.example.com", pathname: "/applab/" },
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
  // It has to carry the path the page was served from, not just its origin: a
  // deployment under a path (ingress.path, which the chart defaults to /applab)
  // is reached at https://host/applab, and every API call built from a bare
  // origin would miss the server. The stub's pathname is "/applab/", so the
  // trailing slash being trimmed is part of what this checks.
  //
  // Read from baseURL() itself, which is what every call is built from. It used
  // to be asserted through the sign-in card, which printed it as "Sent to
  // <address>" — that line was removed, so the check moved to the value rather
  // than being dropped with the display it happened to be read through.
  check(
    "the console reports the address it was served from, trailing slash trimmed",
    vm.runInContext("baseURL()", context),
    "https://applab.example.com/applab"
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

  // Signed out, the console shows the sign-in card and nothing else: no view is
  // left on screen behind it, and the header keeps only what is useful before a
  // key exists. This is the state a first visit lands in, and the one the page
  // has to get right without any JavaScript having run a view.
  {
    check("signed out, the sign-in card is up", elements.get("signin").classList.contains("hidden"), false);
    for (const view of ["overview", "apps", "app"]) {
      check(
        `signed out, the ${view} view is not on screen`,
        elements.get(view + "-view").classList.contains("hidden"),
        true
      );
    }
    check("and neither is the nav", elements.get("nav").classList.contains("hidden"), true);
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
    for (const [card, button] of [
      ["Resources", "app-resources-save"],
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
  // Every app has a git repository from the moment it is created, so this is the
  // address a caller clones from — and it is derived from where the page was
  // served, which is the part that goes wrong. The stub's pathname is "/applab/",
  // so a version that used the bare origin would produce a URL that 404s, and
  // that is the mistake this checks for rather than the shape of the string.
  {
    const gitURL = vm.runInContext(
      'baseURL() + "/git/" + "shop" + ".git"',
      context
    );
    check(
      "the clone URL carries the path the deployment is served under",
      gitURL,
      "https://applab.example.com/applab/git/shop.git"
    );
    check(
      "and is not built from the bare origin",
      gitURL.startsWith("https://applab.example.com/git/"),
      false
    );
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

  // The resource units, both directions.
  //
  // The form and the readings carry fixed units — CPU in cores, memory in GiB —
  // while the API takes Kubernetes quantities. So every value crosses the
  // boundary twice: once to be displayed and once to be sent back. A mistake in
  // either direction is silent and expensive: memory written as a bare number
  // means *bytes*, so a slip here would set a 0.5 GiB limit as "0.5" and have
  // the container OOM-killed on start with nothing on the page to explain it.
  //
  // Round-tripping is the assertion rather than the individual conversions: what
  // matters is that opening an app and saving it unchanged sends back the same
  // quantity.
  {
    const ctx = vm.createContext({ ...sandbox, globalThis: undefined });
    ctx.globalThis = ctx;
    vm.runInContext(source, ctx, { filename: "console.js" });

    const conv = (name, arg) => vm.runInContext(name, ctx)(arg);

    for (const c of [
      { q: "500m", cores: "0.5", sent: "500m" },
      { q: "1500m", cores: "1.5", sent: "1500m" },
      { q: "2", cores: "2", sent: "2000m" },
      { q: "100m", cores: "0.1", sent: "100m" },
    ]) {
      check(`cpu ${c.q} reads as ${c.cores} cores`, conv("toCores", c.q), c.cores);
      check(`and ${c.cores} cores is sent back as ${c.sent}`, conv("fromCores", c.cores), c.sent);
    }

    for (const m of [
      { q: "134217728", gi: "0.125", sent: "134217728" },
      { q: "512Mi", gi: "0.5", sent: "536870912" },
      { q: "1Gi", gi: "1", sent: "1073741824" },
      { q: "2Gi", gi: "2", sent: "2147483648" },
    ]) {
      check(`memory ${m.q} reads as ${m.gi} GiB`, conv("toGi", m.q), m.gi);
      check(`and ${m.gi} GiB is sent back as ${m.sent} bytes`, conv("fromGi", m.gi), m.sent);
    }

    // Empty stays empty, which is what clears a field back to the deployment's
    // default. A conversion that turned it into "0m" or "0" would silently
    // replace the operator's setting with an explicit nothing.
    check("an unset cpu bound stays unset", conv("fromCores", ""), "");
    check("an unset memory bound stays unset", conv("fromGi", ""), "");
    check("and an absent quantity reads as nothing rather than zero", conv("toGi", ""), "");

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
              { name: "applab-shop-abc", ready: false, restarts: 5, reason: "CrashLoopBackOff" },
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
    // Found by class rather than by position: the row grew two usage cells, and
    // an index that has to be re-counted every time a column is added is a check
    // that quietly starts reading the wrong cell instead of failing.
    const actions = row && row.children.find((c) => c.className === "actions");
    const buttons = (actions && actions.children) || [];
    const labels = buttons.map((b) => b.textContent).join(" | ");
    check("an instance row offers its own log", labels.includes("Log"), true);
    check("and its own events", labels.includes("Events"), true);

    // What the pod is using, on its row — the reason the usage read is here at
    // all. Rendered in cores and GiB rather than as the raw quantity, so it can
    // be compared against the limit beside it by eye.
    const cells = row.children.map((c) => c.textContent);
    check("and its cpu, in cores", cells.includes("0.15 cores"), true);
    check("and its memory, in GiB", cells.includes("0.09375 GiB"), true);

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
    const opened = vm.runInContext("openPlatformPodLog", ctx);
    opened("applab-6b9f7-abc");
    await new Promise((r) => setTimeout(r, 0));

    const asked = ctx.requests.find((u) => u.includes("/api/v1/platform/logs")) || "";
    check("and a row's log button is what reads one", asked.length > 0, true);
    check("naming the pod the row was about", asked.includes("pod=applab-6b9f7-abc"), true);
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
      ctx.requests.push(String(url));
      return {
        ok: true,
        status: 200,
        statusText: "OK",
        headers: { get: () => "application/json" },
        // A stream of one chunk, so startLogs finishes rather than waiting.
        body: { getReader: () => ({ read: async () => ({ done: true, value: undefined }) }) },
        text: async () => "",
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
        { name: "applab-6b9f7-abc", ready: true, restarts: 0 },
        { name: "applab-6b9f7-def", ready: false, restarts: 3, reason: "CrashLoopBackOff" },
      ],
      usage: {
        available: true,
        pods: {
          "applab-6b9f7-abc": { cpu: "12m", memory: "80Mi" },
          "applab-6b9f7-def": { cpu: "4m", memory: "20Mi" },
        },
      },
      failure: "",
      onLog: vm.runInContext("openPlatformPodLog", ctx),
    });

    check("one row per pod", body.children.length, 2);
    check("and an empty cluster is not reported", elements.get("platform-instances-empty").classList.contains("hidden"), true);

    const first = body.children[0];
    check("the pod is named", first.children[0].textContent, "applab-6b9f7-abc");
    check("with its readiness", first.children[1].allText(), "ready");
    // Its own reading, on its own row — the case a single total cannot show.
    check("and its own cpu", first.children[3].textContent, "0.012 cores");
    check("and its own memory", first.children[4].textContent, "0.078125 GiB");

    // A not-ready pod's reason is the column that explains it.
    const second = body.children[1];
    check("a failing pod shows why", second.children[5].allText(), "CrashLoopBackOff");

    // And its Log button names *that* pod. Without it the endpoint answers with
    // the newest replica's output, which is not the row that was clicked.
    const actions = second.children[6];
    check("every row offers its own log", actions.children[0].textContent, "Log");
    check(
      "and only a log — there is no platform events endpoint to offer",
      actions.children.length,
      1
    );

    ctx.requests.length = 0;
    actions.children[0].onclick();
    await new Promise((r) => setTimeout(r, 0));
    const asked = ctx.requests.find((u) => u.includes("/platform/logs")) || "";
    check("and the log it reads names that pod", asked.includes("pod=applab-6b9f7-def"), true);
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
      pods: [{ name: "applab-6b9f7-abc", ready: true, restarts: 0 }],
      usage: { available: false },
      failure: "",
      onLog: () => {},
    });

    const note = elements.get("platform-usage-note");
    check("with no metrics the table says so", note.classList.contains("hidden"), false);

    // The rows are still there. Usage is one column of this table, and losing
    // every pod because a column is missing would be the wrong trade — the pods
    // and their restarts are as useful without a reading.
    const row = elements.get("platform-instances").children[0];
    check("and the pod is still listed", row.children[0].textContent, "applab-6b9f7-abc");
    check("with its reading unknown rather than zero", row.children[3].textContent, "–");
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
            events: [
              { object: "applab-build-shop-abc", type: "Warning", reason: "FailedScheduling", message: "no nodes", count: 3 },
              { object: "someone-elses-pod", type: "Normal", reason: "Pulled", message: "a different pod entirely" },
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
      pods: [{ name: "applab-6b9f7-abc", ready: true, restarts: 0 }],
      usage,
      failure,
      onLog: () => {},
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
    // is not an error: the readings are unknown, which the empty strings the
    // server sends render as em dashes.
    show({ available: true, cpu: "", memory: "", pods: null }, "");
    check("an unsampled cluster shows unknown rather than zero", body.children[0].children[3].textContent, "–");
    check("and does not claim metrics are missing", note.classList.contains("hidden"), true);
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
