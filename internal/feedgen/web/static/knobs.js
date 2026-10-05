// The controls on /me for everything about a feed that can be set: how it is mixed, which posts
// it holds, cutoffs and boosts on every tone and signal the model scores, and the numbers of the
// ranking. They are built from what the server says each setting is by default and how far it can
// go, and they read and write a draft (see draft.js); the page decides what to do about a change.

import { el } from "./posts.js";
import { describe, rangeText, boostText, dialActive, newDial } from "./scores.js";
import { dial, gapText, memoryText, windowText, popularityText, RANKING_KEYS } from "./draft.js";
import { topicRulesEditor, topicRulesCount } from "./topic-rules.js";

const ordinal = (n) => n + (["th", "st", "nd", "rd"][n % 100 > 10 && n % 100 < 14 ? 0 : n % 10 < 4 ? n % 10 : 0] || "th");

// A slider can't show a value past its end, and a saved setting may be past where it usually ends
// (the feed's own can be): the end moves out to the value, as far as the server allows.
const uiMax = (v, soft, hard) => Math.min(hard, Math.max(soft, Math.ceil(v)));

function paint(input, from, to) {
  input.style.setProperty("--fill-from", `${from}%`);
  input.style.setProperty("--fill-to", `${to}%`);
}
const place = (input) => ((Number(input.value) - Number(input.min)) / (Number(input.max) - Number(input.min))) * 100;

/**
 * buildKnobs puts the controls in container. ctx says what they work on: info() is what the
 * server said (defaults, limits, names), draft() is the draft being edited (it can be replaced
 * between calls), and changed() is called after a control changes it. The result's sync() shows the
 * draft in every control again, after it was changed from somewhere else.
 */
