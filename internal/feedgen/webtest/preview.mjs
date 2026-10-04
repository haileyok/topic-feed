// A stand-in for the feed service, to look at the page at /me in a real browser without signing in
// or touching any data: the real me.html and scripts, served with the real security policy, and
// made-up answers from /api/me, /api/me/interests, /api/me/tuning and /api/me/preview.
//
//   TOPICFEED_JSDOM=/path/to/node_modules/jsdom node webtest/preview.mjs        (port 8760, or PORT=...)
//
// The page's own settings are kept in memory while this runs. Ways to see the other states, in the
// address: /me?signedout, /me?slow (every answer takes 1.5 s), /me?empty (the preview finds
// nothing), /me?broken (the preview fails), /me?few (too few likes for interests of your own).
// It listens on every interface, so a phone or another machine on the network can open it.
//
// This is a development aid, not part of the service, and it is not served by it.

import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { interestsBody, interest, tuningBody, previewPost, previewBody, TOPICS } from "./harness.mjs";
import { inspectBody, inspectedPost, storedPost, pipelineRow, takes, refuses, wouldHave, forYou, check, unscoredBody, hoursAgo } from "./inspect_data.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.join(here, "..", "web");
const PORT = Number(process.env.PORT || 8760);

// The same policy the service sends (web.go).
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; " +
  "img-src 'self' data: https://cdn.bsky.app https://video.bsky.app https://video.cdn.bsky.app; " +
  "connect-src 'self' https://public.api.bsky.app; frame-ancestors 'none'; base-uri 'none'; form-action 'none'";

// The policy of the page at /feeds (web.go, feedsContentSecurityPolicy): it may connect anywhere over https.
const FEEDS_CSP = "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data: blob: https:; " +
  "connect-src 'self' https:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'";

