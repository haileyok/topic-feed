// The page at /feeds: a signed-in person's own feeds, and publishing them to Bluesky from their browser.
//
// What is saved here (the feeds themselves) comes from the service; what is published is read from, and
// written to, the person's own account by publish.js with a connection that stays in this browser.
// All text is put on the page with textContent, never as HTML.

import { $, str } from "./dom.js";
import { el } from "./posts.js";
import { fetchMe, fetchMine, problemText } from "./mine.js";
import { createPublisher, statusOf, PublishError } from "./publish.js";
import { setupSignInForm, showSignInProblem } from "./signin-form.js";

export const STATUS_TEXT = {
  draft: "Not published",
  published: "Published",
  changed: "Changed since you published it",
  conflict: "Key already in use",
  unknown: "Connect to see",
};

const CONNECTION_TEXT = {
  unavailable: "Publishing isn't available in this browser right now. You can still make, edit and delete feeds; reload the page to try again.",
  disconnected: "Connect your Bluesky account to publish your feeds. Your browser keeps the connection and this service never gets the keys. It lets this page create, change and remove your feed records, and nothing else.",
  declined: "You didn't allow this page to manage your feed records, so nothing was connected. Connect again and allow it to publish.",
};

function show(which) {
  for (const id of ["loading", "signed-out", "feeds-main"]) $(id).hidden = id !== which;
}

/** topicsText is what a feed takes, in a line. */
export function topicsText(paths) {
  const list = Array.isArray(paths) ? paths.filter((p) => typeof p === "string") : [];
  if (list.includes("*")) return "All topics";
  const name = (p) => p.split("/").pop().replaceAll("_", " ");
  const shown = list.slice(0, 4).map(name).join(", ");
  return list.length > 4 ? `${shown} and ${list.length - 4} more` : shown;
}

/** feedUrl is where a feed is in the Bluesky app. */
export const feedUrl = (did, rkey) => `https://bsky.app/profile/${encodeURIComponent(did)}/feed/${encodeURIComponent(rkey)}`;

async function deleteFeed(rkey) {
  let res;
  try {
    res = await fetch(`/api/me/feeds/${encodeURIComponent(rkey)}`, { method: "DELETE", credentials: "same-origin", headers: { Accept: "application/json" } });
  } catch {
    return { ok: false, message: "Couldn't reach the server. Check your connection and try again." };
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    // not JSON: the status says enough
  }
  return res.ok ? { ok: true } : { ok: false, message: problemText(res.status, data).replace("save", "delete") };
}

const messageOf = (err) => (err instanceof PublishError ? err.message : "Something went wrong. Try again.");

/**
 * main runs the page. ClientClass is the OAuth client (null: publishing is not available); confirm asks
 * the person before something that can't be undone. It resolves when the page has been drawn and the
 * connection to Bluesky (if there is one) has been looked at.
 */
