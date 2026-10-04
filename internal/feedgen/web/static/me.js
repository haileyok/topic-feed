// The page at /me: sign in with Bluesky, see what your likes say you're into, and tune your feed
// (the controls for that are in tuning.js).
//
// The sign-in endpoints answer JSON when asked, because this page's content security policy
// doesn't let a form post away and a script can't read where a redirect leads. All text is put
// on the page with textContent, never as HTML.

import "./header.js"; // the header every page shares
import { $, num } from "./dom.js";
import { createTuner } from "./tuning.js";

export { safeLink } from "./dom.js";

// What the sign-in endpoints and the callback report, in words.
export const MESSAGES = {
  denied: "Sign-in was cancelled.",
  invalid: "That doesn't look like a Bluesky handle. Try something like alice.bsky.social.",
  busy: "Lots of people are signing in right now. Try again in a minute.",
  failed: "Sign-in didn't work. Check the handle and try again.",
  limited: "That's been tried a lot. Wait a few seconds and try again.",
  network: "Couldn't reach the server. Check your connection and try again.",
  off: "Signing in isn't turned on for this site yet.",
};

export function messageFor(code) {
  return Object.hasOwn(MESSAGES, code) ? MESSAGES[code] : MESSAGES.failed;
}

function show(which) {
  for (const id of ["loading", "signed-out", "signed-in"]) $(id).hidden = id !== which;
}

function notice(text) {
  const n = $("notice");
  n.textContent = text || "";
  n.hidden = !text;
}

// whoAmI is the signed-in account, or null.
async function whoAmI() {
  const res = await fetch("/api/me", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (res.status === 401) return null;
  if (res.status === 404) throw new Error("off"); // the server has sign-in switched off
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return res.json();
}

// startLogin asks where to send the browser to sign in the account, and returns that address.
async function startLogin(handle) {
  let res;
  try {
    res = await fetch("/oauth/login", {
      method: "POST",
      headers: { Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ handle }),
      credentials: "same-origin",
    });
  } catch {
    throw new Error("network");
  }
  let body = {};
  try {
    body = await res.json();
  } catch {
    // not JSON: handled below
  }
  if (res.ok && typeof body.redirect === "string") {
    const url = new URL(body.redirect); // throws on nonsense
    if (url.protocol !== "https:" && url.protocol !== "http:") throw new Error("failed");
    return url.href;
  }
  throw new Error(typeof body.error === "string" ? body.error : "failed");
}

function setupLogin() {
  const form = $("login");
  const button = $("login-button");
  const label = button.textContent;
  const reset = () => {
    button.disabled = false;
    button.textContent = label;
  };
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const handle = $("handle").value.trim();
    if (!handle) {
      notice(MESSAGES.invalid);
      return;
    }
    notice("");
    button.disabled = true;
    button.textContent = "Opening Bluesky…";
    try {
      location.assign(await startLogin(handle));
    } catch (err) {
      notice(messageFor(err.message));
      reset();
    }
  });
  // Coming back with the browser's back button restores this page as it was left.
  addEventListener("pageshow", (ev) => {
    if (ev.persisted) reset();
  });
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
  // The callback reports a problem as ?signin=...; show it once and keep it out of the address bar.
  const problem = new URLSearchParams(location.search).get("signin");
  if (problem !== null) history.replaceState(null, "", location.pathname);

  setupLogin();
  setupTabs();
  let me;
  try {
    me = await whoAmI();
  } catch (err) {
    show("signed-out");
    if (err.message === "off") {
      notice(MESSAGES.off);
      $("handle").disabled = true;
      $("login-button").disabled = true;
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
  if (problem) notice(messageFor(problem));
}

main();