const TYPES = { ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png", ".html": "text/html; charset=utf-8" };

// ---------- made-up data ----------

const MORE_TOPICS = [
  ["animals_nature/dogs", "Dogs", "Animals and nature"], ["art/fan_art", "Fan art", "Art"], ["art/illustration", "Illustration", "Art"],
  ["food/cooking", "Cooking", "Food"], ["gaming/video_games", "Video games", "Gaming"], ["gaming/tabletop", "Tabletop games", "Gaming"],
  ["humor/shitposts", "Shitposts", "Humor"], ["humor/jokes_puns", "Jokes and puns", "Humor"], ["music/new_releases", "New releases", "Music"],
  ["science/space", "Space", "Science"], ["science/biology", "Biology", "Science"], ["sports/basketball", "Basketball", "Sports"],
  ["technology/software_dev", "Software development", "Technology"], ["technology/cybersecurity", "Cybersecurity", "Technology"],
  ["us_politics/elections", "Elections", "US politics"], ["world_news/europe", "Europe", "World news"],
].map(([path, name, broad]) => ({ path, name, broad }));
const OFFERED = [...TOPICS, ...MORE_TOPICS].sort((a, b) => a.broad.localeCompare(b.broad) || a.name.localeCompare(b.name));

const LIKED = [
  ["technology/ai", "AI", "Technology", 0.29, 86, ["Anthropic published new interpretability work on how models represent concepts internally", "hot take: most 'AI agents' are a cron job with a nicer logo", "Open-weight models just closed the gap on code benchmarks again"]],
  ["technology/software_dev", "Software development", "Technology", 0.1, 31, ["TIL you can bisect a flaky test by running it under `rr` until it fails once", "the best code review comment is a failing test"]],
  ["humor/shitposts", "Shitposts", "Humor", 0.095, 28, ["me: I'll sleep early tonight. also me at 3am: reading about the history of the paper clip"]],
  ["animals_nature/cats", "Cats", "Animals and nature", 0.093, 27, ["this cat has opinions about the Wi-Fi router and none of them are good"]],
  ["online_culture/other", "Online culture", "Online culture", 0.058, 17, ["remember when every website had a guestbook"]],
  ["sports/baseball", "Baseball", "Sports", 0.05, 15, ["Walk-off in the bottom of the 11th. Absolutely unreal ending."]],
  ["technology/cybersecurity", "Cybersecurity", "Technology", 0.042, 12, ["New writeup: a supply-chain compromise via a typosquatted package, with IOCs"]],
  ["technology/consumer_tech", "Consumer tech", "Technology", 0.034, 10, ["The new foldable finally has a hinge that doesn't feel like a gamble"]],
  ["us_politics/trump_administration", "Trump administration", "US politics", 0.028, 8, ["Agency announces another reorganisation; staff learn about it from the press"]],
  ["online_culture/bluesky_meta", "Bluesky meta", "Online culture", 0.026, 8, ["Feeds are the best thing about this site and nobody can convince me otherwise"]],
  ["humor/funny_stories", "Funny stories", "Humor", 0.026, 8, ["My toddler just explained the plot of a movie she has never seen, with total confidence"]],
  ["society/social_commentary", "Social commentary", "Society", 0.023, 7, ["We keep building systems that reward the loudest voice in the room"]],
  ["work_education/work_life", "Work life", "Work and education", 0.022, 6, ["Calendar invites with no agenda should be a punishable offence"]],
  ["gaming/video_games", "Video games", "Gaming", 0.021, 6, ["Finally beat the boss after 40 tries. I'm shaking."]],
];

const POSTS = [
  ["AI", "technology/ai", "Technology", "Researchers released a small model that matches last year's frontier on reasoning benchmarks while running on a laptop. Weights and training recipe are public."],
  ["Cats", "animals_nature/cats", "Animals and nature", "Update: the cat has learned to open the cupboard where the treats live. We are no longer in control of this house."],
  ["Software development", "technology/software_dev", "Technology", "Hot take: the migration you're afraid of is smaller than the one you're already paying for every sprint by not doing it."],
  ["Baseball", "sports/baseball", "Sports", "Three homers in one game and a standing ovation for a rookie who was in the minors on Monday. This sport."],
  ["Shitposts", "humor/shitposts", "Humor", "the duck knows. the duck has always known."],
  ["Cybersecurity", "technology/cybersecurity", "Technology", "Patch your edge devices. Again. A new actively exploited vulnerability, details and detection rules in the thread below."],
  ["Online culture", "online_culture/other", "Online culture", "Every few years the internet rediscovers a 2009 meme and treats it like a fresh discovery, and honestly, good for it."],
  ["Video games", "gaming/video_games", "Gaming", "Spent four hours on a single puzzle in a game I'm not even enjoying. Sunk cost is a hell of a drug."],
  ["Funny stories", "humor/funny_stories", "Humor", "Overheard on the train: \"I don't need a map, I have a sense of direction.\" They were going the wrong way. For forty minutes."],
  ["Consumer tech", "technology/consumer_tech", "Technology", "Reviewed the new earbuds for a week. Battery is great, case is a fingerprint magnet, ANC is the best I've tried."],
  ["Bluesky meta", "online_culture/bluesky_meta", "Online culture", "Custom feeds are the quiet revolution here. Mine is just posts I'd have liked, ranked by how much people reacted."],
  ["Social commentary", "society/social_commentary", "Society", "A society is mostly a set of small agreements about who gets to be annoyed at whom, and when."],
];
const TONES = [{ informative: 0.6, humorous: 0.1, personal: 0.2, outraged: 0.02, supportive: 0.05, other: 0.03 }, { informative: 0.1, humorous: 0.7, personal: 0.1, outraged: 0.02, supportive: 0.05, other: 0.03 }, { informative: 0.2, humorous: 0.1, personal: 0.55, outraged: 0.02, supportive: 0.1, other: 0.03 }];

function state(search) {
  const q = new URLSearchParams(search);
  return { signedOut: q.has("signedout"), slow: q.has("slow"), empty: q.has("empty"), broken: q.has("broken"), few: q.has("few"), forbidden: q.has("forbidden"),
    many: q.has("many"), long: q.has("long") };
}

// ---------- the page at /feeds and the builder's saving: made-up feeds, kept in memory ----------

const savedFeeds = new Map(); // rkey -> feed, as saved or changed through the API
const deletedFeeds = new Set();
const feedOf = (rkey, name, over = {}) => ({
  rkey, uri: `at://did:plc:alice/app.bsky.feed.generator/${rkey}`, createdAt: "2026-10-04T10:00:00Z", updatedAt: "2026-10-04T10:00:00Z",
  spec: { display_name: name, description: `${name}, picked by a topic classifier.`, paths: ["animals_nature/cats"], min_prob: 0.6, accepts_interactions: true,
    tone: {}, signals: {}, ranking: { weights: { like: 1, repost: 2, reply: 2, quote: 3 }, gravity: 1.8, fresh_every: 4, author_gap: 10, promo_penalty: 1 }, ...over },
});
function feedsFor(st) {
  if (st.empty) return []; // an account with no feeds at all, whatever was saved while looking
  let base = [];
  {
    base = [feedOf("cats", "Cats"), feedOf("news", "Calm news", { paths: ["world_news"] }), feedOf("tech", "Technology", { paths: ["technology/ai", "technology/software_dev", "technology/consumer_tech", "technology/cybersecurity", "technology/other"] })];
    if (st.many) base.push(feedOf("food", "Food"), feedOf("music", "Music"));
    if (st.long) base[0] = feedOf("a-long-key-name", "A feed with a really quite long name", { description: "Supercalifragilisticexpialidocious".repeat(10) });
  }
  const out = base.filter((f) => !deletedFeeds.has(f.rkey)).map((f) => savedFeeds.get(f.rkey) || f);
  for (const [k, f] of savedFeeds) if (!out.some((x) => x.rkey === k) && !deletedFeeds.has(k)) out.push(f);
  return out;
}
const SERVICE_DID = "did:web:feeds.example.test";
const BROWSER_SCOPE = "atproto repo:app.bsky.feed.generator";
const mineAnswer = (st) => ({
  did: "did:plc:alice", owner: false, serviceDid: SERVICE_DID, scope: BROWSER_SCOPE, feeds: feedsFor(st),
  limits: { maxFeeds: 5, maxPaths: 40, maxExclude: 60, maxPosts: 3000, maxName: 24, maxDescription: 300 },
});
// What the builder asks of the taxonomy (a few topics are enough to look at the page).
const TAXONOMY = {
  version: "stub", tones: ["informative", "humorous"], signals: ["substance"], per_hour: {}, adult_allowed: false,
  default_ranking: { weights: { like: 1, repost: 2, reply: 2, quote: 3 }, gravity: 1.8, fresh_every: 4, author_gap: 10, promo_penalty: 1 },
  topics: [
    { id: "animals_nature", name: "Animals and nature", description: "", subtopics: [{ id: "animals_nature/cats", name: "Cats", description: "" }, { id: "animals_nature/dogs", name: "Dogs", description: "" }] },
    { id: "technology", name: "Technology", description: "", subtopics: [{ id: "technology/ai", name: "AI", description: "" }] },
    { id: "world_news", name: "World news", description: "", subtopics: [] },
  ],
};

// What /api/inspect answers, by what the address asked about (the preview has no database): a word
// in it picks the kind of answer. Nothing here is real data.
//   unscored, unprocessed, notstored   a post that wasn't scored, and why
//   none, mid                          a scored post that no feed holds / that only matches
//   evil, long                         text that is hostile / far too long for its box
//   bad, limited, broken               an answer that is an error
function inspectAnswer(post) {
  const q = post.toLowerCase();
  const rows = (n) => Array.from({ length: n }, (_, i) => refuses({
    rkey: `feed-${i}`, name: ["Anime", "Art", "Baseball", "Cats", "Food", "Gaming", "Music", "Politics", "Science", "Space", "Sports", "Travel"][i % 12],
    reason: "Topic: none of the feed's topics (a/b) is among the topics the model scored for this post; it needs at least 0.500",
  }));
  if (q.includes("unscored")) return unscoredBody("unscored", ["The pipeline processed this post but the topic model never scored it, so no feed can take it.", "The label policy marks it \"drop\" (labels: porn): posts like that are never classified or shown."]);
  if (q.includes("unprocessed")) return unscoredBody("unprocessed", ["We stored this post, but the pipeline has no record of it.", "Either it was posted before the classifier pipeline began running, or it is brand new: new posts are scored about a minute after they arrive."]);
  if (q.includes("notstored")) return unscoredBody("not_stored", ["This is a reply. Replies aren't stored or classified: the topic model is built for standalone posts, so no feed can take one."]);
  const many = [
    takes(), takes({ rkey: "tech", name: "Technology", live: { inBuild: true, position: 212, total: 2841, builtAt: hoursAgo(0.01) } }),
    wouldHave({ rkey: "science", name: "Science" }), ...rows(10), forYou(),
  ];
  if (q.includes("none")) return inspectBody({ feeds: [...rows(11), forYou({ matches: false, note: "It isn't in the pool For you picks from, so no viewer is shown it." })] });
  if (q.includes("mid")) return inspectBody({ feeds: [wouldHave(), wouldHave({ rkey: "science", name: "Science" }), ...rows(8)] });
  if (q.includes("evil")) {
    const evil = '<img src=x onerror="alert(1)"> <b>bold</b> javascript:alert(2)';
    return inspectBody({ handle: evil, post: inspectedPost({ text: evil, stored: storedPost({ text: evil }) }), feeds: [refuses({ name: evil, reason: evil }), takes({ name: "Safe" })] });
  }
  if (q.includes("long")) {
    const word = "Supercalifragilisticexpialidocious".repeat(8);
    const text = ("A very long post that goes on and on. " + word + " ").repeat(8);
    return inspectBody({
      handle: "a-really-quite-long-handle-that-keeps-going.bsky.social", post: inspectedPost({ text, stored: storedPost({ text, langs: ["en", "ja", "de", "fr", "pt", "es"], selfLabels: ["porn", "sexual", "nudity", "graphic-media"] }), currentLabels: ["spam", "rude", "intolerant"] }),
      feeds: [takes({ name: "A feed with a really quite long name that keeps going", rkey: "a-really-quite-long-rkey-that-keeps-going-and-going" }), refuses({ reason: "Topic: " + word }), forYou()],
    });
  }
  if (q.includes("pictures")) {
    return inspectBody({ post: inspectedPost({
      stored: storedPost({ embedType: "images", mediaKinds: ["image", "image"] }), pipeline: pipelineRow({ picturesWanted: 3, picturesUsed: 2, labels: ["spam"] }),
      retry: { status: "pending", attempts: 3, error: "context deadline exceeded" }, currentLabels: ["spam"], deleted: true, authorInactive: true,
    }), feeds: [takes({ checks: [check("Not deleted", false, "the author deleted it"), check("Author's account is active", false, "deactivated, suspended or taken down"), check("Labels now on it or its author", false, "spam: the label policy doesn't show it"), check("Topic", true, "technology/ai at 0.910")], matches: false, reason: "Not deleted: the author deleted it", live: { inBuild: false, total: 400, builtAt: hoursAgo(0.01) } })] });
  }
  return inspectBody({ feeds: many });
}

let saved = {};
const delay = (ms) => new Promise((r) => setTimeout(r, ms));
const send = (res, status, body) => {
  res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  res.end(body === undefined ? "" : JSON.stringify(body));
};
const readBody = (req) => new Promise((resolve) => {
  let s = "";
  req.on("data", (c) => (s += c));
  req.on("end", () => { try { resolve(JSON.parse(s || "{}")); } catch { resolve({}); } });
});

// The shares the draft would give each interest: your likes' shares, times the weight, normalised.
function sharesFor(tuning) {
  const w = tuning.topics || {};
  const rows = LIKED.map(([p, , , share]) => [p, share * (Object.hasOwn(w, p) ? w[p] : 1)]);
  for (const [p, wt] of Object.entries(w)) if (!LIKED.some((l) => l[0] === p) && wt > 0) rows.push([p, 0.04 * wt]);
  const total = rows.reduce((a, [, v]) => a + v, 0) || 1;
  return rows.filter(([, v]) => v > 0).map(([p, v]) => [p, v / total]).sort((a, b) => b[1] - a[1]).slice(0, 20);
}

function interestsAnswer(st) {
  const tuned = new Map(sharesFor(saved));
  const rows = LIKED.map(([path, name, broad, share, posts, samples]) => interest({
    path, name, broad, share, weight: Object.hasOwn(saved.topics || {}, path) ? saved.topics[path] : 1, tunedShare: tuned.get(path) ?? 0, added: false, posts,
    samples: samples.map((text, i) => ({ text, url: `https://bsky.app/profile/someone.example/post/${i}x${path.length}`, likedAt: new Date(Date.now() - (i + 1) * 36 * 3600e3).toISOString() })),
  })).filter((r) => r.tunedShare > 0 || r.weight === 0);
  return interestsBody({
    handle: "alice.example", personalized: !st.few, state: st.few ? "generic" : "personal",
    coverage: { total: 612, classified: 446, unclassified: 41, repliesOrOther: 97, unseen: 28 },
    interests: st.few ? [] : rows, otherPosts: 38, unclearPosts: 12,
  });
}

function previewAnswer(draft, st) {
  const shares = sharesFor(draft);
  const posts = [];
  for (let i = 0; i < 30; i++) {
    const [name, path, broad, text] = POSTS[i % POSTS.length];
    posts.push(previewPost({
      topic: name, topicPath: path, broad, text: i < POSTS.length ? text : `${text} (${i})`,
      top: [{ path, name, p: 0.93 - (i % 5) * 0.07 }, { path: "online_culture/other", name: "Online culture", p: 0.06 }],
      tone: TONES[i % TONES.length], likes: 240 - i * 7, reposts: 40 - i, replies: 12 + (i % 4), quotes: 3 + (i % 3), score: 9.4 - i * 0.21,
      indexedAt: new Date(Date.now() - (12 + i * 17) * 60e3).toISOString(),
      signals: { substance: 0.55, news: 0.2, promo: 0.04, general_interest: 0.6, sentiment: 0.5, critical: 0.18, ad: 0.01, engagement_bait: 0.02, spam: 0.01, self_promo: 0.03, meme: 0.1 },
    }));
  }
  const names = new Map([...LIKED.map((l) => [l[0], [l[1], l[2]]]), ...OFFERED.map((t) => [t.path, [t.name, t.broad]])]);
  return previewBody(st.empty ? [] : posts, {
    interests: shares.map(([p, share]) => ({ path: p, name: (names.get(p) || [p])[0], broad: (names.get(p) || [, ""])[1], share })),
  });
}

http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  const st = state(url.search);
  const refer = (() => { try { return new URL(req.headers.referer || "").search; } catch { return ""; } })();
  const apiState = state(refer || url.search);
  const p = url.pathname;

  if (p === "/" || p === "/feeds") {
    const raw = fs.readFileSync(path.join(webDir, p === "/" ? "index.html" : "feeds.html"), "utf8");
    const html = raw.replaceAll("__V__", "dev").replaceAll("__ORIGIN__", `http://${req.headers.host}`);
    res.writeHead(200, { "Content-Type": TYPES[".html"], "Content-Security-Policy": p === "/" ? CSP : FEEDS_CSP, "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" });
    res.end(html);
    return;
  }
  if (p === "/oauth/browser-client-metadata.json") {
    const origin = `http://${req.headers.host}`;
    return send(res, 200, {
      client_id: origin + "/oauth/browser-client-metadata.json", client_name: "Feeds at " + req.headers.host, client_uri: origin, redirect_uris: [origin + "/feeds"],
      scope: BROWSER_SCOPE, grant_types: ["authorization_code", "refresh_token"], response_types: ["code"], token_endpoint_auth_method: "none",
      application_type: "web", dpop_bound_access_tokens: true,
    });
  }
  if (p === "/xrpc/com.atproto.identity.resolveHandle") {
    const h = url.searchParams.get("handle");
    return h === "alice.example" ? send(res, 200, { did: "did:plc:alice" }) : send(res, 400, { error: "InvalidRequest", message: "Unable to resolve handle" });
  }
  if (p === "/me") {
    const html = fs.readFileSync(path.join(webDir, "me.html"), "utf8").replaceAll("__V__", "dev");
    res.writeHead(200, { "Content-Type": TYPES[".html"], "Content-Security-Policy": CSP, "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" });
    res.end(html);
    return;
  }
  if (p === "/inspect") {
    const html = fs.readFileSync(path.join(webDir, "inspect.html"), "utf8").replaceAll("__V__", "dev");
    res.writeHead(200, { "Content-Type": TYPES[".html"], "Content-Security-Policy": CSP, "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" });
    res.end(html);
    return;
  }
  const file = p.startsWith("/static/v/dev/") ? p.slice("/static/v/dev/".length) : p.startsWith("/static/") ? p.slice("/static/".length) : null;
  if (file !== null && !file.includes("..")) {
    const full = path.join(webDir, "static", file);
    if (fs.existsSync(full) && fs.statSync(full).isFile()) {
      res.writeHead(200, { "Content-Type": TYPES[path.extname(full)] || "application/octet-stream", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff" });
      res.end(fs.readFileSync(full));
      return;
    }
  }
  if (p.startsWith("/api/")) {
    if (apiState.slow) await delay(1500);
    if (p === "/api/me") return apiState.signedOut ? send(res, 401, { error: "not signed in" }) : send(res, 200, { did: "did:plc:alice", handle: "alice.example" });
    if (p === "/api/taxonomy") return send(res, 200, TAXONOMY);
    if (p === "/api/feeds") return send(res, 200, []);
    if (p === "/api/preview" && req.method === "POST") { await readBody(req); return send(res, 200, { posts: [], total: 0, removed: {}, took_ms: 1 }); }
    if (p === "/api/me/feeds" && req.method === "GET") {
      if (apiState.signedOut) return send(res, 401, { error: "not signed in" });
      return apiState.broken ? send(res, 503, { error: "unavailable" }) : send(res, 200, mineAnswer(apiState));
    }
    if (p.startsWith("/api/me/feeds/") && ["PUT", "DELETE"].includes(req.method)) {
      if (apiState.signedOut) return send(res, 401, { error: "not signed in" });
      const rkey = decodeURIComponent(p.slice("/api/me/feeds/".length));
      if (req.method === "DELETE") { deletedFeeds.add(rkey); savedFeeds.delete(rkey); return send(res, 200, { deleted: rkey }); }
      const spec = await readBody(req);
      if (!spec.display_name) return send(res, 400, { error: "invalid", message: `feed "${rkey}": display_name must be 1-24 characters` });
      if (!Array.isArray(spec.paths) || spec.paths.length === 0) return send(res, 400, { error: "invalid", message: `feed "${rkey}": no paths` });
      const have = feedsFor(apiState);
      const exists = have.some((f) => f.rkey === rkey);
      if (!exists && have.length >= 5) return send(res, 409, { error: "limit", message: "You can have at most 5 feeds. Delete one to make another." });
      deletedFeeds.delete(rkey);
      const feed = feedOf(rkey, spec.display_name);
      feed.spec = spec;
      savedFeeds.set(rkey, feed);
      return send(res, exists ? 200 : 201, { feed, created: !exists });
    }
    if (p === "/api/inspect") {
      if (apiState.signedOut) return send(res, 401, { error: "not signed in" });
      if (apiState.forbidden) return send(res, 403, { error: "forbidden" });
      const post = url.searchParams.get("post");
      if (post === null || post.trim() === "") return send(res, 400, { error: "invalid", message: "Paste a link to a post, or its at:// address." });
      const q = post.toLowerCase();
      if (q.includes("limited")) return send(res, 429, { error: "limited" });
      if (q.includes("broken")) return send(res, 503, { error: "unavailable" });
      if (q.includes("bad")) return send(res, 400, { error: "invalid", message: "That isn't a post address." });
      return send(res, 200, inspectAnswer(post));
    }
    if (p === "/api/me/interests") return send(res, 200, interestsAnswer(apiState));
    if (p === "/api/me/tuning" && req.method === "GET") return send(res, 200, tuningBody({ tuning: saved, topics: OFFERED }));
    if (p === "/api/me/tuning" && req.method === "PUT") { saved = await readBody(req); return send(res, 200, { tuning: saved }); }
    if (p === "/api/me/preview" && req.method === "POST") {
      const draft = await readBody(req);
      if (apiState.broken) return send(res, 503, { error: "unavailable" });
      return send(res, 200, previewAnswer(draft, apiState));
    }
  }
  if (p === "/oauth/logout") return send(res, 204);
  res.writeHead(404);
  res.end("not found");
}).listen(PORT, "0.0.0.0", () => console.log(`preview of /me on http://0.0.0.0:${PORT}/me`));