export async function main({ ClientClass = null, confirm = (m) => globalThis.confirm(m), origin = globalThis.location.origin } = {}) {
  const problem = setupSignInForm(); // a problem the last sign-in came back with, if any
  const me = await fetchMe();
  if (!me) {
    show("signed-out");
    showSignInProblem(problem);
    return;
  }
  const mine = await fetchMine();
  show("feeds-main");
  $("who").textContent = me.handle || me.did;
  $("connect-handle").value = me.handle || "";
  if (!mine) {
    $("count").textContent = "";
    $("empty").hidden = true;
    notice("Couldn't read your feeds just now. Reload the page to try again.", "error");
    return;
  }

  const ctx = { me, mine, publisher: null, records: null, connection: "checking", detail: "", busy: "" };
  const statusFor = (feed) => (ctx.records === null ? "unknown" : statusOf(feed.spec, ctx.records[feed.rkey] ? ctx.records[feed.rkey].value : null, mine.serviceDid));

  function notice(text, tone = "") {
    $("notice").textContent = text || "";
    $("notice").hidden = !text;
    $("notice").dataset.tone = tone;
  }

  // ---------- drawing ----------

  function renderConnection() {
    const box = $("connection");
    box.dataset.state = ctx.connection;
    const handle = me.handle || me.did;
    let text = ctx.detail || CONNECTION_TEXT[ctx.connection] || "Checking…";
    if (ctx.connection === "connected") text = `Connected as ${handle}. This page can create, change and remove your feed records.`;
    if (ctx.connection === "wrong_account") text = `You connected as another account (${ctx.other}), not ${handle}. Connect as ${handle} to publish these feeds.`;
    $("connection-text").textContent = text;
    $("connect-form").hidden = !["disconnected", "declined", "wrong_account"].includes(ctx.connection);
    $("disconnect-button").hidden = ctx.connection !== "connected";
  }

  function card(feed) {
    const status = statusFor(feed);
    const connected = ctx.connection === "connected";
    const busy = ctx.busy === feed.rkey;
    const needsConnection = connected ? "" : "Connect your Bluesky account first.";
    const action = (text, run, { primary = false, title = "", off = false } = {}) => el("button", {
      class: "btn" + (primary ? " btn-primary" : ""), type: "button", text, title: title || null, disabled: (busy || off) ? true : null, onclick: run,
    });
    const publishLabel = status === "changed" ? "Update on Bluesky" : "Publish";
    return el("li", { class: "feeds-card", "data-rkey": feed.rkey, "data-status": status },
      el("div", { class: "feeds-card-head" },
        el("h2", { class: "feeds-name", text: str(feed.spec.display_name) || feed.rkey }),
        el("code", { class: "feeds-key", text: feed.rkey }),
        el("span", { class: "badge feeds-status", text: STATUS_TEXT[status] })),
      feed.spec.description ? el("p", { class: "feeds-desc", text: feed.spec.description }) : null,
      el("p", { class: "feeds-topics", text: `Topics: ${topicsText(feed.spec.paths)}` }),
      status === "conflict" ? el("p", { class: "me-fine", text: "Your account already has a feed with this key that was made with another service, so this one can't be published under it. Save it again under another key from the builder." }) : null,
      el("div", { class: "feeds-actions" },
        status !== "published" && status !== "conflict" ? action(publishLabel, () => publish(feed), { primary: true, off: !connected, title: needsConnection }) : null,
        status === "published" || status === "changed" ? action("Unpublish", () => unpublish(feed), { off: !connected, title: needsConnection }) : null,
        el("a", { class: "btn", href: `/?edit=${encodeURIComponent(feed.rkey)}`, text: "Edit" }),
        status === "published" || status === "changed"
          ? el("a", { class: "btn btn-ghost", href: feedUrl(me.did, feed.rkey), target: "_blank", rel: "noopener noreferrer", text: "View on Bluesky" }) : null,
        action("Delete", () => remove(feed), { title: "" })));
  }

  function render() {
    renderConnection();
    const max = mine.limits ? mine.limits.maxFeeds : 0;
    $("count").textContent = max > 0 ? `${mine.feeds.length} of ${max} feeds.` : `${mine.feeds.length} ${mine.feeds.length === 1 ? "feed" : "feeds"}.`;
    $("empty").hidden = mine.feeds.length > 0;
    $("feeds-list").replaceChildren(...mine.feeds.map(card));
  }

  // ---------- doing things ----------

  async function whileBusy(feed, work) {
    ctx.busy = feed.rkey;
    render();
    try {
      await work();
    } catch (err) {
      notice(messageOf(err), "error");
    } finally {
      ctx.busy = "";
      render();
    }
  }

  const publish = (feed) => whileBusy(feed, async () => {
    notice("");
    const out = await ctx.publisher.publish(feed.rkey, feed.spec);
    ctx.records = { ...(ctx.records || {}), [feed.rkey]: out };
    notice("Published. Bluesky can take a minute to show a new or changed feed.", "ok");
  });

  const unpublish = (feed) => whileBusy(feed, async () => {
    notice("");
    if (!confirm(`Take “${feed.spec.display_name}” off Bluesky? The feed stays saved here, and you can publish it again.`)) return;
    await ctx.publisher.unpublish(feed.rkey);
    const records = { ...(ctx.records || {}) };
    delete records[feed.rkey];
    ctx.records = records;
    notice("Unpublished. It can take a minute to disappear from Bluesky.", "ok");
  });

  const remove = (feed) => whileBusy(feed, async () => {
    notice("");
    const status = statusFor(feed);
    const published = status === "published" || status === "changed";
    const warning = status === "unknown" ? " If it is published on Bluesky, connect your account first so it can be taken off there too." : "";
    if (!confirm(`Delete “${feed.spec.display_name}”? This can't be undone.${warning}`)) return;
    if (published && ctx.connection === "connected") {
      try {
        await ctx.publisher.unpublish(feed.rkey);
      } catch (err) {
        notice(`${messageOf(err)} The feed was not deleted.`, "error");
        return;
      }
    }
    const r = await deleteFeed(feed.rkey);
    if (!r.ok) {
      notice(r.message, "error");
      return;
    }
    mine.feeds = mine.feeds.filter((f) => f.rkey !== feed.rkey);
    if (ctx.records) {
      const records = { ...ctx.records };
      delete records[feed.rkey];
      ctx.records = records;
    }
    notice(`Deleted “${feed.spec.display_name}”.`, "ok");
  });

  // ---------- the connection to Bluesky ----------

  async function connect(ev) {
    ev.preventDefault();
    if (!ctx.publisher) return;
    $("connect-button").disabled = true;
    notice("");
    try {
      await ctx.publisher.connect($("connect-handle").value); // goes to Bluesky: nothing more happens here
    } catch (err) {
      notice(messageOf(err), "error");
    } finally {
      $("connect-button").disabled = false;
    }
  }

  async function disconnect() {
    if (!ctx.publisher) return;
    await ctx.publisher.disconnect();
    ctx.connection = "disconnected";
    ctx.records = null;
    notice("");
    render();
  }

  $("connect-form").addEventListener("submit", connect);
  $("disconnect-button").addEventListener("click", disconnect);
  render();

  if (!ClientClass) {
    ctx.connection = "unavailable";
    render();
    return;
  }
  ctx.publisher = createPublisher({
    ClientClass, clientId: `${origin}/oauth/browser-client-metadata.json`, handleResolver: origin,
    did: me.did, serviceDid: mine.serviceDid, scope: mine.scope,
  });
  try {
    const init = await ctx.publisher.init();
    ctx.connection = init.state;
    ctx.other = init.did || "";
    if (init.state === "connected") {
      try {
        ctx.records = await ctx.publisher.records();
        if (init.returned) notice("Connected. You can publish your feeds now.", "ok");
      } catch (err) {
        notice(messageOf(err), "error");
      }
    }
  } catch (err) {
    ctx.connection = "unavailable";
    ctx.detail = messageOf(err);
  }
  render();
}