export function buildKnobs(container, ctx) {
  const syncers = [];
  const defaults = () => ctx.info().defaults;
  const limits = () => ctx.info().limits;
  const base = () => (defaults().ranking && defaults().ranking[ctx.draft().freshness]) || {};

  const edited = () => {
    sync();
    ctx.changed();
  };

  // A slider for one number, with what it says in words, and a way back to the feed's own.
  // off() says whether the slider has no effect for now: it stays, greyed out, and can't be moved.
  function slider({ id, label, min, max, step, get, set, clear, isDefault, fmt, ends, hint, off }) {
    const input = el("input", { type: "range", id, "aria-label": label });
    const out = el("span", { class: "slider-value" });
    const reset = el("button", { class: "link-btn knob-reset", type: "button", text: "reset", "aria-label": `Reset ${label} to the feed's own` });
    const row = el("div", { class: "slider-row knob" },
      el("div", { class: "slider-label" }, el("label", { for: id, text: label }), el("span", { class: "knob-value" }, out, reset)),
      input,
      ends ? el("div", { class: "slider-ends" }, el("span", { text: ends[0] }), el("span", { text: ends[1] })) : null,
      hint ? el("p", { class: "knob-hint", text: hint }) : null);
    const syncOne = () => {
      const v = get();
      input.min = String(typeof min === "function" ? min() : min);
      input.max = String(typeof max === "function" ? max(v) : max);
      input.step = String(step);
      input.value = String(v);
      out.textContent = fmt(v);
      const own = isDefault(v);
      reset.hidden = own;
      row.classList.toggle("changed", !own);
      const idle = off ? off() : false;
      input.disabled = idle;
      row.classList.toggle("idle", idle);
      paint(input, 0, Math.min(100, Math.max(0, place(input))));
    };
    input.addEventListener("input", () => {
      set(Number(input.value));
      edited();
    });
    reset.addEventListener("click", () => {
      clear();
      edited();
    });
    syncers.push(syncOne);
    return row;
  }

  // A slider for one of the feed's own settings.
  const setting = (key, o) =>
    slider({
      id: `knob-${key}`,
      get: () => ctx.draft().settings[key] ?? defaults()[key],
      set: (v) => (ctx.draft().settings[key] = v),
      clear: () => delete ctx.draft().settings[key],
      isDefault: (v) => v === defaults()[key],
      ...o,
    });

  // A slider for one number of the ranking, which starts from what the chosen freshness sets.
  const ranking = (key, o) =>
    slider({
      id: `knob-${key}`,
      get: () => ctx.draft().ranking[key] ?? base()[key],
      set: (v) => (ctx.draft().ranking[key] = v),
      clear: () => delete ctx.draft().ranking[key],
      isDefault: (v) => v === base()[key],
      ...o,
    });

  // ----- fresh or popular -----

  function freshness() {
    const radios = [
      ["popular", "Popular first", "posts lots of people liked"],
      ["balanced", "Balanced", "the default mix"],
      ["fresh", "Newest first", "recent posts, even if few have seen them"],
    ].map(([value, name, hint]) => {
      const input = el("input", { type: "radio", name: "freshness", value, id: `freshness-${value}` });
      input.addEventListener("change", () => {
        if (input.checked) {
          ctx.draft().freshness = value;
          edited();
        }
      });
      return { input, label: el("label", { class: "me-radio" }, input, el("span", {}, el("strong", { text: name }), " " + hint)) };
    });
    syncers.push(() => {
      for (const r of radios) r.input.checked = r.input.value === ctx.draft().freshness;
    });
    return el("fieldset", { class: "me-field" }, el("legend", { text: "Fresh or popular" }),
      el("div", { class: "segments" }, ...radios.map((r) => r.label)));
  }

  // ----- a score's cutoffs and boost -----

  // kind is "tone" or "signal" (what scores.js calls them), key is the draft's name for the set.
  function dialRow(kind, key, name) {
    const meta = describe(kind, name);
    const read = () => ctx.draft()[key][name] || newDial();
    const id = `dial-${key}-${name}`;
    const boost = el("input", { type: "range", id: `${id}-boost`, step: "0.5", "aria-label": `${meta.name} boost` });
    const lo = el("input", { type: "range", id: `${id}-min`, min: "0", max: "1", step: "0.05", "aria-label": `${meta.name} minimum` });
    const hi = el("input", { type: "range", id: `${id}-max`, min: "0", max: "1", step: "0.05", "aria-label": `${meta.name} maximum` });
    const range = el("output");
    const boostOut = el("output");
    const row = el("div", { class: "dial", "data-score": name },
      el("div", { class: "dial-head" }, el("span", { class: "dial-emoji", text: meta.icon }), el("span", { text: meta.name }), el("span", { class: "dial-desc", text: meta.hint })),
      el("div", { class: "dial-grid" },
        el("label", { for: boost.id, text: "Boost" }), boost, boostOut,
        el("label", { text: "Allowed" }), el("div", { class: "dual" }, lo, hi), range));
    const syncOne = () => {
      const d = read();
      const reach = Math.max(3, Math.ceil(Math.abs(d.w)), 0);
      boost.min = String(-reach);
      boost.max = String(reach);
      boost.value = String(d.w);
      lo.value = String(d.min);
      hi.value = String(d.max);
      boostOut.textContent = boostText(d.w);
      range.textContent = rangeText(d);
      row.classList.toggle("active", dialActive(d));
      const p = place(boost);
      paint(boost, Math.min(50, p), Math.max(50, p));
      boost.classList.toggle("neg", d.w < 0);
      paint(lo, Number(lo.value) * 100, Number(hi.value) * 100);
      lo.classList.toggle("full", Number(lo.value) <= 0 && Number(hi.value) >= 1); // untouched: muted
    };
    boost.addEventListener("input", () => {
      dial(ctx.draft(), key, name).w = Number(boost.value);
      edited();
    });
    const moved = (which) => () => {
      let a = Number(lo.value);
      let b = Number(hi.value);
      if (a > b) {
        if (which === "lo") a = b;
        else b = a;
      }
      const d = dial(ctx.draft(), key, name);
      d.min = a;
      d.max = b;
      edited();
    };
    lo.addEventListener("input", moved("lo"));
    hi.addEventListener("input", moved("hi"));
    syncers.push(syncOne);
    return row;
  }

  // ----- groups -----

  // A group of controls that can be folded away and put back to the feed's own together.
  // icon is a picture for the group's heading; changes(draft) is how many of its settings differ
  // from the feed's own, shown next to the heading so a folded group still says it has something set.
  function group({ title, hint, open = true, reset, icon, changes }, ...children) {
    const button = el("button", { class: "link-btn", type: "button", text: "Reset", "aria-label": `Reset ${title.toLowerCase()} to the feed's own` });
    button.addEventListener("click", () => {
      reset(ctx.draft());
      edited();
    });
    const badge = el("span", { class: "knob-changed", hidden: true });
    syncers.push(() => {
      const n = changes ? changes(ctx.draft()) : 0;
      badge.hidden = n === 0;
      badge.textContent = `${n} changed`;
    });
    return el("details", { class: "knob-group", open },
      el("summary", {}, el("span", { class: "knob-icon", "aria-hidden": "true", text: icon || "" }), el("span", { class: "section-title", text: title }), badge),
      el("div", { class: "knob-body" },
        el("div", { class: "knob-head" }, hint ? el("p", { class: "section-hint", text: hint }) : el("span"), button),
        ...children));
  }

  function promoSwitch() {
    const input = el("input", { type: "checkbox", id: "hide-promo" });
    input.addEventListener("change", () => {
      ctx.draft().hidePromo = input.checked;
      edited();
    });
    syncers.push(() => (input.checked = ctx.draft().hidePromo));
    return el("div", { class: "me-field-row" }, el("label", { class: "switch", for: "hide-promo" }, input,
      el("span", { class: "switch-track" }, el("span", { class: "switch-thumb" })),
      el("span", { text: "Hide promotional posts (ads, engagement bait, spam, self-promotion)" })));
  }

  // The switch is worded the way the feed's own rule is (seen posts are left out), so it is on
  // until someone turns it off; the draft keeps the opposite, showSeen, because that is the setting.
  function seenSwitch() {
    const input = el("input", { type: "checkbox", id: "hide-seen" });
    input.addEventListener("change", () => {
      ctx.draft().showSeen = !input.checked;
      edited();
    });
    syncers.push(() => (input.checked = !ctx.draft().showSeen));
    return el("div", { class: "me-field-row" },
      el("label", { class: "switch", for: "hide-seen" }, input,
        el("span", { class: "switch-track" }, el("span", { class: "switch-thumb" })),
        el("span", { text: "Leave out posts I've already seen" })),
      el("p", { class: "knob-hint", text: "On: a post leaves your feed once Bluesky says you saw it, or once it has been sent as many times as \"Repeats\" allows. Off: posts you've seen can come back, so each refresh starts over from the best posts. Posts you liked or reposted, and your own, stay out either way." }));
  }

  // ----- rules for particular topics -----

  function topicRules() {
    const topics = (ctx.info().topics || []).map((t) => ({ path: t.path, name: t.name, broad: t.path.split("/")[0], broadName: t.broad || t.path.split("/")[0] }));
    const editor = topicRulesEditor({
      topics,
      names: { tone: ctx.info().tones || [], signals: ctx.info().signals || [] },
      get: () => ctx.draft().topicRules,
      own: (key, name) => ({ ...(ctx.draft()[key][name] || newDial()) }),
      changed: () => edited(),
      id: "me-rules",
    });
    // The draft can be replaced (saved, reset, reverted): show the one there is now.
    let shown = null;
    syncers.push(() => {
      if (shown !== ctx.draft().topicRules) {
        shown = ctx.draft().topicRules;
        editor.refresh();
      }
    });
    return editor.node;
  }

  const names = (set, kind) => (Array.isArray(set) ? set : []).map((n) => dialRow(kind, kind === "tone" ? "tone" : "signals", n));

  // How many of these settings the draft has set to something other than the feed's own.
  const setCount = (d, keys) => keys.filter((k) => d.settings[k] !== undefined && d.settings[k] !== defaults()[k]).length;
  const dialCount = (dials) => Object.values(dials).filter(dialActive).length;
  const rankingChanges = (d) => RANKING_KEYS.filter((k) => d.ranking[k] !== undefined && d.ranking[k] !== base()[k]).length;

  const info = ctx.info();
  const L = limits();
  container.replaceChildren(
    group({
      title: "How your feed is mixed",
      icon: "🎚️",
      changes: (d) => (d.freshness !== "balanced" ? 1 : 0) + setCount(d, ["authorGap", "halfLifeDays", "lookbackDays", "interests", "minLikes"]),
      reset: (d) => {
        d.freshness = "balanced";
        for (const k of ["authorGap", "halfLifeDays", "lookbackDays", "interests", "minLikes"]) delete d.settings[k];
      },
    },
    freshness(),
    setting("authorGap", {
      label: "Variety of authors", min: 0, max: (v) => uiMax(v, 30, L.authorGap), step: 1, fmt: gapText, ends: ["Allow runs", "Lots of variety"],
    }),
    setting("halfLifeDays", { label: "Memory of your likes", min: 1, max: 30, step: 1, fmt: memoryText, ends: ["Short memory", "Long memory"] }),
    setting("lookbackDays", {
      label: "How far back likes count", min: 1, max: L.lookbackDays, step: 1,
      fmt: (n) => `Only likes from the last ${n} ${n === 1 ? "day" : "days"} count`, ends: ["Recent", "Long ago"],
    }),
    setting("interests", {
      label: "Number of interests", min: 1, max: (v) => uiMax(v, 40, L.interests), step: 1,
      fmt: (n) => `${n} ${n === 1 ? "interest" : "interests"}`, hint: "How many of your topics the feed is made from, strongest first.",
    }),
    setting("minLikes", {
      label: "Likes needed to use your interests", min: 1, max: (v) => uiMax(v, 50, L.minLikes), step: 1,
      fmt: (n) => `${n} liked ${n === 1 ? "post" : "posts"} with topics`, hint: "With fewer, your feed is a mix of every topic until you have liked enough.",
    })),

    group({
      title: "Which posts",
      icon: "🔎",
      changes: (d) => (d.hidePromo ? 1 : 0) + (d.showSeen ? 1 : 0) + setCount(d, ["windowHours", "minTopicProb", "listSize", "maxServes", "minEngagement"]),
      reset: (d) => {
        d.hidePromo = false;
        d.showSeen = false;
        for (const k of ["windowHours", "minTopicProb", "listSize", "maxServes", "minEngagement"]) delete d.settings[k];
      },
    },
    setting("windowHours", {
      label: "How new", min: L.minWindowHours ?? 1, max: L.windowHours, step: 0.5,
      fmt: windowText, ends: ["Only the newest", "As far back as the feed goes"],
      hint: "Older posts are left out, however popular.",
    }),
    setting("minTopicProb", {
      label: "How sure the topic is", min: () => L.minTopicProb, max: Math.max(0.95, L.minTopicProb), step: 0.05,
      fmt: (v) => `The model is at least ${Math.round(v * 100)}% sure`, ends: ["More posts", "Only clear matches"],
    }),
    setting("minEngagement", {
      label: "How much others have reacted", min: 0, max: (v) => uiMax(v, 50, L.minEngagement), step: 1,
      fmt: (n) => (n === 0 ? "Posts nobody has reacted to yet are fine" : `At least ${n} ${n === 1 ? "like's" : "likes'"} worth of reactions`),
      ends: ["Includes brand-new posts", "Only well-liked posts"],
      hint: "Likes, reposts, replies and quotes all count, each at the weight the feed gives it. When your feed runs out of posts like that it gets shorter, instead of filling up with ones nobody has reacted to.",
    }),
    setting("listSize", {
      label: "Length of your feed", min: 10, max: L.listSize, step: 5, fmt: (n) => `${n} posts at a time`,
      hint: "Your feed is built again each time you pull to refresh.",
    }),
    seenSwitch(),
    setting("maxServes", {
      label: "Repeats", min: 1, max: 10, step: 1, fmt: (n) => (n === 1 ? "Never shown twice" : `Shown at most ${n} times`),
      hint: "A post counts as seen once Bluesky says you saw it, or once it has been sent this many times.",
      off: () => ctx.draft().showSeen,
    }),
    promoSwitch()),

    group({
      title: "Tone", hint: "How each post comes across. Boost moves posts up or down; Allowed drops posts outside the range.",
      icon: "🎭", changes: (d) => dialCount(d.tone), open: dialCount(ctx.draft().tone) > 0,
      reset: (d) => (d.tone = {}),
    }, ...names(info.tones, "tone")),

    group({
      title: "Quality signals", hint: "Scores from 0 to 100% for what a post is like. They don't add up to 100%.",
      icon: "📊", changes: (d) => dialCount(d.signals), open: dialCount(ctx.draft().signals) > 0,
      reset: (d) => (d.signals = {}),
    }, ...names(info.signals, "signal")),

    group({
      title: "Rules for particular topics",
      hint: "Give a topic its own tone or signal setting, e.g. allow critical posts about politics but not about your hobbies. A post counts as about its most likely subtopic; a subtopic's rules win over its broad topic's, and scores a topic doesn't set follow the settings above.",
      icon: "🗂️", changes: (d) => topicRulesCount(d.topicRules), open: topicRulesCount(ctx.draft().topicRules) > 0,
      reset: (d) => (d.topicRules = {}),
    }, topicRules()),

    group({
      title: "Ranking", hint: "Each post scores (engagement and a quality prior) divided by its age. These are the numbers.",
      icon: "⚙️", changes: rankingChanges, open: rankingChanges(ctx.draft()) > 0,
      reset: (d) => (d.ranking = {}),
    },
    ranking("gravity", {
      label: "How fast old posts sink", min: 0, max: (v) => uiMax(v, 5, L.gravity), step: 0.1, fmt: (v) => Number(v.toFixed(1)).toString(),
      ends: ["Popular posts stay up", "Newest first"],
    }),
    ranking("engagementPower", {
      label: "How much popularity counts", min: L.minEngagementPower ?? 0.2, max: 1, step: 0.05, fmt: popularityText,
      ends: ["A little", "Fully"],
      hint: "Lower it so a post with thousands of likes can't stay on top for hours.",
    }),
    ranking("freshEvery", {
      label: "A brand-new post in every", min: 0, max: (v) => uiMax(v, 20, L.freshEvery), step: 1,
      fmt: (n) => (n === 0 ? "off" : `${ordinal(n)} slot`), ends: ["Off", "Rarely"],
    }),
    ranking("promoPenalty", {
      label: "Promotional penalty", min: 0, max: (v) => uiMax(v, 5, L.promoPenalty), step: 0.5, fmt: (v) => String(v),
      hint: "How much a post that is promotion of any kind is held back.",
    }),
    ...["like", "repost", "reply", "quote"].map((k) => ranking(k, {
      label: `${k[0].toUpperCase()}${k.slice(1)}s count for`, min: 0, max: (v) => uiMax(v, 10, L.engagement), step: 0.5, fmt: (v) => `×${v}`,
    }))));

  function sync() {
    for (const s of syncers) s();
  }
  sync();
  return { sync };
}
