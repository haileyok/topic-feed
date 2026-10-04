// What the tests of the page at /me share: the real me.html and me.js, run in jsdom, with fetch
// and navigation stubbed so every answer and failure can be simulated. This directory is not
// under web/ because everything under web/ is embedded in the service and served.

import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.join(here, "..", "web");
const { JSDOM } = createRequire(import.meta.url)(process.env.TOPICFEED_JSDOM);
const scriptURL = pathToFileURL(path.join(webDir, "static", "me.js")).href;
// The page waits a moment after a change before previewing it, and a while before trying a
// preview again: both are made short here.
const pageHTML = fs
  .readFileSync(path.join(webDir, "me.html"), "utf8")
  .replace(/<script[^>]*><\/script>/, "")
  .replace('data-preview-delay="400"', 'data-preview-delay="5"')
  .replace('data-retry-delay="3000"', 'data-retry-delay="30"');

let loads = 0;

// interestsBody is what /api/me/interests answers, with anything given replacing the defaults.
export function interestsBody(over = {}) {
  return {
    did: "did:plc:alice", handle: "alice.test", personalized: true, state: "personal",
    settings: { lookbackDays: 30, halfLifeDays: 7, minLikes: 5, topics: 20 },
    coverage: { total: 40, classified: 30, unclassified: 4, repliesOrOther: 4, unseen: 2 },
    interests: [], otherPosts: 0, unclearPosts: 0,
    ...over,
  };
}

export const interest = (over = {}) => ({
  path: "technology/ai", name: "AI", broad: "Technology", share: 0.5, weight: 1, tunedShare: 0.5, added: false, posts: 0, samples: [],
  ...over,
});

export const signedIn = { status: 200, body: { did: "did:plc:alice", handle: "alice.test" } };

// tuningBody is what GET /api/me/tuning answers: nothing saved, the feed's own settings, and the
// topics that can be added.
export const TOPICS = [
  { path: "animals_nature/cats", name: "Cats", broad: "Animals and nature" },
  { path: "food/baking", name: "Baking", broad: "Food" },
  { path: "sports/baseball", name: "Baseball", broad: "Sports" },
  { path: "sports/soccer", name: "Soccer", broad: "Sports" },
  { path: "technology/ai", name: "AI", broad: "Technology" },
];
export function tuningBody(over = {}) {
  const ranking = (gravity, freshEvery) => ({ gravity, freshEvery, promoPenalty: 1, like: 1, repost: 2, reply: 2, quote: 3 });
  return {
    tuning: {},
    defaults: {
      freshness: "balanced", authorGap: 10, halfLifeDays: 7, lookbackDays: 30, minLikes: 5, interests: 20, windowHours: 24, minTopicProb: 0.5,
      maxServes: 2, listSize: 300, minEngagement: 5,
      ranking: { popular: ranking(1.2, 0), balanced: ranking(1.8, 4), fresh: ranking(3, 3) },
    },
    limits: {
      weight: 5, boost: 10, gravity: 10, freshEvery: 50, promoPenalty: 10, engagement: 20, authorGap: 50, lookbackDays: 30, minLikes: 500,
      interests: 100, windowHours: 24, minTopicProb: 0.5, maxServes: 20, listSize: 300, minEngagement: 200,
    },
    maxWeight: 5,
    tones: ["informative", "humorous", "personal", "outraged", "supportive", "other"],
    signals: ["substance", "news", "promo", "general_interest", "sentiment", "critical", "ad", "engagement_bait", "spam", "self_promo", "meme"],
    topics: TOPICS,
    ...over,
  };
}

