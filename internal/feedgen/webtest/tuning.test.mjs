// Browser-level tests of the controls on /me for tuning your feed: sliders for each interest,
// adding topics, every other setting of the feed, the bar for saving, and the preview. Run with the
// others (see me.test.mjs); the Go test TestMePageScript runs them when TOPICFEED_JSDOM is set.

import { test } from "node:test";
import assert from "node:assert/strict";
import { open, interestsBody, interest, signedIn, tuningBody, previewBody, previewPost, bskyPost } from "./harness.mjs";

const AI = "technology/ai";
const CATS = "animals_nature/cats";
const BASEBALL = "sports/baseball";
const BAKING = "food/baking";

// What the person's likes say: AI and cats.
const likes = (over = {}) =>
  interestsBody({
    interests: [
      interest({ path: AI, name: "AI", broad: "Technology", share: 0.6, tunedShare: 0.6 }),
      interest({ path: CATS, name: "Cats", broad: "Animals and nature", share: 0.4, tunedShare: 0.4 }),
    ],
    ...over,
  });

const doc = (p) => p.win.document;
const rowOf = (p, path) => [...doc(p).querySelectorAll("#interest-list > li")].find((li) => li.dataset.path === path);
const sliderOf = (p, path) => rowOf(p, path).querySelector("input");
const callsTo = (p, method, url) => p.calls.filter((c) => c.method === method && c.url === url);
const previews = (p) => callsTo(p, "POST", "/api/me/preview");
const lastPreview = (p) => previews(p).at(-1).json;
const barState = (p) => (p.$("savebar").hidden ? "hidden" : p.$("savebar").dataset.state);
const busy = (p) => p.$("preview").getAttribute("aria-busy") === "true";
const knob = (p, key) => p.$(`knob-${key}`);
const knobRow = (p, key) => knob(p, key).closest(".knob");
const knobText = (p, key) => knobRow(p, key).querySelector(".slider-value").textContent;
const dialInput = (p, key, name, which) => p.$(`dial-${key}-${name}-${which}`);

function slide(p, input, value) {
  input.value = String(value);
  input.dispatchEvent(new p.win.Event("input", { bubbles: true }));
}
function pick(p, el, value) {
  el.value = value;
  el.dispatchEvent(new p.win.Event("change", { bubbles: true }));
}
function choose(p, radio) {
  radio.checked = true;
  radio.dispatchEvent(new p.win.Event("change", { bubbles: true }));
}
const fresh = (p, value) => choose(p, p.$(`freshness-${value}`));
function deferred() {
  let resolve;
  const promise = new Promise((r) => (resolve = r));
  return { promise, resolve };
}

// settled waits until the preview of the latest settings has arrived (or failed).
const settled = (p) => p.until(() => !busy(p), "the preview to settle");

// openTuned opens the page signed in, and waits until the controls and the first preview are there.
async function openTuned(opts = {}) {
  const p = await open({ api: signedIn, interests: { status: 200, body: likes() }, ...opts });
  await p.until(() => doc(p).querySelectorAll("#interest-list input").length > 0 && !p.$("tuning-controls").hidden, "the controls");
  await p.until(() => previews(p).length > 0 && !busy(p), "the first preview");
  return p;
}

// bskyFor answers Bluesky's public API with a post view for each of these posts (that it is asked for).
const bskyFor = (posts, over = {}) => (call) => {
  const asked = new URL(call.url).searchParams.getAll("uris");
  return { status: 200, body: { posts: posts.filter((p) => asked.includes(p.uri)).map((p) => bskyPost(p, over)) } };
};

// ---------- loading ----------

test("tuning: asking for what the controls need, once, and previewing what is saved", async () => {
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: { topics: { [AI]: 0 } } }) } });
  assert.equal(callsTo(p, "GET", "/api/me/tuning").length, 1);
  assert.equal(previews(p).length, 1);
  assert.deepEqual(lastPreview(p), { topics: { [AI]: 0 } }, "the first preview is of the saved tuning");
  assert.equal(previews(p)[0].headers["Content-Type"], "application/json");
  assert.equal(barState(p), "hidden", "nothing has been changed");
  assert.equal(p.$("tuning-loading").hidden, true);
});

test("tuning: the interests are shown even while the controls are still loading, and gain sliders when they arrive", async () => {
  const late = deferred();
  const p = await open({ api: signedIn, interests: { status: 200, body: likes() }, tuning: () => late.promise });
  await p.until(() => !p.$("interests-body").hidden, "the interests");
  assert.equal(doc(p).querySelectorAll("#interest-list > li").length, 2);
  assert.equal(doc(p).querySelectorAll("#interest-list input").length, 0);
  assert.equal(p.$("tuning-controls").hidden, true);
  assert.equal(p.$("tuning-loading").hidden, false);
  assert.equal(p.$("add-topic-row").hidden, true);
  late.resolve({ status: 200, body: tuningBody() });
  await p.until(() => doc(p).querySelectorAll("#interest-list input").length === 2, "the sliders");
  assert.equal(p.$("tuning-controls").hidden, false);
  assert.equal(p.$("tuning-loading").hidden, true);
});

test("tuning: if the controls can't be loaded, the page says so, still shows the interests, and trying again works", async () => {
  const cases = [
    ["unavailable", { status: 503, body: { error: "unavailable" } }, /Couldn't do that just now/],
    ["rate limited", { status: 429, body: { error: "limited" } }, /being asked a lot/],
    ["the request failing", new TypeError("Failed to fetch"), /Couldn't reach the server/],
    ["an answer that isn't what was asked for", { status: 200, body: { nothing: true } }, /Couldn't do that just now/],
    ["an answer that isn't JSON", { status: 200 }, /Couldn't do that just now/],
  ];
  for (const [name, failure, want] of cases) {
    let n = 0;
    const p = await open({ api: signedIn, interests: { status: 200, body: likes() }, tuning: () => (++n === 1 ? failure : { status: 200, body: tuningBody() }) });
    await p.until(() => !p.$("tuning-error").hidden, `${name}: the error`);
    assert.match(p.$("tuning-error-text").textContent, want, name);
    assert.equal(p.$("tuning-controls").hidden, true, name);
    assert.equal(p.$("tuning-loading").hidden, true, name);
    assert.equal(previews(p).length, 0, `${name}: nothing is previewed without settings to preview`);
    await p.until(() => !p.$("interests-body").hidden, `${name}: the interests`);
    assert.equal(doc(p).querySelectorAll("#interest-list > li").length, 2, name);
    assert.equal(doc(p).querySelectorAll("#interest-list input").length, 0, name);
    doc(p).getElementById("tuning-retry").click();
    await p.until(() => !p.$("tuning-controls").hidden, `${name}: trying again`);
    assert.equal(p.$("tuning-error").hidden, true, name);
    await p.until(() => doc(p).querySelectorAll("#interest-list input").length === 2, `${name}: the sliders`);
  }
});

test("tuning: a session that ended is noticed when the controls are loaded", async () => {
  const p = await open({ api: signedIn, tuning: { status: 401, body: { error: "not signed in" } } });
  await p.until(() => p.reloads.length > 0, "the reload");
  assert.equal(previews(p).length, 0);
});

// ---------- an interest's slider ----------

test("tuning: moving a slider changes a draft: the bar offers to save it, and the preview shows it", async () => {
  const p = await openTuned();
  slide(p, sliderOf(p, AI), 0);
  assert.equal(rowOf(p, AI).querySelector(".weight-value").textContent, "Muted");
  assert.equal(sliderOf(p, AI).classList.contains("neg"), true);
  assert.equal(barState(p), "dirty");
  assert.match(p.$("save-text").textContent, /changes that aren't saved yet/);
  assert.equal(p.$("save").hidden, false);
  assert.equal(p.$("discard").hidden, false);
  assert.equal(busy(p), true, "the preview says it is updating");
  assert.match(p.$("preview-status").textContent, /Updating the preview/);
  await settled(p);
  assert.deepEqual(lastPreview(p), { topics: { [AI]: 0 } });
  assert.equal(callsTo(p, "PUT", "/api/me/tuning").length, 0, "nothing is saved by moving a slider");
  assert.equal(p.$("preview-status").hidden, true);
});

test("tuning: every stop of a slider is a weight the server accepts, and says what it does", async () => {
  const p = await openTuned();
  const want = [[0, "Muted"], [0.25, "A lot less (×¼)"], [0.5, "Less (×½)"], [2, "More (×2)"], [3, "A lot more (×3)"], [5, "Much more (×5)"]];
  for (const [weight, text] of want) {
    slide(p, sliderOf(p, AI), [0, 0.25, 0.5, 1, 2, 3, 5].indexOf(weight));
    assert.equal(rowOf(p, AI).querySelector(".weight-value").textContent, text);
    await settled(p);
    assert.deepEqual(lastPreview(p), { topics: { [AI]: weight } });
  }
  slide(p, sliderOf(p, AI), 3);
  assert.equal(rowOf(p, AI).querySelector(".weight-value").textContent, "As you like it");
});

test("tuning: putting a topic back as it was is no change, and sends nothing for it", async () => {
  const p = await openTuned();
  slide(p, sliderOf(p, AI), 5);
  await settled(p);
  assert.equal(barState(p), "dirty");
  slide(p, sliderOf(p, AI), 3);
  assert.equal(barState(p), "hidden", "the same as what is saved: nothing to save");
  await settled(p);
  assert.deepEqual(lastPreview(p), {}, "a weight of 1 on a topic from their likes says nothing");
});

test("tuning: a saved weight of 1 on a topic from the likes is no setting at all", async () => {
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: { topics: { [AI]: 1 } } }) } });
  assert.deepEqual(lastPreview(p), {}, "it changes nothing, so nothing is sent");
  assert.equal(barState(p), "hidden");
  slide(p, sliderOf(p, AI), 0);
  slide(p, sliderOf(p, AI), 3);
  assert.equal(barState(p), "hidden", "putting it back is still no change");
});

