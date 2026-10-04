// Tests of mine.js: what the builder does to save the feed being built as the signed-in person's own.
//
//   TOPICFEED_JSDOM=/path/to/node_modules/jsdom node --test internal/feedgen/webtest/mine.test.mjs
//
// The Go test TestMinePageScript runs these when TOPICFEED_JSDOM is set.

import path from "node:path";
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const { JSDOM } = createRequire(import.meta.url)(process.env.TOPICFEED_JSDOM);
const win = new JSDOM("<!doctype html><body></body>", { url: "https://feeds.example.test/" }).window;
Object.defineProperty(globalThis, "document", { value: win.document, configurable: true, writable: true });
const mine = await import(pathToFileURL(path.join(here, "..", "web", "static", "mine.js")).href);

// fetch is a stub that records what it was asked and answers from `answer`: {status, body}, an Error, or a function.
let calls = [];
let answer = null;
Object.defineProperty(globalThis, "fetch", {
  configurable: true, writable: true,
  value: async (url, opts = {}) => {
    calls.push({ url, method: opts.method ?? "GET", headers: opts.headers ?? {}, credentials: opts.credentials, body: opts.body === undefined ? undefined : JSON.parse(opts.body) });
    const r = await (typeof answer === "function" ? answer(url, opts) : answer);
    if (r instanceof Error) throw r;
    return {
      ok: r.status >= 200 && r.status < 300, status: r.status,
      json: async () => { if (r.body === undefined) throw new SyntaxError("not JSON"); return r.body; },
    };
  },
});
const reply = (a) => { calls = []; answer = a; };

const spec = (over = {}) => ({
  paths: ["animals_nature/cats"], min_prob: 0.6, exclude: { art: 0.3 }, tone: { max: {}, min: {}, weights: {} },
  signals: { max: {}, min: {}, weights: {} }, ranking: { gravity: 1.8, fresh_every: 4, author_gap: 10, promo_penalty: 1, weights: { like: 1, repost: 2, reply: 2, quote: 3 } },
  ...over,
});
const feedView = (rkey, over = {}) => ({ rkey, uri: `at://did:plc:bob/app.bsky.feed.generator/${rkey}`, spec: { display_name: "Cats" }, createdAt: "2026-10-04T00:00:00Z", updatedAt: "2026-10-04T00:00:00Z", ...over });

// ---------- plain functions ----------

test("a feed's key is made from its name", () => {
  const cases = [
    ["Cats", "cats"], ["My Cool Feed!", "my-cool-feed"], ["  --Trim--  ", "trim"], ["Über Cats", "uber-cats"], ["", "my-feed"], [undefined, "my-feed"],
    ["🐱🐱🐱", "my-feed"], ["a very long feed name that goes on", "a-very-long-fee"], ["abcdefghijklmn-opq", "abcdefghijklmn"],
  ];
  for (const [name, want] of cases) assert.equal(mine.slugify(name), want, String(name));
  for (const [name] of cases) assert.match(mine.slugify(name), mine.RKEY, `${name} makes a valid key`);
});

test("clip cuts to a number of characters, not a number of code units", () => {
  assert.equal(mine.clip("hello", 3), "hel");
  assert.equal(mine.clip("🐱🐱🐱🐱", 2), "🐱🐱");
  assert.equal(mine.clip("short", 50), "short");
  assert.equal(mine.clip(undefined, 5), "");
  assert.equal(mine.clip(7, 5), "");
});

test("the body that is saved is the feed the builder describes, with adult posts left out", () => {
  const s = spec();
  const body = mine.feedBody(s, { name: "Cats", description: "All the cats." });
  assert.deepEqual(body.exclude, { art: 0.3, adult_content: 0.2 });
  assert.deepEqual([body.display_name, body.description, body.accepts_interactions, body.min_prob], ["Cats", "All the cats.", true, 0.6]);
  assert.deepEqual(body.paths, ["animals_nature/cats"]);
  assert.deepEqual(body.ranking, s.ranking);
  assert.deepEqual(s.exclude, { art: 0.3 }, "the builder's own settings are not changed");
  assert.ok(!("allow_adult" in body) && !("max_posts" in body));
  // The builder's adult switch (only the owner has it) takes the exclusion away and allows adult posts, for the owner alone.
  const adult = mine.feedBody(s, { name: "A", adult: true, owner: true });
  assert.deepEqual(adult.exclude, { art: 0.3 });
  assert.equal(adult.allow_adult, true);
  assert.ok(!("allow_adult" in mine.feedBody(s, { name: "A", adult: true, owner: false })), "never asked for by anyone else");
});

