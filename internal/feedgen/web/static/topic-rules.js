// Tone and signal rules for particular topics, on the feed builder (/) and the page at /me. A feed
// has rules of its own for every post; these replace them, score by score, for posts about one
// topic: a broad topic, or a subtopic, whose rules win over its broad topic's. (See TopicRules in
// topic_rules.go.)
//
// On the page, a topic's rules are dials: {topicPath: {tone: {name: dial}, signals: {name: dial}}},
// a dial being a score's cutoffs and boost ({min, max, w}, as in scores.js). Every score with a
// dial is the topic's, even one that cuts nothing and boosts nothing: that one lifts the feed's
// own rule for the topic. So what is sent always names it, with a maximum (1 lets every post by).

import { el } from "./posts.js";
import { describe, rangeText, boostText, newDial } from "./scores.js";

const KINDS = [["tone", "tone"], ["signals", "signal"]]; // the key of a set, and what scores.js calls it

/** topicRulesPayload is the dials as the server takes them; topics without a dial are left out. */
export function topicRulesPayload(rules) {
  const out = {};
  const fine = (v) => typeof v === "number" && Number.isFinite(v);
  for (const [path, sets] of Object.entries(rules && typeof rules === "object" ? rules : {})) {
    const t = {};
    for (const [key] of KINDS) {
      const dials = sets && typeof sets[key] === "object" && sets[key] ? sets[key] : {};
      const r = { max: {}, min: {}, weights: {} };
      for (const [name, d] of Object.entries(dials)) {
        if (!d || typeof d !== "object") continue;
        r.max[name] = fine(d.max) ? d.max : 1; // always: it says the score is the topic's
        if (fine(d.min) && d.min > 0) r.min[name] = d.min;
        if (fine(d.w) && d.w !== 0) r.weights[name] = d.w;
      }
      for (const k of Object.keys(r)) if (Object.keys(r[k]).length === 0) delete r[k];
      if (Object.keys(r).length > 0) t[key] = r;
    }
    if (Object.keys(t).length > 0) out[path] = t;
  }
  return out;
}

const num = (v) => typeof v === "number" && Number.isFinite(v);

/** topicRulesFrom is the dials of rules as the server keeps them. */
export function topicRulesFrom(payload) {
  const out = {};
  if (!payload || typeof payload !== "object") return out;
  for (const [path, sets] of Object.entries(payload)) {
    if (!sets || typeof sets !== "object") continue;
    const t = { tone: {}, signals: {} };
    for (const [key] of KINDS) {
      const r = sets[key] && typeof sets[key] === "object" ? sets[key] : {};
      const names = new Set([...Object.keys(r.max || {}), ...Object.keys(r.min || {}), ...Object.keys(r.weights || {})]);
      for (const name of names) {
        const d = newDial();
        if (num((r.min || {})[name])) d.min = r.min[name];
        if (num((r.max || {})[name])) d.max = r.max[name];
        if (num((r.weights || {})[name])) d.w = r.weights[name];
        t[key][name] = d;
      }
    }
    out[path] = t;
  }
  return out;
}

/** topicRulesCount is how many topics have at least one score of their own. */
export function topicRulesCount(rules) {
  return Object.keys(topicRulesPayload(rules)).length;
}

function paint(input, from, to) {
  input.style.setProperty("--fill-from", `${from}%`);
  input.style.setProperty("--fill-to", `${to}%`);
}

/**
 * topicRulesEditor makes the controls. o says what they work on:
 *   topics: [{path, name, broad, broadName}], every subtopic that can have rules (its broad topic
 *     can too, as "All of <broadName>");
 *   names: {tone: [...], signals: [...]}, the scores;
 *   get(): the dials, an object the editor changes in place;
 *   own(key, name): the feed's own dial for a score, which a topic's starts from;
 *   changed(): called after every change;
 *   id: what the controls' ids start with.
 * The result has the element (node) and refresh(), which shows get() again after it was replaced.
 */
