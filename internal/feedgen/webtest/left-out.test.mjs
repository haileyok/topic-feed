// Tests of the page at /filtered/left-out: the real left-out.html and left-out.js, run in jsdom, with
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
const scriptURL = pathToFileURL(path.join(webDir, "static", "left-out.js")).href;
const pageHTML = fs
  .readFileSync(path.join(webDir, "left-out.html"), "utf8")
  .replace(/<header class="topbar">[\s\S]*?<\/header>/, "")
  .replace(/<script[^>]*><\/script>/, "");

const FEEDS = [
  { rkey: "discover-filter", name: "Discover, Filtered" },
  { rkey: "foryou-filter", name: "For You, Filtered" },
];
const TAX = { topics: [{ id: "us_politics", name: "US politics", subtopics: [] }, { id: "technology", name: "Technology", subtopics: [] }] };

function define(name, value) {
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
}

const at = new Date(Date.now() - 3600 * 1000).toISOString();
const uri = (n) => `at://did:plc:a/app.bsky.feed.post/p${n}`;
const leftOutPost = (n) => ({ uri: uri(n), at, reason: { kind: "topic", name: "us_politics", value: 0.82, cutoff: 0.5, bound: "max" } });
const view = (n) => ({ uri: uri(n), cid: "c", author: { did: "did:plc:a", handle: "a.test", displayName: "A" },
  record: { text: `post number ${n}`, createdAt: at }, indexedAt: at, labels: [] });

let loads = 0;
async function open({ search = "?feed=discover-filter", list = { status: 200, body: { did: "did:plc:alice", connected: true, feeds: FEEDS } },
  pages = {}, hiddenPosts = new Set() } = {}) {
  const win = new JSDOM(pageHTML, { url: "https://feeds.example.test/filtered/left-out" + search }).window;
  const calls = [];
  define("document", win.document);
  define("history", win.history);
  define("location", win.location);
  define("addEventListener", win.addEventListener.bind(win));
  define("IntersectionObserver", undefined); // jsdom has none: the button loads more
  define("fetch", async (url, opts = {}) => {
    calls.push(url);
    const reply = (status, body) => ({ ok: status >= 200 && status < 300, status, json: async () => body });
    const u = new URL(url, "https://feeds.example.test");
    if (u.pathname === "/api/me/filtered") return reply(list.status, list.body);
    if (u.pathname === "/api/taxonomy") return reply(200, TAX);
    const m = /^\/api\/me\/filtered\/([^/]+)\/left-out$/.exec(u.pathname);
    if (m) {
      const page = pages[`${m[1]}|${u.searchParams.get("cursor") || ""}`];
      return page ? reply(200, page) : reply(500, {});
    }
    if (u.origin === "https://public.api.bsky.app") {
      const posts = u.searchParams.getAll("uris").filter((x) => !hiddenPosts.has(x)).map((x) => view(Number(x.split("/p").pop())));
      return reply(200, { posts });
    }
    throw new Error(`unexpected request: ${url}`);
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

test("pages through everything that was left out, more at a time", async () => {
  const first = Array.from({ length: 50 }, (_, i) => leftOutPost(i));
  const second = Array.from({ length: 20 }, (_, i) => leftOutPost(50 + i));
  const p = await open({ pages: {
    "discover-filter|": { posts: first, cursor: "c1" },
    "discover-filter|c1": { posts: second, cursor: "" },
  } });
  await p.until(() => p.$("posts").children.length === 50, "the first page");
  assert.equal(p.$("title").textContent, "Left out of Discover, Filtered");
  assert.equal(p.$("more-row").hidden, false);
  assert.match(p.$("posts").children[0].textContent, /post number 0/);
  assert.match(p.$("posts").children[0].textContent, /About US politics: the model is 82% sure, and you leave it out above 50%/);
  p.$("more").click();
  await p.until(() => p.$("posts").children.length === 70, "the second page");
  assert.equal(p.$("more-row").hidden, true, "nothing more to load");
  assert.equal(p.$("status").textContent, "70 posts left out in the last week.");
  assert.ok(p.calls.some((c) => c.includes("cursor=c1")));
});

test("a post the site doesn't show gets its reason and a link", async () => {
  const p = await open({
    // Numbers no other test uses: posts.js remembers what Bluesky said about a post.
    pages: { "discover-filter|": { posts: [leftOutPost(901), leftOutPost(902)], cursor: "" } },
    hiddenPosts: new Set([uri(902)]),
  });
  await p.until(() => p.$("posts").children.length === 2, "the posts");
  const second = p.$("posts").children[1];
  assert.match(second.textContent, /Not shown here/);
  assert.match(second.textContent, /About US politics/);
  assert.equal(second.querySelector("a").getAttribute("href"), "https://bsky.app/profile/did:plc:a/post/p902");
});

test("switching feeds, and nothing left out", async () => {
  const p = await open({ search: "?feed=foryou-filter", pages: { "foryou-filter|": { posts: [], cursor: "" } } });
  await p.until(() => p.$("status").textContent.startsWith("Nothing"), "the empty list");
  const tabs = [...p.$("feed-tabs").querySelectorAll("a")];
  assert.deepEqual(tabs.map((a) => a.getAttribute("href")), ["/filtered/left-out?feed=discover-filter", "/filtered/left-out?feed=foryou-filter"]);
  assert.equal(tabs[1].getAttribute("aria-current"), "page");
});

test("signed out, it says where to sign in", async () => {
  const p = await open({ list: { status: 401, body: {} } });
  await p.until(() => !p.$("notice").hidden, "the notice");
  assert.match(p.$("notice").textContent, /Sign in on the filtered feeds page/);
});

test("reasons in words", async () => {
  const p = await open({ pages: { "discover-filter|": { posts: [], cursor: "" } } });
  const t = (r) => p.mod.reasonText(r, (x) => ({ technology: "Technology" })[x] || x);
  assert.equal(t({ kind: "signal", name: "critical", value: 0.74, cutoff: 0.6, bound: "max", rule: "technology" }),
    "Critical 74%, above your maximum of 60% (your rule for Technology)");
  assert.equal(t({ kind: "tone", name: "informative", value: 0.1, cutoff: 0.2, bound: "min" }), "Informative 10%, below your minimum of 20%");
  assert.match(t({ kind: "unscored" }), /couldn't judge it/);
});
