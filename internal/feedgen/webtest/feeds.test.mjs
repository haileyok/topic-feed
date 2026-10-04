// Browser-level tests of the page at /feeds: the real feeds.html and feeds.js, run in jsdom, with fetch, the
// OAuth client and the account's server stood in for.
//
//   TOPICFEED_JSDOM=/path/to/node_modules/jsdom node --test internal/feedgen/webtest/feeds.test.mjs
//
// The Go test TestFeedsPageScript runs these when TOPICFEED_JSDOM is set.

import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import { FakeServer, fakeClient, DID, SERVICE, SCOPE, COLLECTION } from "./fakepds.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.join(here, "..", "web");
const { JSDOM } = createRequire(import.meta.url)(process.env.TOPICFEED_JSDOM);
const pageHTML = fs.readFileSync(path.join(webDir, "feeds.html"), "utf8")
  .replace(/<header class="topbar">[\s\S]*?<\/header>/, "") // the shared header has tests of its own (header.test.mjs)
  .replace(/<script[^>]*><\/script>/, "");
const feedsJS = await import(pathToFileURL(path.join(webDir, "static", "feeds.js")).href);
const { main, topicsText, feedUrl, STATUS_TEXT } = feedsJS;

const ORIGIN = "https://feeds.example.test";
const me = { did: DID, handle: "bob.test" };
const specOf = (name, over = {}) => ({ display_name: name, description: `${name} feed.`, paths: ["animals_nature/cats"], min_prob: 0.6, accepts_interactions: true, ...over });
const feedOf = (rkey, name = rkey, over = {}) => ({ rkey, uri: `at://${DID}/${COLLECTION}/${rkey}`, spec: specOf(name, over), createdAt: "2026-10-04T00:00:00Z", updatedAt: "2026-10-04T00:00:00Z" });
const record = (name, over = {}) => ({ $type: COLLECTION, did: SERVICE, displayName: name, description: `${name} feed.`, acceptsInteractions: true, createdAt: "2026-10-01T00:00:00.000Z", ...over });
const mineOf = (feeds, over = {}) => ({ did: DID, owner: false, serviceDid: SERVICE, scope: SCOPE, feeds, limits: { maxFeeds: 5, maxPaths: 40, maxExclude: 60, maxPosts: 3000, maxName: 24, maxDescription: 300 }, ...over });

// open runs the page. ClientClass is a fake OAuth client (null: none); answers are {status, body}.
async function open({ meAnswer = { status: 200, body: me }, mineAnswer, feeds = [feedOf("cats", "Cats"), feedOf("dogs", "Dogs")], client = null, confirms = [], remove = { status: 200, body: { deleted: "x" } } } = {}) {
  const win = new JSDOM(pageHTML, { url: `${ORIGIN}/feeds` }).window;
  const calls = [];
  const asked = [];
  const answers = { "GET /api/me": meAnswer, "GET /api/me/feeds": mineAnswer ?? { status: 200, body: mineOf(feeds) } };
  const fetchStub = async (url, opts = {}) => {
    const call = { url, method: opts.method ?? "GET", headers: opts.headers ?? {}, credentials: opts.credentials };
    calls.push(call);
    let r = answers[`${call.method} ${url.split("?")[0]}`];
    if (!r && call.method === "DELETE" && url.startsWith("/api/me/feeds/")) r = typeof remove === "function" ? remove(url) : remove;
    if (r instanceof Error) throw r;
    if (!r) throw new Error(`unexpected request: ${call.method} ${url}`);
    return { ok: r.status >= 200 && r.status < 300, status: r.status, json: async () => { if (r.body === undefined) throw new SyntaxError("x"); return r.body; } };
  };
  Object.defineProperty(globalThis, "document", { value: win.document, configurable: true, writable: true });
  Object.defineProperty(globalThis, "location", { value: win.location, configurable: true, writable: true });
  Object.defineProperty(globalThis, "fetch", { value: fetchStub, configurable: true, writable: true });
  const confirm = (m) => { asked.push(m); return confirms.length ? confirms.shift() : true; };
  await main({ ClientClass: client ? client.ClientClass : null, confirm, origin: ORIGIN });
  const $ = (id) => win.document.getElementById(id);
  const cards = () => [...win.document.querySelectorAll(".feeds-card")];
  const card = (rkey) => cards().find((c) => c.dataset.rkey === rkey);
  const buttons = (c) => [...c.querySelectorAll("button, a.btn")].map((b) => b.textContent);
  const click = async (rkey, text) => {
    const b = [...card(rkey).querySelectorAll("button")].find((x) => x.textContent === text);
    assert.ok(b, `no "${text}" button on ${rkey}: ${buttons(card(rkey))}`);
    b.click();
    await new Promise((r) => setTimeout(r, 15));
  };
  const visible = () => ["loading", "signed-out", "feeds-main"].filter((id) => !$(id).hidden);
  return { win, $, calls, asked, cards, card, buttons, click, visible, notice: () => ($("notice").hidden ? null : $("notice").textContent), tone: () => $("notice").dataset.tone };
}