export function topicRulesEditor(o) {
  const node = el("div", { class: "topic-rules" });
  const label = (path) => {
    const t = o.topics.find((x) => x.path === path);
    if (t) return { title: t.name, sub: t.broadName };
    const b = o.topics.find((x) => x.broad === path);
    return b ? { title: `All of ${b.broadName}`, sub: "every subtopic without rules of its own" } : { title: path, sub: "" };
  };
  const slug = (s) => s.replace(/[^a-z0-9]+/gi, "-");

  function dialRow(path, key, kind, name) {
    const meta = describe(kind, name);
    const d = o.get()[path][key][name];
    const id = `${o.id}-${slug(path)}-${key}-${name}`;
    const reach = Math.max(3, Math.ceil(Math.abs(d.w)));
    const boost = el("input", { type: "range", id: `${id}-boost`, min: -reach, max: reach, step: 0.5, value: d.w, "aria-label": `${meta.name} boost for ${label(path).title}` });
    const lo = el("input", { type: "range", id: `${id}-min`, min: 0, max: 1, step: 0.05, value: d.min, "aria-label": `${meta.name} minimum for ${label(path).title}` });
    const hi = el("input", { type: "range", id: `${id}-max`, min: 0, max: 1, step: 0.05, value: d.max, "aria-label": `${meta.name} maximum for ${label(path).title}` });
    const boostOut = el("output");
    const range = el("output");
    const show = () => {
      boostOut.textContent = boostText(d.w);
      range.textContent = rangeText(d);
      const p = ((d.w + reach) / (2 * reach)) * 100;
      paint(boost, Math.min(50, p), Math.max(50, p));
      boost.classList.toggle("neg", d.w < 0);
      paint(lo, d.min * 100, d.max * 100);
      lo.classList.toggle("full", d.min <= 0 && d.max >= 1);
    };
    boost.addEventListener("input", () => { d.w = Number(boost.value); show(); o.changed(); });
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
      show();
      o.changed();
    };
    lo.addEventListener("input", moved("lo"));
    hi.addEventListener("input", moved("hi"));
    show();
    const remove = el("button", { class: "link-btn", type: "button", text: "Remove", "aria-label": `Stop ${meta.name} having a rule of its own for ${label(path).title}`,
      onclick: () => { delete o.get()[path][key][name]; render(); o.changed(); } });
    return el("div", { class: "dial active", "data-topic": path, "data-score": name },
      el("div", { class: "dial-head" }, el("span", { class: "dial-emoji", text: meta.icon }), el("span", { text: meta.name }),
        el("span", { class: "dial-desc", text: meta.hint }), remove),
      el("div", { class: "dial-grid" },
        el("label", { for: boost.id, text: "Boost" }), boost, boostOut,
        el("label", { text: "Allowed" }), el("div", { class: "dual" }, lo, hi), range));
  }

  function card(path) {
    const sets = o.get()[path];
    const { title, sub } = label(path);
    const rows = [];
    const unused = [];
    for (const [key, kind] of KINDS) {
      for (const name of o.names[key] || []) {
        if (sets[key][name]) rows.push(dialRow(path, key, kind, name));
        else unused.push([key, kind, name]);
      }
    }
    const add = el("select", { class: "text-input topic-rules-add", "aria-label": `Add a score with a rule of its own for ${title}` },
      el("option", { value: "", text: "Add a tone or signal…" }),
      ...KINDS.map(([key, kind]) => {
        const opts = unused.filter((u) => u[0] === key);
        return opts.length ? el("optgroup", { label: kind === "tone" ? "Tone" : "Quality signals" },
          ...opts.map(([, , name]) => el("option", { value: `${key}:${name}`, text: `${describe(kind, name).icon} ${describe(kind, name).name}` }))) : null;
      }));
    add.addEventListener("change", () => {
      if (!add.value) return;
      const [key, name] = add.value.split(":");
      sets[key][name] = { ...newDial(), ...(o.own(key, name) || {}) };
      render();
      o.changed();
    });
    return el("div", { class: "topic-rule", "data-topic": path },
      el("div", { class: "topic-rule-head" },
        el("div", {}, el("strong", { text: title }), sub ? el("span", { class: "topic-rule-sub", text: sub }) : null),
        el("button", { class: "link-btn", type: "button", text: "Remove topic", "aria-label": `Remove the rules for ${title}`,
          onclick: () => { delete o.get()[path]; render(); o.changed(); } })),
      rows.length ? rows : el("p", { class: "section-hint", text: "Add a tone or signal to give this topic its own rule for it." }),
      unused.length ? add : null);
  }

  function picker() {
    const have = o.get();
    const groups = new Map();
    for (const t of o.topics) {
      if (!groups.has(t.broad)) groups.set(t.broad, { name: t.broadName, subs: [] });
      groups.get(t.broad).subs.push(t);
    }
    const sel = el("select", { class: "text-input topic-rules-add", id: `${o.id}-add-topic`, "aria-label": "Add a topic with rules of its own" },
      el("option", { value: "", text: "Add a topic…" }),
      ...[...groups.entries()].sort((a, b) => a[1].name.localeCompare(b[1].name)).map(([broad, g]) =>
        el("optgroup", { label: g.name },
          have[broad] ? null : el("option", { value: broad, text: `All of ${g.name}` }),
          ...g.subs.filter((t) => !have[t.path]).sort((a, b) => a.name.localeCompare(b.name)).map((t) => el("option", { value: t.path, text: t.name })))));
    sel.addEventListener("change", () => {
      if (!sel.value) return;
      o.get()[sel.value] = { tone: {}, signals: {} };
      render();
      o.changed();
    });
    return sel;
  }

  function render() {
    const rules = o.get();
    // Subtopics after their broad topic, broad topics by name.
    const paths = Object.keys(rules).sort((a, b) => {
      const [ab, bb] = [a.split("/")[0], b.split("/")[0]];
      if (ab !== bb) return label(ab).title.localeCompare(label(bb).title);
      return (a.includes("/") ? 1 : 0) - (b.includes("/") ? 1 : 0) || label(a).title.localeCompare(label(b).title);
    });
    node.replaceChildren(...paths.map(card), picker());
  }

  render();
  return { node, refresh: render };
}
