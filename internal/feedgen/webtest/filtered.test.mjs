// Tests of the page at /filtered: the real filtered.html and filtered.js, run in jsdom, with fetch and
// navigation stubbed.

import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.join(here, "..", "web");
const { JSDOM } = createRequire(import.meta.url)(process.env.TOPICFEED_JSDOM);
const scriptURL = pathToFileURL(path.join(webDir, "static", "filtered.js")).href;
const pageHTML = fs
  .readFileSync(path.join(webDir, "filtered.html"), "utf8")
  .replace(/<header class="topbar">[\s\S]*?<\/header>/, "")
  .replace(/<script[^>]*><\/script>/, "");

const TAX = {
  topics: [
    { id: "us_politics", name: "US politics", subtopics: [{ id: "us_politics/elections", name: "Elections" }] },
    { id: "technology", name: "Technology", subtopics: [{ id: "technology/ai", name: "AI" }] },
    { id: "adult_content", name: "Adult content", adult: true, subtopics: [
      { id: "adult_content/nsfw_art", name: "NSFW art" }, { id: "adult_content/explicit_posts", name: "Explicit posts & promo" }] },
  ],
  tones: ["informative", "outraged"],
  signals: ["critical", "spam"],
};

const feed = (over = {}) => ({
  rkey: "discover-filter", name: "Discover, Filtered", description: "", url: "https://bsky.app/profile/did:plc:owner/feed/discover-filter",
  source: "at://did:plc:x/app.bsky.feed.generator/whats-hot", sourceName: "Discover", sourceUrl: "https://bsky.app/profile/did:plc:x/feed/whats-hot",
  defaults: { exclude: { us_politics: 0.5 }, tone: {}, signals: {} }, filters: null, ...over,
});

function define(name, value) {
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
}

let loads = 0;
async function open({ list, save = (call) => ({ status: 200, body: { filters: call.json } }), connect, disconnect = { status: 200, body: {} } }) {
  const win = new JSDOM(pageHTML, { url: "https://feeds.example.test/filtered" }).window;
  const calls = [];
  const assigned = [];
  const reloads = [];
  define("document", win.document);
  define("history", win.history);
  define("location", new Proxy({}, {
    get(_, key) {
      if (key === "assign") return (u) => assigned.push(u);
      if (key === "reload") return () => reloads.push(1);
      const v = win.location[key];
      return typeof v === "function" ? v.bind(win.location) : v;
    },
  }));
  define("addEventListener", win.addEventListener.bind(win));
  define("fetch", async (url, opts = {}) => {
    const call = { url, method: opts.method ?? "GET", body: opts.body };
    if (typeof opts.body === "string" && opts.body.length) call.json = JSON.parse(opts.body);
    calls.push(call);
    const answer = {
      "GET /api/me/filtered": list,
      "GET /api/me": { status: 200, body: { did: "did:plc:alice", handle: "alice.test" } },
      "GET /api/taxonomy": { status: 200, body: TAX },
      "PUT /api/me/filtered/discover-filter": save,
      "POST /oauth/connect": connect,
      "POST /oauth/disconnect": disconnect,
    }[`${call.method} ${url.split("?")[0]}`];
    const r = typeof answer === "function" ? answer(call) : answer;
    if (!r) throw new Error(`unexpected request: ${call.method} ${url}`);
    return { ok: r.status >= 200 && r.status < 300, status: r.status, json: async () => r.body };
  });
  const mod = await import(`${scriptURL}?load=${++loads}`);
  const $ = (id) => win.document.getElementById(id);
  const until = async (cond, what) => {
    for (let i = 0; i < 200; i++) {
      if (cond()) return;
      await new Promise((r) => setTimeout(r, 5));
    }
    assert.fail(`timed out waiting for ${what}`);
  };
  return { win, $, calls, assigned, reloads, until, mod };
}

const connected = (feeds = [feed()]) => ({ status: 200, body: { did: "did:plc:alice", connected: true, feeds } });

