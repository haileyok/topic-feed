// Topic Feeds: the feed builder page.
import "./header.js"; // the header every page shares
import { hydrate, renderPost, el, compact, setShowAdult } from "./posts.js";
import { fetchMe, fetchMine, savePanel } from "./mine.js";

const TONES = {
  informative: ["📰", "Informative", "facts, news, how-tos"],
  humorous: ["😂", "Funny", "jokes and bits"],
  personal: ["💬", "Personal", "life updates, feelings"],
  outraged: ["😡", "Outraged", "angry, heated"],
  supportive: ["🤗", "Supportive", "warm, encouraging"],
  other: ["✨", "Other", "none of the above"],
};
const SIGNALS = {
  substance: ["🧠", "Substance", "has something to say"],
  general_interest: ["🌍", "Broad appeal", "interesting to most people"],
  news: ["🗞️", "Newsy", "reports what happened"],
  sentiment: ["😊", "Positive", "upbeat rather than negative"],
  critical: ["👎", "Critical", "against or mocking what it's about"],
  promo: ["📣", "Promotional", "any kind of promotion"],
  ad: ["🏷️", "Ad", "selling a product, service, or deal"],
  self_promo: ["🎨", "Own work", "sharing their own art, writing, stream…"],
  engagement_bait: ["🎣", "Engagement bait", "asks for likes, follows, reposts"],
  spam: ["🚫", "Spam", "scams, schemes, junk"],
  meme: ["😂", "Meme", "a captioned or edited joke image, a reaction image"],
};
const PAGE = 30;
const FEED_COLORS = ["#3b6cf6", "#8b5cf6", "#e5484d", "#f59e0b", "#10b981", "#0ea5e9", "#ec4899", "#6366f1", "#14b8a6", "#f97316", "#84cc16", "#a855f7"];

// ---------- state ----------

let TAX = null;          // /api/taxonomy
let FEEDS = [];          // /api/feeds
let NAMES = new Map();   // topic id -> display name
let DEFAULT_RANKING = null;
let ME = null;           // who is signed in ({did, handle}), or null
let MINE = null;         // /api/me/feeds: their feeds and limits, or null

const dial = () => ({ w: 0, min: 0, max: 1 });
function blankState() {
  return {
    name: "My feed",
    any: false,
    topics: {},            // id -> "include" | "exclude"
    minProb: 0.5,
    excludeAbove: 0.3,
    tone: Object.fromEntries(Object.keys(TONES).map((k) => [k, dial()])),
    signals: Object.fromEntries(Object.keys(SIGNALS).map((k) => [k, dial()])),
    ranking: structuredClone(DEFAULT_RANKING),
    adult: false,          // owner only: adult topics and posts (needs the adult-access cookie)
    description: "",       // for saving the feed as one's own (mine.js)
    rkey: "",              // the key typed for a new saved feed; "" makes one from the name
    editing: "",           // the key of the saved feed being changed; "" for a new one
  };
}
let state = null;
const isAdult = (id) => id === "adult_content" || id.startsWith("adult_content/");
const ui = { open: new Set(), search: "", showScores: false };

/** The API spec for the current state (mirrors a feeds.yaml entry). */
function spec() {
  const keep = (k) => state.adult || !isAdult(k);
  const inc = Object.keys(state.topics).filter((k) => state.topics[k] === "include" && keep(k));
  const exc = Object.keys(state.topics).filter((k) => state.topics[k] === "exclude" && keep(k));
  const rules = (dials) => {
    const r = { max: {}, min: {}, weights: {} };
    for (const [k, d] of Object.entries(dials)) {
      if (d.max < 1) r.max[k] = round(d.max);
      if (d.min > 0) r.min[k] = round(d.min);
      if (d.w !== 0) r.weights[k] = d.w;
    }
    return r;
  };
  return {
    paths: state.any ? ["*"] : inc,
    min_prob: round(state.minProb),
    exclude: Object.fromEntries(exc.map((k) => [k, round(state.excludeAbove)])),
    tone: rules(state.tone),
    signals: rules(state.signals),
    ranking: state.ranking,
    allow_adult: state.adult || undefined,
  };
}
const round = (x) => Math.round(x * 100) / 100;

function fromFeed(f) {
  const s = blankState();
  s.name = f.display_name;
  s.description = typeof f.description === "string" ? f.description : "";
  s.adult = f.allow_adult === true;
  s.any = f.paths.includes("*");
  for (const p of f.paths) if (p !== "*" && NAMES.has(p)) s.topics[p] = "include";
  const exc = Object.entries(f.exclude || {}).filter(([k]) => k !== "adult_content" && NAMES.has(k));
  for (const [k, v] of exc) { s.topics[k] = "exclude"; s.excludeAbove = v; }
  s.minProb = f.min_prob;
  const load = (dials, rules) => {
    for (const [k, v] of Object.entries(rules?.max || {})) if (dials[k]) dials[k].max = v;
    for (const [k, v] of Object.entries(rules?.min || {})) if (dials[k]) dials[k].min = v;
    for (const [k, v] of Object.entries(rules?.weights || {})) if (dials[k]) dials[k].w = v;
  };
  load(s.tone, f.tone);
  load(s.signals, f.signals);
  if (f.ranking) s.ranking = structuredClone(f.ranking);
  return s;
}

