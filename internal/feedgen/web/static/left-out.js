// The page at /filtered/left-out?feed=<rkey>: the posts a filtered feed left out of the signed-in
// viewer's own feed in the last week, and which filter did it, a page at a time as they scroll. All
// text is put on the page with textContent, never as HTML.

import "./header.js"; // the header every page shares
import { $ } from "./dom.js";
import { el, hydrate, renderPost, ago } from "./posts.js";
import { describe } from "./scores.js";

const PAGE = 50;

const pctText = (v) => `${Math.round((typeof v === "number" ? v : 0) * 100)}%`;

/** reasonText says, in words, which filter left a post out (feedgen.LeftOutReason). */
export function reasonText(r, topicName = (p) => p) {
  if (!r || typeof r !== "object") return "Left out by your filters";
  switch (r.kind) {
    case "unscored":
      return "The model couldn't judge it (not in English, or a reply), and you leave those out";
    case "topic":
      return `About ${topicName(r.name)}: the model is ${pctText(r.value)} sure, and you leave it out above ${pctText(r.cutoff)}`;
    case "tone":
    case "signal": {
      const name = describe(r.kind, r.name).name;
      const side = r.bound === "min" ? `below your minimum of ${pctText(r.cutoff)}` : `above your maximum of ${pctText(r.cutoff)}`;
      const rule = r.rule ? ` (your rule for ${topicName(r.rule)})` : "";
      return `${name} ${pctText(r.value)}, ${side}${rule}`;
    }
    default:
      return "Left out by your filters";
  }
}

/** postLink is a post's at:// URI as a link to it in the Bluesky app. */
export function postLink(uri) {
  const m = /^at:\/\/([^/]+)\/app\.bsky\.feed\.post\/([^/]+)$/.exec(uri);
  return m ? `https://bsky.app/profile/${m[1]}/post/${m[2]}` : "https://bsky.app";
}

async function getJSON(url) {
  const res = await fetch(url, { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (!res.ok) {
    const err = new Error(String(res.status));
    err.status = res.status;
    throw err;
  }
  return res.json();
}

function notice(text) {
  $("notice").textContent = text;
  $("notice").hidden = !text;
}

async function main() {
  const rkey = new URLSearchParams(location.search).get("feed") || "";
  let state;
  try {
    state = await getJSON("/api/me/filtered");
  } catch (err) {
    $("status").textContent = "";
    notice(err.status === 401 ? "Sign in on the filtered feeds page to see this." : "Couldn't load your feeds. Reload to try again.");
    return;
  }
  const feeds = Array.isArray(state.feeds) ? state.feeds : [];
  const feed = feeds.find((f) => f.rkey === rkey) || feeds[0];
  if (!feed) {
    $("status").textContent = "There are no filtered feeds yet.";
    return;
  }
  $("title").textContent = `Left out of ${feed.name}`;
  $("feed-tabs").replaceChildren(...feeds.map((f) => el("a", {
    class: "filtered-tab", href: `/filtered/left-out?feed=${encodeURIComponent(f.rkey)}`, text: f.name,
    "aria-current": f.rkey === feed.rkey ? "page" : null,
  })));

  const names = new Map();
  try {
    const tax = await getJSON("/api/taxonomy?for=filters");
    for (const b of tax.topics || []) {
      names.set(b.id, b.name);
      for (const t of b.subtopics || []) names.set(t.id, t.name);
    }
  } catch {
    // Topics are named by their ids instead.
  }
  const topicName = (p) => names.get(p) || p;

  let cursor = "";
  let count = 0;
  let loading = false;
  let done = false;
  async function more() {
    if (loading || done) return;
    loading = true;
    $("more").disabled = true;
    $("status").textContent = count ? `${count} so far · loading more…` : "Loading…";
    let page;
    try {
      page = await getJSON(`/api/me/filtered/${encodeURIComponent(feed.rkey)}/left-out?limit=${PAGE}` +
        (cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""));
    } catch {
      $("status").textContent = `${count} so far · couldn't load more.`;
      $("more-row").hidden = false;
      $("more").disabled = false;
      loading = false;
      return;
    }
    const posts = Array.isArray(page.posts) ? page.posts.filter((p) => p && typeof p.uri === "string") : [];
    let shown = new Map();
    try {
      shown = new Map((await hydrate(posts.map((p) => p.uri))).map((v) => [v.uri, v]));
    } catch {
      // Bluesky didn't answer: these posts get their link instead.
    }
    for (const p of posts) {
      const why = el("p", { class: "filtered-why" }, el("strong", { text: "Left out: " }), reasonText(p.reason, topicName),
        p.at ? ` · ${ago(p.at)}` : "");
      const view = shown.get(p.uri);
      $("posts").append(view ? el("li", { "data-uri": p.uri }, renderPost(view, why)) : el("li", { "data-uri": p.uri },
        el("article", { class: "post filtered-hidden-post" }, el("div", {}, why,
          el("p", { class: "me-fine", text: "Not shown here: it has a label this site doesn't show, or it's gone. " },
            el("a", { href: postLink(p.uri), target: "_blank", rel: "noopener", text: "Open it on Bluesky" }))))));
    }
    count += posts.length;
    cursor = typeof page.cursor === "string" ? page.cursor : "";
    done = cursor === "";
    if (count === 0) $("status").textContent = "Nothing has been left out of your feed in the last week.";
    else $("status").textContent = done ? `${count} post${count === 1 ? "" : "s"} left out in the last week.` : `${count} so far.`;
    $("more-row").hidden = done;
    $("more").disabled = false;
    loading = false;
  }
  $("more").addEventListener("click", more);
  // More load as the end of the list comes into view; the button is there too, for when it can't.
  if (typeof IntersectionObserver === "function") {
    new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) more();
    }, { rootMargin: "800px" }).observe($("sentinel"));
  }
  await more();
}

if (document.getElementById("posts")) main();