const connectedClient = (server, over) => fakeClient(server, over);

// ---------- who sees what ----------

test("signed out: the sign-in prompt, and nothing is asked about feeds", async () => {
  const p = await open({ meAnswer: { status: 401, body: { error: "not signed in" } } });
  assert.deepEqual(p.visible(), ["signed-out"]);
  assert.deepEqual(p.calls.map((c) => c.url), ["/api/me"]);
  // The same sign-in form as every page that needs someone signed in (signin-form.js), right here.
  assert.ok(p.win.document.querySelector("#signed-out form#login input#handle"));
  assert.equal(p.$("login-button").textContent, "Sign in with Bluesky");
  assert.equal(p.$("signin-notice").hidden, true);
});

test("feeds that can't be read are said so, and nothing is drawn", async () => {
  const p = await open({ mineAnswer: { status: 503 } });
  assert.deepEqual(p.visible(), ["feeds-main"]);
  assert.match(p.notice(), /Couldn't read your feeds just now/);
  assert.equal(p.tone(), "error");
  assert.equal(p.cards().length, 0);
  assert.equal(p.$("who").textContent, "bob.test");
});

test("the feeds are listed with their names, keys, descriptions and topics, and how many there may be", async () => {
  const many = feedOf("mix", "Mix", { paths: ["a/b_c", "d/e", "f/g", "h/i", "j/k", "l/m"] });
  const p = await open({ feeds: [feedOf("cats", "Cats"), many] });
  assert.equal(p.$("who").textContent, "bob.test");
  assert.equal(p.$("count").textContent, "2 of 5 feeds.");
  assert.deepEqual(p.cards().map((c) => c.dataset.rkey), ["cats", "mix"]);
  const cats = p.card("cats");
  assert.equal(cats.querySelector(".feeds-name").textContent, "Cats");
  assert.equal(cats.querySelector(".feeds-key").textContent, "cats");
  assert.equal(cats.querySelector(".feeds-desc").textContent, "Cats feed.");
  assert.equal(cats.querySelector(".feeds-topics").textContent, "Topics: cats");
  assert.equal(p.card("mix").querySelector(".feeds-topics").textContent, "Topics: b c, e, g, i and 2 more");
  assert.equal(cats.querySelector('a[href="/?edit=cats"]').textContent, "Edit");
  assert.equal(p.$("empty").hidden, true);
  assert.equal(p.$("who").textContent, "bob.test");
});

test("with no feeds yet the page says how to make one, and an owner's count has no limit", async () => {
  const p = await open({ feeds: [] });
  assert.equal(p.cards().length, 0);
  assert.equal(p.$("empty").hidden, false);
  assert.equal(p.$("count").textContent, "0 of 5 feeds.");
  const owner = await open({ mineAnswer: { status: 200, body: mineOf([feedOf("a")], { owner: true, limits: { maxFeeds: 0 } }) } });
  assert.equal(owner.$("count").textContent, "1 feed.");
});

test("what other people put in a feed is shown as text, never as HTML", async () => {
  const evil = '<img src=x onerror="window.pwned=1">';
  const p = await open({ meAnswer: { status: 200, body: { did: DID, handle: evil } }, feeds: [feedOf("e", evil, { description: evil, paths: [evil] })] });
  assert.equal(p.win.document.querySelectorAll("main img").length, 0);
  assert.equal(p.win.document.querySelectorAll("main script").length, 0);
  assert.equal(p.win.pwned, undefined);
  assert.ok(p.card("e").textContent.includes(evil));
  assert.ok(p.$("who").textContent.includes(evil));
});

// ---------- without publishing, and not connected ----------

test("without the OAuth client the page still lists and edits feeds, and says it can't publish", async () => {
  const p = await open({ client: null });
  assert.equal(p.$("connection").dataset.state, "unavailable");
  assert.match(p.$("connection-text").textContent, /isn't available in this browser/);
  assert.equal(p.$("connect-form").hidden, true);
  assert.equal(p.cards().length, 2);
  assert.ok(p.buttons(p.card("cats")).includes("Delete"));
});

test("not connected: the form is there with the handle filled in, and nothing says what is published", async () => {
  const server = new FakeServer();
  const c = connectedClient(server, { restored: false });
  const p = await open({ client: c });
  assert.equal(p.$("connection").dataset.state, "disconnected");
  assert.equal(p.$("connect-form").hidden, false);
  assert.equal(p.$("connect-handle").value, "bob.test");
  assert.equal(p.$("disconnect-button").hidden, true);
  assert.deepEqual(c.log.loads, [{ clientId: `${ORIGIN}/oauth/browser-client-metadata.json`, handleResolver: ORIGIN }]);
  for (const k of ["cats", "dogs"]) {
    assert.equal(p.card(k).dataset.status, "unknown");
    assert.equal(p.card(k).querySelector(".feeds-status").textContent, STATUS_TEXT.unknown);
    const publish = [...p.card(k).querySelectorAll("button")].find((b) => b.textContent === "Publish");
    assert.equal(publish.disabled, true);
    assert.match(publish.title, /Connect your Bluesky account first/);
  }
  assert.equal(server.calls.length, 0, "nothing was read from an account that isn't connected");
});

test("connecting sends the handle that was typed to Bluesky, asking for the permission the service names", async () => {
  const c = connectedClient(new FakeServer(), { restored: false });
  const p = await open({ client: c });
  p.$("connect-handle").value = "  @alice.example.com ";
  p.$("connect-form").dispatchEvent(new p.win.Event("submit", { cancelable: true, bubbles: true }));
  await new Promise((r) => setTimeout(r, 15));
  assert.deepEqual(c.log.signIns, [{ handle: "alice.example.com", opts: { scope: SCOPE, state: "feeds" } }]);
  assert.equal(p.$("connect-button").disabled, false);
  assert.equal(p.notice(), null);
});

test("connecting that fails is said in words and can be tried again", async () => {
  const c = connectedClient(new FakeServer(), { restored: false, signInError: new Error("access_denied") });
  const p = await open({ client: c });
  p.$("connect-form").dispatchEvent(new p.win.Event("submit", { cancelable: true, bubbles: true }));
  await new Promise((r) => setTimeout(r, 15));
  assert.match(p.notice(), /You didn't allow it/);
  assert.equal(p.tone(), "error");
  assert.equal(p.$("connect-button").disabled, false);
  p.$("connect-handle").value = "";
  p.$("connect-form").dispatchEvent(new p.win.Event("submit", { cancelable: true, bubbles: true }));
  await new Promise((r) => setTimeout(r, 15));
  assert.match(p.notice(), /Type your Bluesky handle/);
});

test("a client that can't start is said in words, and the rest of the page works", async () => {
  const p = await open({ client: connectedClient(new FakeServer(), { initError: new Error("Unsupported scope") }) });
  assert.equal(p.$("connection").dataset.state, "unavailable");
  assert.match(p.$("connection-text").textContent, /doesn't support permissions that narrow/);
  assert.equal(p.cards().length, 2);
});

test("an account that isn't the signed in one, or that refused the permission, is not connected", async () => {
  const other = await open({ client: connectedClient(new FakeServer(), { sessionDid: "did:plc:alice" }) });
  assert.equal(other.$("connection").dataset.state, "wrong_account");
  assert.match(other.$("connection-text").textContent, /connected as another account \(did:plc:alice\), not bob\.test/);
  assert.equal(other.$("connect-form").hidden, false);
  const declined = await open({ client: connectedClient(new FakeServer(), { scope: "atproto" }) });
  assert.equal(declined.$("connection").dataset.state, "declined");
  assert.match(declined.$("connection-text").textContent, /didn't allow this page/);
});

// ---------- connected ----------

async function connectedPage(setup = () => {}, over = {}) {
  const server = new FakeServer();
  setup(server);
  const client = connectedClient(server, over.client);
  const p = await open({ client, ...over.page });
  return { p, server, client };
}

test("connected: each feed says where it stands against what is published, and offers what can be done", async () => {
  const { p } = await connectedPage((s) => {
    s.put("cats", record("Cats"));                                   // published
    s.put("dogs", record("Dogs", { displayName: "Old dogs" }));      // changed since
    s.put("fish", { ...record("Fish"), did: "did:web:elsewhere" });  // another service's
  }, { page: { feeds: [feedOf("cats", "Cats"), feedOf("dogs", "Dogs"), feedOf("fish", "Fish"), feedOf("birds", "Birds")] } });
  assert.equal(p.$("connection").dataset.state, "connected");
  assert.match(p.$("connection-text").textContent, /Connected as bob\.test/);
  assert.equal(p.$("connect-form").hidden, true);
  assert.equal(p.$("disconnect-button").hidden, false);
  const status = (k) => [p.card(k).dataset.status, p.card(k).querySelector(".feeds-status").textContent];
  assert.deepEqual(status("cats"), ["published", "Published"]);
  assert.deepEqual(status("dogs"), ["changed", STATUS_TEXT.changed]);
  assert.deepEqual(status("fish"), ["conflict", STATUS_TEXT.conflict]);
  assert.deepEqual(status("birds"), ["draft", "Not published"]);
  assert.deepEqual(p.buttons(p.card("birds")), ["Publish", "Edit", "Delete"]);
  assert.deepEqual(p.buttons(p.card("cats")), ["Unpublish", "Edit", "View on Bluesky", "Delete"]);
  assert.deepEqual(p.buttons(p.card("dogs")), ["Update on Bluesky", "Unpublish", "Edit", "View on Bluesky", "Delete"]);
  assert.deepEqual(p.buttons(p.card("fish")), ["Edit", "Delete"], "a key that is not ours can't be published");
  assert.match(p.card("fish").textContent, /made with another service/);
  assert.equal(p.card("cats").querySelector('a[href^="https://bsky.app/"]').getAttribute("href"), `https://bsky.app/profile/${encodeURIComponent(DID)}/feed/cats`);
  assert.equal(p.card("cats").querySelector('a[href^="https://bsky.app/"]').rel, "noopener noreferrer");
  assert.equal([...p.card("birds").querySelectorAll("button")][0].disabled, false);
  assert.equal(p.notice(), null);
});

test("coming back from Bluesky after allowing it says so", async () => {
  const { p } = await connectedPage(() => {}, { client: { state: "feeds" } });
  assert.match(p.notice(), /Connected\. You can publish your feeds now\./);
  assert.equal(p.tone(), "ok");
});

test("publishing writes the feed's record, and the feed then says it is published", async () => {
  const { p, server } = await connectedPage();
  let busy = null;
  server.onCall = (nsid) => { if (nsid.endsWith("putRecord")) busy = [...p.card("cats").querySelectorAll("button")].every((b) => b.disabled); };
  await p.click("cats", "Publish");
  const put = server.calls.find((c) => c.nsid.endsWith("putRecord"));
  assert.deepEqual([put.body.repo, put.body.collection, put.body.rkey], [DID, COLLECTION, "cats"]);
  assert.deepEqual([put.body.record.did, put.body.record.displayName, put.body.record.description], [SERVICE, "Cats", "Cats feed."]);
  assert.equal(busy, true, "its buttons are off while it is being published");
  assert.equal(p.card("cats").dataset.status, "published");
  assert.match(p.notice(), /Published\. Bluesky can take a minute/);
  assert.equal(p.tone(), "ok");
  assert.equal(p.card("dogs").dataset.status, "draft", "the other feed is not touched");
  assert.ok([...p.card("cats").querySelectorAll("button")].every((b) => !b.disabled), "and its buttons are back");
});

test("a change is published in place", async () => {
  const { p, server } = await connectedPage((s) => s.put("cats", record("Cats", { displayName: "Old" })));
  assert.equal(p.card("cats").dataset.status, "changed");
  await p.click("cats", "Update on Bluesky");
  assert.equal(p.card("cats").dataset.status, "published");
  assert.equal(server.records.get("cats").value.displayName, "Cats");
  assert.ok(server.calls.find((c) => c.nsid.endsWith("putRecord")).body.swapRecord, "it replaces the record it looked at");
});

test("publishing that fails says why and leaves the feed as it was", async () => {
  const { p, server } = await connectedPage();
  server.fail = { nsid: "com.atproto.repo.putRecord", status: 403, body: { error: "ScopeMissingError", message: "Missing required scope" } };
  await p.click("cats", "Publish");
  assert.match(p.notice(), /didn't let this page change your feeds/);
  assert.equal(p.tone(), "error");
  assert.equal(p.card("cats").dataset.status, "draft");
  assert.equal(server.records.has("cats"), false);
});

test("unpublishing asks first, and takes the record off Bluesky but keeps the feed", async () => {
  const { p, server } = await connectedPage((s) => s.put("cats", record("Cats")), { page: { confirms: [false, true] } });
  await p.click("cats", "Unpublish");
  assert.match(p.asked[0], /Take “Cats” off Bluesky\?/);
  assert.equal(server.records.has("cats"), true, "declined: nothing happened");
  assert.equal(p.card("cats").dataset.status, "published");
  await p.click("cats", "Unpublish");
  assert.equal(server.records.has("cats"), false);
  assert.equal(p.card("cats").dataset.status, "draft");
  assert.ok(p.card("cats"), "the feed is still saved");
  assert.match(p.notice(), /Unpublished/);
});

// ---------- deleting ----------

test("deleting a feed that is not published asks, then removes it and counts again", async () => {
  const { p } = await connectedPage(() => {}, { page: { confirms: [false, true] } });
  await p.click("cats", "Delete");
  assert.match(p.asked[0], /Delete “Cats”\? This can't be undone\./);
  assert.equal(p.calls.some((c) => c.method === "DELETE"), false, "declined: nothing was sent");
  assert.equal(p.cards().length, 2);
  await p.click("cats", "Delete");
  const del = p.calls.find((c) => c.method === "DELETE");
  assert.deepEqual([del.url, del.credentials], ["/api/me/feeds/cats", "same-origin"]);
  assert.deepEqual(p.cards().map((c) => c.dataset.rkey), ["dogs"]);
  assert.equal(p.$("count").textContent, "1 of 5 feeds.");
  assert.match(p.notice(), /Deleted “Cats”/);
});

test("deleting a published feed takes its record off Bluesky first", async () => {
  const { p, server } = await connectedPage((s) => s.put("cats", record("Cats")));
  await p.click("cats", "Delete");
  assert.equal(server.records.has("cats"), false);
  assert.equal(p.calls.filter((c) => c.method === "DELETE").length, 1);
  assert.deepEqual(p.cards().map((c) => c.dataset.rkey), ["dogs"]);
});

test("a published feed whose record can't be taken off is not deleted", async () => {
  const { p, server } = await connectedPage((s) => s.put("cats", record("Cats")));
  server.fail = { nsid: "com.atproto.repo.deleteRecord", status: 401, body: { error: "ExpiredToken" } };
  await p.click("cats", "Delete");
  assert.match(p.notice(), /connection to Bluesky has ended\..*The feed was not deleted\./);
  assert.equal(p.calls.some((c) => c.method === "DELETE"), false);
  assert.equal(p.cards().length, 2);
});

test("deleting that fails on the service says why and keeps the feed", async () => {
  const { p } = await connectedPage(() => {}, { page: { remove: { status: 503 } } });
  await p.click("dogs", "Delete");
  assert.match(p.notice(), /Couldn't delete that just now/);
  assert.equal(p.tone(), "error");
  assert.equal(p.cards().length, 2);
  const net = await open({ remove: new TypeError("Failed to fetch") });
  await net.click("dogs", "Delete");
  assert.match(net.notice(), /Couldn't reach the server/);
});

test("deleting without a connection warns that a published feed stays on Bluesky until it is removed there", async () => {
  const p = await open({ client: null });
  await p.click("cats", "Delete");
  assert.match(p.asked[0], /connect your account first so it can be taken off there too/);
  assert.equal(p.cards().length, 1);
});

test("disconnecting drops the connection and forgets what was published", async () => {
  const { p, client } = await connectedPage((s) => s.put("cats", record("Cats")));
  assert.equal(p.card("cats").dataset.status, "published");
  p.$("disconnect-button").click();
  await new Promise((r) => setTimeout(r, 15));
  assert.equal(client.log.signOuts, 1);
  assert.equal(p.$("connection").dataset.state, "disconnected");
  assert.equal(p.card("cats").dataset.status, "unknown");
  assert.equal(p.$("connect-form").hidden, false);
});

test("a connection whose records can't be read is said so, and the feeds are listed without status", async () => {
  const server = new FakeServer();
  server.fail = { nsid: "com.atproto.repo.listRecords", status: 502 };
  const p = await open({ client: connectedClient(server) });
  assert.match(p.notice(), /Couldn't read your feeds just now: Bluesky said 502|Couldn't read your feeds/);
  assert.equal(p.card("cats").dataset.status, "unknown");
});

// ---------- plain helpers ----------

test("topics are put in a line, and a feed's address on Bluesky is made from its account and key", () => {
  assert.equal(topicsText(["*"]), "All topics");
  assert.equal(topicsText(["a/b"]), "b");
  assert.equal(topicsText(["a/b_c", "d/e"]), "b c, e");
  assert.equal(topicsText(["world_news"]), "world news");
  assert.equal(topicsText(["a/1", "a/2", "a/3", "a/4"]), "1, 2, 3, 4");
  assert.equal(topicsText(["a/1", "a/2", "a/3", "a/4", "a/5"]), "1, 2, 3, 4 and 1 more");
  assert.equal(topicsText(undefined), "");
  assert.equal(topicsText([7, "a/b"]), "b");
  assert.equal(feedUrl("did:plc:x", "my-feed"), "https://bsky.app/profile/did%3Aplc%3Ax/feed/my-feed");
});