test("a viewer who isn't signed in for the filtered feeds gets the form, which starts their sign-in", async () => {
  const p = await open({ list: { status: 200, body: { did: "did:plc:alice", connected: false, feeds: [] } },
    connect: { status: 200, body: { redirect: "https://bsky.social/oauth/authorize?x=1" } } });
  await p.until(() => !p.$("connect").hidden, "the form");
  assert.equal(p.$("connected").hidden, true);
  assert.equal(p.$("connect-again").hidden, false, "it says they're signed in on the site but not for these feeds");
  await p.until(() => p.$("handle").value === "alice.test", "the handle filled in");
  p.$("login").dispatchEvent(new p.win.Event("submit", { cancelable: true }));
  await p.until(() => p.assigned.length === 1, "the redirect");
  const login = p.calls.find((c) => c.url === "/oauth/connect");
  assert.ok(login, "the form asks for the filtered feeds' sign-in, not the plain one");
  assert.equal(p.assigned[0], "https://bsky.social/oauth/authorize?x=1");
});

test("nobody signed in at all also gets the form", async () => {
  const p = await open({ list: { status: 401, body: { error: "not signed in" } } });
  await p.until(() => !p.$("connect").hidden, "the form");
  assert.equal(p.$("connect-again").hidden, true);
});

test("a signed-in viewer sees each feed with the feed's own filters, and saves their own", async () => {
  const p = await open({ list: connected() });
  await p.until(() => !p.$("connected").hidden, "the feeds");
  const card = p.win.document.querySelector('[data-feed="discover-filter"]');
  assert.ok(card);
  assert.ok(card.querySelector('[data-topic="us_politics"]'), "the feed's own exclusion is shown");
  const save = [...card.querySelectorAll("button")].find((b) => b.textContent === "Save");
  assert.equal(save.disabled, true, "nothing to save yet");

  // Leave out AI too, and critical posts.
  const addTopic = card.querySelector("#f-discover-filter-add-exclude");
  addTopic.value = "technology/ai";
  addTopic.dispatchEvent(new p.win.Event("change"));
  const addScore = card.querySelector("#f-discover-filter-add-score");
  addScore.value = "signals:critical";
  addScore.dispatchEvent(new p.win.Event("change"));
  const unscored = card.querySelector("#f-discover-filter-unscored");
  unscored.checked = true;
  unscored.dispatchEvent(new p.win.Event("change"));
  assert.equal(save.disabled, false);
  save.click();
  await p.until(() => p.calls.some((c) => c.method === "PUT"), "the save");
  const put = p.calls.find((c) => c.method === "PUT");
  assert.deepEqual(put.json, {
    exclude: { us_politics: 0.5, "technology/ai": 0.5 },
    tone: {},
    signals: { max: { critical: 0.7 } },
    drop_unscored: true,
  });
});

test("adult topics are in the topic list like any other", async () => {
  const p = await open({ list: connected([feed({ defaults: { tone: {}, signals: {} } })]) });
  await p.until(() => !p.$("connected").hidden, "the feeds");
  assert.ok(p.calls.some((c) => c.url === "/api/taxonomy?for=filters"), "asks for every topic a filter can leave out");
  const card = p.win.document.querySelector('[data-feed="discover-filter"]');
  const add = card.querySelector("#f-discover-filter-add-exclude");
  const values = [...add.querySelectorAll("option")].map((o) => o.value);
  assert.ok(values.includes("adult_content") && values.includes("adult_content/nsfw_art") && values.includes("adult_content/explicit_posts"));
  add.value = "adult_content/explicit_posts";
  add.dispatchEvent(new p.win.Event("change"));
  assert.ok(card.querySelector('.filtered-excludes [data-topic="adult_content/explicit_posts"]'));
  [...card.querySelectorAll("button")].find((b) => b.textContent === "Save").click();
  await p.until(() => p.calls.some((c) => c.method === "PUT"), "the save");
  assert.deepEqual(p.calls.find((c) => c.method === "PUT").json, { exclude: { "adult_content/explicit_posts": 0.5 }, tone: {}, signals: {} });
});

