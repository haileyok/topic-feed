// The post inspector at /inspect: paste a post's address, see the post, how the topic model scored it,
// and which feeds would take it (and, for each one, every rule it checked and what the feed holds
// now). It is the owner's page: /api/inspect answers nobody else.
//
// All text is put on the page with textContent, never as HTML: what the page shows includes what
// other people posted.

import "./header.js"; // the header every page shares
import { $, make, str, num, safeLink } from "./dom.js";
import { el, renderPost, hydrate } from "./posts.js";
import { meter } from "./stats.js";
import { describe } from "./scores.js";
import { ago } from "./draft.js";

// What /api/inspect says went wrong, in words, for the errors that don't carry their own message.
export const PROBLEMS = {
  limited: "That's been asked a lot. Wait a few seconds and try again.",
  unavailable: "Couldn't read that just now. Try again in a moment.",
  network: "Couldn't reach the server. Check your connection and try again.",
};

function show(which) {
  for (const id of ["loading", "signed-out", "forbidden", "inspector"]) $(id).hidden = id !== which;
}

// ---------- time and numbers ----------

/** when is a time as UTC text with how long ago it was. */
export function when(iso) {
  const t = new Date(typeof iso === "string" ? iso : "");
  if (Number.isNaN(t.getTime()) || t.getUTCFullYear() < 2000) return "unknown";
  const text = t.toISOString().slice(0, 16).replace("T", " ") + " UTC";
  const old = ago(iso);
  return old ? `${text} (${old} ago)` : text;
}

// strings is a list of text from the server, or none at all when the server sent null.
const strings = (v) => (Array.isArray(v) ? v.filter((s) => typeof s === "string") : []);
const fixed = (n, d) => num(n).toFixed(d);
const thousands = (n) => num(n).toLocaleString("en-US");
const score = (n) => (Math.abs(n) >= 100 ? fixed(n, 0) : Math.abs(n) >= 1 ? fixed(n, 2) : fixed(n, 3));

// ---------- the answer ----------

async function ask(post, signal) {
  let res;
  try {
    res = await fetch("/api/inspect" + (post === null ? "" : "?post=" + encodeURIComponent(post)), {
      headers: { Accept: "application/json" }, credentials: "same-origin", signal,
    });
  } catch (err) {
    return err && err.name === "AbortError" ? { aborted: true } : { status: 0, data: null };
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    // not JSON: the status says enough
  }
  return { status: res.status, ok: res.ok, data };
}

function problemText(r) {
  const d = r.data || {};
  if (r.status === 0) return PROBLEMS.network;
  if (typeof d.message === "string" && d.message) return d.message;
  if (r.status === 429) return PROBLEMS.limited;
  return PROBLEMS.unavailable;
}

// ---------- the post ----------

function ourCard(resp) {
  const p = resp.post || {};
  const stored = p.stored || {};
  const link = safeLink(resp.url);
  const handle = str(resp.handle) ? "@" + resp.handle : str(resp.did);
  return el("article", { class: "post fallback" },
    el("div", { class: "avatar placeholder", text: (str(resp.handle).charAt(0) || "?").toUpperCase() }),
    el("div", {},
      el("div", { class: "post-head" }, el("span", { class: "post-name", text: handle }), el("span", { class: "post-time", text: "· " + (ago(p.indexedAt) || "?") })),
      el("div", { class: "post-text", text: str(stored.text) || "(we hold no text for this post)" }),
      link ? el("div", { class: "post-foot" }, el("a", { href: link, target: "_blank", rel: "noopener noreferrer", text: "Open on Bluesky" })) : null));
}

async function renderPostCard(resp, signal) {
  const box = $("post-card");
  box.replaceChildren(resp.post ? ourCard(resp) : el("p", { class: "me-fine", text: "We hold nothing of this post to show." }));
  const link = safeLink(resp.url);
  if (!link) return;
  // As Bluesky shows it, once it has answered. If it can't or won't, ours stays.
  try {
    const views = await hydrate([resp.uri], signal);
    const view = views.find((v) => v.uri === resp.uri);
    if (view && view.author && !signal.aborted) box.replaceChildren(renderPost(view, null));
  } catch {
    // ours stays
  }
}

function fact(k, v) {
  return el("li", {}, el("span", { class: "k", text: k }), el("span", { class: "v", text: v }));
}