test("tuning: moving things quickly sends one preview, of the latest settings", async () => {
  const p = await openTuned();
  const before = previews(p).length;
  for (const i of [0, 1, 2, 4, 5]) slide(p, sliderOf(p, AI), i);
  await settled(p);
  await new Promise((r) => setTimeout(r, 40));
  assert.equal(previews(p).length, before + 1);
  assert.deepEqual(lastPreview(p), { topics: { [AI]: 3 } });
});

test("tuning: an answer for settings that have since changed never replaces a newer one", async () => {
  const slow = deferred();
  let n = 0;
  const newer = previewPost({ text: "new settings" });
  const preview = () => (++n === 1 ? slow.promise : { status: 200, body: previewBody([newer]) });
  const p = await open({ api: signedIn, interests: { status: 200, body: likes() }, preview, abortable: false });
  await p.until(() => doc(p).querySelectorAll("#interest-list input").length > 0 && n === 1, "the first preview to be asked for");
  slide(p, sliderOf(p, AI), 0);
  await p.until(() => doc(p).querySelectorAll(".preview-item").length === 1, "the newer preview");
  assert.equal(doc(p).querySelector(".preview-item").textContent.includes("new settings"), true);
  slow.resolve({ status: 200, body: previewBody([previewPost({ text: "old settings" })]) });
  await new Promise((r) => setTimeout(r, 30));
  assert.equal(doc(p).querySelector(".preview-item").textContent.includes("new settings"), true, "the late answer was dropped");
  assert.equal(doc(p).querySelectorAll(".preview-item").length, 1);
  assert.equal(busy(p), false);
});

test("tuning: a request for settings that have since changed is abandoned", async () => {
  const slow = deferred();
  let n = 0;
  const p = await open({ api: signedIn, interests: { status: 200, body: likes() }, preview: () => (++n === 1 ? slow.promise : { status: 200, body: previewBody() }) });
  await p.until(() => n === 1, "the first preview to be asked for");
  const first = previews(p)[0];
  slide(p, sliderOf(p, AI), 0);
  await p.until(() => n === 2, "the second preview");
  assert.equal(first.signal.aborted, true);
  slow.resolve({ status: 200, body: previewBody([previewPost({ text: "an answer for stale settings" })]) });
  await settled(p);
  assert.equal(doc(p).body.textContent.includes("an answer for stale settings"), false);
});

// ---------- saving ----------

test("tuning: saving sends the whole tuning, says it is saving, then that it is saved, and reads the interests again", async () => {
  const saving = deferred();
  let reads = 0;
  const again = deferred();
  const p = await openTuned({
    save: () => saving.promise,
    interests: () => (++reads === 1 ? { status: 200, body: likes() } : again.promise),
  });
  slide(p, sliderOf(p, AI), 6);
  fresh(p, "fresh");
  slide(p, knob(p, "authorGap"), 4);
  await settled(p);
  p.$("save").click();
  assert.equal(barState(p), "saving");
  assert.equal(p.$("save-text").textContent, "Saving…");
  assert.equal(p.$("save").disabled, true, "no second save while one is on its way");
  assert.equal(p.$("discard").disabled, true);
  p.$("save").click(); // ignored: the button is disabled
  p.$("save").dispatchEvent(new p.win.MouseEvent("click", { bubbles: true, cancelable: true })); // and so is anything else that reaches it
  const puts = callsTo(p, "PUT", "/api/me/tuning");
  assert.equal(puts.length, 1);
  assert.deepEqual(puts[0].json, { topics: { [AI]: 5 }, freshness: "fresh", authorGap: 4 });
  assert.equal(puts[0].headers["Content-Type"], "application/json");

  saving.resolve({ status: 200, body: { tuning: puts[0].json } });
  await p.until(() => barState(p) === "saved", "the bar to say it is saved");
  assert.match(p.$("save-text").textContent, /^Saved\./);
  assert.equal(p.$("save").hidden, true);
  assert.equal(p.$("discard").hidden, true);
  // The interests are read again with the tuning applied, and while that is going on the list
  // stays where it was instead of going back to "Reading your likes…".
  await p.until(() => reads === 2, "the interests to be read again");
  assert.equal(p.$("interests-loading").hidden, true);
  assert.equal(p.$("interests-body").hidden, false);
  assert.equal(doc(p).querySelectorAll("#interest-list > li").length, 2);
  again.resolve({
    status: 200,
    body: likes({ interests: [interest({ path: AI, name: "AI", share: 0.6, weight: 5, tunedShare: 0.9 }), interest({ path: CATS, name: "Cats", share: 0.4, tunedShare: 0.1 })] }),
  });
  await p.until(() => rowOf(p, AI).querySelector(".interest-share").textContent.includes("90% of your feed"), "the new shares");
  assert.equal(rowOf(p, AI).querySelector("input").value, "6", "the slider is where it was left");
  assert.equal(knob(p, "authorGap").value, "4", "so are the settings");
  assert.equal(barState(p), "saved");
});

test("tuning: a change after saving brings the bar back, and a change made while saving is still unsaved after", async () => {
  const saving = deferred();
  const p = await openTuned({ save: () => saving.promise });
  slide(p, sliderOf(p, AI), 0);
  p.$("save").click();
  slide(p, sliderOf(p, AI), 1); // changed again before the save is done
  saving.resolve({ status: 200, body: { tuning: { topics: { [AI]: 0 } } } });
  await p.until(() => barState(p) === "dirty", "the bar to ask again");
  assert.match(p.$("save-text").textContent, /aren't saved yet/);
  assert.equal(p.$("save").disabled, false);
  await settled(p);
  assert.deepEqual(lastPreview(p), { topics: { [AI]: 0.25 } });
});

test("tuning: every way saving can go wrong is said in words, the draft is kept, and saving again works", async () => {
  const cases = [
    ["a setting that can't be used", { status: 400, body: { error: "invalid", message: "the weight for technology/ai must be between 0 and 5" } }, /Those settings can't be used: the weight for technology\/ai must be between 0 and 5\./],
    ["rate limited", { status: 429, body: { error: "limited" } }, /being asked a lot/],
    ["unavailable", { status: 503, body: { error: "unavailable" } }, /Couldn't do that just now/],
    ["refused", { status: 403, body: { error: "forbidden" } }, /was refused/],
    ["too big", { status: 413, body: { error: "too_large" } }, /more than can be saved/],
    ["a server error with no body", { status: 500 }, /Couldn't do that just now/],
    ["the request failing", new TypeError("Failed to fetch"), /Couldn't reach the server/],
  ];
  for (const [name, failure, want] of cases) {
    let n = 0;
    let reads = 0;
    const p = await openTuned({
      save: (call) => (++n === 1 ? failure : { status: 200, body: { tuning: call.json } }),
      interests: () => (++reads, { status: 200, body: likes() }),
    });
    slide(p, sliderOf(p, AI), 6);
    await settled(p);
    p.$("save").click();
    await p.until(() => barState(p) === "error", `${name}: the bar to say what went wrong`);
    assert.match(p.$("save-text").textContent, want, name);
    assert.equal(p.$("save").disabled, false, `${name}: can try again`);
    assert.equal(p.$("save").hidden, false, name);
    assert.equal(p.$("discard").hidden, false, name);
    assert.equal(sliderOf(p, AI).value, "6", `${name}: the draft is kept`);
    assert.equal(reads, 1, `${name}: the interests were not read again for a save that didn't happen`);
    assert.equal(p.reloads.length, 0, name);
    p.$("save").click();
    await p.until(() => barState(p) === "saved", `${name}: saving again`);
    assert.equal(callsTo(p, "PUT", "/api/me/tuning").length, 2, name);
  }
});

test("tuning: the complaint about a save that failed goes as soon as something is changed", async () => {
  const p = await openTuned({ save: { status: 503, body: { error: "unavailable" } } });
  slide(p, sliderOf(p, AI), 0);
  p.$("save").click();
  await p.until(() => barState(p) === "error", "the complaint");
  slide(p, sliderOf(p, AI), 1);
  assert.equal(barState(p), "dirty");
  assert.match(p.$("save-text").textContent, /aren't saved yet/);
});

test("tuning: a session that ended while saving starts again from the sign-in form", async () => {
  const p = await openTuned({ save: { status: 401, body: { error: "not signed in" } } });
  slide(p, sliderOf(p, AI), 0);
  p.$("save").click();
  await p.until(() => p.reloads.length > 0, "the reload");
});

test("tuning: if the interests can't be read again after saving, you are told, and the list stays", async () => {
  let reads = 0;
  const p = await openTuned({ interests: () => (++reads === 1 ? { status: 200, body: likes() } : { status: 503, body: { error: "unavailable" } }) });
  slide(p, sliderOf(p, AI), 0);
  p.$("save").click();
  await p.until(() => !p.$("signed-in-notice").hidden, "the notice");
  assert.match(p.$("signed-in-notice").textContent, /Saved, but couldn't read your interests again/);
  assert.equal(p.$("interests-body").hidden, false);
  assert.equal(doc(p).querySelectorAll("#interest-list > li").length, 2);
  assert.equal(barState(p), "saved");
});

test("tuning: discarding goes back to what was saved, in every control", async () => {
  const saved = {
    topics: { [CATS]: 2 }, freshness: "popular", authorGap: 4, halfLifeDays: 14, hidePromo: true, windowHours: 12,
    ranking: { gravity: 2.5 }, tone: { max: { outraged: 0.4 } }, signals: { min: { substance: 0.3 }, weights: { news: 1 } },
  };
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: saved }) } });
  slide(p, sliderOf(p, AI), 0);
  slide(p, sliderOf(p, CATS), 6);
  fresh(p, "fresh");
  slide(p, knob(p, "authorGap"), 20);
  slide(p, knob(p, "halfLifeDays"), 3);
  slide(p, knob(p, "gravity"), 0.5);
  p.$("hide-promo").checked = false;
  p.$("hide-promo").dispatchEvent(new p.win.Event("change", { bubbles: true }));
  slide(p, dialInput(p, "tone", "outraged", "max"), 1);
  slide(p, dialInput(p, "signals", "substance", "min"), 0);
  slide(p, dialInput(p, "signals", "news", "boost"), -2);
  await settled(p);
  assert.equal(barState(p), "dirty");
  p.$("discard").click();
  assert.equal(barState(p), "hidden");
  assert.equal(sliderOf(p, AI).value, "3");
  assert.equal(sliderOf(p, CATS).value, "4");
  assert.equal(doc(p).querySelector('input[name="freshness"]:checked').value, "popular");
  assert.equal(knob(p, "authorGap").value, "4");
  assert.equal(knob(p, "halfLifeDays").value, "14");
  assert.equal(knob(p, "gravity").value, "2.5");
  assert.equal(p.$("hide-promo").checked, true);
  assert.equal(dialInput(p, "tone", "outraged", "max").value, "0.4");
  assert.equal(dialInput(p, "signals", "substance", "min").value, "0.3");
  assert.equal(dialInput(p, "signals", "news", "boost").value, "1");
  await settled(p);
  assert.deepEqual(lastPreview(p), saved);
  assert.equal(callsTo(p, "PUT", "/api/me/tuning").length, 0);
  // What is saved isn't what is being edited: changing the restored settings is a change again.
  slide(p, sliderOf(p, AI), 0);
  assert.equal(barState(p), "dirty");
  p.$("discard").click();
  assert.equal(barState(p), "hidden");
  assert.equal(sliderOf(p, AI).value, "3", "and discarding again restores the same saved settings");
});

test("tuning: resetting everything is a draft, not a save", async () => {
  const saved = { topics: { [AI]: 0 }, freshness: "fresh", hidePromo: true, showSeen: true, authorGap: 3, ranking: { like: 2 }, tone: { weights: { humorous: 2 } } };
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: saved }) } });
  doc(p).getElementById("reset-all").click();
  assert.equal(barState(p), "dirty");
  assert.equal(sliderOf(p, AI).value, "3");
  assert.equal(doc(p).querySelector('input[name="freshness"]:checked').value, "balanced");
  assert.equal(p.$("hide-promo").checked, false);
  assert.equal(p.$("hide-seen").checked, true, "seen posts are left out again");
  assert.equal(knob(p, "authorGap").value, "10");
  assert.equal(knob(p, "like").value, "1");
  assert.equal(dialInput(p, "tone", "humorous", "boost").value, "0");
  await settled(p);
  assert.deepEqual(lastPreview(p), {});
  assert.equal(callsTo(p, "PUT", "/api/me/tuning").length, 0);
  p.$("save").click();
  await p.until(() => barState(p) === "saved", "saved");
  assert.deepEqual(callsTo(p, "PUT", "/api/me/tuning")[0].json, {}, "saving the defaults sends an empty tuning");
  assert.equal(p.$("discard").hidden, true);
});

