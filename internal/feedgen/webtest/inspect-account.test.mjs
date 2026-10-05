// Tests of the account inspector at /inspect/account: the real page and script, run in jsdom, with
// fetch stubbed.

import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.join(here, "..", "web");
const { JSDOM } = createRequire(import.meta.url)(process.env.TOPICFEED_JSDOM);
const scriptURL = pathToFileURL(path.join(webDir, "static", "inspect-account.js")).href;
const pageHTML = fs
  .readFileSync(path.join(webDir, "inspect-account.html"), "utf8")
  .replace(/<header class="topbar">[\s\S]*?<\/header>/, "")
  .replace(/<script[^>]*><\/script>/, "");

function define(name, value) {
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
}

const ALICE = {
  did: "did:plc:alice", handle: "alice.test", state: "personal", personalized: true,
  settings: { lookbackDays: 30, halfLifeDays: 7, minLikes: 5, topics: 20 },
  coverage: { total: 120, classified: 90 },
  interests: [
    { path: "technology/ai", name: "AI", broad: "Technology", share: 0.6, weight: 1, tunedShare: 0.6, posts: 30,
      samples: [{ url: "https://bsky.app/profile/x/post/1", text: "a post about models", likedAt: "2026-10-01T00:00:00Z", p: 0.9 }] },
    { path: "sports/soccer", name: "Soccer", broad: "Sports", share: 0.4, weight: 0, tunedShare: 0, posts: 12, samples: [] },
  ],
  otherPosts: 5, unclearPosts: 2,
};

let loads = 0;
async function open({ search = "", me = { status: 200, body: { did: "did:plc:owner", handle: "hailey.at", owner: true } },
  account = () => ({ status: 200, body: ALICE }) } = {}) {
  const win = new JSDOM(pageHTML, { url: "https://feeds.example.test/inspect/account" + search }).window;
  const calls = [];
  define("document", win.document);
  define("history", { pushState: (_, __, url) => calls.push({ pushed: url }), replaceState: () => {} });
  define("location", win.location);
  define("addEventListener", win.addEventListener.bind(win));
  define("fetch", async (url) => {
    calls.push({ url });
    const u = new URL(url, "https://feeds.example.test");
    const r = u.pathname === "/api/me" ? me : u.pathname === "/api/inspect/account" ? account(u.searchParams.get("account")) : null;
    if (!r) throw new Error(`unexpected request: ${url}`);
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
  return { win, $, calls, until, mod };
}

test("the owner looks an account up and sees what it's into, strongest first", async () => {
  const p = await open();
  await p.until(() => !p.$("inspector").hidden, "the inspector");
  p.$("account-input").value = "@alice.test";
  p.$("account-form").dispatchEvent(new p.win.Event("submit", { cancelable: true }));
  await p.until(() => !p.$("account-result").hidden, "the result");
  assert.ok(p.calls.some((c) => c.url === "/api/inspect/account?account=%40alice.test"));
  assert.ok(p.calls.some((c) => c.pushed === "/inspect/account?account=%40alice.test"), "the address names the account");
  assert.equal(p.$("account-who-title").textContent, "@alice.test");
  const rows = [...p.$("acct-interests").children];
  assert.deepEqual(rows.map((r) => r.dataset.path), ["technology/ai", "sports/soccer"]);
  assert.match(rows[0].textContent, /AI.*Technology.*60%/);
  assert.match(rows[0].textContent, /a post about models/);
  assert.match(rows[1].textContent, /they muted it/);
  assert.match(p.$("account-other").textContent, /5 more liked posts/);
});

test("an account in the address is looked up straight away", async () => {
  const p = await open({ search: "?account=did%3Aplc%3Aalice" });
  await p.until(() => !p.$("account-result").hidden, "the result");
  assert.equal(p.$("account-input").value, "did:plc:alice");
});

test("a handle with no account says so", async () => {
  const p = await open({ search: "?account=nobody.test", account: () => ({ status: 404, body: { error: "no_account", message: "No account has that handle." } }) });
  await p.until(() => !p.$("account-error").hidden, "the error");
  assert.equal(p.$("account-error").textContent, "No account has that handle.");
});

test("anyone but the owner is told the page isn't theirs", async () => {
  const p = await open({ me: { status: 200, body: { did: "did:plc:bob", handle: "bob.test" } } });
  await p.until(() => !p.$("forbidden").hidden, "the forbidden notice");
  assert.equal(p.calls.some((c) => c.url && c.url.startsWith("/api/inspect/account")), false);
});

test("signed out, the sign-in form", async () => {
  const p = await open({ me: { status: 401, body: {} } });
  await p.until(() => !p.$("signed-out").hidden, "the form");
});