// URL hash: the whole state, so a link reproduces the feed.
function saveHash() {
  const json = JSON.stringify(state);
  const b64 = btoa(String.fromCharCode(...new TextEncoder().encode(json))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  history.replaceState(null, "", "#s=" + b64);
}
function loadHash() {
  const m = location.hash.match(/^#s=([\w-]+)/);
  if (!m) return null;
  try {
    const b64 = m[1].replace(/-/g, "+").replace(/_/g, "/");
    const json = new TextDecoder().decode(Uint8Array.from(atob(b64), (c) => c.charCodeAt(0)));
    const s = Object.assign(blankState(), JSON.parse(json));
    for (const k of Object.keys(TONES)) s.tone[k] = Object.assign(dial(), s.tone[k]);
    for (const k of Object.keys(SIGNALS)) s.signals[k] = Object.assign(dial(), s.signals[k]);
    s.ranking = Object.assign(structuredClone(DEFAULT_RANKING), s.ranking);
    s.ranking.weights = Object.assign(structuredClone(DEFAULT_RANKING.weights), s.ranking.weights);
    for (const k of Object.keys(s.topics)) if (!NAMES.has(k)) delete s.topics[k];
    return s;
  } catch {
    return null;
  }
}

// ---------- controls ----------

function setFill(input, from, to) {
  input.style.setProperty("--fill-from", from + "%");
  input.style.setProperty("--fill-to", to + "%");
}
const pct = (input) => ((input.value - input.min) / (input.max - input.min)) * 100;

function slider({ label, min, max, step, value, fmt, ends, onInput, centered = false }) {
  const out = el("span", { class: "slider-value" });
  const input = el("input", { type: "range", min, max, step, value, "aria-label": label });
  const paint = () => {
    const v = Number(input.value);
    out.textContent = fmt(v);
    if (centered) {
      const p = pct(input);
      setFill(input, Math.min(50, p), Math.max(50, p));
      input.classList.toggle("neg", v < 0);
    } else setFill(input, 0, pct(input));
  };
  input.addEventListener("input", () => { paint(); onInput(Number(input.value)); });
  paint();
  return el("div", { class: "slider-row" },
    el("div", { class: "slider-label" }, el("span", { text: label }), out),
    input,
    ends ? el("div", { class: "slider-ends" }, el("span", { text: ends[0] }), el("span", { text: ends[1] })) : null);
}

function dualRange(d, onChange, label) {
  const lo = el("input", { type: "range", min: 0, max: 1, step: 0.05, value: d.min, "aria-label": label + " minimum" });
  const hi = el("input", { type: "range", min: 0, max: 1, step: 0.05, value: d.max, "aria-label": label + " maximum" });
  const paint = () => {
    setFill(lo, lo.value * 100, hi.value * 100);
    lo.classList.toggle("full", Number(lo.value) <= 0 && Number(hi.value) >= 1); // untouched: muted
  };
  const on = (which) => () => {
    let a = Number(lo.value), b = Number(hi.value);
    if (a > b) { if (which === "lo") a = b; else b = a; lo.value = a; hi.value = b; }
    d.min = a; d.max = b;
    paint();
    onChange();
  };
  lo.addEventListener("input", on("lo"));
  hi.addEventListener("input", on("hi"));
  paint();
  return el("div", { class: "dual" }, lo, hi);
}

function rangeText(d) {
  if (d.min <= 0 && d.max >= 1) return "any";
  if (d.min <= 0) return `≤ ${Math.round(d.max * 100)}%`;
  if (d.max >= 1) return `≥ ${Math.round(d.min * 100)}%`;
  return `${Math.round(d.min * 100)}–${Math.round(d.max * 100)}%`;
}
const boostText = (w) => (w === 0 ? "none" : (w > 0 ? "+" : "") + w);

function dialRow(key, meta, d, desc) {
  const [emoji, name, hint] = meta;
  const rng = el("output");
  const boost = el("output");
  const row = el("div", { class: "dial" });
  const refresh = () => {
    rng.textContent = rangeText(d);
    boost.textContent = boostText(d.w);
    row.classList.toggle("active", d.w !== 0 || d.min > 0 || d.max < 1);
  };
  const w = el("input", { type: "range", min: -3, max: 3, step: 0.5, value: d.w, "aria-label": `${name} boost` });
  const paintW = () => { const p = pct(w); setFill(w, Math.min(50, p), Math.max(50, p)); w.classList.toggle("neg", Number(w.value) < 0); };
  w.addEventListener("input", () => { d.w = Number(w.value); paintW(); refresh(); changed(); });
  paintW();
  row.append(
    el("div", { class: "dial-head" }, el("span", { class: "dial-emoji", text: emoji }), el("span", { text: name }),
      el("span", { class: "dial-desc", text: desc || hint })),
    el("div", { class: "dial-grid" },
      el("label", { text: "Boost" }), w, boost,
      el("label", { text: "Allowed" }), dualRange(d, () => { refresh(); changed(); }, name), rng));
  refresh();
  return row;
}

// Topic picker. Chips cycle off -> include -> exclude -> off (with "all topics" on: off <-> exclude).
function cycle(id) {
  const cur = state.topics[id];
  const next = state.any ? (cur ? undefined : "exclude")
    : cur === undefined ? "include" : cur === "include" ? "exclude" : undefined;
  if (next) state.topics[id] = next; else delete state.topics[id];
  renderTopics();
  changed();
}

const vol = (id) => TAX.per_hour?.[id];
const volText = (id) => (vol(id) ? `~${compact(vol(id))}/h` : "");

function renderTopics() {
  const box = document.getElementById("topics");
  if (!box) return;
  const q = ui.search.trim().toLowerCase();
  const picked = document.getElementById("picked");
  picked.replaceChildren(...Object.entries(state.topics).map(([id, st]) =>
    el("span", { class: "pick" + (st === "exclude" ? " excluded" : "") },
      (st === "exclude" ? "− " : "") + NAMES.get(id),
      el("button", { "aria-label": `Remove ${NAMES.get(id)}`, onclick: () => { delete state.topics[id]; renderTopics(); changed(); }, text: "×" }))));

  const groups = [];
  for (const t of TAX.topics) {
    if (t.adult && !state.adult) continue;
    const subs = (t.subtopics || []).filter((s) => !q || match(s, q) || match(t, q));
    if (q && !subs.length && !match(t, q)) continue;
    const st = state.topics[t.id];
    const count = (t.subtopics || []).filter((s) => state.topics[s.id]).length;
    const open = q ? true : ui.open.has(t.id);
    const g = el("div", { class: "group" + (open ? " open" : "") },
      el("div", { class: "group-head" },
        el("button", {
          class: "group-toggle", "aria-expanded": String(open), title: t.description,
          onclick: () => { ui.open.has(t.id) ? ui.open.delete(t.id) : ui.open.add(t.id); renderTopics(); },
        },
          el("span", { class: "chev", text: "▶" }),
          el("span", { class: "group-name", text: t.name }),
          count ? el("span", { class: "group-count", text: String(count) }) : null,
          el("span", { class: "chip-vol vol", text: volText(t.id) })),
        el("button", {
          class: "state-btn" + (st ? " " + st : ""), title: "Cycle: whole topic in, left out, off",
          onclick: () => cycle(t.id),
          text: st === "include" ? "✓ All" : st === "exclude" ? "✕ All" : state.any ? "Leave out" : "+ All",
        })),
      el("div", { class: "group-body" },
        subs.map((s) => {
          const sst = state.topics[s.id];
          const implied = !sst && (st === "include" || state.any);
          return el("button", {
            class: "chip" + (sst ? " " + sst : "") + (implied ? " implied" : ""),
            title: s.description, onclick: () => cycle(s.id),
          }, el("span", { text: s.name }), el("span", { class: "vol", text: volText(s.id) }));
        })));
    groups.push(g);
  }
  box.replaceChildren(...(groups.length ? groups : [el("p", { class: "section-hint", text: "No topics match." })]));
  document.getElementById("exclude-row").hidden = !Object.values(state.topics).includes("exclude");
  document.getElementById("strictness-row").hidden = state.any;
}
const match = (t, q) => t.name.toLowerCase().includes(q) || t.id.includes(q) || (t.description || "").toLowerCase().includes(q);

function renderControls() {
  const c = document.getElementById("controls");
  const presets = el("select", {
    class: "select", "aria-label": "Start from",
    onchange: (e) => {
      const f = FEEDS.find((x) => x.rkey === e.target.value);
      state = f ? fromFeed(f) : blankState();
      if (f) state.name = f.display_name + " remix";
      ui.open = new Set(Object.keys(state.topics).map((k) => k.split("/")[0]));
      renderControls();
      changed();
      e.target.value = "";
    },
  }, el("option", { value: "", text: "Start from a feed…" }), el("option", { value: "__blank", text: "Blank" }),
    FEEDS.map((f) => el("option", { value: f.rkey, text: f.display_name })));

  const r = state.ranking;
  c.replaceChildren(
    el("div", { class: "section" },
      el("div", { class: "section-title", text: "Name & starting point" }),
      el("div", { class: "name-row" },
        el("input", { class: "text-input", value: state.name, maxlength: 24, "aria-label": "Feed name",
          oninput: (e) => { state.name = e.target.value; document.getElementById("preview-title").textContent = state.name || "Your feed"; saveHash(); } }),
        presets)),

    el("div", { class: "section" },
      el("div", { class: "section-title" }, el("span", { text: "Topics" }),
        el("button", { class: "link-btn", text: "Clear", onclick: () => { state.topics = {}; renderTopics(); changed(); } })),
      el("div", { class: "any-topic" },
        el("div", {}, el("strong", { text: "All topics" }), el("span", { text: "Pick by vibe alone; click topics to leave them out." })),
        el("label", { class: "switch" },
          el("input", { type: "checkbox", checked: state.any, onchange: (e) => {
            state.any = e.target.checked;
            if (state.any) for (const k of Object.keys(state.topics)) if (state.topics[k] === "include") delete state.topics[k];
            renderTopics(); changed();
          } }),
          el("span", { class: "switch-track" }, el("span", { class: "switch-thumb" })))),
      el("div", { class: "picked", id: "picked" }),
      el("div", { class: "search" }, el("input", { class: "text-input", type: "search", placeholder: "Search topics", value: ui.search,
        "aria-label": "Search topics", oninput: (e) => { ui.search = e.target.value; renderTopics(); } })),
      el("div", { class: "topic-list", id: "topics" }),
      el("div", { class: "legend" },
        el("span", {}, el("i", { style: "background:var(--accent)" }), "in"),
        el("span", {}, el("i", { style: "background:var(--danger)" }), "left out"),
        el("span", { text: "Click a topic again to change it. ~n/h: posts per hour." })),
      el("div", { id: "strictness-row" }, slider({
        label: "Match strictness", min: 0.3, max: 0.95, step: 0.05, value: state.minProb,
        fmt: (v) => `≥ ${Math.round(v * 100)}% sure`, ends: ["More posts", "Only clear matches"],
        onInput: (v) => { state.minProb = v; changed(); },
      })),
      el("div", { id: "exclude-row" }, slider({
        label: "Leave out posts that are at least", min: 0.1, max: 0.6, step: 0.05, value: state.excludeAbove,
        fmt: (v) => `${Math.round(v * 100)}% a left-out topic`, ends: ["Strict", "Lenient"],
        onInput: (v) => { state.excludeAbove = v; changed(); },
      }))),

    el("div", { class: "section" },
      el("div", { class: "section-title" }, el("span", { text: "Vibe" }),
        el("button", { class: "link-btn", text: "Reset", onclick: () => { for (const k in state.tone) state.tone[k] = dial(); renderControls(); changed(); } })),
      el("p", { class: "section-hint", text: "How each post comes across. Boost moves posts up or down; Allowed drops posts outside the range." }),
      Object.entries(TONES).map(([k, m]) => dialRow(k, m, state.tone[k]))),

    el("div", { class: "section" },
      el("div", { class: "section-title" }, el("span", { text: "Quality signals" }),
        el("button", { class: "link-btn", text: "Reset", onclick: () => { for (const k in state.signals) state.signals[k] = dial(); renderControls(); changed(); } })),
      el("p", { class: "section-hint", text: "Scores from 0 to 100% for what a post is like. They don't add up to 100%." }),
      Object.entries(SIGNALS).map(([k, m]) => dialRow(k, m, state.signals[k]))),

    el("details", { class: "section" },
      el("summary", {}, el("div", { class: "section-title", text: "Ranking" })),
      slider({ label: "Freshness", min: 0.6, max: 3, step: 0.1, value: r.gravity, fmt: (v) => v.toFixed(1),
        ends: ["Popular posts stay up", "Newest first"], onInput: (v) => { r.gravity = v; changed(); } }),
      // Position 1 is "off": every slot fresh (1) would leave no room for ranking.
      slider({ label: "Brand-new post in every", min: 1, max: 10, step: 1, value: r.fresh_every || 1,
        fmt: (v) => (v <= 1 ? "off" : `${ordinal(v)} slot`), ends: ["Off", "Rarely"],
        onInput: (v) => { r.fresh_every = v <= 1 ? 0 : v; changed(); } }),
      slider({ label: "Same author at most every", min: 0, max: 30, step: 1, value: r.author_gap,
        fmt: (v) => (v === 0 ? "no limit" : `${v} posts`), onInput: (v) => { r.author_gap = v; changed(); } }),
      slider({ label: "Promotional penalty", min: 0, max: 5, step: 0.5, value: r.promo_penalty, fmt: (v) => String(v), onInput: (v) => { r.promo_penalty = v; changed(); } }),
      el("p", { class: "section-hint", text: "Engagement weights: how much each counts toward a post's score." }),
      ["like", "repost", "reply", "quote"].map((k) => slider({
        label: k[0].toUpperCase() + k.slice(1) + "s", min: 0, max: 10, step: 0.5, value: r.weights[k], fmt: (v) => "×" + v,
        onInput: (v) => { r.weights[k] = v; changed(); },
      })),
      el("button", { class: "btn btn-ghost", text: "Reset ranking", onclick: () => { state.ranking = structuredClone(DEFAULT_RANKING); renderControls(); changed(); } })),
  );
  renderTopics();
  document.getElementById("preview-title").textContent = state.name || "Your feed";
}
const ordinal = (n) => n + (["th", "st", "nd", "rd"][n % 100 > 10 && n % 100 < 14 ? 0 : n % 10 < 4 ? n % 10 : 0] || "th");

function renderActions(notice = null) {
  document.getElementById("actions").replaceChildren(
    // Signed in: the feed can be saved as one's own (and published from /feeds).
    ME && MINE ? savePanel({
      me: ME, mine: MINE, state, spec, notice, adult: () => state.adult,
      onSaved: (feed, n) => {
        history.replaceState(null, "", "?edit=" + encodeURIComponent(feed.rkey) + location.hash);
        renderActions(n);
      },
    }) : null,
    el("div", { class: "btn-row" },
      el("button", { class: "btn", onclick: copyLink, text: "Copy link" }),
      el("button", { class: "btn btn-primary", onclick: copyYAML, text: "Copy as feeds.yaml" })),
    el("button", { class: "btn btn-ghost", text: "Start over", onclick: () => { state = blankState(); ui.open.clear(); renderControls(); changed(); } }));
}

async function copy(text, msg) {
  try { await navigator.clipboard.writeText(text); toast(msg); }
  catch { toast("Couldn't copy: your browser blocked it"); }
}
function copyLink() { saveHash(); copy(location.href, "Link copied"); }

function copyYAML() {
  const s = spec();
  const rkey = (state.name || "my-feed").toLowerCase().normalize("NFKD").replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 15).replace(/-+$/, "") || "my-feed";
  const map = (o) => "{" + Object.entries(o).map(([k, v]) => `${k}: ${v}`).join(", ") + "}";
  const lines = [
    `  - rkey: ${rkey}`,
    `    display_name: ${JSON.stringify((state.name || "My feed").slice(0, 24))}`,
    `    description: ${JSON.stringify(`${state.name || "My feed"}, picked by a topic classifier. No keyword lists.`)}`,
    `    paths: [${s.paths.map((p) => (p === "*" ? '"*"' : p)).join(", ")}]`,
    `    min_prob: ${s.min_prob}`,
  ];
  const exclude = state.adult ? { ...s.exclude } : { ...s.exclude, adult_content: 0.2 };
  if (Object.keys(exclude).length) lines.push(`    exclude: ${map(exclude)}`);
  if (state.adult) lines.push("    allow_adult: true");
  for (const [name, r] of [["tone", s.tone], ["signals", s.signals]]) {
    const parts = ["max", "min", "weights"].filter((k) => Object.keys(r[k]).length);
    if (!parts.length) continue;
    lines.push(`    ${name}:`);
    for (const k of parts) lines.push(`      ${k}: ${map(r[k])}`);
  }
  const d = DEFAULT_RANKING, r = s.ranking;
  const changedKeys = ["gravity", "fresh_every", "author_gap", "promo_penalty"].filter((k) => r[k] !== d[k]);
  const w = Object.entries(r.weights).filter(([k, v]) => v !== d.weights[k]);
  if (changedKeys.length || w.length) {
    lines.push("    ranking:");
    for (const k of changedKeys) lines.push(`      ${k}: ${r[k]}`);
    if (w.length) lines.push(`      weights: ${map(Object.fromEntries(w))}`);
  }
  if (state.any || s.paths.some((p) => !p.includes("/"))) lines.push("    max_posts: 10000");
  copy(lines.join("\n") + "\n", "Copied: paste it into config/feeds.yaml");
}

// ---------- preview ----------

const view = { reqId: 0, loaded: 0, total: 0, loading: false, abort: null, slot: 0 };
let timer = null;

function changed() {
  saveHash();
  renderSummary();
  clearTimeout(timer);
  timer = setTimeout(() => refresh(), 350);
}

function renderSummary() {
  const tags = [];
  const inc = Object.values(state.topics).filter((v) => v === "include").length;
  const exc = Object.values(state.topics).filter((v) => v === "exclude").length;
  tags.push(state.any ? "All topics" : `${inc} topic${inc === 1 ? "" : "s"}`);
  if (exc) tags.push(`${exc} left out`);
  if (!state.any) tags.push(`≥ ${Math.round(state.minProb * 100)}% sure`);
  for (const [dials, meta] of [[state.tone, TONES], [state.signals, SIGNALS]]) {
    for (const [k, d] of Object.entries(dials)) {
      const bits = [];
      if (d.min > 0 || d.max < 1) bits.push(rangeText(d));
      if (d.w) bits.push("boost " + boostText(d.w));
      if (bits.length) tags.push(`${meta[k][0]} ${meta[k][1]} ${bits.join(", ")}`);
    }
  }
  document.getElementById("summary").replaceChildren(...tags.map((t) => el("span", { class: "tag", text: t })));
}

function setLoading(on) {
  document.getElementById("progress").hidden = !on;
  document.getElementById("posts").classList.toggle("stale", on && view.loaded > 0);
}

async function refresh() {
  const s = spec();
  const postsBox = document.getElementById("posts");
  const stats = document.getElementById("preview-stats");
  if (!s.paths.length) {
    view.reqId++;
    view.abort?.abort();
    setLoading(false);
    stats.textContent = "Pick some topics to start.";
    view.loaded = view.total = 0;
    document.getElementById("posts-end").textContent = "";
    postsBox.replaceChildren(emptyState());
    return;
  }
  if (!view.loaded) postsBox.replaceChildren(...Array.from({ length: 4 }, () => el("div", { class: "skeleton" })));
  await load(true);
}

async function load(reset) {
  const id = reset ? ++view.reqId : view.reqId;
  if (reset) { view.abort?.abort(); view.abort = new AbortController(); }
  const signal = view.abort.signal;
  const offset = reset ? 0 : view.loaded;
  view.loading = true;
  setLoading(true);
  try {
    const res = await fetch("/api/preview", {
      method: "POST", headers: { "Content-Type": "application/json" }, signal,
      body: JSON.stringify({ ...spec(), offset, limit: PAGE }),
    });
    const body = await res.json().catch(() => ({}));
    if (id !== view.reqId) return;
    if (res.status === 429) { toast(body.error || "Slow down a little"); setTimeout(() => { if (id === view.reqId) load(reset); }, 1500); return; }
    if (!res.ok) throw new Error(body.error || `Server returned ${res.status}`);
    const posts = await hydrate(body.posts.map((p) => p.uri), signal);
    if (id !== view.reqId) return;
    const byUri = new Map(body.posts.map((p, i) => [p.uri, { ...p, slot: offset + i + 1 }]));
    const cards = posts.map((p) => renderPost(p, why(byUri.get(p.uri))));
    const box = document.getElementById("posts");
    if (reset) {
      box.replaceChildren(...(cards.length ? cards : [emptyState(true)]));
      window.scrollTo(0, 0);
    } else box.append(...cards);
    view.loaded = offset + body.posts.length;
    view.total = body.total;
    const removed = Object.values(body.removed).reduce((a, b) => a + b, 0);
    document.getElementById("preview-stats").textContent =
      `${body.total.toLocaleString()} posts from the last 24 hours · built in ${body.took_ms} ms` + (removed ? ` · ${removed} deleted or hidden` : "");
    document.getElementById("posts-end").textContent = view.loaded >= view.total ? (view.total ? "That's everything for now." : "") : "";
  } catch (e) {
    if (e.name === "AbortError" || id !== view.reqId) return;
    document.getElementById("posts").replaceChildren(el("div", { class: "empty" },
      el("h3", { text: "Something went wrong" }), el("p", { text: e.message }),
      el("button", { class: "btn", text: "Try again", onclick: () => load(true) })));
  } finally {
    if (id === view.reqId) { view.loading = false; setLoading(false); }
  }
}

// Everything the model and the ranking said about a post, shown when "Scores" is on.
function why(p) {
  if (!p) return null;
  const pct = (v) => Math.round((v || 0) * 100);
  const topTone = Object.entries(p.tone || {}).sort((a, b) => b[1] - a[1])[0];
  const fresh = state.ranking.fresh_every > 0 && p.slot % state.ranking.fresh_every === 0;
  const age = p.age_minutes < 60 ? `${p.age_minutes} min` : p.age_minutes < 2880 ? `${Math.round(p.age_minutes / 60)} h` : `${Math.round(p.age_minutes / 1440)} d`;

  // One bar per score. "ruled" marks scores the current settings use (a range or a boost).
  const meter = (label, v, { title = "", ruled = false, note = "" } = {}) =>
    el("div", { class: "meter" + (ruled ? " ruled" : ""), title: title || label },
      el("span", { class: "m-label", text: label }),
      el("span", { class: "m-bar" }, el("span", { class: "m-fill", style: `width:${pct(v)}%` })),
      el("span", { class: "m-val", text: `${pct(v)}%` }),
      note ? el("span", { class: "m-note", text: note }) : null);
  const ruleNote = (d) => [d.min > 0 || d.max < 1 ? `allowed ${rangeText(d)}` : "", d.w ? `boost ${d.w > 0 ? "+" : ""}${d.w}` : ""].filter(Boolean).join(", ");
  const used = (d) => d && (d.min > 0 || d.max < 1 || d.w !== 0);
  const section = (title, ...rows) => el("div", { class: "why-sec" }, el("h4", { text: title }), ...rows);

  const topics = section("Topics",
    ...(p.top || []).map(([k, v]) => meter(NAMES.get(k) || k, v, { title: k, ruled: state.topics[k] === "include" })),
    el("div", { class: "kv" }, el("span", { text: "Matches this feed" }), el("b", { text: `${pct(p.match)}%` })));
  const signals = section("Signals",
    ...Object.entries(SIGNALS).filter(([k]) => p.signals && k in p.signals).map(([k, [icon, name, desc]]) =>
      meter(`${icon} ${name}`, p.signals[k], { title: `${name}: ${desc}`, ruled: used(state.signals[k]), note: used(state.signals[k]) ? ruleNote(state.signals[k]) : "" })));
  const tone = section("Tone",
    ...Object.entries(p.tone || {}).sort((a, b) => b[1] - a[1]).map(([k, v]) =>
      meter(`${TONES[k]?.[0] || ""} ${TONES[k]?.[1] || k}`, v, { title: TONES[k]?.[2] || k, ruled: used(state.tone[k]), note: used(state.tone[k]) ? ruleNote(state.tone[k]) : "" })));
  const kv = (k, v, title = "") => el("div", { class: "kv", title }, el("span", { text: k }), el("b", { text: v }));
  const ranking = section("Ranking",
    kv("Score", p.score.toFixed(3), "Ranking score: engagement plus a quality prior, decayed by age"),
    kv("Slot", `#${p.slot}${fresh ? " (fresh slot)" : ""}`),
    kv("Age", age),
    kv("Likes · reposts", `${compact(p.likes)} · ${compact(p.reposts)}`),
    kv("Replies · quotes", `${compact(p.replies)} · ${compact(p.quotes)}`),
    p.labels?.length ? el("div", { class: "labels" }, ...p.labels.map((l) => el("span", { class: "label", text: l }))) : null);

  // Clicks inside the panel don't open the post, so it can be read and selected.
  return el("div", { class: "why", onclick: (e) => e.stopPropagation() },
    el("div", { class: "why-chips" },
      el("span", { class: "topic", text: `${NAMES.get(p.topic) || p.topic} ${pct(p.p)}%` }),
      topTone ? el("span", { title: "Tone", text: `${TONES[topTone[0]]?.[0] || ""} ${TONES[topTone[0]]?.[1] || topTone[0]} ${pct(topTone[1])}%` }) : null,
      el("span", { title: "Ranking score", text: `score ${p.score.toFixed(2)}` }),
      el("span", { title: "Slot in the feed", text: `#${p.slot}` }),
      fresh ? el("span", { class: "fresh", title: "Brand-new post slot", text: "fresh slot" }) : null),
    el("div", { class: "why-grid" }, topics, signals, tone, ranking));
}

function emptyState(noResults = false) {
  if (noResults) {
    return el("div", { class: "empty" }, el("h3", { text: "No posts match" }),
      el("p", { text: "Try loosening the match strictness or the Allowed ranges." }));
  }
  return el("div", { class: "empty" },
    el("h3", { text: "Build a feed from topics and vibes" }),
    el("p", { text: "Pick topics on the left, or start from one of these:" }),
    el("div", { class: "starters" }, FEEDS.slice(0, 8).map((f) =>
      el("button", { class: "btn", text: f.display_name, onclick: () => { state = fromFeed(f); state.name = f.display_name + " remix"; ui.open = new Set(Object.keys(state.topics).map((k) => k.split("/")[0])); renderControls(); changed(); } }))));
}

// ---------- browse ----------

async function renderBrowse() {
  const grid = document.getElementById("feed-grid");
  if (grid.childElementCount) return;
  grid.replaceChildren(...FEEDS.map((f, i) => {
    const sample = el("div", { class: "sample", text: " " });
    const topics = f.paths.includes("*") ? ["All topics"] : f.paths.map((p) => NAMES.get(p) || p);
    const extras = [];
    for (const [k, v] of Object.entries(f.tone?.max || {})) extras.push(`${TONES[k]?.[0] || ""} ≤ ${Math.round(v * 100)}%`);
    for (const [k, v] of Object.entries(f.tone?.min || {})) extras.push(`${TONES[k]?.[0] || ""} ≥ ${Math.round(v * 100)}%`);
    const card = el("article", { class: "feed-card" },
      el("div", { class: "feed-card-head" },
        el("div", { class: "feed-mark", style: `background:${FEED_COLORS[i % FEED_COLORS.length]}`, text: f.display_name.charAt(0) }),
        el("div", {}, el("h3", { text: f.display_name }), el("div", { class: "count", text: `${f.posts.toLocaleString()} posts right now` }))),
      el("p", { text: f.description }),
      el("div", { class: "topics" }, [...topics, ...extras].map((t) => el("span", { text: t }))),
      sample,
      el("div", { class: "btn-row" },
        el("a", { class: "btn btn-primary", href: f.url, target: "_blank", rel: "noopener", text: "Open in Bluesky" }),
        el("button", { class: "btn", text: "Remix", onclick: () => {
          state = fromFeed(f); state.name = f.display_name + " remix";
          ui.open = new Set(Object.keys(state.topics).map((k) => k.split("/")[0]));
          show("build"); renderControls(); changed();
        } })));
    topPost(f).then((p) => { sample.textContent = p ? `“${(p.record?.text || "").slice(0, 180)}” — @${p.author.handle}` : ""; });
    return card;
  }));
}

async function topPost(f) {
  try {
    const res = await fetch(`/xrpc/app.bsky.feed.getFeedSkeleton?feed=${encodeURIComponent(f.uri)}&limit=3`);
    const body = await res.json();
    const posts = await hydrate(body.feed.map((x) => x.post));
    return posts.find((p) => p.record?.text) || null;
  } catch { return null; }
}

// ---------- shell ----------

// The header's "Build a feed" and "Browse feeds" links are this page's two views: here they switch
// views without reloading (which would lose the feed being built), and ?view=browse says which.
function show(name) {
  for (const a of document.querySelectorAll(".sitenav-link[data-view]")) {
    if (a.dataset.view === name) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  const q = new URLSearchParams(location.search);
  if (name === "browse") q.set("view", "browse");
  else q.delete("view");
  const query = q.toString();
  history.replaceState(null, "", location.pathname + (query ? "?" + query : "") + location.hash);
  document.getElementById("view-build").hidden = name !== "build";
  document.getElementById("view-browse").hidden = name !== "browse";
  if (name === "browse") renderBrowse();
}

let toastTimer = null;
function toast(msg) {
  const t = document.getElementById("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.hidden = true; }, 2600);
}

function panel(open) {
  document.getElementById("panel").classList.toggle("open", open);
  document.body.classList.toggle("panel-open", open);
}

async function main() {
  const [tax, feeds] = await Promise.all([
    fetch("/api/taxonomy").then((r) => r.json()),
    fetch("/api/feeds").then((r) => r.json()).catch(() => []),
  ]);
  TAX = tax;
  FEEDS = feeds;
  DEFAULT_RANKING = tax.default_ranking;
  for (const t of tax.topics) {
    NAMES.set(t.id, t.name);
    for (const s of t.subtopics || []) NAMES.set(s.id, s.name);
  }
  ME = await fetchMe();
  MINE = ME ? await fetchMine() : null;
  state = loadHash() || blankState();
  state.editing = ""; // a link can't make you the editor of a feed: only ?edit= for one of yours does
  // /?edit=<key> opens one of the signed-in account's own feeds in the builder.
  const editKey = new URLSearchParams(location.search).get("edit");
  const mine = MINE && editKey ? MINE.feeds.find((x) => x.rkey === editKey) : null;
  if (mine) {
    state = fromFeed(mine.spec);
    state.editing = mine.rkey;
  }
  if (!tax.adult_allowed) state.adult = false;
  setShowAdult(state.adult);
  ui.open = new Set(Object.keys(state.topics).map((k) => k.split("/")[0]));

  for (const a of document.querySelectorAll(".sitenav-link[data-view]")) {
    a.addEventListener("click", (e) => {
      if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; // a new tab or window: let it open
      e.preventDefault();
      show(a.dataset.view);
    });
  }
  document.getElementById("open-panel").addEventListener("click", () => panel(true));
  document.getElementById("close-panel").addEventListener("click", () => panel(false));
  // Tap outside the drawer closes it. composedPath is fixed at dispatch, so it still holds
  // the panel when the click re-rendered (and detached) the button that was tapped.
  document.addEventListener("click", (e) => {
    if (!document.body.classList.contains("panel-open")) return;
    if (!e.composedPath().some((n) => n.id === "panel" || n.id === "open-panel")) panel(false);
  });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") panel(false); });
  document.getElementById("show-scores").addEventListener("change", (e) => {
    ui.showScores = e.target.checked;
    document.getElementById("posts").classList.toggle("scores", ui.showScores);
  });
  // Adult content: only offered when the server says this browser has the owner's key.
  if (tax.adult_allowed) {
    const sw = document.getElementById("adult-switch");
    const input = document.getElementById("show-adult");
    sw.hidden = false;
    input.checked = state.adult;
    input.addEventListener("change", () => {
      state.adult = input.checked;
      setShowAdult(state.adult);
      if (!state.adult) for (const k of Object.keys(state.topics)) if (isAdult(k)) delete state.topics[k];
      view.loaded = 0;
      renderControls();
      changed();
    });
  }
  new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting) && !view.loading && view.loaded && view.loaded < view.total) load(false);
  }, { rootMargin: "600px" }).observe(document.getElementById("posts-end"));

  renderControls();
  renderActions();
  renderSummary();
  refresh();
  // The other pages link to the browse view as /?view=browse.
  if (new URLSearchParams(location.search).get("view") === "browse") show("browse");
}

main().catch((e) => {
  document.getElementById("posts").replaceChildren(el("div", { class: "empty" },
    el("h3", { text: "Couldn't load the builder" }), el("p", { text: e.message })));
});
