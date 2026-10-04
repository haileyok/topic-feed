// Browser-level tests of the page at /inspect: the real inspect.html and inspect.js, run in jsdom,
// with fetch and navigation stubbed so every answer and failure can be simulated.
//
//   TOPICFEED_JSDOM=/path/to/node_modules/jsdom node --test internal/feedgen/webtest/inspect.test.mjs
//
// The Go test TestMePageScript runs these when TOPICFEED_JSDOM is set.

import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import assert from "node:assert/strict";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import {
  inspectBody, inspectedPost, storedPost, pipelineRow, takes, refuses, wouldHave, forYou, check, unscoredBody, bskyView, hoursAgo,
} from "./inspect_data.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.join(here, "..", "web");
const { JSDOM } = createRequire(import.meta.url)(process.env.TOPICFEED_JSDOM);
const scriptURL = pathToFileURL(path.join(webDir, "static", "inspect.js")).href;
const pageHTML = fs.readFileSync(path.join(webDir, "inspect.html"), "utf8").replace(/<script[^>]*><\/script>/, "");

const owner = { status: 200, body: { did: "did:plc:owner", handle: "owner.example" } };
const BSKY = "https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts";

function define(name, value) {
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
}

// A page leaves requests behind when a test ends; they run against whatever page the globals belong
// to by then. So a page waits for the last one to go quiet before it takes them over.
let previous = null;
async function quiet() {
  if (!previous) return;
  for (let i = 0; i < 6; i++) {
    const n = previous.length;
    await new Promise((r) => setTimeout(r, 25));
    if (previous.length === n) return;
  }
}

let loads = 0;

// open loads the page with these answers. An answer is {status, body}, an Error (the request fails),
// or a function of the call returning either (or a promise of either). Asking about no post at all
// is answered like the server does for the owner: that isn't a post address.
async function open({
  search = "",
  me = owner,
  inspect = { status: 200, body: inspectBody() },
  // what asking about no post at all is answered with: for the owner, that it isn't a post address
  probe = { status: 400, body: { error: "invalid", message: "Paste a link to a post, or its at:// address." } },
  bsky = { status: 200, body: { posts: [] } },
  abortable = true, // whether abandoning a request makes it fail, as in a browser
} = {}) {
  await quiet();
  const win = new JSDOM(pageHTML, { url: "https://feeds.example.test/inspect" + search }).window;
  const calls = [];
  previous = calls;
  const reloads = [];
  const fetchStub = async (url, opts = {}) => {
    const call = { url, method: opts.method ?? "GET", headers: opts.headers ?? {}, signal: opts.signal, credentials: opts.credentials };
    calls.push(call);
    const hasPost = url.includes("?post=");
    const answer = {
      "GET /api/me": me,
      "GET /api/inspect": hasPost ? inspect : probe,
      [`GET ${BSKY}`]: bsky,
    }[`${call.method} ${url.split("?")[0]}`];
    const r = await (typeof answer === "function" ? answer(call) : answer);
    if (abortable && opts.signal?.aborted) throw new DOMException("aborted", "AbortError");
    if (r instanceof Error) throw r;
    if (!r) throw new Error(`unexpected request: ${call.method} ${url}`);
    return {
      ok: r.status >= 200 && r.status < 300,
      status: r.status,
      json: async () => {
        if (r.body === undefined) throw new SyntaxError("not JSON");
        return r.body;
      },
    };
  };
  // A stand-in for window.location that records reloads instead of attempting them.
  const location = new Proxy({}, {
    get(_, key) {
      if (key === "reload") return () => reloads.push(1);
      const v = win.location[key];
      return typeof v === "function" ? v.bind(win.location) : v;
    },
  });
  define("document", win.document);
  define("history", win.history);
  define("location", location);
  define("fetch", fetchStub);
  const mod = await import(`${scriptURL}?load=${++loads}`);

  const $ = (id) => win.document.getElementById(id);
  const until = async (cond, what) => {
    for (let i = 0; i < 400; i++) {
      if (cond()) return;
      await new Promise((r) => setTimeout(r, 5));
    }
    throw new Error(`timed out waiting for ${what}`);
  };
  await until(() => $("loading").hidden, "the page to load");
  const visible = () => ["loading", "signed-out", "forbidden", "inspector"].filter((id) => !$(id).hidden);
  const submit = (value) => {
    $("post").value = value;
    const ev = new win.Event("submit", { cancelable: true, bubbles: true });
    $("inspect-form").dispatchEvent(ev);
    return ev;
  };
  // asked is the questions put to /api/inspect, as the posts asked about
  const asked = () => calls.filter((c) => c.url.startsWith("/api/inspect?post=")).map((c) => decodeURIComponent(c.url.slice("/api/inspect?post=".length)));
  const finished = () => until(() => !$("inspect-button").disabled && $("inspect-status").hidden, "the answer");
  return { win, mod, $, calls, reloads, until, visible, submit, asked, finished, q: (sel) => win.document.querySelector(sel), qa: (sel) => [...win.document.querySelectorAll(sel)] };
}

