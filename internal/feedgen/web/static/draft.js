// The draft of a tuning on the page at /me, and how it becomes what is sent to the server. All of
// it is plain data and functions, so it can be tested without a page.
//
// A draft is the tuning as someone is editing it. Only what they have set is in it: a setting
// that isn't there is the feed's own, and follows the feed (and the fresh-or-popular choice,
// for the numbers of the ranking) wherever that goes.

import { newDial, dialActive } from "./scores.js";

// ---------- an interest's slider ----------

// The stops of a topic's slider, and what each does: it multiplies the topic's share of the feed.
export const STOPS = [0, 0.25, 0.5, 1, 2, 3, 5];
export const NORMAL = STOPS.indexOf(1);
const STOP_TEXT = ["Muted", "A lot less (×¼)", "Less (×½)", "As you like it", "More (×2)", "A lot more (×3)", "Much more (×5)"];

/** stopIndex is the stop nearest a weight. */
export function stopIndex(w) {
  let best = 0;
  for (let i = 1; i < STOPS.length; i++) if (Math.abs(STOPS[i] - w) < Math.abs(STOPS[best] - w)) best = i;
  return best;
}

export function weightText(w) {
  const i = STOPS.indexOf(w);
  return i >= 0 ? STOP_TEXT[i] : `×${Number(Number(w).toFixed(2))}`;
}

// ---------- the draft ----------

/** Settings about the feed that are one number each; the server's names for them. */
export const SETTING_KEYS = ["authorGap", "halfLifeDays", "lookbackDays", "minLikes", "interests", "windowHours", "minTopicProb", "maxServes", "listSize", "minEngagement"];
/** The settings where zero is a value of its own (no gap between authors, no minimum), not "the feed's". */
const ZERO_IS_A_VALUE = new Set(["authorGap", "minEngagement"]);
/** The numbers of the ranking. */
export const RANKING_KEYS = ["gravity", "freshEvery", "promoPenalty", "like", "repost", "reply", "quote"];

const num = (v) => typeof v === "number" && Number.isFinite(v);

/** emptyDraft is a tuning that changes nothing. */
export function emptyDraft() {
  return { topics: {}, freshness: "balanced", hidePromo: false, showSeen: false, settings: {}, ranking: {}, tone: {}, signals: {} };
}

function dialsFrom(rules) {
  const out = {};
  if (!rules || typeof rules !== "object") return out;
  const names = new Set([...Object.keys(rules.max || {}), ...Object.keys(rules.min || {}), ...Object.keys(rules.weights || {})]);
  for (const name of names) {
    const d = newDial();
    if (num((rules.min || {})[name])) d.min = rules.min[name];
    if (num((rules.max || {})[name])) d.max = rules.max[name];
    if (num((rules.weights || {})[name])) d.w = rules.weights[name];
    if (dialActive(d)) out[name] = d;
  }
  return out;
}

/** draftFrom is a draft of a tuning as the server stores it. */
export function draftFrom(t) {
  const d = emptyDraft();
  if (!t || typeof t !== "object") return d;
  for (const [path, w] of Object.entries(t.topics && typeof t.topics === "object" ? t.topics : {})) {
    if (num(w)) d.topics[path] = w;
  }
  if (t.freshness === "popular" || t.freshness === "fresh") d.freshness = t.freshness;
  d.hidePromo = t.hidePromo === true;
  d.showSeen = t.showSeen === true;
  for (const k of SETTING_KEYS) if (num(t[k]) && (t[k] > 0 || ZERO_IS_A_VALUE.has(k))) d.settings[k] = t[k];
  const r = t.ranking && typeof t.ranking === "object" ? t.ranking : {};
  for (const k of RANKING_KEYS) if (num(r[k])) d.ranking[k] = r[k];
  d.tone = dialsFrom(t.tone);
  d.signals = dialsFrom(t.signals);
  return d;
}

/** clone is a copy of a draft that shares nothing with it. */
export function clone(d) {
  return JSON.parse(JSON.stringify(d));
}

function rulesOf(dials) {
  const rules = { max: {}, min: {}, weights: {} };
  for (const [name, d] of Object.entries(dials)) {
    if (d.max < 1) rules.max[name] = d.max;
    if (d.min > 0) rules.min[name] = d.min;
    if (d.w !== 0) rules.weights[name] = d.w;
  }
  const out = {};
  for (const [k, v] of Object.entries(rules)) if (Object.keys(v).length > 0) out[k] = v;
  return out;
}

/**
 * payloadOf is a draft as it is saved and previewed: only what differs from the feed's own
 * settings (defaults, from the server). natural is the set of topics that come from the person's
 * likes: a weight of 1 on those changes nothing, but on any other topic it is what adds it.
 */
export function payloadOf(draft, defaults, natural) {
  const t = {};
  const topics = {};
  for (const [path, w] of Object.entries(draft.topics)) {
    if (w === 1 && natural.has(path)) continue;
    topics[path] = w;
  }
  if (Object.keys(topics).length > 0) t.topics = topics;
  if (draft.freshness !== "balanced") t.freshness = draft.freshness;
  if (draft.hidePromo) t.hidePromo = true;
  if (draft.showSeen) t.showSeen = true;
  for (const k of SETTING_KEYS) {
    const v = draft.settings[k];
    if (v !== undefined && v !== defaults[k]) t[k] = v;
  }
  // The ranking's numbers are compared with what the chosen freshness starts from.
  const base = (defaults.ranking && defaults.ranking[draft.freshness]) || {};
  const ranking = {};
  for (const k of RANKING_KEYS) {
    const v = draft.ranking[k];
    if (v !== undefined && v !== base[k]) ranking[k] = v;
  }
  if (Object.keys(ranking).length > 0) t.ranking = ranking;
  const tone = rulesOf(draft.tone);
  if (Object.keys(tone).length > 0) t.tone = tone;
  const signals = rulesOf(draft.signals);
  if (Object.keys(signals).length > 0) t.signals = signals;
  return t;
}

/** same is whether two payloads say the same thing. */
export function same(a, b) {
  const canon = (v) => {
    if (Array.isArray(v)) return v.map(canon);
    if (v && typeof v === "object") return Object.fromEntries(Object.keys(v).sort().map((k) => [k, canon(v[k])]));
    return v;
  };
  return JSON.stringify(canon(a)) === JSON.stringify(canon(b));
}

/** dial is the draft's cutoffs and boost for a score, made if there are none yet. */
export function dial(draft, kind, name) {
  const dials = draft[kind];
  return dials[name] || (dials[name] = newDial());
}

// ---------- words ----------

export function gapText(n) {
  if (n <= 0) return "No spacing: one author can fill several places in a row";
  return n === 1 ? "At least 1 other post between two by the same author" : `At least ${n} other posts between two by the same author`;
}

export function memoryText(days) {
  const d = Number(Number(days).toFixed(1));
  return `A like counts half as much after ${d} ${d === 1 ? "day" : "days"}`;
}

/** ago is how long ago a time was, for a post in the preview. */
export function ago(iso, now = Date.now()) {
  const t = new Date(typeof iso === "string" ? iso : "").getTime();
  if (Number.isNaN(t)) return "";
  const mins = Math.max(0, Math.round((now - t) / 60000));
  if (mins < 60) return `${mins} min`;
  const hours = Math.round(mins / 60);
  return hours < 48 ? `${hours} h` : `${Math.round(hours / 24)} days`;
}
