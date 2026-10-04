// The page at /me: sign in with Bluesky, see what your likes say you're into, and tune your feed
// (the controls for that are in tuning.js; the sign-in form, shared with the other pages, is
// signin-form.js). All text is put on the page with textContent, never as HTML.

import "./header.js"; // the header every page shares
import { $, num } from "./dom.js";
import { createTuner } from "./tuning.js";
import { MESSAGES, notice, setupSignInForm, showSignInProblem, signInOff } from "./signin-form.js";

export { safeLink } from "./dom.js";
export { MESSAGES, messageFor } from "./signin-form.js";

function show(which) {
  for (const id of ["loading", "signed-out", "signed-in"]) $(id).hidden = id !== which;
}

// whoAmI is the signed-in account, or null.
async function whoAmI() {
  const res = await fetch("/api/me", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (res.status === 401) return null;
  if (res.status === 404) throw new Error("off"); // the server has sign-in switched off
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

// ---------- the parts of the page ----------

// On a narrow screen the interests, the settings and the preview are three views you switch
// between (a wide screen shows them side by side and hides the switch): the page would otherwise
// be thousands of pixels long. Which one is showing is data-view of the grid.
function setupTabs() {
  const grid = $("me-grid");
  const tabs = [...$("me-tabs").querySelectorAll(".me-tab")];
  const show = (view) => {
    grid.dataset.view = view;
    for (const t of tabs) t.setAttribute("aria-pressed", String(t.dataset.view === view));
  };
  for (const t of tabs) t.addEventListener("click", () => show(t.dataset.view));
  // "See the preview" in the save bar goes to the preview, whichever view is showing.
  $("jump").addEventListener("click", (ev) => {
    ev.preventDefault();
    show("preview");
    $("me-tabs").scrollIntoView?.({ block: "start" });
  });
}

// ---------- interests ----------

const INTEREST_PROBLEMS = {
  limited: "That's been asked a lot. Wait a few seconds and try again.",
  unavailable: "Couldn't read your likes just now. Try again in a moment.",
  network: MESSAGES.network,
};

let tuner = null;

// renderInterests says what the likes were read as. The interests themselves, with the controls
// for tuning each, are drawn by the tuner.
export function renderInterests(data) {
  const cov = data.coverage || {};
  const set = data.settings || {};
  $("interests-summary").textContent =
    `You liked or reposted ${num(cov.total)} posts in the time we have data for (up to ${num(set.lookbackDays)} days). ` +
    `${num(cov.classified)} of them have topics, and those are what this is built from. ` +
    `Newer likes count more: a like counts half as much every ${num(set.halfLifeDays)} days.`;

  const mix = $("interests-mix");
  mix.hidden = data.state !== "generic";
  if (data.state === "generic") {
    mix.textContent = data.personalized
      ? "Your feed is a mix of topics right now, because every topic you like is turned off."
      : `You haven't liked enough yet (at least ${num(set.minLikes)} posts with topics), so your feed is a mix of every topic until you have.`;
  }

  tuner.setInterests(data.interests);

  // The same numbers as tiles, for a quick look.
  const inFeed = Array.isArray(data.interests) ? data.interests.filter((r) => r && num(r.tunedShare) > 0).length : 0;
  $("stat-liked").textContent = num(cov.total).toLocaleString();
  $("stat-topics").textContent = num(cov.classified).toLocaleString();
  $("stat-interests").textContent = inFeed > 0 ? String(inFeed) : "–";
  $("stat-memory").textContent = String(num(set.halfLifeDays));
  $("me-stats").hidden = false;

  const other = [];
  if (num(data.otherPosts) > 0) other.push(`${num(data.otherPosts)} more liked posts are about topics outside your top ${num(set.topics)}.`);
  if (num(data.unclearPosts) > 0) other.push(`${num(data.unclearPosts)} liked posts couldn't be placed in a topic.`);
  $("interests-other").textContent = other.join(" ");
  $("interests-other").hidden = other.length === 0;
}

function interestsState(which, problem) {
  $("interests-loading").hidden = which !== "loading";
  $("interests-error").hidden = which !== "error";
  $("interests-body").hidden = which !== "body";
  if (which === "error") $("interests-error-text").textContent = INTEREST_PROBLEMS[problem] || INTEREST_PROBLEMS.unavailable;
}

// loadInterests reads the likes and shows them. After saving a tuning it is quiet: the list stays
// as it is until the new reading arrives, instead of going back to "Reading your likes…".
export async function loadInterests({ quiet = false } = {}) {
  const note = $("signed-in-notice");
  if (!quiet) interestsState("loading");
  const failed = (problem) => {
    if (!quiet) {
      interestsState("error", problem);
      return;
    }
    note.textContent = "Saved, but couldn't read your interests again just now. Reload the page to see them with your changes.";
    note.hidden = false;
  };
  let res;
  try {
    res = await fetch("/api/me/interests", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  } catch {
    failed("network");
    return;
  }
  if (res.status === 401) {
    location.reload(); // the session ended: start again from the sign-in form
    return;
  }
  if (res.status === 429) {
    failed("limited");
    return;
  }
  let data;
  try {
    data = res.ok ? await res.json() : null;
  } catch {
    data = null;
  }
  if (!data) {
    failed("unavailable");
    return;
  }
  note.hidden = true;
  renderInterests(data);
  interestsState("body");
}

export async function main() {
  const problem = setupSignInForm(); // a problem the last sign-in came back with, if any
  setupTabs();
  let me;
  try {
    me = await whoAmI();
  } catch (err) {
    show("signed-out");
    if (err.message === "off") {
      signInOff();
    } else {
      notice(MESSAGES.network);
    }
    return;
  }
  if (me) {
    $("who").textContent = me.handle ? "@" + me.handle : me.did;
    show("signed-in");
    tuner = createTuner({ onSaved: () => loadInterests({ quiet: true }) });
    $("interests-retry").addEventListener("click", () => loadInterests());
    tuner.start();
    loadInterests();
    return;
  }
  show("signed-out");
  showSignInProblem(problem);
}

main();