// ---------- every setting of the feed ----------

const SETTINGS = ["authorGap", "halfLifeDays", "lookbackDays", "minLikes", "interests", "windowHours", "minTopicProb", "maxServes", "listSize", "minEngagement"];
const RANKING = ["gravity", "freshEvery", "promoPenalty", "like", "repost", "reply", "quote", "engagementPower"];

test("knobs: there is a control for every setting, and each starts as the feed's own", async () => {
  const p = await openTuned();
  const d = tuningBody().defaults;
  for (const k of SETTINGS) {
    assert.ok(knob(p, k), `a slider for ${k}`);
    assert.equal(Number(knob(p, k).value), d[k], `${k} starts as the feed's own`);
    assert.equal(knobRow(p, k).classList.contains("changed"), false, k);
    assert.equal(knobRow(p, k).querySelector(".knob-reset").hidden, true, `${k} has nothing to go back to`);
  }
  for (const k of RANKING) {
    assert.ok(knob(p, k), `a slider for ${k}`);
    assert.equal(Number(knob(p, k).value), d.ranking.balanced[k], `${k} starts as the ranking's own`);
    assert.equal(knobRow(p, k).classList.contains("changed"), false, k);
  }
  for (const key of ["tone", "signals"]) {
    for (const name of tuningBody()[key === "tone" ? "tones" : "signals"]) {
      assert.equal(dialInput(p, key, name, "boost").value, "0", `${key} ${name}`);
      assert.equal(dialInput(p, key, name, "min").value, "0", `${key} ${name}`);
      assert.equal(dialInput(p, key, name, "max").value, "1", `${key} ${name}`);
    }
  }
  assert.equal(doc(p).querySelector('input[name="freshness"]:checked').value, "balanced");
  assert.equal(p.$("hide-promo").checked, false);
  assert.deepEqual(lastPreview(p), {});
  assert.equal(barState(p), "hidden");
});

test("knobs: each setting sends only itself, says what it is in words, and goes back to the feed's own", async () => {
  const p = await openTuned();
  const cases = [
    ["authorGap", 4, "At least 4 other posts between two by the same author", (v) => ({ authorGap: v })],
    ["authorGap", 1, "At least 1 other post between two by the same author", (v) => ({ authorGap: v })],
    ["halfLifeDays", 14, "A like counts half as much after 14 days", (v) => ({ halfLifeDays: v })],
    ["halfLifeDays", 1, "A like counts half as much after 1 day", (v) => ({ halfLifeDays: v })],
    ["lookbackDays", 7, "Only likes from the last 7 days count", (v) => ({ lookbackDays: v })],
    ["lookbackDays", 1, "Only likes from the last 1 day count", (v) => ({ lookbackDays: v })],
    ["minLikes", 10, "10 liked posts with topics", (v) => ({ minLikes: v })],
    ["interests", 12, "12 interests", (v) => ({ interests: v })],
    ["windowHours", 6, "Posts from the last 6 hours", (v) => ({ windowHours: v })],
    ["windowHours", 0.5, "Posts from the last 30 minutes", (v) => ({ windowHours: v })],
    ["windowHours", 1.5, "Posts from the last 1.5 hours", (v) => ({ windowHours: v })],
    ["minTopicProb", 0.8, "The model is at least 80% sure", (v) => ({ minTopicProb: v })],
    ["listSize", 100, "100 posts at a time", (v) => ({ listSize: v })],
    ["minEngagement", 12, "At least 12 likes' worth of reactions", (v) => ({ minEngagement: v })],
    ["minEngagement", 1, "At least 1 like's worth of reactions", (v) => ({ minEngagement: v })],
    ["maxServes", 1, "Never shown twice", (v) => ({ maxServes: v })],
    ["maxServes", 5, "Shown at most 5 times", (v) => ({ maxServes: v })],
    ["gravity", 2.5, "2.5", (v) => ({ ranking: { gravity: v } })],
    ["freshEvery", 7, "7th slot", (v) => ({ ranking: { freshEvery: v } })],
    ["promoPenalty", 3, "3", (v) => ({ ranking: { promoPenalty: v } })],
    ["like", 2.5, "×2.5", (v) => ({ ranking: { like: v } })],
    ["repost", 4, "×4", (v) => ({ ranking: { repost: v } })],
    ["reply", 0.5, "×0.5", (v) => ({ ranking: { reply: v } })],
    ["quote", 9, "×9", (v) => ({ ranking: { quote: v } })],
    ["engagementPower", 0.5, "About half: 4× the likes count 2× as much (0.50)", (v) => ({ ranking: { engagementPower: v } })],
    ["engagementPower", 0.2, "A little: big numbers barely matter (0.20)", (v) => ({ ranking: { engagementPower: v } })],
  ];
  for (const [key, value, text, payload] of cases) {
    const own = knob(p, key).value;
    slide(p, knob(p, key), value);
    assert.equal(knobText(p, key), text, `${key} in words`);
    assert.equal(knobRow(p, key).classList.contains("changed"), true, `${key} is marked as changed`);
    const reset = knobRow(p, key).querySelector(".knob-reset");
    assert.equal(reset.hidden, false, `${key} can go back`);
    assert.equal(barState(p), "dirty", key);
    await settled(p);
    assert.deepEqual(lastPreview(p), payload(value), key);
    reset.click();
    assert.equal(knob(p, key).value, own, `${key} goes back to the feed's own`);
    assert.equal(knobRow(p, key).classList.contains("changed"), false, key);
    assert.equal(barState(p), "hidden", `${key}: no change left`);
    await settled(p);
    assert.deepEqual(lastPreview(p), {}, `${key} sends nothing once back`);
  }
});

test("knobs: a setting put back to the feed's own value by hand is no setting", async () => {
  const p = await openTuned();
  slide(p, knob(p, "authorGap"), 4);
  slide(p, knob(p, "authorGap"), 10);
  slide(p, knob(p, "gravity"), 3);
  slide(p, knob(p, "gravity"), 1.8);
  assert.equal(barState(p), "hidden");
  await settled(p);
  assert.deepEqual(lastPreview(p), {});
});

test("knobs: zero is a setting, not the absence of one", async () => {
  const p = await openTuned();
  slide(p, knob(p, "authorGap"), 0);
  assert.match(knobText(p, "authorGap"), /^No spacing/);
  await settled(p);
  assert.deepEqual(lastPreview(p), { authorGap: 0 });
  slide(p, knob(p, "freshEvery"), 0);
  assert.equal(knobText(p, "freshEvery"), "off");
  slide(p, knob(p, "like"), 0);
  assert.equal(knobText(p, "like"), "×0");
  await settled(p);
  assert.deepEqual(lastPreview(p), { authorGap: 0, ranking: { freshEvery: 0, like: 0 } });
});

test("knobs: no minimum of reactions is a setting (zero), sent as zero, and a saved zero is read back as zero", async () => {
  const p = await openTuned();
  assert.equal(knob(p, "minEngagement").value, "5", "starts as the feed's own minimum");
  slide(p, knob(p, "minEngagement"), 0);
  assert.equal(knobText(p, "minEngagement"), "Posts nobody has reacted to yet are fine");
  assert.equal(knobRow(p, "minEngagement").classList.contains("changed"), true);
  await settled(p);
  assert.deepEqual(lastPreview(p), { minEngagement: 0 }, "zero is sent: it is not the absence of a setting");
  knobRow(p, "minEngagement").querySelector(".knob-reset").click();
  assert.equal(knob(p, "minEngagement").value, "5");
  await settled(p);
  assert.deepEqual(lastPreview(p), {}, "back to the feed's own sends nothing");

  const saved = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: { minEngagement: 0 } }) } });
  assert.equal(knob(saved, "minEngagement").value, "0");
  assert.equal(knobText(saved, "minEngagement"), "Posts nobody has reacted to yet are fine");
  assert.equal(knobRow(saved, "minEngagement").classList.contains("changed"), true);
  assert.deepEqual(lastPreview(saved), { minEngagement: 0 });
  assert.equal(barState(saved), "hidden");
});

