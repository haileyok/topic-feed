// Tests of the header every page shares (web/static/header.js), run in jsdom against the header of a
// real page, with fetch and navigation stubbed. TestEveryPageHasTheSameHeader checks the pages all
// carry this same header.

import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.join(here, "..", "web");
const { JSDOM } = createRequire(import.meta.url)(process.env.TOPICFEED_JSDOM);
const scriptURL = pathToFileURL(path.join(webDir, "static", "header.js")).href;
const header = fs.readFileSync(path.join(webDir, "me.html"), "utf8").match(/<header class="topbar">[\s\S]*?<\/header>/)[0];

let loads = 0;

// open puts the header on a page and runs header.js with these answers from the server. An answer is
// {status, body} or an Error (the request fails).
async function open({ api, logout = { status: 204 } }) {
  const win = new JSDOM(`<!doctype html><body>${header}</body>`, { url: "https://feeds.example.test/feeds" }).window;
  const calls = [];
  const reloads = [];
  const fetchStub = async (url, opts = {}) => {
    calls.push({ url, method: opts.method ?? "GET", headers: opts.headers ?? {}, credentials: opts.credentials });
    const r = { "GET /api/me": api, "POST /oauth/logout": logout }[`${opts.method ?? "GET"} ${url}`];
    if (r instanceof Error) throw r;
    if (!r) throw new Error(`unexpected request: ${url}`);
    return {
      ok: r.status >= 200 && r.status < 300,
      status: r.status,
      json: async () => {
        if (r.body === undefined) throw new SyntaxError("not JSON");
        return r.body;
      },
    };
  };
  const location = new Proxy({}, {
    get(_, key) {
      if (key === "reload") return () => reloads.push(1);
      const v = win.location[key];
      return typeof v === "function" ? v.bind(win.location) : v;
    },
  });
  for (const [name, value] of Object.entries({ document: win.document, fetch: fetchStub, location })) {
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  const mod = await import(`${scriptURL}?load=${++loads}`);
  const $ = (id) => win.document.getElementById(id);
  const until = async (cond, what) => {
    for (let i = 0; i < 400; i++) {
      if (cond()) return;
      await new Promise((r) => setTimeout(r, 5));
    }
    throw new Error(`timed out waiting for ${what}`);
  };
  await until(() => $("account").dataset.state !== "checking", "the header to ask who is signed in");
  const shown = () => ["account-signin", "account-who", "account-signout", "nav-inspect"].filter((id) => !$(id).hidden);
  return { win, mod, $, calls, reloads, until, shown };
}

const alice = { status: 200, body: { did: "did:plc:alice", handle: "alice.test" } };

test("someone signed out is offered Sign in, which goes to the sign-in page", async () => {
  const p = await open({ api: { status: 401, body: { error: "not signed in" } } });
  assert.equal(p.$("account").dataset.state, "signed-out");
  assert.deepEqual(p.shown(), ["account-signin"]);
  assert.equal(p.$("account-signin").getAttribute("href"), "/me");
  const call = p.calls[0];
  assert.deepEqual([call.url, call.method, call.headers.Accept, call.credentials], ["/api/me", "GET", "application/json", "same-origin"]);
});

test("someone signed in sees their handle and Sign out, and no inspector unless they are the owner", async () => {
  const p = await open({ api: alice });
  assert.equal(p.$("account").dataset.state, "signed-in");
  assert.deepEqual(p.shown(), ["account-who", "account-signout"]);
  assert.equal(p.$("account-who").textContent, "@alice.test");
  assert.equal(p.$("account-who").title, "did:plc:alice");
});

test("the owner also sees the post inspector's link", async () => {
  const p = await open({ api: { status: 200, body: { ...alice.body, owner: true } } });
  assert.deepEqual(p.shown(), ["account-who", "account-signout", "nav-inspect"]);
  // Only a real true counts.
  const q = await open({ api: { status: 200, body: { ...alice.body, owner: "yes" } } });
  assert.deepEqual(q.shown(), ["account-who", "account-signout"]);
});

test("an account with no handle is shown by its DID, as text", async () => {
  const p = await open({ api: { status: 200, body: { did: "did:plc:alice", handle: "" } } });
  assert.equal(p.$("account-who").textContent, "did:plc:alice");
  const q = await open({ api: { status: 200, body: { did: "did:plc:alice", handle: "<img src=x onerror=alert(1)>" } } });
  assert.equal(q.$("account-who").textContent, "@<img src=x onerror=alert(1)>");
  assert.equal(q.$("account-who").children.length, 0, "shown as text");
});

test("when sign-in is off or the server can't be asked, the header shows no account at all", async () => {
  for (const [name, api] of [
    ["sign-in off", { status: 404 }],
    ["a server error", { status: 500, body: {} }],
    ["no connection", new TypeError("Failed to fetch")],
    ["an answer that isn't JSON", { status: 200 }],
    ["an answer without a DID", { status: 200, body: { handle: "alice.test" } }],
  ]) {
    const p = await open({ api });
    assert.equal(p.$("account").dataset.state, "unknown", name);
    assert.deepEqual(p.shown(), [], name);
  }
});

test("signing out posts the request, then reloads the page as it is for someone signed out", async () => {
  const p = await open({ api: alice });
  p.$("account-signout").click();
  await p.until(() => p.reloads.length > 0, "the reload");
  const call = p.calls.find((c) => c.url === "/oauth/logout");
  assert.deepEqual([call.method, call.headers.Accept, call.credentials], ["POST", "application/json", "same-origin"]);
  assert.equal(p.$("account-problem").hidden, true);
});

test("if signing out fails, you are told, still signed in, and can try again", async () => {
  for (const [name, logout] of [
    ["the request failing", new TypeError("offline")],
    ["a refusal", { status: 403 }],
    ["a server error", { status: 500 }],
  ]) {
    const p = await open({ api: alice, logout });
    p.$("account-signout").click();
    await p.until(() => !p.$("account-problem").hidden, name);
    assert.match(p.$("account-problem").textContent, /Couldn't sign out/, name);
    assert.equal(p.reloads.length, 0, `${name}: must not reload as if signed out`);
    assert.equal(p.$("account-signout").disabled, false, `${name}: can try again`);
    assert.deepEqual(p.shown(), ["account-who", "account-signout"], name);
  }
});