const post = "https://bsky.app/profile/author.example/post/3kabc";
const error = (p) => (p.$("inspect-error").hidden ? null : p.$("inspect-error").textContent);
const facts = (p) => Object.fromEntries(p.qa("#post-facts li").map((li) => [li.querySelector(".k").textContent, li.querySelector(".v").textContent]));
const cards = (p) => p.qa(".insp-feed");
const cardOf = (p, name) => cards(p).find((c) => c.querySelector(".insp-feed-name").textContent === name);
const lines = (card) => [...card.querySelectorAll(".insp-feed-line, .insp-feed-rank")].map((n) => n.textContent);
const groups = (p) => p.qa(".insp-group-title").map((n) => n.textContent);
const labels = (el) => [...el.querySelectorAll(".m-label")].map((n) => n.textContent);

// ---------- who may see it ----------

test("signed out: the sign-in prompt is shown and nothing is asked about any post", async () => {
  const p = await open({ me: { status: 401, body: { error: "not signed in" } } });
  assert.deepEqual(p.visible(), ["signed-out"]);
  assert.deepEqual(p.calls.map((c) => c.url), ["/api/me"]);
  assert.equal(p.q("#signed-out a").getAttribute("href"), "/me");
});

test("a server without the sign-in answers 404 to /api/me: the same prompt", async () => {
  const p = await open({ me: { status: 404 } });
  assert.deepEqual(p.visible(), ["signed-out"]);
});

