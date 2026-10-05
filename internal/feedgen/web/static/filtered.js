// The page at /filtered: sign in for the filtered feeds (a sign-in this site keeps, so it can ask for
// Discover and For You as you), and choose what each filtered feed leaves out. All text is put on the
// page with textContent, never as HTML.
//
// A viewer's filters for a feed, as the server keeps them (feedgen.Filters):
//   {exclude: {topic: p}, tone: {max, min}, signals: {max, min}, topic_rules: {...}, drop_unscored}
// On the page they are a draft: {exclude: {topic: p}, tone: {name: dial}, signals: {name: dial},
// topicRules: dials (topic-rules.js), dropUnscored}, a dial being {min, max, w} with w always 0: a
// filtered feed only leaves posts out. Adult content is topics like any other (the model's
// adult_content and its subtopics).

import "./header.js"; // the header every page shares
import { $ } from "./dom.js";
import { el } from "./posts.js";
import { describe, rangeText, newDial } from "./scores.js";
import { topicRulesEditor, topicRulesPayload, topicRulesFrom } from "./topic-rules.js";
import { setupSignInForm, showSignInProblem, signInOff } from "./signin-form.js";

const KINDS = [["tone", "tone"], ["signals", "signal"]];
const fine = (v) => typeof v === "number" && Number.isFinite(v);

function show(which) {
  for (const id of ["loading", "connect", "connected"]) $(id).hidden = id !== which;
}

/** draftOf is the page's draft of filters as the server keeps them. */
export function draftOf(f) {
  const d = { exclude: {}, tone: {}, signals: {}, topicRules: {}, dropUnscored: false };
  if (!f || typeof f !== "object") return d;
  for (const [path, p] of Object.entries(f.exclude || {})) if (fine(p)) d.exclude[path] = p;
  for (const [key] of KINDS) {
    const r = f[key] && typeof f[key] === "object" ? f[key] : {};
    for (const name of new Set([...Object.keys(r.max || {}), ...Object.keys(r.min || {})])) {
      const dial = newDial();
      if (fine((r.min || {})[name])) dial.min = r.min[name];
      if (fine((r.max || {})[name])) dial.max = r.max[name];
      d[key][name] = dial;
    }
  }
  d.topicRules = topicRulesFrom(f.topic_rules);
  d.dropUnscored = f.drop_unscored === true;
  return d;
}

/** payloadOf is a draft as the server takes it. A score with no cutoff is left out. */
export function payloadOf(d) {
  const out = { exclude: {}, tone: {}, signals: {} };
  for (const [path, p] of Object.entries(d.exclude)) out.exclude[path] = p;
  for (const [key] of KINDS) {
    const r = { max: {}, min: {} };
    for (const [name, dial] of Object.entries(d[key])) {
      if (dial.max < 1) r.max[name] = dial.max;
      if (dial.min > 0) r.min[name] = dial.min;
    }
    for (const k of Object.keys(r)) if (Object.keys(r[k]).length === 0) delete r[k];
    out[key] = r;
  }
  const rules = topicRulesPayload(d.topicRules);
  if (Object.keys(rules).length > 0) out.topic_rules = rules;
  if (Object.keys(out.exclude).length === 0) delete out.exclude;
  if (d.dropUnscored) out.drop_unscored = true;
  return out;
}

const same = (a, b) => JSON.stringify(payloadOf(a)) === JSON.stringify(payloadOf(b));
const clone = (d) => draftOf(payloadOf(d));

function paint(input, from, to) {
  input.style.setProperty("--fill-from", `${from}%`);
  input.style.setProperty("--fill-to", `${to}%`);
}

// ---------- one feed's filters ----------

