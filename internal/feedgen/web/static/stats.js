// A post in the preview on /me: the post as Bluesky shows it (its author, words, pictures and
// counts, from Bluesky's public API) with what the feed knows about it underneath: why it was
// picked, how the model scored it, and what the ranking counted. If Bluesky can't be asked, the
// post is shown from what we have. Everything goes in as text, never as HTML.

import { el, renderPost, compact } from "./posts.js";
import { describe, rangeText, dialActive } from "./scores.js";
import { ago } from "./draft.js";
import { str, num, safeLink, postLink } from "./dom.js";

const pct = (v) => Math.round(Math.min(1, Math.max(0, num(v))) * 100);

// dialOf is the cutoffs and boost a set of rules (as sent to the server) holds for a score.
function dialOf(rules, name) {
  const r = rules || {};
  return { min: num((r.min || {})[name]), max: (r.max || {})[name] === undefined ? 1 : num(r.max[name]), w: num((r.weights || {})[name]) };
}

const ruleNote = (d) => [d.min > 0 || d.max < 1 ? `allowed ${rangeText(d)}` : "", d.w ? `boost ${d.w > 0 ? "+" : ""}${d.w}` : ""].filter(Boolean).join(", ");

// A bar for one score; "ruled" marks a score the current settings put a cutoff or a boost on.
export function meter(label, v, { title = "", ruled = false, note = "" } = {}) {
  return el("div", { class: "meter" + (ruled ? " ruled" : ""), title: title || label },
    el("span", { class: "m-label", text: label }),
    el("span", { class: "m-bar" }, el("span", { class: "m-fill", style: `width:${pct(v)}%` })),
    el("span", { class: "m-val", text: `${pct(v)}%` }),
    note ? el("span", { class: "m-note", text: note }) : null);
}

const kv = (k, v, title = "") => el("div", { class: "kv", title }, el("span", { text: k }), el("b", { text: v }));
const plural = (n, one, many) => `${compact(n)} ${n === 1 ? one : many}`;

/**
 * statsPanel is what the feed knows about a post of the preview, which sits slot number `slot`
 * in the feed. ctx says what the settings are: freshEvery (which slots are brand-new posts),
 * tone and signals (the cutoffs and boosts, as sent to the server), and whether the scores are
 * open.
 */
export function statsPanel(p, slot, ctx) {
  const top = Array.isArray(p.top) ? p.top : [];
  const tone = Object.entries(p.tone && typeof p.tone === "object" ? p.tone : {}).sort((a, b) => num(b[1]) - num(a[1]));
  const signals = p.signals && typeof p.signals === "object" ? p.signals : {};
  const fresh = ctx.freshEvery > 0 && slot % ctx.freshEvery === 0;
  const topTone = tone[0] ? describe("tone", tone[0][0]) : null;

  const topics = el("div", { class: "why-sec" }, el("h4", { text: "Topics" }),
    ...top.map((t) => meter(str(t.name) || str(t.path), t.p, { title: str(t.path) })));

  const used = (kind, rules, name) => {
    const d = dialOf(rules, name);
    return { ruled: dialActive(d), note: dialActive(d) ? ruleNote(d) : "" };
  };
  const signalRows = Object.keys(signals).map((name) => {
    const m = describe("signal", name);
    return meter(`${m.icon} ${m.name}`, signals[name], { title: `${m.name}: ${m.hint}`, ...used("signal", ctx.signals, name) });
  });
  const toneRows = tone.map(([name, v]) => {
    const m = describe("tone", name);
    return meter(`${m.icon} ${m.name}`, v, { title: m.hint, ...used("tone", ctx.tone, name) });
  });
  const labels = Array.isArray(p.labels) ? p.labels.filter((l) => typeof l === "string") : [];

  const counted = el("div", { class: "why-sec" }, el("h4", { text: "Ranking" }),
    kv("Score", num(p.score).toFixed(3), "What the post ranks by: engagement and a quality prior, over its age"),
    kv("Slot", `#${slot}${fresh ? " (fresh slot)" : ""}`),
    kv("Age", ago(p.indexedAt) || "?"),
    kv("Likes · reposts", `${compact(num(p.likes))} · ${compact(num(p.reposts))}`, "Counted in the ranking"),
    kv("Replies · quotes", `${compact(num(p.replies))} · ${compact(num(p.quotes))}`, "Counted in the ranking"),
    labels.length > 0 ? el("div", { class: "labels" }, ...labels.map((l) => el("span", { class: "label", text: l }))) : null);

  const grid = el("div", { class: "why-grid" }, topics,
    el("div", { class: "why-sec" }, el("h4", { text: "Signals" }), ...signalRows),
    el("div", { class: "why-sec" }, el("h4", { text: "Tone" }), ...toneRows), counted);

  const chips = el("div", { class: "why-chips" },
    el("span", { class: "topic", title: "The topic this post was picked for", text: `${str(p.topic)} ${pct(top[0] && top[0].p)}%` }),
    topTone ? el("span", { title: "Tone", text: `${topTone.icon} ${topTone.name} ${pct(tone[0][1])}%` }) : null,
    el("span", { title: "Ranking score", text: `score ${num(p.score).toFixed(2)}` }),
    el("span", { title: "Slot in the feed", text: `#${slot}` }),
    fresh ? el("span", { class: "fresh", title: "A slot for the newest post", text: "fresh slot" }) : null,
    el("span", { title: "What the ranking counted", text: [plural(num(p.likes), "like", "likes"), plural(num(p.reposts), "repost", "reposts"),
      plural(num(p.replies), "reply", "replies"), plural(num(p.quotes), "quote", "quotes")].join(" · ") }));

  // Clicks inside the panel don't open the post, so it can be read and selected.
  return el("div", { class: "why", onclick: (e) => e.stopPropagation() }, chips,
    el("details", { class: "why-details", open: ctx.open === true }, el("summary", { text: "All the scores" }), grid));
}

// A post as we know it, when Bluesky's own view of it isn't there: deleted, hidden by a label, or
// the AppView can't be reached.
function ourCard(p, why) {
  const link = safeLink(p.url);
  const initial = (str(p.topic).trim().charAt(0) || "?").toUpperCase();
  return el("article", {
    class: "post fallback", tabindex: link ? "0" : null, role: link ? "link" : null,
    onclick: link ? () => window.open(link, "_blank", "noopener") : null,
    onkeydown: link ? (e) => { if (e.key === "Enter") window.open(link, "_blank", "noopener"); } : null,
  },
  el("div", { class: "avatar placeholder", text: initial }),
  el("div", {},
    el("div", { class: "post-head" },
      el("span", { class: "post-name", text: str(p.topic) }),
      el("span", { class: "post-time", text: "· " + (ago(p.indexedAt) || "?") })),
    el("div", { class: "post-text" }, postLink(str(p.text) || "(the text of this post isn't available)", p.url)),
    why));
}

/** previewItem is one post of the preview. view is Bluesky's post view of it, or null. */
export function previewItem(p, slot, view, ctx) {
  const why = statsPanel(p, slot, ctx);
  let card;
  try {
    card = view && view.author ? renderPost(view, why) : ourCard(p, why);
  } catch {
    card = ourCard(p, why); // a post view that isn't what it should be
  }
  return el("li", { class: "preview-item", "data-uri": str(p.uri) }, card);
}
