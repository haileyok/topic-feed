// What a signed-in person can do with their own feeds from the builder: save the feed being built as
// theirs, or change one they saved. (Publishing it on Bluesky is done on the page at /feeds.)
//
// All text is put on the page with textContent, never as HTML.

import { el } from "./posts.js";

export const RKEY = /^[a-z0-9][a-z0-9-]{0,14}$/;
export const MAX_NAME = 24;
export const MAX_DESCRIPTION = 300;

const asJSON = { Accept: "application/json" };

/** slugify is a feed's key made from its name: lowercase letters, digits and dashes, at most 15. */
export const slugify = (name) =>
  (name || "my-feed").toLowerCase().normalize("NFKD").replace(/[\u0300-\u036f]/g, "") // "ü" is "u" and a mark: drop the mark
    .replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 15).replace(/-+$/, "") || "my-feed";

/** clip is text cut to at most n characters (not UTF-16 units, so an emoji isn't cut in half). */
export const clip = (text, n) => Array.from(typeof text === "string" ? text : "").slice(0, n).join("");

export const defaultDescription = (name) => clip(`${name || "My feed"}, picked by a topic classifier. No keyword lists.`, MAX_DESCRIPTION);

/** fetchMe is who is signed in ({did, handle}), or null. */
export async function fetchMe() {
  try {
    const res = await fetch("/api/me", { headers: asJSON, credentials: "same-origin" });
    if (!res.ok) return null;
    const me = await res.json();
    return me && typeof me.did === "string" ? me : null;
  } catch {
    return null;
  }
}

/** fetchMine is the signed-in account's feeds and what they may do with them ({feeds, limits, owner, …}), or null. */
export async function fetchMine() {
  try {
    const res = await fetch("/api/me/feeds", { headers: asJSON, credentials: "same-origin" });
    if (!res.ok) return null;
    const data = await res.json();
    return data && Array.isArray(data.feeds) ? data : null;
  } catch {
    return null;
  }
}

/**
 * feedBody is what is sent to save the feed the builder describes (spec is its settings). Posts about
 * adult topics are left out unless the owner has switched adult content on, and a feed of whole topics
 * reaches back further, which needs more candidates.
 */
export function feedBody(spec, { name, description, adult = false, owner = false, maxPosts = 0 } = {}) {
  const exclude = adult ? { ...spec.exclude } : { ...spec.exclude, adult_content: 0.2 };
  const broad = spec.paths.some((p) => p === "*" || !p.includes("/"));
  const body = {
    display_name: clip(name || "My feed", MAX_NAME),
    description: clip(description, MAX_DESCRIPTION),
    accepts_interactions: true,
    paths: spec.paths,
    min_prob: spec.min_prob,
    exclude,
    tone: spec.tone,
    signals: spec.signals,
    ranking: spec.ranking,
  };
  if (spec.max_age_minutes) body.max_age_minutes = spec.max_age_minutes;
  if (spec.topic_rules && Object.keys(spec.topic_rules).length) body.topic_rules = spec.topic_rules;
  if (adult && owner) body.allow_adult = true;
  if (broad) body.max_posts = Math.min(maxPosts > 0 ? maxPosts : 10000, 10000);
  return body;
}

const PROBLEMS = {
  401: "You're signed out. Sign in again to save.",
  403: "That didn't come from this page. Reload it and try again.",
  413: "That feed is too big.",
  429: "Slow down a moment, then try again.",
};

/** problemText says in words what went wrong with a request, from what the server said. */
export function problemText(status, data) {
  if (data && typeof data.message === "string" && data.message) return data.message;
  return PROBLEMS[status] || "Couldn't save that just now. Try again in a moment.";
}

/** saveFeed saves a feed under this key. It never throws: {ok: true, feed, created} or {ok: false, message}. */
export async function saveFeed(rkey, body) {
  let res;
  try {
    res = await fetch(`/api/me/feeds/${encodeURIComponent(rkey)}`, {
      method: "PUT", credentials: "same-origin", headers: { ...asJSON, "Content-Type": "application/json" }, body: JSON.stringify(body),
    });
  } catch {
    return { ok: false, message: "Couldn't reach the server. Check your connection and try again." };
  }
  let data = null;
  try {
    data = await res.json();
  } catch {
    // not JSON: the status says enough
  }
  if (res.ok && data && data.feed && typeof data.feed.rkey === "string") return { ok: true, feed: data.feed, created: data.created === true };
  return { ok: false, status: res.status, message: problemText(res.status, data) };
}