function renderFacts(resp) {
  const list = $("post-facts");
  list.replaceChildren();
  const p = resp.post;
  if (!p) return;
  const st = p.stored;
  const pl = p.pipeline;
  const rows = [];
  rows.push(["We saw it", when(p.indexedAt)]);
  if (st && st.createdAt && new Date(st.createdAt).getFullYear() > 2000) rows.push(["Its author's date", when(st.createdAt)]);
  if (pl) {
    const labels = strings(pl.labels);
    rows.push(["Scored", pl.model ? `by ${pl.model}, ${when(pl.processedAt)}` : "never (no model ran)"]);
    rows.push(["Label policy", str(pl.feedPolicy) + (labels.length ? ` (labels: ${labels.join(", ")})` : "")]);
    if (pl.picturesWanted > 0) rows.push(["Pictures the model saw", `${num(pl.picturesUsed)} of ${num(pl.picturesWanted)}`]);
  }
  if (st) {
    const langs = strings(st.langs);
    const own = strings(st.selfLabels);
    rows.push(["Language", (langs.length ? langs.join(", ") : "none tagged") + (st.detectedLang ? ` · detected ${st.detectedLang}` : "")]);
    rows.push(["Contains", st.embedType === "none" || !st.embedType ? "text only" : st.embedType]);
    if (own.length) rows.push(["Its author's labels", own.join(", ")]);
  }
  rows.push(["Reactions", `${thousands(p.likes)} likes · ${thousands(p.reposts)} reposts · ${thousands(p.replies)} replies · ${thousands(p.quotes)} quotes`]);
  const now = strings(p.currentLabels);
  if (now.length) rows.push(["Labels on it or its author now", now.join(", ")]);
  if (p.deleted) rows.push(["Deleted", "yes: the author deleted it"]);
  if (p.authorInactive) rows.push(["Author's account", "deactivated, suspended or taken down"]);
  if (p.retry) rows.push(["Picture retries", `${p.retry.status}, ${p.retry.attempts} tried${p.retry.error ? `: ${p.retry.error}` : ""}`]);
  for (const [k, v] of rows) list.append(fact(k, v));
}

// ---------- the scores ----------

function section(title, ...rows) {
  return el("div", { class: "why-sec" }, el("h4", { text: title }), ...rows);
}

function renderScores(resp) {
  const panel = $("scores-panel");
  const p = resp.post;
  panel.hidden = !(p && resp.state === "scored");
  if (panel.hidden) return;
  const topic = (t) => meter(str(t.name) || str(t.path), t.p, { title: str(t.path) });
  const byScore = (m) => Object.entries(m || {}).sort((a, b) => num(b[1]) - num(a[1]) || a[0].localeCompare(b[0]));
  const named = (kind) => ([name, v]) => {
    const m = describe(kind, name);
    return meter([m.icon, m.name].filter(Boolean).join(" "), v, { title: m.hint });
  };
  $("scores").replaceChildren(el("div", { class: "why-grid insp-scores" },
    section("Subtopics", ...(p.subtopics || []).slice(0, 8).map(topic)),
    section("Broad topics", ...(p.broads || []).map(topic)),
    section("Tone", ...byScore(p.tone).map(named("tone"))),
    section("Signals", ...byScore(p.signals).map(named("signal")))));
}

// ---------- the feeds ----------

const MARK = { pass: "✓", fail: "✗", soft: "◔" };

function renderCheck(c) {
  const state = c.soft && !c.pass ? "soft" : c.pass ? "pass" : "fail";
  return el("li", { class: "insp-check", "data-state": state },
    el("span", { class: "insp-mark", "aria-hidden": "true", text: MARK[state] }),
    el("span", { class: "visually-hidden", text: c.pass ? "Passes: " : c.soft ? "Not within: " : "Fails: " }),
    el("span", { class: "insp-check-body" }, el("b", { text: c.name }), el("span", { text: " " + c.detail })));
}

/** checksText is the one line over a feed's list of rules: how many the post fails. */
export function checksText(checks) {
  const fails = checks.filter((c) => !c.pass && !c.soft).length;
  if (fails > 0) return `${fails} of ${checks.length} rules fail`;
  if (checks.some((c) => !c.pass)) return "Meets every rule, but not the feed's window";
  return `Meets all ${checks.length} rules`;
}

export function rankingLine(r) {
  return `Ranking score ${score(r.score)} = (${fixed(r.prior, 2)} prior + ${score(r.engagement)} from reactions) ÷ ${fixed(r.decay, 2)} for its age of ${fixed(r.ageHours, 1)} h`;
}

function liveLine(v) {
  const l = v.live;
  if (v.personal) return str(v.note);
  if (!l) return "";
  if (l.inBuild) return `In the feed right now: #${thousands(l.position)} of ${thousands(l.total)}.`;
  if (v.matches && l.why) return `Not in the feed right now: ${l.why}.`;
  return "";
}

function renderFeed(v) {
  const status = v.personal ? (v.matches ? "Eligible" : "Not eligible") : v.matches ? (v.fresh ? "Takes it" : "Would have") : "Doesn't take it";
  const live = liveLine(v);
  return el("li", { class: "insp-feed", "data-match": v.matches ? "yes" : "no", "data-personal": v.personal ? "yes" : "no" },
    el("div", { class: "insp-feed-head" },
      el("span", { class: "insp-feed-name", text: str(v.name) || str(v.rkey) }),
      el("code", { class: "insp-feed-key", text: str(v.rkey) }),
      el("span", { class: "badge " + (v.matches ? "badge-added" : "badge-muted"), text: status })),
    live ? el("p", { class: "insp-feed-line", text: live }) : null,
    !v.matches && v.reason ? el("p", { class: "insp-feed-line insp-reason", text: str(v.reason) }) : null,
    v.matches && !v.fresh && !v.personal && !live ? el("p", { class: "insp-feed-line", text: "It is older than the feed's window now, but it matched while it was fresh." }) : null,
    v.ranking ? el("p", { class: "insp-feed-rank", text: rankingLine(v.ranking) }) : null,
    el("details", { class: "insp-checks" },
      el("summary", { text: checksText(v.checks) }),
      el("ul", {}, ...v.checks.map(renderCheck))));
}

