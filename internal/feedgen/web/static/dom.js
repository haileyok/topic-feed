// Small helpers shared by the scripts of the page at /me. Everything put on the page goes through
// textContent, never as HTML: what the page shows includes what other people posted.

export const $ = (id) => document.getElementById(id);

export function make(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

export const str = (v) => (typeof v === "string" ? v : "");
export const num = (v) => (typeof v === "number" && Number.isFinite(v) ? v : 0);
export const pct = (share) => Math.round(Math.min(1, Math.max(0, num(share))) * 100);

// safeLink is the address of a post on Bluesky, or "" for anything else. Posts' text and addresses
// come from what other people posted, so only a real bsky.app address becomes a link.
export function safeLink(url) {
  try {
    const u = new URL(str(url));
    return u.protocol === "https:" && u.hostname === "bsky.app" && !u.username && !u.password ? u.href : "";
  } catch {
    return "";
  }
}

// postLink is a post's text as a link to the post on Bluesky, or as plain words if its address
// isn't one.
export function postLink(text, url) {
  const link = safeLink(url);
  if (!link) return document.createTextNode(text);
  const a = make("a", "", text);
  a.href = link;
  a.target = "_blank";
  a.rel = "noopener noreferrer";
  return a;
}
