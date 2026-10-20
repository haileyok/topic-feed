// The account inspector at /inspect/account: what any account's likes say it is most interested in, as
// its For you feed reads them. Only the owner of the feeds may use it (/api/inspect/account answers
// anyone else 403). All text is put on the page with textContent, never as HTML.

import "./header.js"; // the header every page shares
import { $, num, pct, safeLink } from "./dom.js";
import { el } from "./posts.js";
import { setupSignInForm, showSignInProblem, signInOff } from "./signin-form.js";

function show(which) {
  for (const id of ["loading", "signed-out", "forbidden", "inspector"]) $(id).hidden = id !== which;
}

async function whoAmI() {
  const res = await fetch("/api/me", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (res.status === 401) return null;
  if (res.status === 404) return "off";
  if (!res.ok) throw new Error(String(res.status));
  return res.json();
}

function showError(text) {
  $("account-error").textContent = text || "";
  $("account-error").hidden = !text;
}

/** weightText says how an account tuned a topic, or "" when it left it as it is. */
export function weightText(w) {
  if (typeof w !== "number" || w === 1) return "";
  if (w === 0) return "muted";
  return `turned ${w > 1 ? "up" : "down"} to ×${w}`;
}

function sampleItem(s) {
  const when = s.likedAt ? new Date(s.likedAt) : null;
  const link = safeLink(s.url);
  return el("li", { class: "acct-sample" },
    link ? el("a", { href: link, target: "_blank", rel: "noopener", text: s.text || "(no text)" }) : el("span", { text: s.text || "(no text)" }),
    el("span", { class: "me-fine", text: ` · ${pct(s.p)}% sure${when && !Number.isNaN(when.getTime()) ? ` · liked ${when.toLocaleDateString()}` : ""}` }));
}

/** renderAccount shows an /api/inspect/account answer (feedgen.InterestsResponse). */
export function renderAccount(data) {
  const cov = data.coverage || {};
  const set = data.settings || {};
  $("account-who-title").textContent = data.handle ? `@${data.handle}` : data.did;
  $("account-who-title").title = data.did || "";
  $("account-summary").textContent =
    `Liked or reposted ${num(cov.total).toLocaleString()} posts in the last ${num(set.lookbackDays)} days; ` +
    `${num(cov.classified).toLocaleString()} of them have topics, and those are what this is built from. ` +
    `Newer likes count more (half as much every ${num(set.halfLifeDays)} days).`;
  const mix = $("account-mix");
  mix.hidden = data.state !== "generic";
  if (data.state === "generic") {
    mix.textContent = data.personalized
      ? "Their feed is a mix of every topic: they've muted every topic they like."
      : `Not enough likes with topics yet (at least ${num(set.minLikes)}), so their feed is a mix of every topic.`;
  }
  const interests = Array.isArray(data.interests) ? data.interests.filter((r) => r && typeof r.path === "string") : [];
  const top = Math.max(0.0001, ...interests.map((r) => num(r.share)));
  $("acct-interests").replaceChildren(...interests.map((r, i) => {
    const tuned = weightText(r.weight);
    const samples = Array.isArray(r.samples) ? r.samples : [];
    return el("li", { class: "interest acct-interest", "data-path": r.path },
      el("div", { class: "acct-interest-head" },
        el("span", { class: "acct-rank", text: String(i + 1) }),
        el("strong", { text: r.name || r.path }),
        el("span", { class: "me-fine", text: r.broad ? ` · ${r.broad}` : "" }),
        el("span", { class: "acct-share", text: `${pct(r.share)}%` })),
      el("div", { class: "acct-bar" }, el("span", { style: `width:${Math.round((num(r.share) / top) * 100)}%` })),
      el("p", { class: "me-fine" },
        `${num(r.posts)} liked post${num(r.posts) === 1 ? "" : "s"} filed under it`,
        tuned ? ` · they ${tuned} it (${pct(r.tunedShare)}% of their feed)` : "",
        r.added ? " · added by their tuning" : ""),
      samples.length ? el("details", {}, el("summary", { text: "Liked posts behind it" }),
        el("ul", { class: "acct-samples" }, ...samples.map(sampleItem))) : null);
  }));
  const other = [];
  if (num(data.otherPosts) > 0) other.push(`${num(data.otherPosts)} more liked posts are about topics outside their top ${num(set.topics)}.`);
  if (num(data.unclearPosts) > 0) other.push(`${num(data.unclearPosts)} liked posts couldn't be placed in a topic.`);
  $("account-other").textContent = other.join(" ");
  $("account-other").hidden = other.length === 0;
  $("account-result").hidden = false;
}

async function ask(account) {
  const q = account === null ? "" : `?account=${encodeURIComponent(account)}`;
  const res = await fetch(`/api/inspect/account${q}`, { headers: { Accept: "application/json" }, credentials: "same-origin" });
  let body = {};
  try { body = await res.json(); } catch { /* reported below */ }
  return { status: res.status, body };
}

async function run(account, { push = true } = {}) {
  account = account.trim();
  if (!account) {
    showError("Type a handle, a DID, or a link to a profile.");
    return;
  }
  showError("");
  $("account-result").hidden = true;
  $("account-status").hidden = false;
  $("account-button").disabled = true;
  let r;
  try {
    r = await ask(account);
  } catch {
    r = { status: 0, body: {} };
  }
  $("account-status").hidden = true;
  $("account-button").disabled = false;
  if (r.status === 403) {
    show("forbidden");
    return;
  }
  if (r.status !== 200) {
    showError(typeof r.body.message === "string" && r.body.message ? r.body.message
      : r.status === 0 ? "Couldn't reach the server. Try again." : `Couldn't read that account (${r.status}). Try again.`);
    return;
  }
  if (push) history.pushState(null, "", `/inspect/account?account=${encodeURIComponent(account)}`);
  renderAccount(r.body);
}

export async function main() {
  const problem = setupSignInForm();
  let me;
  try {
    me = await whoAmI();
  } catch {
    show("inspector");
    showError("Couldn't reach the server. Reload to try again.");
    return;
  }
  if (me === "off") {
    show("signed-out");
    signInOff();
    return;
  }
  if (!me) {
    show("signed-out");
    showSignInProblem(problem);
    return;
  }
  if (me.owner !== true) {
    show("forbidden");
    return;
  }
  show("inspector");
  $("account-form").addEventListener("submit", (ev) => {
    ev.preventDefault();
    run($("account-input").value);
  });
  const initial = new URLSearchParams(location.search).get("account");
  if (initial) {
    $("account-input").value = initial;
    await run(initial, { push: false });
    return;
  }
  $("account-input").focus();
}

if (document.getElementById("account-form")) main();