function group(title, feeds) {
  if (feeds.length === 0) return null;
  return el("div", { class: "insp-group" }, el("h3", { class: "insp-group-title", text: `${title} (${feeds.length})` }), el("ol", { class: "insp-feeds-list" }, ...feeds.map(renderFeed)));
}

function renderFeeds(resp) {
  const panel = $("feeds-panel");
  panel.hidden = resp.feeds.length === 0;
  if (panel.hidden) return;
  const topic = resp.feeds.filter((v) => !v.personal);
  const personal = resp.feeds.filter((v) => v.personal);
  $("feeds-tip").textContent = `Each feed's rules, checked against this post's scores. A feed holds the newest posts that meet its rules, within the last ${num(resp.windowHours)} hours.`;
  $("feed-groups").replaceChildren(
    ...[group("Would take it", topic.filter((v) => v.matches)), group("Wouldn't take it", topic.filter((v) => !v.matches)), group("For you, the personal feed", personal)].filter(Boolean));
}

// ---------- the verdict ----------

export function verdictOf(resp) {
  const s = resp.summary || {};
  if (resp.state !== "scored") {
    const titles = { unscored: "Not scored", unprocessed: "Not processed yet", not_stored: "We don't hold this post" };
    return { tone: "none", title: titles[resp.state] || "Not scored", text: (resp.why && resp.why[0]) || "" };
  }
  const you = resp.feeds.find((v) => v.personal);
  const tail = you && you.matches ? " It can also appear in For you, for viewers whose interests include its topic." : "";
  if (s.inNow > 0) {
    return { tone: "good", title: `In ${s.inNow} of ${s.feeds} topic feeds right now`, text: `It meets the rules of ${s.matching} ${s.matching === 1 ? "feed" : "feeds"}.${tail}` };
  }
  if (s.matching > 0) {
    return { tone: "mid", title: `Meets the rules of ${s.matching} of ${s.feeds} topic feeds, but none holds it now`, text: "Each feed's page says why: usually the post is older than the feeds' window." + tail };
  }
  return { tone: "none", title: `No topic feed takes it (0 of ${s.feeds})`, text: "Each feed's page below says which rule it fails." + tail };
}

function renderVerdict(resp) {
  const v = verdictOf(resp);
  $("verdict").dataset.tone = v.tone;
  $("verdict-title").textContent = v.title;
  $("verdict-text").textContent = v.text;
  const why = $("why");
  why.hidden = !(resp.why && resp.why.length > 1);
  if (!why.hidden) why.replaceChildren(...resp.why.slice(1).map((line) => el("p", { text: line })));
}

// ---------- running ----------

let seq = 0;
let aborter = null;

function showError(text) {
  $("inspect-error").textContent = text || "";
  $("inspect-error").hidden = !text;
}

async function run(input, { push = true } = {}) {
  const post = input.trim();
  showError("");
  if (!post) {
    showError("Paste a link to a post, or its at:// address.");
    return;
  }
  if (aborter) aborter.abort();
  aborter = new AbortController();
  const mine = ++seq;
  const signal = aborter.signal;
  if (push) history.replaceState(null, "", "?post=" + encodeURIComponent(post));
  $("result").hidden = true;
  $("inspect-status").hidden = false;
  $("inspect-button").disabled = true;
  const r = await ask(post, signal);
  if (r.aborted || mine !== seq) return;
  $("inspect-status").hidden = true;
  $("inspect-button").disabled = false;
  if (r.status === 401) {
    location.reload(); // the session ended: start again from the sign-in page
    return;
  }
  if (r.status === 403) {
    show("forbidden");
    return;
  }
  if (!r.ok || !r.data || !Array.isArray(r.data.feeds)) {
    showError(problemText(r));
    return;
  }
  const resp = r.data;
  renderVerdict(resp);
  renderFacts(resp);
  renderScores(resp);
  renderFeeds(resp);
  $("result").hidden = false;
  await renderPostCard(resp, signal);
}

async function whoAmI() {
  const res = await fetch("/api/me", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (res.status === 401 || res.status === 404) return null;
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

export async function main() {
  let me;
  try {
    me = await whoAmI();
  } catch {
    show("inspector");
    showError(PROBLEMS.network);
    return;
  }
  if (!me) {
    show("signed-out");
    return;
  }
  show("inspector");
  $("inspect-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    run($("post").value);
  });
  const initial = new URLSearchParams(location.search).get("post");
  if (initial) {
    $("post").value = initial;
    await run(initial, { push: false });
    return;
  }
  // Without a post to ask about, ask anyway: the owner is told what an address looks like, anyone
  // else is told this page isn't theirs, before they type anything.
  const r = await ask(null);
  if (r.status === 403) show("forbidden");
  $("post").focus();
}

main();