const left = (mine) => {
  const max = mine.limits ? mine.limits.maxFeeds : 0;
  return max > 0 ? `You have ${mine.feeds.length} of ${max} feeds.` : `You have ${mine.feeds.length} ${mine.feeds.length === 1 ? "feed" : "feeds"}.`;
};

/**
 * savePanel is the part of the builder's side panel where the feed being built is saved as the signed-in
 * account's own. state holds name, description, rkey (a key the person typed, "" for one made from the
 * name) and editing (the key of the feed being changed, "" for a new one); spec() is the builder's
 * settings. onSaved(feed, notice) is told when the feed is saved, with what to tell the person; the
 * panel is made anew then, and shows notice ({text, tone}) when given it.
 *
 * Someone signed out (me is null) gets the same panel, and saving asks them to sign in instead
 * (onSignIn). mine, their feeds, is null when it couldn't be read: saving still works.
 */
export function savePanel({ me, mine, state, spec, adult = () => false, onSaved = () => {}, onSignIn = () => {}, notice = null }) {
  const msg = el("p", { class: "mine-msg", role: "status", "aria-live": "polite", hidden: true });
  const say = (text, tone) => {
    msg.textContent = text || "";
    msg.hidden = !text;
    msg.dataset.tone = tone || "";
  };
  if (notice) say(notice.text, notice.tone);
  const editing = state.editing || "";
  const button = el("button", { class: "btn btn-primary", type: "button", text: editing ? "Save changes" : "Save as my feed" });
  const key = editing
    ? el("p", { class: "section-hint", text: `Feed key: ${editing}` })
    : el("div", { class: "field" },
      el("label", { for: "mine-rkey", text: "Feed key (its address on Bluesky)" }),
      el("input", {
        class: "text-input", id: "mine-rkey", maxlength: 15, placeholder: slugify(state.name), value: state.rkey || "", autocomplete: "off", spellcheck: "false",
        oninput: (e) => { state.rkey = e.target.value.trim(); },
      }));
  const description = el("textarea", {
    class: "text-input", id: "mine-description", rows: 3, maxlength: MAX_DESCRIPTION, "aria-label": "Description",
    placeholder: defaultDescription(state.name),
    oninput: (e) => { state.description = e.target.value; },
  });
  description.value = state.description || "";

  button.addEventListener("click", async () => {
    if (!me) {
      say("", "");
      onSignIn();
      return;
    }
    const name = (state.name || "").trim();
    const s = spec();
    if (!name) return say("Give the feed a name first.", "error");
    if (s.paths.length === 0) return say("Pick at least one topic, or turn on All topics.", "error");
    const rkey = editing || (state.rkey ? state.rkey.trim() : slugify(name));
    if (!RKEY.test(rkey)) return say("The feed key must be 1-15 lowercase letters, digits or dashes.", "error");
    button.disabled = true;
    say("Saving…", "");
    const body = feedBody(s, {
      name, description: state.description || defaultDescription(name), adult: adult(), owner: !!(mine && mine.owner),
      maxPosts: mine && mine.limits ? mine.limits.maxPosts : 0,
    });
    const r = await saveFeed(rkey, body);
    button.disabled = false;
    if (!r.ok) {
      if (r.status === 401) return onSignIn(); // signed out since the page loaded
      return say(r.message, "error");
    }
    if (mine) {
      const i = mine.feeds.findIndex((f) => f.rkey === r.feed.rkey);
      if (i >= 0) mine.feeds[i] = r.feed;
      else mine.feeds.push(r.feed);
    }
    state.editing = r.feed.rkey;
    const text = r.created ? "Saved. It's yours now: publish it from your feeds." : "Changes saved. Publish them from your feeds to update Bluesky.";
    say(text, "ok");
    onSaved(r.feed, { text, tone: "ok" });
  });

  return el("div", { class: "section", id: "mine" },
    el("div", { class: "section-title", text: editing ? "Your feed" : "Save as my feed" }),
    el("p", {
      class: "section-hint",
      text: me ? `Signed in as ${me.handle || me.did}.${mine ? " " + left(mine) : ""}` : "Sign in to save this feed as yours and publish it on Bluesky.",
    }),
    description, key, msg,
    el("div", { class: "btn-row" }, button, el("a", { class: "btn btn-ghost", href: "/feeds", text: "My feeds" })));
}