test("each feed links to the page of what it left out", async () => {
  const p = await open({ list: connected() });
  await p.until(() => !p.$("connected").hidden, "the feeds");
  const link = p.win.document.querySelector('[data-left-out-link="discover-filter"]');
  assert.equal(link.getAttribute("href"), "/filtered/left-out?feed=discover-filter");
  assert.equal(p.calls.some((c) => c.url.includes("/left-out")), false, "the list isn't on this page");
});

test("going back to the feed's own filters sends null", async () => {
  const p = await open({ list: connected([feed({ filters: { exclude: { technology: 0.3 }, tone: {}, signals: {} } })]) });
  await p.until(() => !p.$("connected").hidden, "the feeds");
  const card = p.win.document.querySelector('[data-feed="discover-filter"]');
  assert.ok(card.querySelector('[data-topic="technology"]'), "their own filters are shown, not the feed's");
  const back = [...card.querySelectorAll("button")].find((b) => b.textContent === "Use the feed's own filters");
  assert.equal(back.hidden, false);
  back.click();
  await p.until(() => p.calls.some((c) => c.method === "PUT"), "the save");
  assert.equal(p.calls.find((c) => c.method === "PUT").body, "null");
  await p.until(() => card.querySelector('[data-topic="us_politics"]'), "the feed's own filters shown again");
});

test("a feed with no filters of its own starts with nothing left out, and clearing goes back to that", async () => {
  const p = await open({ list: connected([feed({ defaults: { tone: {}, signals: {} }, filters: { exclude: { technology: 0.3 }, tone: {}, signals: {} } })]) });
  await p.until(() => !p.$("connected").hidden, "the feeds");
  const card = p.win.document.querySelector('[data-feed="discover-filter"]');
  const clear = [...card.querySelectorAll("button")].find((b) => b.textContent === "Clear all filters");
  assert.ok(clear, "the reset says what it does");
  clear.click();
  await p.until(() => p.calls.some((c) => c.method === "PUT"), "the save");
  assert.equal(p.calls.find((c) => c.method === "PUT").body, "null");
  await p.until(() => !card.querySelector('[data-topic="technology"]'), "the filter gone");
  assert.equal(card.querySelector(".filtered-status").textContent, "Cleared: nothing is filtered out.");
});

test("per-topic rules have no boosts and no how-sure setting", async () => {
  const p = await open({ list: connected([feed({ filters: { signals: {}, tone: {}, topic_rules: { technology: { signals: { max: { critical: 1 } } } } } })]) });
  await p.until(() => !p.$("connected").hidden, "the feeds");
  const rules = p.win.document.querySelector(".filtered-rules");
  assert.ok(rules.querySelector('[data-topic="technology"][data-score="critical"]'));
  assert.equal(rules.querySelector('input[id$="-boost"]'), null, "a boost slider");
  assert.equal([...rules.querySelectorAll("option")].some((o) => o.value === "min_prob"), false, "a how-sure option");
});

test("signing out of the filtered feeds", async () => {
  const p = await open({ list: connected() });
  await p.until(() => !p.$("connected").hidden, "the feeds");
  p.$("disconnect").click();
  await p.until(() => p.reloads.length === 1, "the reload");
  assert.ok(p.calls.some((c) => c.method === "POST" && c.url === "/oauth/disconnect"));
});

test("draftOf and payloadOf are each other's inverse on what the server keeps", async () => {
  const p = await open({ list: { status: 401, body: {} } });
  const f = { exclude: { us_politics: 0.4 }, tone: { max: { outraged: 0.5 } }, signals: { min: { critical: 0.1 }, max: { spam: 0.2 } },
    topic_rules: { technology: { signals: { max: { critical: 1 } } } }, drop_unscored: true };
  assert.deepEqual(p.mod.payloadOf(p.mod.draftOf(f)), f);
});