test("a feed of whole topics asks for more candidates, up to what the account may", () => {
  const broad = spec({ paths: ["world_news"] });
  assert.equal(mine.feedBody(broad, { name: "N" }).max_posts, 10000, "no limit given: the owner's");
  assert.equal(mine.feedBody(broad, { name: "N", maxPosts: 3000 }).max_posts, 3000);
  assert.equal(mine.feedBody(broad, { name: "N", maxPosts: 50000 }).max_posts, 10000, "never above what the builder offers");
  assert.equal(mine.feedBody(spec({ paths: ["*"] }), { name: "N", maxPosts: 3000 }).max_posts, 3000, "all topics counts as broad");
  assert.equal(mine.feedBody(spec({ paths: ["art/painting", "world_news"] }), { name: "N", maxPosts: 3000 }).max_posts, 3000, "one whole topic among subtopics is enough");
  assert.ok(!("max_posts" in mine.feedBody(spec({ paths: ["art/painting"] }), { name: "N", maxPosts: 3000 })), "subtopics alone don't need more");
});

test("a max age and how much popularity counts are saved with the feed", () => {
  const s = spec({ max_age_minutes: 90, ranking: { gravity: 3, engagement_power: 0.5 } });
  const body = mine.feedBody(s, { name: "N" });
  assert.equal(body.max_age_minutes, 90);
  assert.equal(body.ranking.engagement_power, 0.5);
  assert.ok(!("max_age_minutes" in mine.feedBody(spec(), { name: "N" })), "no max age: the whole day, left out");
});

test("names and descriptions are cut to what Bluesky allows", () => {
  const body = mine.feedBody(spec(), { name: "🐱".repeat(40), description: "d".repeat(400) });
  assert.equal(Array.from(body.display_name).length, 24);
  assert.equal(Array.from(body.description).length, 300);
  assert.equal(mine.feedBody(spec(), { name: "", description: undefined }).display_name, "My feed");
  assert.equal(mine.feedBody(spec(), { name: "x" }).description, "");
});