test("not being able to tell who you are is said, and the form still works", async () => {
  for (const me of [new TypeError("Failed to fetch"), { status: 500 }]) {
    const p = await open({ me });
    assert.deepEqual(p.visible(), ["inspector"]);
    assert.match(error(p), /Couldn't reach the server/);
  }
});

test("someone who isn't the owner is told so as soon as the page opens, before typing anything", async () => {
  const p = await open({ probe: { status: 403, body: { error: "forbidden" } } });
  await p.until(() => p.visible().includes("forbidden"), "the notice");
  assert.deepEqual(p.visible(), ["forbidden"]);
  assert.deepEqual(p.asked(), []);
});

test("any other answer to the opening question is ignored: the form stays", async () => {
  for (const probe of [{ status: 429, body: { error: "limited" } }, { status: 503 }, new TypeError("Failed to fetch"), { status: 200, body: {} }]) {
    const p = await open({ probe });
    await p.until(() => p.win.document.activeElement === p.$("post"), "focus");
    assert.deepEqual(p.visible(), ["inspector"]);
    assert.equal(error(p), null);
  }
});

test("the owner who opens the page gets the form, focused, and no result yet", async () => {
  const p = await open();
  await p.until(() => p.win.document.activeElement === p.$("post"), "focus");
  assert.deepEqual(p.visible(), ["inspector"]);
  assert.equal(p.$("result").hidden, true);
  assert.equal(error(p), null, "the owner is not scolded for the question that asks nothing");
  assert.deepEqual(p.asked(), [], "no post has been asked about");
});

// ---------- asking ----------

test("submitting asks about the post as typed, trimmed, and keeps the address in the URL", async () => {
  const body = inspectBody();
  const p = await open({ inspect: { status: 200, body } });
  const ev = p.submit(`  ${post}  `);
  assert.equal(ev.defaultPrevented, true, "the form must not navigate");
  await p.finished();
  assert.deepEqual(p.asked(), [post]);
  const call = p.calls.find((c) => c.url.startsWith("/api/inspect?post="));
  assert.equal(call.headers.Accept, "application/json");
  assert.equal(call.credentials, "same-origin");
  assert.equal(p.win.location.search, "?post=" + encodeURIComponent(post));
  assert.equal(p.$("result").hidden, false);
});

test("a post in the address bar is asked about straight away, and the address isn't rewritten", async () => {
  const search = "?post=" + encodeURIComponent("at://did:plc:author1/app.bsky.feed.post/3kabc");
  const p = await open({ search });
  await p.until(() => !p.$("result").hidden, "the result");
  assert.equal(p.$("post").value, "at://did:plc:author1/app.bsky.feed.post/3kabc");
  assert.deepEqual(p.asked(), ["at://did:plc:author1/app.bsky.feed.post/3kabc"]);
  assert.equal(p.win.location.search, search);
});

test("asking about nothing says what to paste, and asks nobody", async () => {
  const p = await open();
  const before = p.calls.length;
  for (const empty of ["", "   ", "\n\t"]) {
    p.submit(empty);
    assert.match(error(p), /Paste a link to a post/);
  }
  assert.equal(p.calls.length, before);
});

test("while the answer is awaited the button is off, the status shows, and the old result is hidden", async () => {
  let release;
  const gate = new Promise((r) => (release = r));
  const p = await open({ inspect: async () => (await gate, { status: 200, body: inspectBody() }) });
  p.submit(post);
  assert.equal(p.$("inspect-button").disabled, true);
  assert.equal(p.$("inspect-status").hidden, false);
  assert.equal(p.$("result").hidden, true);
  release();
  await p.finished();
  assert.equal(p.$("result").hidden, false);
  assert.equal(p.$("inspect-status").hidden, true);
});

test("asking again hides the answer to the one before until the new one arrives, and a good answer clears an old complaint", async () => {
  let tries = 0;
  const p = await open({ inspect: () => (++tries === 1 ? { status: 400, body: { error: "invalid", message: "That isn't a post address." } } : { status: 200, body: inspectBody() }) });
  p.submit("nonsense");
  await p.finished();
  assert.equal(error(p), "That isn't a post address.");
  p.submit(post);
  assert.equal(error(p), null, "the complaint goes as soon as the next question is asked");
  await p.finished();
  assert.equal(p.$("result").hidden, false);
  p.submit("nonsense again");
  assert.equal(p.$("result").hidden, true, "the old answer is not left beside a new question");
});

test("the newest question wins: an older answer arriving late is dropped, and the older request is abandoned", async () => {
  for (const abortable of [true, false]) {
    const pending = [];
    const a = inspectBody({ feeds: [takes()] });
    const b = inspectBody({ feeds: [refuses()] });
    const p = await open({ abortable, inspect: (call) => new Promise((r) => pending.push({ call, r })) });
    p.submit("first");
    p.submit("second");
    assert.equal(pending.length, 2);
    assert.equal(pending[0].call.signal.aborted, true, "the first question is abandoned when the second is asked");
    pending[1].r({ status: 200, body: b });
    await p.finished();
    assert.match(p.$("verdict-title").textContent, /^No topic feed takes it/);
    pending[0].r({ status: 200, body: a });
    await new Promise((r) => setTimeout(r, 40));
    assert.match(p.$("verdict-title").textContent, /^No topic feed takes it/, `abortable=${abortable}: the late answer must not replace the newer one`);
    assert.equal(p.$("inspect-button").disabled, false);
  }
});

// ---------- every way an answer can go wrong ----------

test("every way asking can go wrong is said in words, and asking works again", async () => {
  const cases = [
    ["a message from the server", { status: 404, body: { error: "no_such_account", message: 'Couldn\'t find an account with the handle "nobody.example". Check the spelling.' } }, /Couldn't find an account with the handle "nobody\.example"/],
    ["asked too often", { status: 429, body: { error: "limited" } }, /asked a lot/],
    ["unavailable", { status: 503, body: { error: "unavailable" } }, /Couldn't read that just now/],
    ["an error we don't know", { status: 500, body: { error: "something_new" } }, /Couldn't read that just now/],
    ["not JSON at all", { status: 502 }, /Couldn't read that just now/],
    ["success that isn't an answer", { status: 200, body: { hello: "world" } }, /Couldn't read that just now/],
    ["feeds that aren't a list", { status: 200, body: { feeds: null } }, /Couldn't read that just now/],
    ["a message that isn't text", { status: 400, body: { error: "invalid", message: 7 } }, /Couldn't read that just now/],
    ["the request failing", new TypeError("Failed to fetch"), /Couldn't reach the server/],
  ];
  for (const [name, inspect, want] of cases) {
    const p = await open({ inspect });
    p.submit(post);
    await p.finished();
    assert.match(error(p), want, name);
    assert.equal(p.$("result").hidden, true, `${name}: no result`);
    assert.equal(p.$("inspect-button").disabled, false, `${name}: button`);
    assert.equal(p.reloads.length, 0, `${name}: no reload`);
  }
});

test("a session that ended reloads the page, to the sign-in", async () => {
  const p = await open({ inspect: { status: 401, body: { error: "not signed in" } } });
  p.submit(post);
  await p.until(() => p.reloads.length > 0, "the reload");
  assert.equal(p.reloads.length, 1);
});

test("a 403 from the inspector says the page isn't yours", async () => {
  const p = await open({ inspect: { status: 403, body: { error: "forbidden" } } });
  p.submit(post);
  await p.until(() => p.visible().includes("forbidden"), "the notice");
  assert.deepEqual(p.visible(), ["forbidden"]);
});

// ---------- the verdict ----------

test("a post in feeds right now: good, with how many feeds hold it and how many take it", async () => {
  const p = await open({ inspect: { status: 200, body: inspectBody() } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("verdict").dataset.tone, "good");
  assert.equal(p.$("verdict-title").textContent, "In 1 of 3 topic feeds right now");
  assert.equal(p.$("verdict-text").textContent, "It meets the rules of 2 feeds. It can also appear in For you, for viewers whose interests include its topic.");
});

test("one matching feed is said in the singular, and a For you that doesn't match adds nothing", async () => {
  const body = inspectBody({ feeds: [takes(), refuses(), forYou({ matches: false })] });
  const p = await open({ inspect: { status: 200, body } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("verdict-title").textContent, "In 1 of 2 topic feeds right now");
  assert.equal(p.$("verdict-text").textContent, "It meets the rules of 1 feed.");
});

test("a post that meets rules but is in no feed: in between, and says to look at each feed", async () => {
  const body = inspectBody({ feeds: [wouldHave(), refuses()] });
  const p = await open({ inspect: { status: 200, body } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("verdict").dataset.tone, "mid");
  assert.equal(p.$("verdict-title").textContent, "Meets the rules of 1 of 2 topic feeds, but none holds it now");
  assert.match(p.$("verdict-text").textContent, /usually the post is older than the feeds' window/);
});

test("a post no feed takes: none", async () => {
  const body = inspectBody({ feeds: [refuses(), refuses({ rkey: "b", name: "B" })] });
  const p = await open({ inspect: { status: 200, body } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("verdict").dataset.tone, "none");
  assert.equal(p.$("verdict-title").textContent, "No topic feed takes it (0 of 2)");
});

test("a post that wasn't scored says what happened to it, in the server's words", async () => {
  const cases = [
    ["unscored", "Not scored", "The label policy marks it \"drop\"."],
    ["unprocessed", "Not processed yet", "It was stored a moment ago."],
    ["not_stored", "We don't hold this post", "It is a reply, and replies are dropped."],
    ["something_new", "Not scored", "Some other reason."],
  ];
  for (const [state, title, why] of cases) {
    const body = unscoredBody(state, [why, "A second line."]);
    const p = await open({ inspect: { status: 200, body } });
    p.submit(post);
    await p.finished();
    assert.equal(p.$("verdict").dataset.tone, "none", state);
    assert.equal(p.$("verdict-title").textContent, title, state);
    assert.equal(p.$("verdict-text").textContent, why, state);
    assert.equal(p.$("why").hidden, false, `${state}: the rest of the story`);
    assert.deepEqual(p.qa("#why p").map((n) => n.textContent), ["A second line."], state);
    assert.equal(p.$("scores-panel").hidden, true, `${state}: nothing was scored`);
    assert.equal(p.$("feeds-panel").hidden, true, `${state}: no feeds were asked`);
  }
});

test("the story is hidden when there is only the one line", async () => {
  const p = await open({ inspect: { status: 200, body: unscoredBody("unprocessed", ["Only this."]) } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("why").hidden, true);
  assert.equal(p.$("verdict-text").textContent, "Only this.");
});

// ---------- the feeds ----------

test("feeds are grouped: those that take it, those that don't, and For you apart", async () => {
  const p = await open({ inspect: { status: 200, body: inspectBody() } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("feeds-panel").hidden, false);
  assert.deepEqual(groups(p), ["Would take it (2)", "Wouldn't take it (1)", "For you, the personal feed (1)"]);
  assert.match(p.$("feeds-tip").textContent, /within the last 24 hours/);
  const status = (name) => cardOf(p, name).querySelector(".badge").textContent;
  assert.equal(status("AI"), "Takes it");
  assert.equal(status("Tech"), "Would have");
  assert.equal(status("Anime"), "Doesn't take it");
  assert.equal(status("For you"), "Eligible");
  assert.equal(cardOf(p, "AI").dataset.match, "yes");
  assert.equal(cardOf(p, "Anime").dataset.match, "no");
  assert.equal(cardOf(p, "For you").dataset.personal, "yes");
  assert.equal(cardOf(p, "AI").dataset.personal, "no");
  assert.equal(cardOf(p, "AI").querySelector(".insp-feed-key").textContent, "ai");
});

test("a group with no feeds is not shown, and with no feeds at all nor is the panel", async () => {
  let p = await open({ inspect: { status: 200, body: inspectBody({ feeds: [refuses()] }) } });
  p.submit(post);
  await p.finished();
  assert.deepEqual(groups(p), ["Wouldn't take it (1)"]);
  p = await open({ inspect: { status: 200, body: inspectBody({ feeds: [] }) } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("feeds-panel").hidden, true);
  assert.equal(p.$("result").hidden, false);
});

test("a feed says where it stands in the feed now, or why it isn't there", async () => {
  const p = await open({ inspect: { status: 200, body: inspectBody() } });
  p.submit(post);
  await p.finished();
  assert.deepEqual(lines(cardOf(p, "AI")), ["In the feed right now: #14 of 439.", "Ranking score 2.86 = (2.15 prior + 18.40 from reactions) ÷ 7.20 for its age of 3.0 h"]);
  assert.deepEqual(lines(cardOf(p, "Tech")).slice(0, 1), ["Not in the feed right now: it is older than the 24h0m0s the feed reaches back."]);
  assert.equal(lines(cardOf(p, "Tech")).filter((l) => /matched while it was fresh/.test(l)).length, 0, "said once");
  assert.equal(lines(cardOf(p, "Anime"))[0], refuses().reason, "the first rule it fails, as the server worded it");
  assert.deepEqual(lines(cardOf(p, "For you")), [forYou().note], "the personal feed says what the server said, with no position");
  assert.equal(cardOf(p, "For you").querySelector(".insp-feed-rank"), null);
});

test("a matching post older than the window, with nothing known about the build, still says why", async () => {
  const body = inspectBody({ feeds: [wouldHave({ live: undefined })] });
  const p = await open({ inspect: { status: 200, body } });
  p.submit(post);
  await p.finished();
  assert.match(lines(cardOf(p, "Tech"))[0], /older than the feed's window now, but it matched while it was fresh/);
});

test("each feed lists the rules it checked, closed until opened, with a line over them", async () => {
  const p = await open({ inspect: { status: 200, body: inspectBody() } });
  p.submit(post);
  await p.finished();
  const summary = (name) => cardOf(p, name).querySelector("summary").textContent;
  assert.equal(summary("AI"), "Meets all 7 rules");
  assert.equal(summary("Anime"), "1 of 7 rules fail");
  assert.equal(summary("Tech"), "Meets every rule, but not the feed's window");
  assert.equal(cardOf(p, "AI").querySelector("details").open, false);
  const rows = [...cardOf(p, "Anime").querySelectorAll(".insp-check")];
  assert.equal(rows.length, 7);
  const topic = rows.find((r) => r.querySelector("b").textContent === "Topic");
  assert.equal(topic.dataset.state, "fail");
  assert.equal(topic.querySelector(".insp-mark").textContent, "✗");
  assert.equal(topic.querySelector(".insp-mark").getAttribute("aria-hidden"), "true");
  assert.equal(topic.querySelector(".visually-hidden").textContent, "Fails: ", "the state is in words for a screen reader");
  assert.match(topic.querySelector(".insp-check-body").textContent, /none of the feed's topics/);
  const ok = [...cardOf(p, "AI").querySelectorAll(".insp-check")][0];
  assert.deepEqual([ok.dataset.state, ok.querySelector(".insp-mark").textContent, ok.querySelector(".visually-hidden").textContent], ["pass", "✓", "Passes: "]);
  const soft = [...cardOf(p, "Tech").querySelectorAll(".insp-check")].at(-1);
  assert.deepEqual([soft.dataset.state, soft.querySelector(".insp-mark").textContent, soft.querySelector(".visually-hidden").textContent], ["soft", "◔", "Not within: "]);
  const softOk = [...cardOf(p, "AI").querySelectorAll(".insp-check")].at(-1);
  assert.equal(softOk.dataset.state, "pass", "a soft rule that holds is a pass");
});

test("two failing rules are counted", async () => {
  const feed = refuses({ checks: [check("a", false, "x"), check("b", false, "y"), check("c", true, "z")] });
  const p = await open({ inspect: { status: 200, body: inspectBody({ feeds: [feed] }) } });
  p.submit(post);
  await p.finished();
  assert.equal(cardOf(p, "Anime").querySelector("summary").textContent, "2 of 3 rules fail");
});

test("nothing from the server is put on the page as HTML", async () => {
  const evil = '<img src="x" onerror="window.pwned=1"><script>window.pwned=1</script>';
  const body = inspectBody({
    handle: evil,
    post: inspectedPost({ text: evil, stored: storedPost({ text: evil, langs: [evil], selfLabels: [evil], embedType: evil }), currentLabels: [evil], pipeline: pipelineRow({ model: evil, labels: [evil] }) }),
    feeds: [refuses({ name: evil, rkey: evil, reason: evil, checks: [check(evil, false, evil)], live: { inBuild: false, total: 1, why: evil } }), forYou({ note: evil })],
  });
  const p = await open({ inspect: { status: 200, body } });
  p.submit(post);
  await p.finished();
  assert.equal(p.qa("#result img").length, 0, "no element made from the server's text");
  assert.equal(p.qa("#result script").length, 0);
  assert.equal(p.win.pwned, undefined);
  assert.ok(p.$("result").textContent.includes(evil), "it is shown, as words");
  assert.ok(p.$("post-card").textContent.includes("@" + evil), "also in the card");
});

// ---------- the post and what is known of it ----------

test("what is known of the post is listed, in words", async () => {
  const p = await open({ inspect: { status: 200, body: inspectBody() } });
  p.submit(post);
  await p.finished();
  const f = facts(p);
  assert.match(f["We saw it"], /^\d{4}-\d\d-\d\d \d\d:\d\d UTC \(3 h ago\)$/);
  assert.match(f["Its author's date"], /UTC \(3 h ago\)$/);
  assert.match(f["Scored"], /^by microblog-topic-classifier-v5, .* UTC/);
  assert.equal(f["Label policy"], "ok");
  assert.equal(f["Language"], "en · detected en");
  assert.equal(f["Contains"], "text only");
  assert.equal(f["Reactions"], "1,234 likes · 40 reposts · 12 replies · 3 quotes");
  for (const k of ["Pictures the model saw", "Its author's labels", "Labels on it or its author now", "Deleted", "Author's account", "Picture retries"]) assert.ok(!(k in f), `${k} only when it applies`);
});

test("the facts that only sometimes apply appear when they do", async () => {
  const post_ = inspectedPost({
    deleted: true, authorInactive: true, currentLabels: ["porn", "spam"], retry: { status: "pending", attempts: 3, error: "timeout" },
    stored: storedPost({ langs: [], detectedLang: "", embedType: "images", selfLabels: ["nudity"], createdAt: "1970-01-01T00:00:00Z" }),
    pipeline: pipelineRow({ model: "", feedPolicy: "adult_only", labels: ["porn"], picturesWanted: 4, picturesUsed: 2 }),
  });
  const p = await open({ inspect: { status: 200, body: inspectBody({ post: post_ }) } });
  p.submit(post);
  await p.finished();
  const f = facts(p);
  assert.equal(f["Scored"], "never (no model ran)");
  assert.equal(f["Label policy"], "adult_only (labels: porn)");
  assert.equal(f["Pictures the model saw"], "2 of 4");
  assert.equal(f["Language"], "none tagged");
  assert.equal(f["Contains"], "images");
  assert.equal(f["Its author's labels"], "nudity");
  assert.equal(f["Labels on it or its author now"], "porn, spam");
  assert.equal(f["Deleted"], "yes: the author deleted it");
  assert.equal(f["Author's account"], "deactivated, suspended or taken down");
  assert.equal(f["Picture retries"], "pending, 3 tried: timeout");
  assert.ok(!("Its author's date" in f), "a date that is plainly wrong is not shown");
});

test("lists the server sent as null don't break the page", async () => {
  const post_ = inspectedPost({
    currentLabels: null, tone: null, signals: null, subtopics: null, broads: null,
    stored: storedPost({ langs: null, selfLabels: null }), pipeline: pipelineRow({ labels: null }),
  });
  const p = await open({ inspect: { status: 200, body: inspectBody({ post: post_ }) } });
  p.submit(post);
  await p.finished();
  assert.equal(error(p), null);
  assert.equal(p.$("result").hidden, false);
  assert.equal(facts(p)["Language"], "none tagged · detected en");
  assert.equal(p.qa("#scores .why-sec").length, 4, "the score panels still show, empty");
});

test("times are said in UTC with how long ago, and an impossible time is 'unknown'", async () => {
  const p = await open();
  assert.equal(p.mod.when("2026-10-04T12:34:56Z").startsWith("2026-10-04 12:34 UTC ("), true);
  assert.match(p.mod.when(hoursAgo(30)), /\(30 h ago\)$/);
  assert.match(p.mod.when(hoursAgo(72)), /\(3 days ago\)$/);
  for (const bad of ["", "garbage", "0001-01-01T00:00:00Z", "1970-01-01T00:00:00Z", null, undefined, 7]) assert.equal(p.mod.when(bad), "unknown", String(bad));
});

test("the ranking line shows its parts", async () => {
  const p = await open();
  assert.equal(p.mod.rankingLine({ score: 2.86, prior: 2.15, engagement: 18.4, decay: 7.2, ageHours: 3 }), "Ranking score 2.86 = (2.15 prior + 18.40 from reactions) ÷ 7.20 for its age of 3.0 h");
  assert.equal(p.mod.rankingLine({ score: 0.0421, prior: 0.1, engagement: 0, decay: 2.4, ageHours: 0.2 }), "Ranking score 0.042 = (0.10 prior + 0.000 from reactions) ÷ 2.40 for its age of 0.2 h");
  assert.equal(p.mod.rankingLine({ score: 1234.5, prior: 3, engagement: 5000, decay: 4, ageHours: 12 }), "Ranking score 1235 = (3.00 prior + 5000 from reactions) ÷ 4.00 for its age of 12.0 h");
  assert.match(p.mod.rankingLine({}), /Ranking score 0\.000/, "missing numbers are zeros, not NaN");
});

// ---------- the scores ----------

test("the model's scores: subtopics (the best 8), broad topics, tone and signals, best first", async () => {
  const many = Array.from({ length: 10 }, (_, i) => ({ path: `a/t${i}`, name: `Topic ${i}`, broad: "A", p: 0.9 - i * 0.05 }));
  const post_ = inspectedPost({
    subtopics: many, tone: { personal: 0.2, informative: 0.62, mystery: 0.2 }, signals: { news: 0.2, substance: 0.55, meme: 0.1 },
  });
  const p = await open({ inspect: { status: 200, body: inspectBody({ post: post_ }) } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("scores-panel").hidden, false);
  const sections = Object.fromEntries(p.qa("#scores .why-sec").map((s) => [s.querySelector("h4").textContent, s]));
  assert.deepEqual(Object.keys(sections), ["Subtopics", "Broad topics", "Tone", "Signals"]);
  assert.deepEqual(labels(sections["Subtopics"]), many.slice(0, 8).map((t) => t.name));
  assert.deepEqual(labels(sections["Broad topics"]), ["Technology", "Online culture"]);
  // each name has its emoji in front; a score we have no name for is shown as it came, with no stray space
  const names = (s) => labels(s).map((l) => l.replace(/^\S+ /, ""));
  assert.deepEqual(names(sections["Tone"]), ["Informative", "mystery", "Personal"], "best first, ties in name order");
  assert.deepEqual(names(sections["Signals"]), ["Substance", "Newsy", "Meme"]);
  assert.match(labels(sections["Tone"])[0], /^\S+ Informative$/, "with its emoji");
  assert.deepEqual([...sections["Subtopics"].querySelectorAll(".m-val")].map((n) => n.textContent).slice(0, 2), ["90%", "85%"]);
  assert.equal(sections["Subtopics"].querySelector(".meter").title, "a/t0", "the topic's path is on hover");
});

test("scores are shown only for a post the model scored, even when a row is known", async () => {
  const p = await open({ inspect: { status: 200, body: inspectBody({ state: "unscored", why: ["It was dropped."] }) } });
  p.submit(post);
  await p.finished();
  assert.equal(p.$("scores-panel").hidden, true);
});

// ---------- the post card ----------

test("the post is first shown as we hold it, then as Bluesky shows it", async () => {
  const body = inspectBody();
  let release;
  const gate = new Promise((r) => (release = r));
  const p = await open({ inspect: { status: 200, body }, bsky: async () => (await gate, { status: 200, body: { posts: [bskyView(body)] } }) });
  p.submit(post);
  await p.until(() => !p.$("result").hidden, "the result");
  const card = () => p.$("post-card");
  assert.ok(card().querySelector(".post.fallback"), "ours, while Bluesky hasn't answered");
  assert.equal(card().querySelector(".post-name").textContent, "@author.example");
  assert.match(card().querySelector(".post-text").textContent, /Researchers released a small model/);
  const open_ = card().querySelector("a");
  assert.equal(open_.href, body.url);
  assert.equal(open_.rel, "noopener noreferrer");
  assert.equal(open_.target, "_blank");
  release();
  await p.until(() => !card().querySelector(".fallback"), "Bluesky's card");
  assert.equal(card().querySelector(".post-name").textContent, "Author Example");
  assert.equal(card().querySelector(".post-text").textContent, "the words on Bluesky");
  const asked = p.calls.find((c) => c.url.startsWith(BSKY));
  assert.equal(decodeURIComponent(asked.url.split("uris=")[1]), body.uri);
});

test("when Bluesky can't or won't show the post, ours stays and nothing is complained of", async () => {
  const hiddenLabels = [{ val: "gore" }];
  for (const [name, bsky] of [
    ["an error", { status: 500 }],
    ["the request failing", new TypeError("Failed to fetch")],
    ["no such post", { status: 200, body: { posts: [] } }],
  ]) {
    const p = await open({ inspect: { status: 200, body: inspectBody() }, bsky });
    p.submit(post);
    await p.finished();
    await new Promise((r) => setTimeout(r, 30));
    assert.ok(p.$("post-card").querySelector(".fallback"), name);
    assert.equal(error(p), null, name);
  }
  const body = inspectBody();
  const p = await open({ inspect: { status: 200, body }, bsky: { status: 200, body: { posts: [bskyView(body, { labels: hiddenLabels })] } } });
  p.submit(post);
  await p.finished();
  await new Promise((r) => setTimeout(r, 30));
  assert.ok(p.$("post-card").querySelector(".fallback"), "a post Bluesky labels as graphic is not shown beyond our text");
});

test("a post we don't hold is still shown if Bluesky has it", async () => {
  const body = unscoredBody("not_stored", ["We don't hold it."]);
  let release;
  const gate = new Promise((r) => (release = r));
  const p = await open({ inspect: { status: 200, body }, bsky: async () => (await gate, { status: 200, body: { posts: [bskyView(body)] } }) });
  p.submit(post);
  await p.until(() => !p.$("result").hidden, "the result");
  assert.equal(p.$("post-card").textContent, "We hold nothing of this post to show.");
  release();
  await p.until(() => /the words on Bluesky/.test(p.$("post-card").textContent), "Bluesky's card");
});

test("a post we don't hold and Bluesky doesn't show says so", async () => {
  const body = unscoredBody("not_stored", ["We don't hold it."]);
  const p = await open({ inspect: { status: 200, body } });
  p.submit(post);
  await p.finished();
  await new Promise((r) => setTimeout(r, 30));
  assert.equal(p.$("post-card").textContent, "We hold nothing of this post to show.");
});

test("an address that isn't a bsky.app address is never linked and never sent to Bluesky", async () => {
  for (const url of ["javascript:alert(1)", "https://evil.example/profile/x/post/y", "http://bsky.app/profile/x/post/y", "https://user:pw@bsky.app/x", "", undefined]) {
    const body = inspectBody({ url });
    const p = await open({ inspect: { status: 200, body } });
    p.submit(post);
    await p.finished();
    assert.equal(p.$("post-card").querySelector("a"), null, String(url));
    assert.equal(p.calls.some((c) => c.url.startsWith(BSKY)), false, `${url}: not asked of Bluesky`);
    assert.ok(p.$("post-card").querySelector(".fallback"), `${url}: our card is shown`);
  }
});

test("our card copes with a post that has no text, no handle, no date", async () => {
  const body = inspectBody({ handle: undefined, post: inspectedPost({ stored: storedPost({ text: "" }), indexedAt: "garbage" }) });
  const p = await open({ inspect: { status: 200, body } });
  p.submit(post);
  await p.finished();
  const card = p.$("post-card");
  assert.equal(card.querySelector(".post-name").textContent, body.did);
  assert.equal(card.querySelector(".post-text").textContent, "(we hold no text for this post)");
  assert.equal(card.querySelector(".avatar").textContent, "?");
  assert.equal(card.querySelector(".post-time").textContent, "· ?");
});

test("an answer from Bluesky that arrives after a newer question is not shown", async () => {
  const first = inspectBody();
  const second = inspectBody({ feeds: [refuses()] });
  const answers = [first, second];
  let n = 0;
  let release;
  const gate = new Promise((r) => (release = r));
  const p = await open({
    inspect: () => ({ status: 200, body: answers[n++] }),
    bsky: async (call) => {
      if (call.url.includes(encodeURIComponent(first.uri))) await gate;
      const isFirst = call.url.includes(encodeURIComponent(first.uri));
      return { status: 200, body: { posts: [bskyView(isFirst ? first : second, isFirst ? { record: { text: "from the first post" } } : {})] } };
    },
    abortable: false,
  });
  p.submit("one");
  await p.until(() => !p.$("result").hidden, "the first result");
  p.submit("two");
  await p.finished();
  await p.until(() => !p.$("post-card").querySelector(".fallback"), "the second card");
  release();
  await new Promise((r) => setTimeout(r, 40));
  assert.equal(p.$("post-card").querySelectorAll("article").length, 1);
  assert.equal(p.$("post-card").querySelector(".post-text").textContent, "the words on Bluesky", "the card is the newer post's");
  assert.equal(p.$("verdict-title").textContent, "No topic feed takes it (0 of 1)");
});

test("the page's pieces: every id the script uses exists, none twice", async () => {
  const src = fs.readFileSync(path.join(webDir, "static", "inspect.js"), "utf8");
  const used = new Set([...src.matchAll(/\$\("([a-z-]+)"\)/g)].map((m) => m[1]));
  const p = await open();
  for (const id of used) assert.ok(p.$(id), `#${id} is missing from inspect.html`);
  const ids = p.qa("[id]").map((n) => n.id);
  assert.equal(new Set(ids).size, ids.length, "ids are unique");
});
