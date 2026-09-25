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
    location: { origin: "https://applab.example.com", pathname: "/applab/" },
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

  // Not deployed: the app has no `url` from the API yet, but its address is
  // still known and is still what the row should offer. A plain-text address
  // that becomes a link on first deploy teaches nothing, and "where will this
  // be" is exactly the question someone has before deploying.
  {
    const body = await render([
      { id: "shop", status: "created", hostname: "www.example.com", path: "/apps/shop" },
    ]);
    const text = body.allText();
    check("an undeployed app shows its address", text.includes("www.example.com/apps/shop"), true);
    check("marked as not deployed", text.includes("not deployed"), true);

    const link = body.children[0].children[3].children.find((c) => c.tagName === "A");
    check("and the address is a link even before it is serving", link !== undefined, true);
    check(
      "pointing at where the app will be",
      link && link.href,
      "https://www.example.com/apps/shop"
    );
    // The scheme comes from the page: a deployment served over https serves its
    // apps over https, through the same gateway.
    check("over the same scheme as the console", link && link.href.startsWith("https://"), true);
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
      // Reached as t(shown ? "Hide" : "Show") and t(copied ? "Copied" : "Copy"),
      // which the static scan cannot read — the argument is an expression.
      // Exercised by the eye checks above.
      "Show", "Hide", "Copy", "Copied",
      // A build status rendered by buildPill as t(status), where the status is
      // whatever the API returned. Every value of model.BuildStatus has to be
      // here or a build shows its raw API word.
      "cancelled",
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
  // controls belong next to the state they change, so this asserts they are in
  // that card rather than only that they exist somewhere.
  {
    const card = markup.match(/<h2 data-i18n="State"[\s\S]*?<\/div>\s*<\/div>/);
    check("the State card exists", card !== null, true);
    if (card) {
      for (const id of ["app-build", "app-deploy", "app-branch", "app-branch-use"]) {
        check(`the State card carries ${id}`, card[0].includes(`id="${id}"`), true);
      }
      // Replicas is deliberately not here: it belongs with the instances it
      // counts, in the card below, not with the app's other settings.
      check("and does not carry app-replicas", card[0].includes('id="app-replicas"'), false);
    }
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

  if (failures > 0) {
    console.error(`\n${failures} check(s) failed`);
    process.exit(1);
  }
  console.log("\nall console rendering checks passed");
})();