// Every post gets an address of its own: the page remembers what Bluesky said about each address.
let postCounter = 0;
export const previewPost = (over = {}) => {
  const n = ++postCounter;
  return {
    uri: `at://did:plc:author${n}/app.bsky.feed.post/${n}`, url: `https://bsky.app/profile/did:plc:author${n}/post/${n}`, did: `did:plc:author${n}`,
    text: "a post", topic: "AI", topicPath: "technology/ai", broad: "Technology", top: [{ path: "technology/ai", name: "AI", p: 0.9 }],
    tone: { informative: 0.7, humorous: 0.3 }, signals: { substance: 0.5, news: 0.25 }, labels: [], score: 1.234,
    indexedAt: new Date(Date.now() - 2 * 3600 * 1000).toISOString(), likes: 12, reposts: 3, replies: 2, quotes: 1, ...over,
  };
};
export const previewBody = (posts = [previewPost()], over = {}) => ({
  state: "personal", posts, tookMs: 3,
  interests: [], // what the draft builds the feed from; tests that look at the shares say so
  ...over,
});

// A post as Bluesky's public API describes it.
export const bskyPost = (p, over = {}) => ({
  uri: p.uri, cid: "bafy", author: { did: p.did, handle: "alice.example", displayName: "Alice Example", avatar: "https://cdn.bsky.app/img/avatar/x.jpg" },
  record: { text: "the words on Bluesky", createdAt: p.indexedAt }, replyCount: 5, repostCount: 6, quoteCount: 1, likeCount: 70, indexedAt: p.indexedAt,
  labels: [], ...over,
});

function define(name, value) {
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
}

// open loads the page with these answers from the server. An answer is {status, body}, an
// Error (the request fails), or a function returning either.
const emptyInterests = interestsBody({ interests: [] });

// A page leaves timers and requests behind when a test ends; they run against whatever page the
// globals belong to by then. So a page waits for the last one to go quiet before it takes them over.
let previous = null;
async function quiet() {
  if (!previous) return;
  for (let i = 0; i < 6; i++) {
    const n = previous.length;
    await new Promise((r) => setTimeout(r, 25));
    if (previous.length === n) return;
  }
}

export async function open({
  search = "",
  api = { status: 401, body: { error: "not signed in" } },
  login,
  logout = { status: 204 },
  interests = { status: 200, body: emptyInterests },
  tuning = { status: 200, body: tuningBody() },
  // saving answers with the tuning it was sent, as the server does
  save = (call) => ({ status: 200, body: { tuning: call.json } }),
  preview = { status: 200, body: previewBody() },
  // what Bluesky's public API answers when asked for posts: none, by default, so posts are shown as we know them
  bsky = { status: 200, body: { posts: [] } },
  abortable = true, // whether abandoning a request makes it fail, as in a browser
} = {}) {
  await quiet();
  const win = new JSDOM(pageHTML, { url: "https://feeds.example.test/me" + search }).window;
  const calls = [];
  previous = calls;
  const assigned = [];
  const reloads = [];
  const fetchStub = async (url, opts = {}) => {
    const call = { url, method: opts.method ?? "GET", headers: opts.headers ?? {}, body: opts.body === undefined ? undefined : String(opts.body), signal: opts.signal };
    if (call.body !== undefined && call.headers["Content-Type"] === "application/json") call.json = JSON.parse(call.body);
    calls.push(call);
    const answer = {
      "GET /api/me": api, "POST /oauth/login": login, "POST /oauth/logout": logout, "GET /api/me/interests": interests,
      "GET /api/me/tuning": tuning, "PUT /api/me/tuning": save, "POST /api/me/preview": preview,
      "GET https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts": bsky,
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
  // A stand-in for window.location that records navigation instead of attempting it. (It must not
  // be a proxy around the real one: jsdom's assign and reload can't be replaced that way.)
  const location = new Proxy({}, {
    get(_, key) {
      if (key === "assign") return (u) => assigned.push(u);
      if (key === "reload") return () => reloads.push(1);
      const v = win.location[key];
      return typeof v === "function" ? v.bind(win.location) : v;
    },
  });
  define("document", win.document);
  define("history", win.history);
  define("location", location);
  define("fetch", fetchStub);
  define("addEventListener", win.addEventListener.bind(win));
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
  const submit = async (handle) => {
    $("handle").value = handle;
    $("login").dispatchEvent(new win.Event("submit", { cancelable: true, bubbles: true }));
  };
  const visible = () => ["loading", "signed-out", "signed-in"].filter((id) => !$(id).hidden);
  return { win, mod, $, calls, assigned, reloads, until, submit, visible };
}