function feedCard(feed, tax) {
  const saved = draftOf(feed.filters || feed.defaults);
  const defaults = draftOf(feed.defaults);
  let draft = clone(saved);
  let own = feed.filters !== null; // the viewer has filters of their own (not the feed's)
  const id = `f-${feed.rkey}`;
  const names = new Map();
  const topics = []; // every subtopic, for the topic-rules editor
  for (const b of tax.topics) {
    names.set(b.id, b.name);
    for (const s of b.subtopics || []) {
      names.set(s.id, s.name);
      topics.push({ path: s.id, name: s.name, broad: b.id, broadName: b.name });
    }
  }
  const topicName = (p) => names.get(p) || p;

  const status = el("p", { class: "me-fine filtered-status", role: "status" });
  const save = el("button", { class: "btn btn-primary", type: "button", text: "Save" });
  const discard = el("button", { class: "btn", type: "button", text: "Discard changes" });
  // A feed with no filters of its own (the usual case) starts with nothing left out: going back to it
  // is clearing every filter.
  const plain = JSON.stringify(payloadOf(defaults)) === JSON.stringify(payloadOf(draftOf(null)));
  const ownWords = plain
    ? { button: "Clear all filters", status: "Nothing is filtered out.", back: "Cleared: nothing is filtered out." }
    : { button: "Use the feed's own filters", status: "These are the feed's own filters.", back: "Back to the feed's own filters." };
  const useOwn = el("button", { class: "link-btn", type: "button", text: ownWords.button });

  // Topics to leave out: each with how sure the model must be that a post is about it.
  const excludeBox = el("div", { class: "filtered-excludes" });
  function excludeRow(path) {
    const input = el("input", { type: "range", id: `${id}-ex-${path.replace(/[^a-z0-9]+/gi, "-")}`, min: 0.05, max: 0.95, step: 0.05,
      value: draft.exclude[path], "aria-label": `How sure the model must be that a post is about ${topicName(path)} to leave it out` });
    const out = el("output");
    const showIt = () => {
      out.textContent = `> ${Math.round(draft.exclude[path] * 100)}% sure`;
      paint(input, ((draft.exclude[path] - 0.05) / 0.9) * 100, 100);
    };
    input.addEventListener("input", () => { draft.exclude[path] = Number(input.value); showIt(); changed(); });
    showIt();
    return el("div", { class: "dial active", "data-topic": path },
      el("div", { class: "dial-head" }, el("span", { class: "dial-emoji", text: "🚫" }), el("span", { text: topicName(path) }),
        el("span", { class: "dial-desc", text: path.includes("/") ? names.get(path.split("/")[0]) || "" : "the whole topic" }),
        el("button", { class: "link-btn", type: "button", text: "Remove", "aria-label": `Stop leaving out ${topicName(path)}`,
          onclick: () => { delete draft.exclude[path]; renderExcludes(); changed(); } })),
      el("div", { class: "dial-grid" }, el("label", { for: input.id, text: "Leave out when" }), input, out));
  }
  function renderExcludes() {
    const sel = el("select", { class: "text-input topic-rules-add", id: `${id}-add-exclude`, "aria-label": "Add a topic to leave out" },
      el("option", { value: "", text: "Add a topic to leave out…" }),
      ...[...tax.topics].sort((a, b) => a.name.localeCompare(b.name)).map((b) =>
        el("optgroup", { label: b.name },
          draft.exclude[b.id] !== undefined ? null : el("option", { value: b.id, text: `All of ${b.name}` }),
          ...(b.subtopics || []).filter((s) => draft.exclude[s.id] === undefined).sort((x, y) => x.name.localeCompare(y.name))
            .map((s) => el("option", { value: s.id, text: s.name })))));
    sel.addEventListener("change", () => {
      if (!sel.value) return;
      draft.exclude[sel.value] = 0.5;
      renderExcludes();
      changed();
    });
    const paths = Object.keys(draft.exclude).sort((a, b) => topicName(a).localeCompare(topicName(b)));
    excludeBox.replaceChildren(...paths.map(excludeRow), sel);
  }

  // Cutoffs on tones and signals, for every post.
  const scoresBox = el("div", { class: "filtered-scores" });
  function scoreRow(key, kind, name) {
    const meta = describe(kind, name);
    const d = draft[key][name];
    const base = `${id}-${key}-${name}`;
    const lo = el("input", { type: "range", id: `${base}-min`, min: 0, max: 1, step: 0.05, value: d.min, "aria-label": `${meta.name} minimum` });
    const hi = el("input", { type: "range", id: `${base}-max`, min: 0, max: 1, step: 0.05, value: d.max, "aria-label": `${meta.name} maximum` });
    const range = el("output");
    const showIt = () => {
      range.textContent = rangeText(d);
      paint(lo, d.min * 100, d.max * 100);
      lo.classList.toggle("full", d.min <= 0 && d.max >= 1);
    };
    const moved = (which) => () => {
      let a = Number(lo.value);
      let b = Number(hi.value);
      if (a > b) {
        if (which === "lo") a = b;
        else b = a;
        lo.value = String(a);
        hi.value = String(b);
      }
      d.min = a;
      d.max = b;
      showIt();
      changed();
    };
    lo.addEventListener("input", moved("lo"));
    hi.addEventListener("input", moved("hi"));
    showIt();
    return el("div", { class: "dial active", "data-score": name },
      el("div", { class: "dial-head" }, el("span", { class: "dial-emoji", text: meta.icon }), el("span", { text: meta.name }),
        el("span", { class: "dial-desc", text: meta.hint }),
        el("button", { class: "link-btn", type: "button", text: "Remove", "aria-label": `Stop filtering by ${meta.name}`,
          onclick: () => { delete draft[key][name]; renderScores(); changed(); } })),
      el("div", { class: "dial-grid" }, el("label", { text: "Keep" }), el("div", { class: "dual" }, lo, hi), range));
  }
  function renderScores() {
    const rows = [];
    const unused = [];
    for (const [key, kind] of KINDS) {
      for (const name of (key === "tone" ? tax.tones : tax.signals) || []) {
        if (draft[key][name]) rows.push(scoreRow(key, kind, name));
        else unused.push([key, kind, name]);
      }
    }
    const sel = el("select", { class: "text-input topic-rules-add", id: `${id}-add-score`, "aria-label": "Add a tone or signal to filter by" },
      el("option", { value: "", text: "Add a tone or kind of post…" }),
      ...KINDS.map(([key, kind]) => {
        const opts = unused.filter((u) => u[0] === key);
        return opts.length ? el("optgroup", { label: kind === "tone" ? "Tone" : "Kind of post" },
          ...opts.map(([, , name]) => el("option", { value: `${key}:${name}`, text: `${describe(kind, name).icon} ${describe(kind, name).name}` }))) : null;
      }));
    sel.addEventListener("change", () => {
      if (!sel.value) return;
      const [key, name] = sel.value.split(":");
      draft[key][name] = { ...newDial(), max: key === "tone" ? 0.5 : 0.7 };
      renderScores();
      changed();
    });
    scoresBox.replaceChildren(...rows, sel);
  }

  // Cutoffs for particular topics, in place of the ones above.
  const rulesEditor = topicRulesEditor({
    topics,
    names: { tone: tax.tones || [], signals: tax.signals || [] },
    get: () => draft.topicRules,
    own: (key, name) => draft[key][name] || null,
    sure: null,
    noBoost: true,
    changed: () => changed(),
    id: `${id}-rules`,
  });

  const unscored = el("input", { type: "checkbox", id: `${id}-unscored`, checked: draft.dropUnscored });
  unscored.addEventListener("change", () => { draft.dropUnscored = unscored.checked; changed(); });

  function renderAll() {
    renderExcludes();
    renderScores();
    rulesEditor.refresh();
    unscored.checked = draft.dropUnscored;
    changed();
  }

  function changed() {
    const dirty = !same(draft, saved);
    save.disabled = !dirty;
    discard.hidden = !dirty;
    useOwn.hidden = !own;
    if (dirty) status.textContent = "Unsaved changes.";
    else status.textContent = own ? "These are your own filters." : ownWords.status;
  }

  async function put(body, what) {
    save.disabled = true;
    status.textContent = "Saving…";
    let res;
    try {
      res = await fetch(`/api/me/filtered/${encodeURIComponent(feed.rkey)}`, {
        method: "PUT", credentials: "same-origin",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify(body),
      });
    } catch {
      status.textContent = "Couldn't reach the server. Try again.";
      save.disabled = false;
      return false;
    }
    let reply = {};
    try { reply = await res.json(); } catch { /* reported below */ }
    if (!res.ok) {
      status.textContent = typeof reply.message === "string" && reply.message ? reply.message : `Couldn't save (${res.status}). Try again.`;
      save.disabled = false;
      return false;
    }
    status.textContent = what;
    return true;
  }

  save.addEventListener("click", async () => {
    const body = payloadOf(draft);
    if (await put(body, "Saved. The feed uses them within half a minute.")) {
      Object.assign(saved, clone(draft));
      own = true;
      changed();
      status.textContent = "Saved. The feed uses them within half a minute.";
    }
  });
  discard.addEventListener("click", () => { draft = clone(saved); renderAll(); });
  useOwn.addEventListener("click", async () => {
    if (await put(null, ownWords.back)) {
      Object.assign(saved, clone(defaults));
      draft = clone(defaults);
      own = false;
      renderAll();
      status.textContent = ownWords.back;
    }
  });

  renderAll();
  const sourceName = feed.sourceName || "the original feed";
  return el("article", { class: "filtered-feed", "data-feed": feed.rkey },
    el("div", { class: "filtered-head" },
      el("div", {},
        el("h2", { class: "me-h2", text: feed.name }),
        el("p", { class: "me-fine filtered-sub" },
          "Filters ",
          feed.sourceUrl ? el("a", { href: feed.sourceUrl, target: "_blank", rel: "noopener", text: sourceName }) : sourceName,
          feed.description ? ` · ${feed.description}` : "")),
      el("a", { class: "btn", href: feed.url, target: "_blank", rel: "noopener", text: "Open in Bluesky" })),
    el("h3", { class: "filtered-h3", text: "Leave out topics" }),
    el("p", { class: "section-hint", text: "Posts the model thinks are about a topic, when it's more sure than you say." }),
    excludeBox,
    el("h3", { class: "filtered-h3", text: "Leave out by tone or kind of post" }),
    el("p", { class: "section-hint", text: "Keep only posts whose score is in the range: move the right end down to leave out, say, angry or critical posts." }),
    scoresBox,
    el("details", { class: "filtered-rules" },
      el("summary", { text: "Different filters for particular topics" }),
      el("p", { class: "section-hint", text: "For posts about a topic, these replace the tone and kind-of-post filters above, score by score (a subtopic's win over its topic's)." }),
      rulesEditor.node),
    el("label", { class: "me-check filtered-check", for: unscored.id }, unscored,
      " Also leave out posts the model can't judge (not in English, and replies)"),
    el("div", { class: "filtered-actions" }, save, discard, useOwn),
    status,
    el("p", { class: "filtered-left-out-link" },
      el("a", { href: `/filtered/left-out?feed=${encodeURIComponent(feed.rkey)}`, "data-left-out-link": feed.rkey,
        text: "See the posts your filters left out of this feed →" })));
}

