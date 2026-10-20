// Tuning your feed on /me: a slider on each interest, topics to add, and (see knobs.js) every other
// setting of the feed, with a preview of the posts the settings would pick and a bar to save them.
//
// You change a draft. The draft is previewed (a moment after you stop moving things) and nothing
// else happens until you save it. All text is put on the page with textContent, never as HTML.

import { $, make, str, num, pct, postLink } from "./dom.js";
import { STOPS, NORMAL, SETTING_KEYS, RANKING_KEYS, stopIndex, weightText, emptyDraft, draftFrom, clone, payloadOf, same } from "./draft.js";
import { buildKnobs } from "./knobs.js";
import { previewItem } from "./stats.js";
import { hydrate } from "./posts.js";

const DEFAULT_PREVIEW_DELAY = 400;
const DEFAULT_RETRY_DELAY = 3000;

// What a limit is taken to be if the server doesn't say.
const LIMITS = {
  boost: 10, gravity: 10, freshEvery: 50, promoPenalty: 10, engagement: 20, authorGap: 50, lookbackDays: 30, minLikes: 500,
  interests: 100, windowHours: 24, minTopicProb: 0.3, maxServes: 20, listSize: 300, minEngagement: 200,
  minWindowHours: 0.5, minEngagementPower: 0.2,
};

// Each broad topic has a colour of its own, the same on every visit: a hue from its name.
export function hueOf(name) {
  let h = 0;
  for (const c of String(name)) h = (h * 31 + c.charCodeAt(0)) % 360;
  return h;
}

const PROBLEMS = {
  limited: "That's being asked a lot. Wait a few seconds and try again.",
  unavailable: "Couldn't do that just now. Try again in a moment.",
  loading: "Your feed is still being prepared. Trying again in a few seconds…",
  network: "Couldn't reach the server. Check your connection and try again.",
  forbidden: "That was refused. Reload the page and try again.",
  too_large: "That's more than can be saved at once.",
  invalid: "Those settings aren't valid.",
};

// ---------- talking to the server ----------