test("what went wrong is said in words", () => {
  assert.equal(mine.problemText(400, { message: "The feed's key must be 1-15 lowercase letters." }), "The feed's key must be 1-15 lowercase letters.");
  assert.match(mine.problemText(401, null), /signed out/);
  assert.match(mine.problemText(403, {}), /didn't come from this page/);
  assert.match(mine.problemText(413, {}), /too big/);
  assert.match(mine.problemText(429, {}), /Slow down/);
  assert.match(mine.problemText(500, null), /Couldn't save that just now/);
  assert.match(mine.problemText(503, { message: 7 }), /Couldn't save that just now/, "a message that isn't text is not shown");
});

// ---------- talking to the server ----------

test("who is signed in, and what they have, or nothing when they are not or it fails", async () => {
  reply({ status: 200, body: { did: "did:plc:bob", handle: "bob.test" } });
  assert.deepEqual(await mine.fetchMe(), { did: "did:plc:bob", handle: "bob.test" });
  assert.equal(calls[0].url, "/api/me");
  assert.equal(calls[0].credentials, "same-origin");
  for (const a of [{ status: 401, body: { error: "not signed in" } }, { status: 404 }, { status: 200, body: {} }, { status: 200, body: { did: 7 } }, { status: 200 }, new TypeError("Failed to fetch")]) {
    reply(a);
    assert.equal(await mine.fetchMe(), null);
  }
  reply({ status: 200, body: { feeds: [], limits: { maxFeeds: 5 } } });
  assert.deepEqual(await mine.fetchMine(), { feeds: [], limits: { maxFeeds: 5 } });
  assert.equal(calls[0].url, "/api/me/feeds");
  for (const a of [{ status: 401, body: {} }, { status: 503 }, { status: 200, body: { feeds: "no" } }, { status: 200, body: null }, new TypeError("Failed to fetch")]) {
    reply(a);
    assert.equal(await mine.fetchMine(), null);
  }
});

test("saving puts the feed under its key, as JSON, from this site's own page", async () => {
  reply({ status: 201, body: { feed: feedView("my-cats"), created: true } });
  const r = await mine.saveFeed("my-cats", { display_name: "Cats" });
  assert.deepEqual([r.ok, r.created, r.feed.rkey], [true, true, "my-cats"]);
  const c = calls[0];
  assert.deepEqual([c.url, c.method, c.credentials, c.headers["Content-Type"], c.headers.Accept], ["/api/me/feeds/my-cats", "PUT", "same-origin", "application/json", "application/json"]);
  assert.deepEqual(c.body, { display_name: "Cats" });
  reply({ status: 200, body: { feed: feedView("x"), created: false } });
  assert.equal((await mine.saveFeed("x", {})).created, false);
  reply({ status: 201, body: { feed: feedView("a b/c") } });
  await mine.saveFeed("a b/c", {});
  assert.equal(calls[0].url, "/api/me/feeds/a%20b%2Fc", "the key is encoded");
});

test("every way saving can fail is an answer, never an exception", async () => {
  for (const [a, want] of [
    [{ status: 400, body: { error: "invalid", message: "That isn't a feed: nope" } }, /That isn't a feed: nope/],
    [{ status: 409, body: { error: "limit", message: "You can have at most 5 feeds." } }, /at most 5 feeds/],
    [{ status: 401, body: { error: "not signed in" } }, /signed out/],
    [{ status: 429, body: { error: "limited" } }, /Slow down/],
    [{ status: 502 }, /Couldn't save that just now/],
    [{ status: 200, body: { hello: 1 } }, /Couldn't save that just now/],
    [{ status: 200, body: { feed: { rkey: 7 } } }, /Couldn't save that just now/],
    [new TypeError("Failed to fetch"), /Couldn't reach the server/],
  ]) {
    reply(a);
    const r = await mine.saveFeed("k", {});
    assert.equal(r.ok, false);
    assert.match(r.message, want);
  }
});

// ---------- the panel ----------

const me = { did: "did:plc:bob", handle: "bob.test" };
const stateOf = (over = {}) => ({ name: "Cats", description: "", rkey: "", editing: "", ...over });
const panelOf = (over = {}) => {
  const saved = [];
  const signIns = [];
  const m = "mine" in over ? over.mine : { owner: false, feeds: [], limits: { maxFeeds: 5, maxPosts: 3000 } };
  const state = over.state ?? stateOf();
  const el = mine.savePanel({
    me: "me" in over ? over.me : me, mine: m, state, spec: over.spec ?? (() => spec()), adult: () => !!over.adult,
    onSaved: (...a) => saved.push(a), onSignIn: () => signIns.push(1), notice: over.notice ?? null,
  });
  win.document.body.replaceChildren(el);
  const q = (s) => el.querySelector(s);
  return { el, q, saved, signIns, state, mine: m, button: q("button.btn-primary"), msg: q(".mine-msg"), click: async () => { q("button.btn-primary").click(); await new Promise((r) => setTimeout(r, 10)); } };
};

test("signed out: the same panel and button, and saving asks to sign in instead of sending anything", async () => {
  reply({ status: 201, body: { feed: feedView("cats"), created: true } }); // what it would get, if it asked
  const p = panelOf({ me: null, mine: null });
  assert.equal(p.q(".section-title").textContent, "Save as my feed");
  assert.equal(p.button.textContent, "Save as my feed");
  assert.match(p.q(".section-hint").textContent, /Sign in to save this feed as yours/);
  assert.ok(p.q("#mine-rkey") && p.q("#mine-description"), "the same fields to fill in");
  assert.ok(!p.el.textContent.includes("null"), "nothing says null");
  await p.click();
  assert.equal(p.signIns.length, 1);
  assert.equal(calls.length, 0, "nothing was sent");
  assert.equal(p.msg.hidden, true);
});

test("a save the server refuses as signed out (the session ended) asks to sign in again", async () => {
  reply({ status: 401, body: { error: "not signed in" } });
  const p = panelOf();
  await p.click();
  assert.equal(p.signIns.length, 1);
  assert.equal(p.saved.length, 0);
});

test("signed in but the list of feeds couldn't be read: saving still works", async () => {
  reply({ status: 201, body: { feed: feedView("cats"), created: true } });
  const p = panelOf({ mine: null });
  assert.equal(p.q(".section-hint").textContent, "Signed in as bob.test.");
  await p.click();
  assert.equal(calls[0].url, "/api/me/feeds/cats");
  assert.equal(p.saved.length, 1);
});

test("the panel says who is signed in and how many feeds they have of how many they may", () => {
  const p = panelOf({ mine: { owner: false, feeds: [feedView("a"), feedView("b")], limits: { maxFeeds: 5 } } });
  assert.match(p.q(".section-hint").textContent, /Signed in as bob\.test\. You have 2 of 5 feeds\./);
  assert.equal(p.q(".section-title").textContent, "Save as my feed");
  assert.equal(p.button.textContent, "Save as my feed");
  assert.equal(p.q("a.btn").getAttribute("href"), "/feeds");
  assert.equal(p.msg.hidden, true);
  const owner = panelOf({ mine: { owner: true, feeds: [feedView("a")], limits: { maxFeeds: 0 } } });
  assert.match(owner.q(".section-hint").textContent, /You have 1 feed\./, "no limit to speak of");
  assert.equal(panelOf({ mine: { owner: true, feeds: [], limits: { maxFeeds: 0 } } }).q(".section-hint").textContent.endsWith("You have 0 feeds."), true);
});

test("what comes from the server is text, never HTML", () => {
  const evil = '<img src=x onerror="window.pwned=1">';
  win.document.body.replaceChildren(mine.savePanel({ me: { did: "did:plc:x", handle: evil }, mine: { feeds: [], limits: {} }, state: stateOf({ name: evil, description: evil }), spec: () => spec() }));
  assert.equal(win.document.querySelectorAll("img").length, 0);
  assert.ok(win.document.body.textContent.includes(evil));
  assert.equal(win.pwned, undefined);
});

test("a feed with no name or no topics, or a bad key, is not sent", async () => {
  reply({ status: 201, body: { feed: feedView("x"), created: true } });
  let p = panelOf({ state: stateOf({ name: "  " }) });
  await p.click();
  assert.match(p.msg.textContent, /Give the feed a name/);
  assert.equal(p.msg.dataset.tone, "error");
  p = panelOf({ spec: () => spec({ paths: [] }) });
  await p.click();
  assert.match(p.msg.textContent, /Pick at least one topic/);
  p = panelOf({ state: stateOf({ rkey: "Bad Key!" }) });
  await p.click();
  assert.match(p.msg.textContent, /1-15 lowercase letters, digits or dashes/);
  assert.equal(calls.length, 0, "nothing was sent");
});

test("a new feed's key is made from its name unless one is typed", async () => {
  reply({ status: 201, body: { feed: feedView("cats"), created: true } });
  let p = panelOf({ state: stateOf({ name: "Cats & Dogs!" }) });
  assert.equal(p.q("#mine-rkey").placeholder, "cats-dogs");
  await p.click();
  assert.equal(calls[0].url, "/api/me/feeds/cats-dogs");
  reply({ status: 201, body: { feed: feedView("my-key"), created: true } });
  p = panelOf({ state: stateOf({ name: "Cats" }) });
  const input = p.q("#mine-rkey");
  input.value = "  my-key ";
  input.dispatchEvent(new win.Event("input"));
  await p.click();
  assert.equal(calls[0].url, "/api/me/feeds/my-key");
});

test("saving sends the feed with its description, and the panel then belongs to that feed", async () => {
  reply({ status: 201, body: { feed: feedView("cats", { updatedAt: "2026-10-05T00:00:00Z" }), created: true } });
  const p = panelOf({ state: stateOf({ description: "All the cats." }) });
  p.q("#mine-description").value = "All the cats, and more.";
  p.q("#mine-description").dispatchEvent(new win.Event("input"));
  let during = null;
  answer = async () => { during = p.button.disabled; return { status: 201, body: { feed: feedView("cats"), created: true } }; };
  await p.click();
  assert.equal(during, true, "no second click while it saves");
  assert.equal(p.button.disabled, false);
  assert.equal(calls[0].body.description, "All the cats, and more.");
  assert.equal(calls[0].body.display_name, "Cats");
  assert.match(p.msg.textContent, /Saved\. It's yours now/);
  assert.equal(p.msg.dataset.tone, "ok");
  assert.equal(p.state.editing, "cats");
  assert.deepEqual(p.mine.feeds.map((f) => f.rkey), ["cats"], "it is among their feeds now");
  assert.equal(p.saved.length, 1);
  assert.equal(p.saved[0][0].rkey, "cats");
  assert.deepEqual(p.saved[0][1], { text: p.msg.textContent, tone: "ok" });
});

test("an empty description is saved as the one the panel suggests", async () => {
  reply({ status: 201, body: { feed: feedView("cats"), created: true } });
  const p = panelOf();
  assert.equal(p.q("#mine-description").placeholder, mine.defaultDescription("Cats"));
  await p.click();
  assert.equal(calls[0].body.description, "Cats, picked by a topic classifier. No keyword lists.");
});

test("changing a feed that is theirs keeps its key and replaces it among their feeds", async () => {
  reply({ status: 200, body: { feed: feedView("cats", { spec: { display_name: "Cats and more" } }), created: false } });
  const p = panelOf({ state: stateOf({ editing: "cats", name: "Cats and more" }), mine: { owner: false, feeds: [feedView("cats"), feedView("dogs")], limits: { maxFeeds: 5 } } });
  assert.equal(p.q("#mine-rkey"), null, "the key of a saved feed can't be changed");
  assert.match(p.q(".section-hint:last-of-type, .section-hint + .section-hint")?.textContent ?? p.el.textContent, /Feed key: cats/);
  assert.equal(p.q(".section-title").textContent, "Your feed");
  assert.equal(p.button.textContent, "Save changes");
  await p.click();
  assert.equal(calls[0].url, "/api/me/feeds/cats");
  assert.deepEqual(p.mine.feeds.map((f) => f.rkey), ["cats", "dogs"], "replaced, not added");
  assert.equal(p.mine.feeds[0].spec.display_name, "Cats and more");
  assert.match(p.msg.textContent, /Changes saved/);
});

test("a feed that is not saved says why, keeps the panel as it was and lets them try again", async () => {
  reply({ status: 409, body: { error: "limit", message: "You can have at most 5 feeds. Delete one to make another." } });
  const p = panelOf();
  await p.click();
  assert.match(p.msg.textContent, /at most 5 feeds/);
  assert.equal(p.msg.dataset.tone, "error");
  assert.equal(p.button.disabled, false);
  assert.equal(p.saved.length, 0);
  assert.equal(p.state.editing, "");
  assert.deepEqual(p.mine.feeds, []);
});

test("the notice of a save that has just happened is shown when the panel is made again", () => {
  const p = panelOf({ notice: { text: "Saved. It's yours now: publish it from your feeds.", tone: "ok" } });
  assert.equal(p.msg.hidden, false);
  assert.equal(p.msg.textContent, "Saved. It's yours now: publish it from your feeds.");
  assert.equal(p.msg.dataset.tone, "ok");
});

test("the owner's adult switch and limits are passed on to the body", async () => {
  reply({ status: 201, body: { feed: feedView("a"), created: true } });
  const p = panelOf({ adult: true, mine: { owner: true, feeds: [], limits: { maxFeeds: 0, maxPosts: 0 } }, spec: () => spec({ paths: ["adult_content"] }) });
  await p.click();
  assert.equal(calls[0].body.allow_adult, true);
  assert.deepEqual(calls[0].body.exclude, { art: 0.3 });
  assert.equal(calls[0].body.max_posts, 10000);
  reply({ status: 201, body: { feed: feedView("a"), created: true } });
  const user = panelOf({ adult: true, spec: () => spec({ paths: ["world_news"] }) });
  await user.click();
  assert.ok(!("allow_adult" in calls[0].body));
  assert.equal(calls[0].body.max_posts, 3000);
});