// ---------- the page ----------

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (res.status === 401) return { status: 401 };
  if (res.status === 404) return { status: 404 };
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return { status: 200, body: await res.json() };
}

async function main() {
  const problem = setupSignInForm({ endpoint: "/oauth/connect" });
  let state;
  try {
    state = await getJSON("/api/me/filtered");
  } catch {
    show("connect");
    $("signin-notice").textContent = "Couldn't reach the server. Reload to try again.";
    $("signin-notice").hidden = false;
    return;
  }
  if (state.status === 404) {
    show("connect");
    signInOff();
    return;
  }
  if (state.status === 401 || !state.body.connected) {
    show("connect");
    showSignInProblem(problem);
    if (state.status === 200) {
      // Signed in on this site, but not for the filtered feeds (or that sign-in ran out).
      const again = $("connect-again");
      again.textContent = "You're signed in on this site, but not for the filtered feeds yet, or that sign-in has run out. " +
        "Until you sign in for them, they show only a post asking you to.";
      again.hidden = false;
      const me = await getJSON("/api/me").catch(() => null);
      if (me && me.status === 200 && me.body.handle) $("handle").value = me.body.handle;
    }
    return;
  }
  const tax = await fetch("/api/taxonomy?for=filters").then((r) => r.json());
  const me = await getJSON("/api/me").catch(() => null);
  $("who").textContent = me && me.status === 200 && me.body.handle ? "@" + me.body.handle : state.body.did;
  $("feeds").replaceChildren(...state.body.feeds.map((f) => feedCard(f, tax)));
  if (state.body.feeds.length === 0) $("feeds").replaceChildren(el("p", { class: "me-lead", text: "There are no filtered feeds yet." }));
  $("disconnect").addEventListener("click", async () => {
    $("disconnect").disabled = true;
    try {
      const res = await fetch("/oauth/disconnect", { method: "POST", headers: { Accept: "application/json" }, credentials: "same-origin" });
      if (res.ok) {
        location.reload();
        return;
      }
    } catch { /* reported below */ }
    $("connected-notice").textContent = "Couldn't sign out of the filtered feeds. Try again.";
    $("connected-notice").hidden = false;
    $("disconnect").disabled = false;
  });
  show("connected");
}

if (document.getElementById("feeds")) main();