test("knobs: the numbers of the ranking start from the chosen freshness, and follow it until they are set", async () => {
  const p = await openTuned();
  assert.equal(knob(p, "gravity").value, "1.8");
  assert.equal(knob(p, "freshEvery").value, "4");
  fresh(p, "popular");
  assert.equal(knob(p, "gravity").value, "1.2", "popular sinks old posts less");
  assert.equal(knob(p, "freshEvery").value, "0");
  assert.equal(knobRow(p, "gravity").classList.contains("changed"), false, "that is the feed's own for popular");
  await settled(p);
  assert.deepEqual(lastPreview(p), { freshness: "popular" });
  slide(p, knob(p, "gravity"), 2);
  await settled(p);
  assert.deepEqual(lastPreview(p), { freshness: "popular", ranking: { gravity: 2 } });
  fresh(p, "fresh");
  assert.equal(knob(p, "gravity").value, "2", "a number that was set stays as it is");
  assert.equal(knob(p, "freshEvery").value, "3", "one that wasn't set follows");
  await settled(p);
  assert.deepEqual(lastPreview(p), { freshness: "fresh", ranking: { gravity: 2 } });
  slide(p, knob(p, "gravity"), 3); // fresh's own
  await settled(p);
  assert.deepEqual(lastPreview(p), { freshness: "fresh" });
});

test("knobs: hiding promotional posts", async () => {
  const p = await openTuned();
  p.$("hide-promo").checked = true;
  p.$("hide-promo").dispatchEvent(new p.win.Event("change", { bubbles: true }));
  await settled(p);
  assert.deepEqual(lastPreview(p), { hidePromo: true });
  assert.equal(barState(p), "dirty");
});

const flip = (p, id, on) => {
  p.$(id).checked = on;
  p.$(id).dispatchEvent(new p.win.Event("change", { bubbles: true }));
};

test("knobs: posts you've seen are left out until you turn that off; off is sent as showSeen, and Repeats stops mattering", async () => {
  const p = await openTuned();
  assert.equal(p.$("hide-seen").checked, true, "the feed's own rule leaves seen posts out");
  assert.equal(knob(p, "maxServes").disabled, false);
  assert.equal(knobRow(p, "maxServes").classList.contains("idle"), false);

  flip(p, "hide-seen", false);
  await settled(p);
  assert.deepEqual(lastPreview(p), { showSeen: true });
  assert.equal(barState(p), "dirty");
  assert.equal(knob(p, "maxServes").disabled, true, "how often a post may be sent doesn't apply while seen posts are shown");
  assert.equal(knobRow(p, "maxServes").classList.contains("idle"), true);

  flip(p, "hide-seen", true);
  await settled(p);
  assert.deepEqual(lastPreview(p), {}, "leaving them out again is the feed's own rule: nothing to send");
  assert.equal(barState(p), "hidden");
  assert.equal(knob(p, "maxServes").disabled, false);
});

test("knobs: a saved choice to see seen posts is shown as the switch turned off, and saves as it was", async () => {
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: { showSeen: true, maxServes: 4 } }) } });
  assert.equal(p.$("hide-seen").checked, false);
  assert.equal(knob(p, "maxServes").disabled, true);
  assert.equal(knob(p, "maxServes").value, "4", "the number it had is kept for when seen posts are left out again");
  await settled(p);
  assert.deepEqual(lastPreview(p), { showSeen: true, maxServes: 4 });
  assert.equal(barState(p), "hidden", "nothing changed by looking at it");
  flip(p, "hide-seen", true);
  p.$("save").click();
  await p.until(() => barState(p) === "saved", "saved");
  assert.deepEqual(callsTo(p, "PUT", "/api/me/tuning")[0].json, { maxServes: 4 });
});

test("knobs: cutoffs and boosts on every tone and signal, each sent under its own name", async () => {
  const p = await openTuned();
  const sent = async () => {
    await settled(p);
    return lastPreview(p);
  };
  slide(p, dialInput(p, "tone", "outraged", "max"), 0.4);
  const row = p.$("dial-tone-outraged-max").closest(".dial");
  assert.equal(row.classList.contains("active"), true);
  assert.equal(row.querySelector("output:last-child").textContent, "≤ 40%");
  assert.deepEqual(await sent(), { tone: { max: { outraged: 0.4 } } });

  slide(p, dialInput(p, "tone", "humorous", "boost"), 1.5);
  assert.equal(p.$("dial-tone-humorous-boost").closest(".dial").querySelectorAll("output")[0].textContent, "+1.5");
  assert.deepEqual(await sent(), { tone: { max: { outraged: 0.4 }, weights: { humorous: 1.5 } } });

  slide(p, dialInput(p, "signals", "substance", "min"), 0.3);
  slide(p, dialInput(p, "signals", "spam", "max"), 0.1);
  slide(p, dialInput(p, "signals", "news", "boost"), -2);
  assert.deepEqual(await sent(), {
    tone: { max: { outraged: 0.4 }, weights: { humorous: 1.5 } },
    signals: { min: { substance: 0.3 }, max: { spam: 0.1 }, weights: { news: -2 } },
  });
  assert.equal(dialInput(p, "signals", "news", "boost").classList.contains("neg"), true);

  // A tone and a signal of the same name would be two things: they are kept apart.
  assert.equal(dialInput(p, "tone", "substance", "min"), null);

  // Back to saying nothing.
  slide(p, dialInput(p, "tone", "outraged", "max"), 1);
  slide(p, dialInput(p, "tone", "humorous", "boost"), 0);
  slide(p, dialInput(p, "signals", "substance", "min"), 0);
  slide(p, dialInput(p, "signals", "spam", "max"), 1);
  slide(p, dialInput(p, "signals", "news", "boost"), 0);
  assert.deepEqual(await sent(), {});
  assert.equal(barState(p), "hidden");
  assert.equal(p.$("dial-tone-outraged-max").closest(".dial").classList.contains("active"), false);
});

test("knobs: neither end of a range can be taken past the other", async () => {
  const p = await openTuned();
  slide(p, dialInput(p, "tone", "informative", "max"), 0.4);
  slide(p, dialInput(p, "tone", "informative", "min"), 0.7); // past the maximum: it stops there
  assert.equal(dialInput(p, "tone", "informative", "min").value, "0.4");
  assert.equal(dialInput(p, "tone", "informative", "max").value, "0.4");
  slide(p, dialInput(p, "tone", "informative", "max"), 0.1); // below the minimum: it stops there
  assert.equal(dialInput(p, "tone", "informative", "min").value, "0.4");
  assert.equal(dialInput(p, "tone", "informative", "max").value, "0.4");
  await settled(p);
  assert.deepEqual(lastPreview(p), { tone: { max: { informative: 0.4 }, min: { informative: 0.4 } } });
  slide(p, dialInput(p, "tone", "informative", "min"), 0.2); // and each can move the other way
  assert.equal(dialInput(p, "tone", "informative", "min").value, "0.2");
  assert.equal(dialInput(p, "tone", "informative", "max").value, "0.4");
});

test("knobs: saved cutoffs and boosts are shown as they were saved, and nothing changes by looking at them", async () => {
  const saved = {
    tone: { max: { outraged: 0.4 }, min: { informative: 0.2 }, weights: { supportive: 1.5 } },
    signals: { min: { substance: 0.3 }, max: { spam: 0.1, ad: 0.3 }, weights: { news: -1 } },
  };
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: saved }) } });
  assert.equal(dialInput(p, "tone", "outraged", "max").value, "0.4");
  assert.equal(dialInput(p, "tone", "informative", "min").value, "0.2");
  assert.equal(dialInput(p, "tone", "supportive", "boost").value, "1.5");
  assert.equal(dialInput(p, "signals", "spam", "max").value, "0.1");
  assert.equal(dialInput(p, "signals", "news", "boost").value, "-1");
  assert.equal(doc(p).querySelectorAll(".dial.active").length, 7, "three tones and four signals have something said about them");
  assert.deepEqual(lastPreview(p), saved);
  assert.equal(barState(p), "hidden");
});

test("knobs: a saved boost beyond what the slider usually reaches is still shown", async () => {
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: { tone: { weights: { humorous: 7.5 } } } }) } });
  assert.equal(dialInput(p, "tone", "humorous", "boost").value, "7.5");
  assert.equal(Number(dialInput(p, "tone", "humorous", "boost").max) >= 7.5, true);
  assert.deepEqual(lastPreview(p), { tone: { weights: { humorous: 7.5 } } });
});

test("knobs: a setting past where a slider usually ends is still shown, as far as the server allows", async () => {
  const saved = { authorGap: 45, minLikes: 300, interests: 80, ranking: { gravity: 8, freshEvery: 40, like: 18 } };
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: saved }) } });
  for (const [k, v] of [["authorGap", 45], ["minLikes", 300], ["interests", 80], ["gravity", 8], ["freshEvery", 40], ["like", 18]]) {
    assert.equal(Number(knob(p, k).value), v, k);
    assert.equal(Number(knob(p, k).max) >= v, true, `${k} reaches ${v}`);
  }
  assert.equal(Number(knob(p, "authorGap").max) <= 50, true, "no further than the server allows");
  assert.deepEqual(lastPreview(p), saved);
  assert.equal(barState(p), "hidden");
});

test("knobs: a saved gap of zero is read as a gap of zero", async () => {
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: { authorGap: 0 } }) } });
  assert.equal(knob(p, "authorGap").value, "0");
  assert.match(knobText(p, "authorGap"), /^No spacing/);
  assert.equal(knobRow(p, "authorGap").classList.contains("changed"), true);
  assert.deepEqual(lastPreview(p), { authorGap: 0 });
  assert.equal(barState(p), "hidden");
});