async function call(method, url, body, signal) {
  let res;
  try {
    res = await fetch(url, {
      method,
      headers: body === undefined ? { Accept: "application/json" } : { Accept: "application/json", "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "same-origin",
      signal,
    });
  } catch {
    return { status: 0, ok: false, data: null };
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    // not JSON: the status says enough
  }
  return { status: res.status, ok: res.ok, data };
}

// problemText says in words what went wrong with a request that didn't work.
function problemText(r) {
  if (r.status === 0) return PROBLEMS.network;
  if (r.status === 429) return PROBLEMS.limited;
  const code = r.data && typeof r.data.error === "string" ? r.data.error : "";
  if (code === "invalid" && typeof r.data.message === "string" && r.data.message) return `Those settings can't be used: ${r.data.message}.`;
  return Object.hasOwn(PROBLEMS, code) ? PROBLEMS[code] : PROBLEMS.unavailable;
}

// What the server says about the feed's own settings and limits, with a number wherever one is
// expected.
function readInfo(d) {
  const defaults = { freshness: "balanced", ranking: {} };
  const given = d.defaults && typeof d.defaults === "object" ? d.defaults : {};
  for (const k of SETTING_KEYS) defaults[k] = num(given[k]);
  for (const f of ["popular", "balanced", "fresh"]) {
    const r = given.ranking && typeof given.ranking === "object" ? given.ranking[f] : null;
    defaults.ranking[f] = Object.fromEntries(RANKING_KEYS.map((k) => [k, num(r && r[k])]));
  }
  const limits = { ...LIMITS };
  const sent = d.limits && typeof d.limits === "object" ? d.limits : {};
  for (const k of Object.keys(LIMITS)) if (num(sent[k]) > 0) limits[k] = sent[k];
  const names = (v) => (Array.isArray(v) ? v.filter((n) => typeof n === "string") : []);
  return {
    defaults, limits, tones: names(d.tones), signals: names(d.signals),
    topics: d.topics.filter((t) => t && typeof t.path === "string").map((t) => ({ path: t.path, name: str(t.name) || t.path, broad: str(t.broad) })),
  };
}

// ---------- the tuner ----------

/**
 * createTuner wires the controls in the page. onSaved is called after a tuning is saved, so the
 * interests can be read again with it applied. Call start() once someone is signed in, and
 * setInterests with each reading of their interests.
 */
export function createTuner({ onSaved }) {
  const root = $("signed-in");
  const delayOf = (key, fallback) => {
    const n = Number(root.dataset[key]);
    return Number.isFinite(n) && n >= 0 && root.dataset[key] !== undefined ? n : fallback;
  };
  const previewDelay = delayOf("previewDelay", DEFAULT_PREVIEW_DELAY);
  const retryDelay = delayOf("retryDelay", DEFAULT_RETRY_DELAY);

  let info = null; // the answer to GET /api/me/tuning: defaults, limits, names, and the topics that can be added
  let rows = []; // the interests, as the server last read them
  let saved = emptyDraft();
  let draft = emptyDraft();
  let knobs = null;
  let saving = false;
  let note = null; // {kind: "saved" | "error", text}, shown in the save bar
  let seq = 0; // which preview is the latest asked for
  let sent = {}; // what that preview was asked about
  let previewTimer = null;
  let retryTimer = null;
  let aborter = null;
  let shares = new Map(); // each interest's share of the feed, from the latest preview
  let showScores = false;
  const rowEls = new Map(); // path -> the parts of an interest's row that follow the shares

  const defaults = () => (info ? info.defaults : { ranking: {} });
  const natural = () => new Set(rows.filter((r) => r.added === false).map((r) => r.path));
  const payload = (d = draft) => payloadOf(d, defaults(), natural());
  const dirty = () => !same(payload(draft), payload(saved));
  const offered = () => (info ? new Map(info.topics.map((t) => [t.path, t])) : new Map());

  // ----- the list of interests, with a slider on each -----

  // visibleRows are the interests to show: those from the person's likes, those the server
  // moved up because they muted others, and the topics they added, until they are removed.
  function visibleRows() {
    const out = [];
    const seen = new Set();
    for (const r of rows) {
      const hasEntry = Object.hasOwn(saved.topics, r.path);
      if (r.added === true && hasEntry && !Object.hasOwn(draft.topics, r.path)) continue; // removed since
      out.push(r);
      seen.add(r.path);
    }
    const names = offered();
    for (const path of Object.keys(draft.topics)) {
      if (seen.has(path)) continue;
      const t = names.get(path);
      out.push({ path, name: t ? t.name : path, broad: t ? t.broad : "", share: 0, tunedShare: 0, weight: 1, added: true, fresh: true, posts: 0, samples: [] });
    }
    return out;
  }

  function setWeight(path, w) {
    if (w === 1 && natural().has(path)) delete draft.topics[path];
    else draft.topics[path] = w;
  }

  function weightControl(r) {
    const wrap = make("div", "weight");
    const input = make("input");
    input.type = "range";
    input.min = "0";
    input.max = String(STOPS.length - 1);
    input.step = "1";
    input.setAttribute("aria-label", `How much of your feed is ${str(r.name) || str(r.path)}`);
    const readout = make("span", "weight-value");
    const current = () => (Object.hasOwn(draft.topics, r.path) ? draft.topics[r.path] : 1);
    const show = () => {
      const i = stopIndex(current());
      input.value = String(i);
      readout.textContent = weightText(current());
      input.classList.toggle("neg", i < NORMAL);
      const at = (i / (STOPS.length - 1)) * 100;
      const mid = (NORMAL / (STOPS.length - 1)) * 100; // the track is filled from "as you like it"
      input.style.setProperty("--fill-from", `${Math.min(at, mid)}%`);
      input.style.setProperty("--fill-to", `${Math.max(at, mid)}%`);
    };
    input.addEventListener("input", () => {
      setWeight(r.path, STOPS[Number(input.value)]);
      show();
      changed();
    });
    show();
    wrap.append(input, readout);
    return wrap;
  }

  // What an interest's share is, in words: of their likes, and (once the draft has been previewed)
  // of the feed.
  function feedShare(r) {
    return shares.size > 0 ? shares.get(r.path) ?? 0 : num(r.tunedShare);
  }
  function shareText(r) {
    const feed = pct(feedShare(r));
    if (r.fresh) return shares.size > 0 ? `${feed}% of your feed · not saved yet` : "Not in your feed until you save";
    if (r.added) return `${feed}% of your feed`;
    const likes = pct(r.share);
    return likes === feed ? `${likes}% of your likes` : `${likes}% of your likes → ${feed}% of your feed`;
  }

  // The share as a number for the row's heading; a topic that is only a draft has none until the
  // draft has been previewed.
  function pctText(r) {
    return r.fresh && shares.size === 0 ? "–" : `${pct(feedShare(r))}%`;
  }

  function updateShares() {
    for (const { r, text, fill, pctEl } of rowEls.values()) {
      text.textContent = shareText(r);
      pctEl.textContent = pctText(r);
      fill.style.width = pct(feedShare(r)) + "%"; // through the style object: the page's policy forbids style attributes
    }
  }

  function renderRow(r) {
    const li = make("li", "interest");
    li.dataset.path = r.path;
    li.style.setProperty("--hue", String(hueOf(str(r.broad) || str(r.path).split("/")[0]))); // through the style object: the page's policy forbids style attributes
    const top = make("div", "interest-top");
    top.append(make("span", "interest-name", str(r.name) || str(r.path)), make("span", "interest-broad", str(r.broad)));
    const userAdded = Object.hasOwn(draft.topics, r.path) && !natural().has(r.path);
    if (r.added) top.append(make("span", "badge badge-added", userAdded ? "added" : "moved up"));
    const pctEl = make("span", "interest-pct", pctText(r));
    const text = make("span", "interest-share", shareText(r));
    top.append(pctEl, text);
    li.append(top);

    const bar = make("div", "bar");
    const fill = make("div", "bar-fill");
    fill.style.width = pct(feedShare(r)) + "%";
    bar.append(fill);
    li.append(bar);
    rowEls.set(r.path, { r, text, fill, pctEl });

    if (offered().has(r.path)) {
      const control = weightControl(r);
      if (userAdded) {
        const remove = make("button", "link-btn", "Remove");
        remove.type = "button";
        remove.setAttribute("aria-label", `Remove ${str(r.name) || str(r.path)} from your feed`);
        remove.addEventListener("click", () => {
          delete draft.topics[r.path];
          renderRows();
          changed();
        });
        control.append(remove);
      }
      li.append(control);
    }

    const samples = Array.isArray(r.samples) ? r.samples : [];
    const posts = num(r.posts);
    if (samples.length === 0) {
      if (r.fresh) li.append(make("p", "interest-none", "You haven't liked posts about this. It starts as strong as your typical interest, then the slider applies."));
      else li.append(make("p", "interest-none", posts > 0 ? `${posts} liked ${posts === 1 ? "post" : "posts"}` : "No liked posts are filed under this topic."));
      return li;
    }
    const details = make("details");
    details.append(make("summary", "", `${posts} liked ${posts === 1 ? "post" : "posts"}`));
    const list = make("ul", "samples");
    for (const s of samples) list.append(renderSample(s));
    if (posts > samples.length) list.append(make("li", "sample-more", `…and ${posts - samples.length} more`));
    details.append(list);
    li.append(details);
    return li;
  }

  function renderSample(s) {
    const li = make("li", "sample");
    li.append(postLink(str(s.text) || "(a post with no text)", s.url));
    const when = new Date(str(s.likedAt));
    if (!Number.isNaN(when.getTime())) {
      li.append(make("span", "sample-when", "liked " + when.toLocaleDateString(undefined, { month: "short", day: "numeric" })));
    }
    return li;
  }

  function renderRows() {
    const list = $("interest-list");
    list.replaceChildren();
    rowEls.clear();
    for (const r of visibleRows()) list.append(renderRow(r));
    renderAddOptions();
  }

  // ----- adding a topic -----

  function renderAddOptions() {
    const select = $("add-topic");
    const row = $("add-topic-row");
    row.hidden = !info;
    select.replaceChildren();
    const first = make("option", "", "Choose a topic…");
    first.value = "";
    select.append(first);
    if (!info) return;
    const shown = new Set(visibleRows().map((r) => r.path));
    let group = null;
    let groupName = null;
    for (const t of info.topics) {
      if (shown.has(t.path)) continue;
      if (t.broad !== groupName) {
        group = make("optgroup");
        group.label = t.broad;
        select.append(group);
        groupName = t.broad;
      }
      const o = make("option", "", t.name);
      o.value = t.path;
      group.append(o);
    }
  }

  function addTopic() {
    const path = $("add-topic").value;
    if (!path || !offered().has(path)) return;
    draft.topics[path] = 1;
    renderRows();
    changed();
    const row = [...$("interest-list").children].find((li) => li.dataset.path === path);
    const slider = row && row.querySelector("input");
    if (slider) slider.focus();
  }

  function bindControls() {
    $("add-topic-button").addEventListener("click", addTopic);
    $("reset-all").addEventListener("click", () => {
      draft = emptyDraft();
      knobs.sync();
      renderRows();
      changed();
    });
    $("discard").addEventListener("click", () => {
      draft = clone(saved);
      knobs.sync();
      renderRows();
      note = null;
      changed();
    });
    $("save").addEventListener("click", save);
    $("tuning-retry").addEventListener("click", load);
    $("preview-retry").addEventListener("click", () => schedulePreview(0));
    $("preview-scores").addEventListener("change", (ev) => {
      showScores = ev.target.checked;
      for (const d of $("preview-list").querySelectorAll("details.why-details")) d.open = showScores;
    });
  }

  // ----- the save bar -----

  function renderBar() {
    const bar = $("savebar");
    const isDirty = dirty();
    let text = "";
    let state = "clean";
    if (saving) {
      state = "saving";
      text = "Saving…";
    } else if (isDirty) {
      state = note && note.kind === "error" ? "error" : "dirty";
      text = state === "error" ? note.text : "You have changes that aren't saved yet. The preview and the shares show them.";
    } else if (note) {
      state = note.kind;
      text = note.text;
    }
    bar.hidden = state === "clean";
    bar.dataset.state = state;
    $("save-text").textContent = text;
    $("save").hidden = !(isDirty || saving);
    $("discard").hidden = !(isDirty || saving);
    $("save").disabled = saving;
    $("discard").disabled = saving;
    $("save").textContent = saving ? "Saving…" : "Save changes";
  }

  function changed() {
    note = null;
    renderBar();
    schedulePreview();
  }

  async function save() {
    if (saving) return;
    saving = true;
    renderBar();
    const body = payload();
    const r = await call("PUT", "/api/me/tuning", body);
    saving = false;
    if (r.status === 401) {
      location.reload(); // the session ended: start again from the sign-in form
      return;
    }
    if (!r.ok) {
      note = { kind: "error", text: problemText(r) };
      renderBar();
      return;
    }
    saved = draftFrom(r.data && r.data.tuning ? r.data.tuning : body);
    note = {
      kind: "saved",
      text: saved.showSeen
        ? "Saved. Your feed uses these settings from now on."
        : "Saved. Your feed uses these settings from now on; posts you've already seen stay out of it.",
    };
    renderBar();
    onSaved();
  }

  // ----- the preview -----

  function previewState(text, { retry = false, busy = false } = {}) {
    $("preview-status").textContent = text;
    $("preview-status").hidden = !text;
    $("preview-retry").hidden = !retry;
    $("preview").setAttribute("aria-busy", busy ? "true" : "false");
  }

  function schedulePreview(delay = previewDelay) {
    clearTimeout(previewTimer);
    clearTimeout(retryTimer);
    if (aborter) aborter.abort(); // an answer for settings that have since changed isn't wanted
    const mine = ++seq;
    previewState("Updating the preview…", { busy: true });
    previewTimer = setTimeout(() => runPreview(mine), delay);
  }

  async function runPreview(mine) {
    aborter = new AbortController();
    const signal = aborter.signal;
    sent = payload();
    const r = await call("POST", "/api/me/preview", sent, signal);
    if (mine !== seq) return; // settings changed meanwhile: that preview is on its way
    if (r.status === 401) {
      location.reload();
      return;
    }
    if (!r.ok) {
      const text = problemText(r);
      previewState(text, { retry: true });
      // Both of these clear up by themselves; a failure that doesn't only gets the button.
      const code = r.data && r.data.error;
      if (r.status === 429 || code === "loading") retryTimer = setTimeout(() => schedulePreview(0), retryDelay);
      return;
    }
    await renderPreview(r.data || {}, mine, signal);
  }

  // What the preview was asked about says which slots are brand-new posts and which scores the
  // settings put a cutoff or a boost on.
  function previewContext() {
    const base = (defaults().ranking && defaults().ranking[draft.freshness]) || {};
    const own = (sent.ranking && sent.ranking.freshEvery) ?? base.freshEvery;
    return { freshEvery: num(own), tone: sent.tone, signals: sent.signals, open: showScores };
  }

  async function renderPreview(data, mine, signal) {
    const posts = Array.isArray(data.posts) ? data.posts.filter((p) => p && typeof p.uri === "string") : [];
    $("preview-note").hidden = data.state !== "generic";
    if (data.state === "generic") $("preview-note").textContent = "With these settings your feed is a mix of every topic, because none of the topics you like is turned on.";
    if (Array.isArray(data.interests)) {
      shares = new Map(data.interests.filter((i) => i && typeof i.path === "string").map((i) => [i.path, num(i.share)]));
      updateShares();
    }
    const ctx = previewContext();
    const list = $("preview-list");
    // The posts as we know them first, and as Bluesky shows them as soon as it has answered.
    list.replaceChildren(...posts.map((p, i) => previewItem(p, i + 1, null, ctx)));
    previewState(posts.length === 0 ? "No posts match these settings right now. Try loosening a filter or turning a topic up." : "");
    if (posts.length === 0) return;
    let views;
    try {
      views = await hydrate(posts.map((p) => p.uri), signal);
    } catch {
      if (mine === seq) previewState("Couldn't get these posts from Bluesky, so they are shown with what we have.");
      return;
    }
    if (mine !== seq) return;
    const byURI = new Map(views.map((v) => [v.uri, v]));
    posts.forEach((p, i) => {
      const view = byURI.get(p.uri);
      if (view && list.children[i]) list.children[i].replaceWith(previewItem(p, i + 1, view, ctx));
    });
  }

  // ----- starting -----

  async function load() {
    $("tuning-error").hidden = true;
    $("tuning-loading").hidden = false;
    const r = await call("GET", "/api/me/tuning");
    if (r.status === 401) {
      location.reload();
      return;
    }
    $("tuning-loading").hidden = true;
    const d = r.data;
    if (!r.ok || !d || !d.defaults || !Array.isArray(d.topics)) {
      $("tuning-error-text").textContent = problemText(r.ok ? { status: 200, data: null } : r);
      $("tuning-error").hidden = false;
      return;
    }
    info = readInfo(d);
    saved = draftFrom(d.tuning);
    draft = draftFrom(d.tuning);
    knobs = buildKnobs($("knobs"), { info: () => info, draft: () => draft, changed });
    $("tuning-controls").hidden = false;
    renderRows();
    renderBar();
    schedulePreview(0);
  }

  let started = false;
  return {
    start() {
      if (!started) {
        started = true;
        bindControls();
      }
      return load();
    },
    // setInterests shows a new reading of the interests, keeping what has been changed since.
    setInterests(list) {
      rows = (Array.isArray(list) ? list : []).filter((r) => r && typeof r.path === "string");
      renderRows();
      renderBar();
    },
  };
}
