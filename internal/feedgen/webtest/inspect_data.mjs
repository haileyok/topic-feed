// Made-up answers of /api/inspect, in the shape the service sends (inspect.go), for the tests of the
// page at /inspect and for its browser preview. Nothing here is real data.

const HOUR = 3600e3;
export const hoursAgo = (h) => new Date(Date.now() - h * HOUR).toISOString();

export const check = (name, pass, detail, soft = false) => ({ name, pass, detail, ...(soft ? { soft: true } : {}) });

const topic = (path, name, broad, p) => ({ path, name, broad, p });

export const AI = topic("technology/ai", "AI", "Technology", 0.91);
export const SOFTWARE = topic("technology/software_dev", "Software development", "Technology", 0.22);
export const OTHER = topic("online_culture/other", "Online culture", "Online culture", 0.05);

export function storedPost(over = {}) {
  return {
    cid: "bafyreia", createdAt: hoursAgo(3.1), indexedAt: hoursAgo(3), text: "Researchers released a small model that matches last year's frontier.",
    langs: ["en"], detectedLang: "en", embedType: "none", selfLabels: [], tags: [], mediaKinds: [], ...over,
  };
}

export function pipelineRow(over = {}) {
  return {
    indexedAt: hoursAgo(3), processedAt: hoursAgo(2.99), feedPolicy: "ok", labels: [], model: "microblog-topic-classifier-v5",
    broadProbs: { technology: 0.93 }, pathProbs: { "technology/ai": 0.91 }, signals: {}, tone: {}, topBroad: "technology", topPath: "technology/ai", topPathP: 0.91,
    picturesWanted: 0, picturesUsed: 0, ...over,
  };
}

export function inspectedPost(over = {}) {
  return {
    text: "Researchers released a small model that matches last year's frontier.", topic: "AI", topicPath: "technology/ai", broad: "Technology",
    top: [AI, SOFTWARE, OTHER], tone: { informative: 0.62, humorous: 0.08, personal: 0.2 },
    signals: { substance: 0.55, news: 0.2, promo: 0.04, general_interest: 0.6, meme: 0.1 }, labels: [], indexedAt: hoursAgo(3),
    likes: 1234, reposts: 40, replies: 12, quotes: 3,
    stored: storedPost(), pipeline: pipelineRow(), subtopics: [AI, SOFTWARE, OTHER],
    broads: [topic("technology", "Technology", "Technology", 0.93), topic("online_culture", "Online culture", "Online culture", 0.06)],
    deleted: false, authorInactive: false, currentLabels: [], ...over,
  };
}

const ranking = (over = {}) => ({ prior: 2.15, engagement: 18.4, ageHours: 3, decay: 7.2, score: 2.86, ...over });

const topicChecks = (pass, detail) => [
  check("Scored by the topic model", true, "scored by microblog-topic-classifier-v5"),
  check("Label policy when it was processed", true, "ok"),
  check("Topic", pass, detail),
  check("Not deleted", true, "still there"),
  check("Author's account is active", true, "active"),
  check("Labels now on it or its author", true, "none"),
  check("Within the feed's window", true, "posted 3h0m0s ago, and the feed reaches back 24h0m0s", true),
];

// A topic feed that takes the post and holds it right now.
export const takes = (over = {}) => ({
  rkey: "ai", name: "AI", matches: true, fresh: true, checks: topicChecks(true, "technology/ai at 0.910, and the feed needs at least 0.500"),
  match: 0.91, ranking: ranking(), live: { inBuild: true, position: 14, total: 439, builtAt: hoursAgo(0.01) }, ...over,
});

// A topic feed whose rules the post fails.
export const refuses = (over = {}) => ({
  rkey: "anime", name: "Anime", matches: false, fresh: true,
  reason: "Topic: none of the feed's topics (anime_manga/anime) is among the topics the model scored for this post; it needs at least 0.500",
  checks: topicChecks(false, "none of the feed's topics (anime_manga/anime) is among the topics the model scored for this post; it needs at least 0.500"),
  match: 0, ranking: ranking({ score: 1.1 }), live: { inBuild: false, total: 2018, builtAt: hoursAgo(0.01) }, ...over,
});

// A topic feed whose rules the post meets, but which doesn't hold it (too old, or beyond its size).
export const wouldHave = (over = {}) => ({
  rkey: "tech", name: "Tech", matches: true, fresh: false,
  checks: [...topicChecks(true, "technology/ai at 0.910, and the feed needs at least 0.500").slice(0, -1),
    check("Within the feed's window", false, "posted 30h0m0s ago, but the feed only reaches back 24h0m0s: it matched while it was fresh enough", true)],
  match: 0.91, ranking: ranking(), live: { inBuild: false, total: 3000, builtAt: hoursAgo(0.01), why: "it is older than the 24h0m0s the feed reaches back" }, ...over,
});

export const forYou = (over = {}) => ({
  rkey: "for-you", name: "For you", personal: true, matches: true, fresh: true, checks: topicChecks(true, "technology/ai at 0.910, and the feed needs at least 0.500"),
  match: 0.91, note: "Whether a viewer is shown it depends on their interests and what they have seen.", ...over,
});

// Every answer is about a post of its own: the page remembers what Bluesky said about each address.
let counter = 0;
export function inspectBody(over = {}) {
  const id = "3k" + (++counter).toString(36).padStart(4, "0");
  const feeds = over.feeds ?? [takes(), refuses(), wouldHave(), forYou()];
  const topicFeeds = feeds.filter((f) => !f.personal);
  const summary = { feeds: topicFeeds.length, matching: topicFeeds.filter((f) => f.matches).length, inNow: topicFeeds.filter((f) => f.live && f.live.inBuild).length };
  return {
    uri: `at://did:plc:author1/app.bsky.feed.post/${id}`, url: `https://bsky.app/profile/did:plc:author1/post/${id}`, did: "did:plc:author1",
    handle: "author.example", state: "scored", post: inspectedPost(), feeds, summary, now: new Date().toISOString(), windowHours: 24, ...over,
  };
}

// A post that was never scored: what is known is why.
export const unscoredBody = (state, why, over = {}) => inspectBody({ state, why, post: undefined, feeds: [], ...over });

// A post as Bluesky's public API describes it.
export const bskyView = (resp, over = {}) => ({
  uri: resp.uri, cid: "bafy", author: { did: resp.did, handle: "author.example", displayName: "Author Example" },
  record: { text: "the words on Bluesky", createdAt: hoursAgo(3) }, replyCount: 5, repostCount: 6, quoteCount: 1, likeCount: 70, indexedAt: hoursAgo(3),
  labels: [], ...over,
});