test("knobs: when the server doesn't say how far things can go, the sliders still have ends", async () => {
  const body = tuningBody();
  delete body.limits;
  const p = await openTuned({ tuning: { status: 200, body } });
  assert.equal(knob(p, "authorGap").max, "30");
  assert.equal(knob(p, "listSize").max, "300");
  assert.equal(knob(p, "lookbackDays").max, "30");
  assert.equal(knob(p, "minLikes").max, "50");
});

test("knobs: a slider never goes past what the feed allows, however far it usually reaches", async () => {
  const body = tuningBody();
  body.limits.authorGap = 20;
  body.limits.interests = 15;
  body.limits.freshEvery = 8;
  const p = await openTuned({ tuning: { status: 200, body } });
  assert.equal(knob(p, "authorGap").max, "20");
  assert.equal(knob(p, "interests").max, "15");
  assert.equal(knob(p, "freshEvery").max, "8");
});

test("knobs: the ends of the sliders are what the server says the feed allows", async () => {
  const body = tuningBody();
  body.defaults.windowHours = 12;
  body.limits.windowHours = 12;
  body.defaults.listSize = 200;
  body.limits.listSize = 200;
  body.defaults.minTopicProb = 0.6;
  body.limits.minTopicProb = 0.6;
  const p = await openTuned({ tuning: { status: 200, body } });
  assert.equal(knob(p, "windowHours").max, "12", "no further back than the feed holds posts");
  assert.equal(knob(p, "windowHours").min, "0.5", "down to half an hour");
  assert.equal(knob(p, "engagementPower").min, "0.2");
  assert.equal(knob(p, "engagementPower").max, "1");
  assert.equal(knob(p, "listSize").max, "200", "no longer than the feed's own list");
  assert.equal(knob(p, "minTopicProb").min, "0.6", "no less sure than the feed's own pool");
  assert.equal(knob(p, "lookbackDays").max, "30");
  assert.equal(knob(p, "halfLifeDays").max, "30");
  assert.equal(knob(p, "maxServes").max, "10");
});

test("knobs: each group goes back to the feed's own on its own", async () => {
  const p = await openTuned();
  fresh(p, "fresh");
  slide(p, knob(p, "authorGap"), 2);
  slide(p, knob(p, "windowHours"), 6);
  slide(p, knob(p, "maxServes"), 1);
  p.$("hide-promo").checked = true;
  p.$("hide-promo").dispatchEvent(new p.win.Event("change", { bubbles: true }));
  flip(p, "hide-seen", false);
  slide(p, dialInput(p, "tone", "outraged", "max"), 0.4);
  slide(p, dialInput(p, "signals", "spam", "max"), 0.1);
  slide(p, knob(p, "gravity"), 2);
  await settled(p);
  const group = (title) => [...doc(p).querySelectorAll(".knob-group")].find((g) => g.querySelector(".section-title").textContent === title);
  const reset = (title) => group(title).querySelector(".knob-head .link-btn").click();
  const sent = async () => (await settled(p), lastPreview(p));

  reset("How your feed is mixed");
  assert.deepEqual(await sent(), {
    hidePromo: true, showSeen: true, windowHours: 6, maxServes: 1, tone: { max: { outraged: 0.4 } }, signals: { max: { spam: 0.1 } }, ranking: { gravity: 2 },
  });
  reset("Which posts");
  assert.deepEqual(await sent(), { tone: { max: { outraged: 0.4 } }, signals: { max: { spam: 0.1 } }, ranking: { gravity: 2 } });
  reset("Tone");
  assert.deepEqual(await sent(), { signals: { max: { spam: 0.1 } }, ranking: { gravity: 2 } });
  reset("Quality signals");
  assert.deepEqual(await sent(), { ranking: { gravity: 2 } });
  reset("Ranking");
  assert.deepEqual(await sent(), {});
  assert.equal(barState(p), "hidden");
  assert.equal(group("Ranking").open, false, "the ranking numbers are folded away until wanted");
  assert.equal(group("Tone").open, false, "so are the tone and signal scores, until something in them is set");
  assert.equal(group("How your feed is mixed").open, true);
  assert.equal(group("Which posts").open, true);
});

const groupOf = (p, title) => [...doc(p).querySelectorAll(".knob-group")].find((g) => g.querySelector(".section-title").textContent === title);
const GROUPS = ["How your feed is mixed", "Which posts", "Tone", "Quality signals", "Ranking"];

test("knobs: fresh or popular is three choices side by side in one set (the styling depends on it)", async () => {
  const p = await openTuned();
  const set = doc(p).querySelector("fieldset.me-field .segments");
  assert.ok(set, "the choices sit in a set of their own");
  assert.deepEqual([...set.querySelectorAll('input[name="freshness"]')].map((i) => i.value), ["popular", "balanced", "fresh"]);
  assert.equal(set.querySelectorAll("label.me-radio").length, 3);
});

test("knobs: a group says how many of its settings differ from the feed's own, and drops the count when they are put back", async () => {
  const p = await openTuned();
  const badge = (title) => groupOf(p, title).querySelector(".knob-changed");
  for (const t of GROUPS) assert.equal(badge(t).hidden, true, `${t} has nothing set`);

  fresh(p, "fresh");
  slide(p, knob(p, "authorGap"), 2);
  flip(p, "hide-seen", false);
  flip(p, "hide-promo", true);
  slide(p, dialInput(p, "tone", "outraged", "max"), 0.4);
  slide(p, dialInput(p, "signals", "spam", "max"), 0.1);
  slide(p, dialInput(p, "signals", "ad", "max"), 0.2);
  slide(p, knob(p, "gravity"), 2);
  await settled(p);
  assert.equal(badge("How your feed is mixed").textContent, "2 changed", "fresh, and the gap between authors");
  assert.equal(badge("Which posts").textContent, "2 changed", "seen posts shown, and promotional ones hidden");
  assert.equal(badge("Tone").textContent, "1 changed");
  assert.equal(badge("Quality signals").textContent, "2 changed");
  assert.equal(badge("Ranking").textContent, "1 changed");
  for (const t of GROUPS) assert.equal(badge(t).hidden, false, `${t} has something set`);

  slide(p, knob(p, "authorGap"), 10); // the feed's own again
  assert.equal(badge("How your feed is mixed").textContent, "1 changed");
  groupOf(p, "Quality signals").querySelector(".knob-head .link-btn").click();
  assert.equal(badge("Quality signals").hidden, true, "a group put back to the feed's own has nothing set");
});

test("knobs: saved scores and ranking numbers open their groups, and the others stay folded", async () => {
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: { tone: { max: { outraged: 0.4 } }, ranking: { like: 2 } } }) } });
  assert.equal(groupOf(p, "Tone").open, true);
  assert.equal(groupOf(p, "Ranking").open, true);
  assert.equal(groupOf(p, "Quality signals").open, false);
  assert.equal(groupOf(p, "Tone").querySelector(".knob-changed").textContent, "1 changed");
  assert.equal(groupOf(p, "Ranking").querySelector(".knob-changed").textContent, "1 changed");
  assert.equal(barState(p), "hidden", "nothing changed by looking at it");
});

test("saving: the page says what was saved, and promises that seen posts stay out only when that is so", async () => {
  const p = await openTuned();
  slide(p, knob(p, "authorGap"), 3);
  p.$("save").click();
  await p.until(() => barState(p) === "saved", "saved");
  assert.match(p.$("save-text").textContent, /posts you've already seen stay out of it/);

  flip(p, "hide-seen", false);
  assert.equal(barState(p), "dirty");
  p.$("save").click();
  await p.until(() => barState(p) === "saved", "saved again");
  assert.equal(p.$("save-text").textContent, "Saved. Your feed uses these settings from now on.");
});

// ---------- topics to add ----------

const groups = (p) => [...p.$("add-topic").querySelectorAll("optgroup")].map((g) => [g.label, [...g.children].map((o) => o.textContent)]);

test("tuning: adding a topic you haven't liked: offered by group, added as a draft, and removable", async () => {
  const p = await openTuned();
  assert.equal(p.$("add-topic-row").hidden, false);
  assert.equal(p.$("add-topic").options[0].textContent, "Choose a topic…");
  assert.deepEqual(groups(p), [["Food", ["Baking"]], ["Sports", ["Baseball", "Soccer"]]], "topics already listed aren't offered again");

  doc(p).getElementById("add-topic-button").click(); // nothing chosen
  assert.equal(doc(p).querySelectorAll("#interest-list > li").length, 2);
  assert.equal(barState(p), "hidden");

  pick(p, p.$("add-topic"), BASEBALL);
  doc(p).getElementById("add-topic-button").click();
  const row = rowOf(p, BASEBALL);
  assert.ok(row, "a row for the new topic");
  assert.deepEqual([...row.querySelectorAll(".badge")].map((b) => b.textContent), ["added"]);
  assert.equal(row.querySelector(".interest-share").textContent, "Not in your feed until you save");
  assert.match(row.querySelector(".interest-none").textContent, /haven't liked posts about this/);
  assert.equal(row.querySelector("input").value, "3");
  assert.equal(row.querySelector(".weight-value").textContent, "As you like it");
  assert.equal(doc(p).activeElement, row.querySelector("input"), "ready to be moved");
  assert.equal(barState(p), "dirty");
  assert.deepEqual(groups(p), [["Food", ["Baking"]], ["Sports", ["Soccer"]]], "no longer offered");
  await settled(p);
  assert.deepEqual(lastPreview(p), { topics: { [BASEBALL]: 1 } }, "a weight of 1 on a topic they haven't liked is what adds it");

  slide(p, row.querySelector("input"), 5);
  await settled(p);
  assert.deepEqual(lastPreview(p), { topics: { [BASEBALL]: 3 } });
  slide(p, row.querySelector("input"), 3);
  await settled(p);
  assert.deepEqual(lastPreview(p), { topics: { [BASEBALL]: 1 } }, "back to normal is still added: it only goes away if it is removed");

  const remove = [...row.querySelectorAll("button")].find((b) => b.textContent === "Remove");
  assert.ok(remove);
  assert.match(remove.getAttribute("aria-label"), /Remove Baseball/);
  remove.click();
  assert.equal(rowOf(p, BASEBALL), undefined);
  assert.equal(barState(p), "hidden");
  assert.deepEqual(groups(p), [["Food", ["Baking"]], ["Sports", ["Baseball", "Soccer"]]], "offered again");
  await settled(p);
  assert.deepEqual(lastPreview(p), {});
});

test("tuning: topics are only added from what the server offers", async () => {
  const p = await openTuned();
  const select = p.$("add-topic");
  select.append(new p.win.Option("Bogus", "adult_content/x"));
  pick(p, select, "adult_content/x");
  doc(p).getElementById("add-topic-button").click();
  assert.equal(rowOf(p, "adult_content/x"), undefined);
  assert.equal(barState(p), "hidden");
});

test("tuning: a topic added and saved is an interest you can take away again", async () => {
  const body = likes({
    interests: [
      interest({ path: AI, name: "AI", share: 0.6, tunedShare: 0.5 }),
      interest({ path: BAKING, name: "Baking", broad: "Food", added: true, share: 0, tunedShare: 0.3, weight: 1 }),
    ],
  });
  const p = await openTuned({ interests: { status: 200, body }, tuning: { status: 200, body: tuningBody({ tuning: { topics: { [BAKING]: 1 } } }) } });
  const row = rowOf(p, BAKING);
  assert.deepEqual([...row.querySelectorAll(".badge")].map((b) => b.textContent), ["added"]);
  assert.equal(row.querySelector(".interest-share").textContent, "30% of your feed");
  assert.equal(row.querySelector(".bar-fill").style.width, "30%");
  assert.deepEqual(lastPreview(p), { topics: { [BAKING]: 1 } });
  [...row.querySelectorAll("button")].find((b) => b.textContent === "Remove").click();
  assert.equal(rowOf(p, BAKING), undefined);
  assert.equal(barState(p), "dirty");
  await settled(p);
  assert.deepEqual(lastPreview(p), {});
  p.$("discard").click();
  assert.ok(rowOf(p, BAKING), "discarding brings it back");
  assert.equal(barState(p), "hidden");
});

test("tuning: an interest that moved up because others were muted can be turned, but isn't something you added", async () => {
  const body = likes({
    interests: [interest({ path: AI, name: "AI" }), interest({ path: BASEBALL, name: "Baseball", broad: "Sports", added: true, share: 0, tunedShare: 0.2 })],
  });
  const p = await openTuned({ interests: { status: 200, body } });
  const row = rowOf(p, BASEBALL);
  assert.deepEqual([...row.querySelectorAll(".badge")].map((b) => b.textContent), ["moved up"]);
  assert.equal([...row.querySelectorAll("button")].some((b) => b.textContent === "Remove"), false, "there is nothing to remove");
  slide(p, row.querySelector("input"), 5);
  await settled(p);
  assert.deepEqual(lastPreview(p), { topics: { [BASEBALL]: 3 } });
});

test("tuning: only topics that can be tuned get a slider", async () => {
  const body = likes({
    interests: [interest({ path: AI, name: "AI" }), interest({ path: "unclear", name: "Unclear", broad: "Unclear" }), interest({ path: "technology", name: "Technology" })],
  });
  const p = await openTuned({ interests: { status: 200, body } });
  assert.equal(doc(p).querySelectorAll("#interest-list > li").length, 3);
  assert.equal(rowOf(p, AI).querySelectorAll("input").length, 1);
  assert.equal(rowOf(p, "unclear").querySelectorAll("input").length, 0);
  assert.equal(rowOf(p, "technology").querySelectorAll("input").length, 0);
  assert.match(sliderOf(p, AI).getAttribute("aria-label"), /AI/);
});

// ---------- the shares, live ----------

test("tuning: each interest's share of the feed follows the draft as it is previewed, without redrawing the list", async () => {
  let n = 0;
  const shares = (ai, cats) => previewBody([previewPost()], { interests: [{ path: AI, name: "AI", broad: "Technology", share: ai }, { path: CATS, name: "Cats", broad: "Animals", share: cats }] });
  const p = await openTuned({ preview: () => ({ status: 200, body: ++n === 1 ? shares(0.6, 0.4) : shares(0.9, 0.1) }) });
  assert.equal(rowOf(p, AI).querySelector(".interest-share").textContent, "60% of your likes");
  const ai = rowOf(p, AI);
  const slider = sliderOf(p, AI);
  slide(p, slider, 5);
  await settled(p);
  assert.equal(rowOf(p, AI), ai, "the row is the same one, so a slider being dragged isn't lost");
  assert.equal(rowOf(p, AI).querySelector(".interest-share").textContent, "60% of your likes → 90% of your feed");
  assert.equal(rowOf(p, AI).querySelector(".bar-fill").style.width, "90%");
  assert.equal(rowOf(p, CATS).querySelector(".interest-share").textContent, "40% of your likes → 10% of your feed");
  assert.equal(rowOf(p, CATS).querySelector(".bar-fill").style.width, "10%");
  assert.equal(rowOf(p, AI).querySelector(".interest-pct").textContent, "90%", "the share is also the number in the row's heading");
  assert.equal(rowOf(p, CATS).querySelector(".interest-pct").textContent, "10%");
});

test("tuning: a topic that isn't among the draft's interests has no share of the feed", async () => {
  const p = await openTuned({
    preview: { status: 200, body: previewBody([previewPost()], { interests: [{ path: AI, name: "AI", broad: "Technology", share: 1 }] }) },
  });
  assert.equal(rowOf(p, CATS).querySelector(".interest-share").textContent, "40% of your likes → 0% of your feed");
  assert.equal(rowOf(p, CATS).querySelector(".bar-fill").style.width, "0%");
});

test("tuning: a topic added to the draft shows what share it would get once the draft is previewed", async () => {
  const p = await openTuned({
    preview: (call) => ({
      status: 200,
      body: previewBody([previewPost()], {
        interests: [{ path: AI, name: "AI", broad: "T", share: 0.5 }, ...(call.json.topics?.[BASEBALL] ? [{ path: BASEBALL, name: "Baseball", broad: "Sports", share: 0.5 }] : [])],
      }),
    }),
  });
  pick(p, p.$("add-topic"), BASEBALL);
  doc(p).getElementById("add-topic-button").click();
  await settled(p);
  assert.equal(rowOf(p, BASEBALL).querySelector(".interest-share").textContent, "50% of your feed · not saved yet");
});

// ---------- the preview ----------

const previewItems = (p) => [...doc(p).querySelectorAll("#preview-list > li")];
const chipTexts = (li) => [...li.querySelectorAll(".why-chips > span")].map((s) => s.textContent);

test("preview: each post with what the feed knows about it, even when Bluesky can't be asked about it", async () => {
  const posts = [
    previewPost({ text: "first post", topic: "AI", top: [{ path: AI, name: "AI", p: 0.9 }, { path: CATS, name: "Cats", p: 0.07 }], labels: ["graphic-media", "other-label"] }),
    previewPost({ text: "", topic: "Cats", score: 0.5, likes: 1, reposts: 0, replies: 0, quotes: 0, indexedAt: new Date(Date.now() - 5 * 60 * 1000).toISOString() }),
    previewPost({ text: "an old one", indexedAt: new Date(Date.now() - 96 * 3600 * 1000).toISOString() }),
  ];
  const p = await openTuned({ preview: { status: 200, body: previewBody(posts) } });
  const items = previewItems(p);
  assert.equal(items.length, 3);
  const age = (li) => [...li.querySelectorAll(".kv")].find((k) => k.firstChild.textContent === "Age").querySelector("b").textContent;
  assert.deepEqual([age(items[0]), age(items[1]), age(items[2])], ["2 h", "5 min", "4 days"]);
  for (const li of items) for (const chip of li.querySelectorAll(".why-chips > span")) assert.equal(chip.hidden, false, "every chip is shown");
  assert.deepEqual([...items[0].querySelectorAll(".labels .label")].map((l) => l.textContent), ["graphic-media", "other-label"]);
  assert.equal(items[1].querySelector(".labels"), null, "no labels, no list of them");
  assert.equal(items[0].querySelector(".post-text").textContent, "first post");
  assert.match(items[1].textContent, /the text of this post isn't available/);
  assert.equal(items[0].querySelector(".avatar").textContent, "A", "no avatar to show: the topic's initial");
  assert.deepEqual(chipTexts(items[0]).slice(0, 5), ["AI 90%", "📰 Informative 70%", "score 1.23", "#1", "12 likes · 3 reposts · 2 replies · 1 quote"]);
  assert.deepEqual(chipTexts(items[1]).slice(2, 4), ["score 0.50", "#2"]);
  assert.match(chipTexts(items[1]).at(-1), /^1 like · 0 reposts · 0 replies · 0 quotes$/);
  const link = items[0].querySelector(".post-text a");
  assert.equal(link.href, posts[0].url);
  assert.equal(link.target, "_blank");
  assert.equal(link.rel, "noopener noreferrer");
  // All the scores, folded away until asked for.
  const details = items[0].querySelector("details.why-details");
  assert.equal(details.open, false);
  const meters = (title) => [...[...details.querySelectorAll(".why-sec")].find((s) => s.querySelector("h4").textContent === title).querySelectorAll(".meter")];
  assert.deepEqual(meters("Topics").map((m) => [m.querySelector(".m-label").textContent, m.querySelector(".m-val").textContent]), [["AI", "90%"], ["Cats", "7%"]]);
  assert.deepEqual(meters("Signals").map((m) => [m.querySelector(".m-label").textContent, m.querySelector(".m-val").textContent]), [["🧠 Substance", "50%"], ["🗞️ Newsy", "25%"]]);
  assert.deepEqual(meters("Tone").map((m) => m.querySelector(".m-label").textContent), ["📰 Informative", "😂 Funny"]);
  assert.equal(meters("Signals")[0].querySelector(".m-fill").style.width, "50%");
  const kvs = Object.fromEntries([...details.querySelectorAll(".kv")].map((k) => [k.firstChild.textContent, k.querySelector("b").textContent]));
  assert.deepEqual(kvs, { Score: "1.234", Slot: "#1", Age: "2 h", "Likes · reposts": "12 · 3", "Replies · quotes": "2 · 1" });
  assert.equal(p.$("preview-status").hidden, true);
});

test("preview: posts as Bluesky shows them, with the feed's own scores underneath", async () => {
  const posts = [previewPost({ text: "our words" }), previewPost({ text: "second" }), previewPost({ text: "gone from Bluesky" })];
  const p = await openTuned({ preview: { status: 200, body: previewBody(posts) }, bsky: bskyFor(posts.slice(0, 2)) });
  await p.until(() => doc(p).querySelectorAll(".preview-item .post:not(.fallback)").length === 2, "the posts from Bluesky");
  const items = previewItems(p);
  assert.equal(items.length, 3);
  assert.equal(items[0].querySelector(".post-name").textContent, "Alice Example");
  assert.equal(items[0].querySelector(".post-handle").textContent, "@alice.example");
  assert.equal(items[0].querySelector(".post-text").textContent, "the words on Bluesky", "Bluesky's own words, not our copy");
  assert.equal(items[0].querySelector(".avatar").src, "https://cdn.bsky.app/img/avatar/x.jpg");
  assert.match(items[0].querySelector(".post-foot").textContent, /5.*7.*70/); // replies, reposts and quotes, likes
  assert.deepEqual(chipTexts(items[1]).slice(0, 4).slice(2), ["score 1.23", "#2"], "in the feed's order, with its own slot");
  assert.equal(items[2].querySelector(".post.fallback") !== null, true, "a post Bluesky doesn't have is shown as we know it");
  assert.equal(items[2].querySelector(".post-text").textContent, "gone from Bluesky");
  for (const li of items) assert.ok(li.querySelector(".why"), "every post has what the feed knows");
  assert.equal(p.$("preview-status").hidden, true);
  const asked = p.calls.filter((c) => c.url.startsWith("https://public.api.bsky.app/"));
  assert.equal(asked.length, 1);
  assert.deepEqual(new URL(asked[0].url).searchParams.getAll("uris"), posts.map((x) => x.uri));
});

test("preview: if Bluesky can't be asked, the page says so, and the posts stay as we know them", async () => {
  for (const [name, bsky] of [["an error", { status: 500, body: {} }], ["the request failing", new TypeError("Failed to fetch")]]) {
    const posts = [previewPost({ text: `known here ${name}` })];
    const p = await openTuned({ preview: { status: 200, body: previewBody(posts) }, bsky });
    await p.until(() => !p.$("preview-status").hidden, `${name}: the note`);
    assert.match(p.$("preview-status").textContent, /Couldn't get these posts from Bluesky/, name);
    assert.equal(previewItems(p).length, 1, name);
    assert.equal(previewItems(p)[0].querySelector(".post-text").textContent, `known here ${name}`, name);
    assert.equal(p.$("preview-retry").hidden, true, `${name}: nothing to retry, the preview itself worked`);
  }
});

test("preview: the scores of every post open and close together, and stay as they were set", async () => {
  const posts = [previewPost(), previewPost()];
  const p = await openTuned({ preview: { status: 200, body: previewBody(posts) } });
  const open = () => [...doc(p).querySelectorAll("details.why-details")].map((d) => d.open);
  assert.deepEqual(open(), [false, false]);
  p.$("preview-scores").checked = true;
  p.$("preview-scores").dispatchEvent(new p.win.Event("change", { bubbles: true }));
  assert.deepEqual(open(), [true, true]);
  slide(p, knob(p, "authorGap"), 3); // a new preview comes: it keeps the choice
  await settled(p);
  assert.deepEqual(open(), [true, true]);
  p.$("preview-scores").checked = false;
  p.$("preview-scores").dispatchEvent(new p.win.Event("change", { bubbles: true }));
  assert.deepEqual(open(), [false, false]);
});

test("preview: which slot is a brand-new post follows the ranking, and the scores with a rule on them are marked", async () => {
  const posts = Array.from({ length: 5 }, () => previewPost({ tone: { outraged: 0.2, informative: 0.8 }, signals: { substance: 0.5, spam: 0.01 } }));
  const p = await openTuned({ preview: { status: 200, body: previewBody(posts) } });
  const freshSlots = () => previewItems(p).map((li, i) => (chipTexts(li).includes("fresh slot") ? i + 1 : null)).filter(Boolean);
  assert.deepEqual(freshSlots(), [4], "every 4th slot, as the feed ranks");
  slide(p, knob(p, "freshEvery"), 2);
  await settled(p);
  assert.deepEqual(freshSlots(), [2, 4]);
  slide(p, knob(p, "freshEvery"), 0);
  await settled(p);
  assert.deepEqual(freshSlots(), []);
  knobRow(p, "freshEvery").querySelector(".knob-reset").click(); // back to following the ranking
  fresh(p, "fresh");
  await settled(p);
  assert.deepEqual(freshSlots(), [3], "fresh starts at every 3rd");

  // A cutoff or a boost on a score marks it in every post, and says what it is.
  slide(p, dialInput(p, "tone", "outraged", "max"), 0.4);
  slide(p, dialInput(p, "signals", "spam", "boost"), -2);
  await settled(p);
  const meter = (label) => [...previewItems(p)[0].querySelectorAll(".meter")].find((m) => m.querySelector(".m-label").textContent.includes(label));
  assert.equal(meter("Outraged").classList.contains("ruled"), true);
  assert.equal(meter("Outraged").querySelector(".m-note").textContent, "allowed ≤ 40%");
  assert.equal(meter("Spam").querySelector(".m-note").textContent, "boost -2");
  assert.equal(meter("Informative").classList.contains("ruled"), false);
  assert.equal(meter("Substance").classList.contains("ruled"), false);
});

test("preview: what other people posted is shown as words, and only real post addresses are links", async () => {
  const markup = "<img src=x onerror=alert(1)>";
  const posts = [
    previewPost({ text: markup, topic: markup, labels: [markup], top: [{ path: "x/y", name: markup, p: 0.5 }], url: "javascript:alert(1)" }),
    previewPost({ text: "elsewhere", url: "https://evil.test/profile/x/post/1" }),
    previewPost({ text: "lookalike", url: "https://bsky.app.evil.test/p" }),
    previewPost({ text: "credentials", url: "https://bsky.app@evil.test/p" }),
    previewPost({ text: "the real one", url: "https://bsky.app/profile/ok/post/1" }),
    { uri: "at://did:plc:odd/app.bsky.feed.post/odd", text: 7, topic: null, likes: "many", indexedAt: 5, score: "high", tone: "loud", signals: null, labels: "no", top: 3 },
  ];
  const p = await openTuned({ preview: { status: 200, body: previewBody(posts) } });
  const d = doc(p);
  assert.equal(d.querySelectorAll("img[src='x']").length, 0);
  assert.equal(d.querySelectorAll("[onerror]").length, 0);
  assert.ok(previewItems(p)[0].textContent.includes(markup), "shown as words");
  assert.deepEqual([...d.querySelectorAll("#preview-list .post-text a")].map((a) => a.href), ["https://bsky.app/profile/ok/post/1"]);
  assert.equal(previewItems(p).length, 6, "an odd post doesn't stop the rest");
});

test("preview: a new answer replaces the old one", async () => {
  let n = 0;
  const p = await openTuned({
    preview: () => ++n && { status: 200, body: previewBody(Array.from({ length: n === 1 ? 3 : 1 }, (_, i) => previewPost({ text: `answer ${n} post ${i}` }))) },
  });
  assert.equal(previewItems(p).length, 3);
  slide(p, sliderOf(p, AI), 0);
  await settled(p);
  assert.equal(previewItems(p).length, 1);
  assert.match(previewItems(p)[0].textContent, /answer 2 post 0/);
});

test("preview: Bluesky's answer for an old preview never replaces the posts of a newer one", async () => {
  const slow = deferred();
  const first = [previewPost({ text: "first preview" })];
  const second = [previewPost({ text: "second preview" })];
  let n = 0;
  const preview = () => ({ status: 200, body: previewBody(++n === 1 ? first : second) });
  const bsky = (call) => (new URL(call.url).searchParams.getAll("uris").includes(first[0].uri) ? slow.promise : bskyFor(second)(call));
  const p = await open({ api: signedIn, interests: { status: 200, body: likes() }, preview, bsky, abortable: false });
  await p.until(() => callsTo(p, "GET", "https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts").length === 0 && n === 1 && doc(p).querySelectorAll(".preview-item").length === 1, "the first preview as we know it");
  slide(p, sliderOf(p, AI), 0);
  await p.until(() => doc(p).querySelectorAll(".preview-item .post:not(.fallback)").length === 1, "the second preview from Bluesky");
  slow.resolve({ status: 200, body: { posts: [bskyPost(first[0], { record: { text: "late words" } })] } });
  await new Promise((r) => setTimeout(r, 30));
  assert.equal(doc(p).body.textContent.includes("late words"), false);
  assert.equal(doc(p).querySelectorAll(".preview-item").length, 1);
});

test("preview: when the settings leave nothing, it says so; when they leave a mix of topics, it says that", async () => {
  const empty = await openTuned({ preview: { status: 200, body: previewBody([]) } });
  assert.equal(previewItems(empty).length, 0);
  assert.equal(empty.$("preview-status").hidden, false);
  assert.match(empty.$("preview-status").textContent, /No posts match these settings right now/);

  const mix = await openTuned({ preview: { status: 200, body: previewBody([previewPost()], { state: "generic" }) } });
  assert.equal(mix.$("preview-note").hidden, false);
  assert.match(mix.$("preview-note").textContent, /a mix of every topic/);
  assert.equal(previewItems(mix).length, 1);
});

test("preview: if it can't be had, the page says why; some failures are tried again by themselves, others offer a button", async () => {
  const cases = [
    ["your feed is still loading", { status: 503, body: { error: "loading" } }, /still being prepared/, true],
    ["rate limited", { status: 429, body: { error: "limited" } }, /being asked a lot/, true],
    ["unavailable", { status: 503, body: { error: "unavailable" } }, /Couldn't do that just now/, false],
    ["an error with no body", { status: 500 }, /Couldn't do that just now/, false],
    ["the request failing", new TypeError("Failed to fetch"), /Couldn't reach the server/, false],
    ["settings the server refuses", { status: 400, body: { error: "invalid", message: "authorGap must be between 0 and 50" } }, /Those settings can't be used: authorGap must be between 0 and 50\./, false],
  ];
  for (const [name, failure, want, byItself] of cases) {
    let n = 0;
    const p = await open({
      api: signedIn,
      interests: { status: 200, body: likes() },
      preview: () => (++n === 1 ? failure : { status: 200, body: previewBody([previewPost({ text: "it worked" })]) }),
    });
    await p.until(() => n >= 1 && !busy(p), `${name}: the first answer`);
    assert.match(p.$("preview-status").textContent, want, name);
    assert.equal(p.$("preview-status").hidden, false, name);
    assert.equal(p.$("preview-retry").hidden, false, `${name}: a button to try again`);
    if (byItself) {
      await p.until(() => previewItems(p).length === 1, `${name}: trying again by itself`);
      assert.equal(p.$("preview-retry").hidden, true, name);
      assert.equal(p.$("preview-status").hidden, true, name);
      continue;
    }
    await new Promise((r) => setTimeout(r, 120)); // longer than the page waits before trying again by itself
    assert.equal(n, 1, `${name}: not tried again by itself`);
    p.$("preview-retry").click();
    await p.until(() => previewItems(p).length === 1, `${name}: trying again with the button`);
    assert.equal(p.$("preview-retry").hidden, true, name);
    assert.equal(n, 2, name);
  }
});

test("preview: a retry that is waiting is dropped when the settings change, so only the latest is asked for", async () => {
  let n = 0;
  const p = await open({
    api: signedIn,
    interests: { status: 200, body: likes() },
    preview: () => (++n === 1 ? { status: 503, body: { error: "loading" } } : { status: 200, body: previewBody([previewPost({ text: "latest" })]) }),
  });
  await p.until(() => n === 1 && !busy(p), "the first answer, which says to wait");
  slide(p, sliderOf(p, AI), 0); // before the page tries again by itself
  await p.until(() => previewItems(p).length === 1, "the preview of the new settings");
  await new Promise((r) => setTimeout(r, 120));
  assert.equal(n, 2, "the waiting retry didn't also go out");
  assert.deepEqual(lastPreview(p), { topics: { [AI]: 0 } });
});

test("preview: a session that ended starts again from the sign-in form", async () => {
  const p = await open({ api: signedIn, interests: { status: 200, body: likes() }, preview: { status: 401, body: { error: "not signed in" } } });
  await p.until(() => p.reloads.length > 0, "the reload");
});

test("preview: nothing is asked for someone who isn't signed in", async () => {
  const p = await open();
  assert.deepEqual(p.calls.map((c) => c.url), ["/api/me"]);
});

test("the page makes no requests it shouldn't, and sends JSON only where it means to", async () => {
  const posts = [previewPost()];
  const p = await openTuned({ preview: { status: 200, body: previewBody(posts) }, bsky: bskyFor(posts) });
  slide(p, sliderOf(p, AI), 0);
  await settled(p);
  p.$("save").click();
  await p.until(() => barState(p) === "saved", "saved");
  for (const c of p.calls) {
    assert.ok(c.url.startsWith("/") || c.url.startsWith("https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts?"), `${c.url} is neither this site nor Bluesky's public API`);
    if (c.method !== "GET") assert.equal(c.headers["Content-Type"], "application/json", `${c.method} ${c.url}`);
  }
  assert.deepEqual([...new Set(p.calls.map((c) => `${c.method} ${c.url.split("?")[0]}`))].sort(), [
    "GET /api/me", "GET /api/me/interests", "GET /api/me/tuning", "GET https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts",
    "POST /api/me/preview", "PUT /api/me/tuning",
  ]);
});

// ---------- rules for particular topics ----------

const ruleCard = (p, path) => doc(p).querySelector(`.topic-rule[data-topic="${path}"]`);
const ruleInput = (p, path, key, name, which) => p.$(`me-rules-${path.replace(/[^a-z0-9]+/gi, "-")}-${key}-${name}-${which}`);

test("topic rules: a topic gets its own setting for a score, starting from the feed-wide one, and only that is sent", async () => {
  const p = await openTuned();
  const group = groupOf(p, "Rules for particular topics");
  assert.ok(group, "a group for them");
  assert.equal(group.open, false, "folded away until a topic has some");
  slide(p, dialInput(p, "signals", "critical", "max"), 0.3); // critical posts left out everywhere
  await settled(p);

  // Every broad topic and every subtopic that can be added can have rules.
  const add = p.$("me-rules-add-topic");
  const options = [...add.querySelectorAll("option")].map((o) => o.value).filter(Boolean);
  assert.ok(options.includes("sports") && options.includes(BASEBALL) && options.includes(AI), options.join());
  pick(p, add, "sports");
  assert.ok(ruleCard(p, "sports"), "a card for the whole broad topic");
  assert.match(ruleCard(p, "sports").textContent, /All of Sports/);
  assert.ok(!p.$("me-rules-add-topic").querySelector('option[value="sports"]'), "it can't be added twice");
  await settled(p);
  assert.deepEqual(lastPreview(p), { signals: { max: { critical: 0.3 } } }, "a topic with no score of its own changes nothing");

  pick(p, ruleCard(p, "sports").querySelector("select"), "signals:critical");
  assert.equal(ruleInput(p, "sports", "signals", "critical", "max").value, "0.3", "it starts as the feed-wide setting");
  slide(p, ruleInput(p, "sports", "signals", "critical", "max"), 1);
  await settled(p);
  assert.deepEqual(lastPreview(p), { signals: { max: { critical: 0.3 } }, topicRules: { sports: { signals: { max: { critical: 1 } } } } },
    "critical sports posts are let in");
  assert.equal(barState(p), "dirty");
  assert.equal(groupOf(p, "Rules for particular topics").querySelector(".knob-changed").textContent, "1 changed");

  // A subtopic of it, with a boost of its own.
  pick(p, p.$("me-rules-add-topic"), BASEBALL);
  pick(p, ruleCard(p, BASEBALL).querySelector("select"), "tone:humorous");
  slide(p, ruleInput(p, BASEBALL, "tone", "humorous", "boost"), 2);
  await settled(p);
  assert.deepEqual(lastPreview(p).topicRules, {
    sports: { signals: { max: { critical: 1 } } },
    [BASEBALL]: { tone: { max: { humorous: 1 }, weights: { humorous: 2 } } },
  });

  // Taking a score away, then the topic.
  ruleCard(p, BASEBALL).querySelector('.dial[data-score="humorous"] .link-btn').click();
  await settled(p);
  assert.deepEqual(lastPreview(p).topicRules, { sports: { signals: { max: { critical: 1 } } } });
  [...ruleCard(p, "sports").querySelectorAll(".topic-rule-head .link-btn")][0].click();
  assert.equal(ruleCard(p, "sports"), null);
  await settled(p);
  assert.deepEqual(lastPreview(p), { signals: { max: { critical: 0.3 } } });
});

test("topic rules: a topic's own how-sure, down to what the pool holds, starting from the viewer's", async () => {
  const p = await openTuned();
  assert.equal(knob(p, "minTopicProb").min, "0.3", "the feed-wide setting goes down to the pool's 0.3");
  slide(p, knob(p, "minTopicProb"), 0.7);
  pick(p, p.$("me-rules-add-topic"), BAKING);
  pick(p, ruleCard(p, BAKING).querySelector("select"), "min_prob");
  const sure = p.$("me-rules-food-baking-min-prob");
  assert.equal(sure.value, "0.7", "it starts from the viewer's own setting");
  assert.equal(sure.min, "0.3");
  slide(p, sure, 0.35);
  await settled(p);
  assert.deepEqual(lastPreview(p), { minTopicProb: 0.7, topicRules: { [BAKING]: { min_prob: 0.35 } } });
  assert.ok(!ruleCard(p, BAKING).querySelector('option[value="min_prob"]'), "only one how-sure per topic");
  ruleCard(p, BAKING).querySelector('.dial[data-score="min_prob"] .link-btn').click();
  await settled(p);
  assert.deepEqual(lastPreview(p), { minTopicProb: 0.7 }, "removed, the topic has nothing of its own");
});

test("topic rules: saved ones are shown, saved again as they were, and the group puts them all back", async () => {
  const saved = { topicRules: { sports: { signals: { max: { critical: 1 } } }, [AI]: { tone: { max: { outraged: 0.2 }, min: { informative: 0.5 }, weights: { humorous: -1 } } } } };
  const p = await openTuned({ tuning: { status: 200, body: tuningBody({ tuning: saved }) } });
  // Previewed as saved. Every score a topic sets is sent with a maximum, which says the score is the
  // topic's: a maximum of 1 cuts nothing, so this is the same as what was saved.
  assert.deepEqual(lastPreview(p), { topicRules: { sports: { signals: { max: { critical: 1 } } },
    [AI]: { tone: { max: { outraged: 0.2, informative: 1, humorous: 1 }, min: { informative: 0.5 }, weights: { humorous: -1 } } } } });
  assert.equal(barState(p), "hidden", "and nothing is changed by showing them");
  assert.equal(groupOf(p, "Rules for particular topics").open, true, "open when there are some");
  assert.equal(ruleInput(p, AI, "tone", "outraged", "max").value, "0.2");
  assert.equal(ruleInput(p, AI, "tone", "informative", "min").value, "0.5");
  assert.equal(ruleInput(p, AI, "tone", "humorous", "boost").value, "-1");
  assert.equal(ruleInput(p, "sports", "signals", "critical", "max").value, "1");
  groupOf(p, "Rules for particular topics").querySelector(".knob-head .link-btn").click();
  assert.equal(doc(p).querySelectorAll(".topic-rule").length, 0, "reset takes every topic's rules away");
  await settled(p);
  assert.deepEqual(lastPreview(p), {});
  assert.equal(barState(p), "dirty");
});
