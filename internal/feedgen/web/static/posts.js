// Fetching and rendering Bluesky posts. Posts come from Bluesky's public AppView; every
// piece of post content goes into the page as text nodes, never as HTML.

const APPVIEW = "https://public.api.bsky.app/xrpc/app.bsky.feed.getPosts";
const BATCH = 25;
// Labels that keep a post off this public page even if our own filters missed them.
const HIDE_LABELS = new Set(["porn", "sexual", "nudity", "sexual-figurative", "graphic-media", "gore", "!hide", "!takedown"]);

const cache = new Map(); // uri -> post view, or null when the AppView didn't return it

/** Fetches post views for uris (cached), in batches of 25. */
export async function hydrate(uris, signal) {
  const missing = uris.filter((u) => !cache.has(u));
  const batches = [];
  for (let i = 0; i < missing.length; i += BATCH) batches.push(missing.slice(i, i + BATCH));
  await Promise.all(batches.map(async (batch) => {
    const qs = batch.map((u) => "uris=" + encodeURIComponent(u)).join("&");
    const res = await fetch(`${APPVIEW}?${qs}`, { signal });
    if (!res.ok) throw new Error(`Bluesky returned ${res.status}`);
    const body = await res.json();
    const got = new Map((body.posts || []).map((p) => [p.uri, p]));
    for (const u of batch) cache.set(u, got.get(u) || null);
  }));
  return uris.map((u) => cache.get(u)).filter((p) => p && !hidden(p));
}

function hidden(p) {
  const labels = [...(p.labels || []), ...(p.author?.labels || [])];
  return labels.some((l) => HIDE_LABELS.has(l.val));
}

// ---------- small DOM helpers ----------

export function el(tag, attrs = {}, ...children) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") n.className = v;
    else if (k === "text") n.textContent = v;
    else if (k === "style") n.style.cssText = v; // CSSOM, allowed by the page's CSP
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) if (c !== null && c !== undefined && c !== false) n.append(c);
  return n;
}

function svg(path) {
  const s = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  s.setAttribute("viewBox", "0 0 24 24");
  s.setAttribute("fill", "none");
  s.setAttribute("stroke", "currentColor");
  s.setAttribute("stroke-width", "2");
  s.setAttribute("stroke-linecap", "round");
  s.setAttribute("stroke-linejoin", "round");
  const p = document.createElementNS("http://www.w3.org/2000/svg", "path");
  p.setAttribute("d", path);
  s.append(p);
  return s;
}
const ICON_REPLY = "M21 12a8 8 0 0 1-11.6 7.1L4 20l1-4.6A8 8 0 1 1 21 12z";
const ICON_REPOST = "M17 2l4 4-4 4M3 11V9a3 3 0 0 1 3-3h15M7 22l-4-4 4-4M21 13v2a3 3 0 0 1-3 3H3";
const ICON_LIKE = "M20.8 4.6a5.5 5.5 0 0 0-7.8 0L12 5.7l-1-1.1a5.5 5.5 0 0 0-7.8 7.8L12 21.2l8.8-8.8a5.5 5.5 0 0 0 0-7.8z";

export function compact(n) {
  if (!n) return "0";
  if (n < 1000) return String(n);
  if (n < 10000) return (n / 1000).toFixed(1).replace(/\.0$/, "") + "K";
  if (n < 1e6) return Math.round(n / 1000) + "K";
  return (n / 1e6).toFixed(1).replace(/\.0$/, "") + "M";
}

export function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "now";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

function rkey(uri) { return uri.split("/").pop(); }
export function postURL(p) { return `https://bsky.app/profile/${p.author.handle}/post/${rkey(p.uri)}`; }

// ---------- rich text ----------

const utf8 = new TextEncoder();
const utf8d = new TextDecoder();

/** Renders post text with its facets (links, mentions, hashtags). Offsets are UTF-8 bytes. */
function richText(text, facets) {
  const out = el("div", { class: "post-text" });
  if (!text) return out;
  if (!facets?.length) { out.textContent = text; return out; }
  const bytes = utf8.encode(text);
  const sorted = [...facets].sort((a, b) => a.index.byteStart - b.index.byteStart);
  let pos = 0;
  for (const f of sorted) {
    const { byteStart, byteEnd } = f.index;
    if (byteStart < pos || byteEnd > bytes.length || byteEnd <= byteStart) continue;
    out.append(utf8d.decode(bytes.slice(pos, byteStart)));
    const label = utf8d.decode(bytes.slice(byteStart, byteEnd));
    const feat = f.features?.[0] || {};
    let href = null;
    if (feat.$type === "app.bsky.richtext.facet#link" && /^https?:\/\//i.test(feat.uri)) href = feat.uri;
    else if (feat.$type === "app.bsky.richtext.facet#mention") href = `https://bsky.app/profile/${feat.did}`;
    else if (feat.$type === "app.bsky.richtext.facet#tag") href = `https://bsky.app/hashtag/${encodeURIComponent(feat.tag)}`;
    out.append(href ? el("a", { href, target: "_blank", rel: "noopener nofollow", onclick: (e) => e.stopPropagation(), text: label }) : label);
    pos = byteEnd;
  }
  out.append(utf8d.decode(bytes.slice(pos)));
  return out;
}

// ---------- embeds ----------

function images(view) {
  const imgs = (view.images || []).slice(0, 4);
  return el("div", { class: `images n${imgs.length}` },
    imgs.map((i) => el("img", { src: i.thumb, alt: i.alt || "", loading: "lazy", decoding: "async" })));
}

function video(view) {
  if (!view.thumbnail) return null;
  return el("div", { class: "video" }, el("img", { src: view.thumbnail, alt: view.alt || "Video", loading: "lazy" }));
}

function external(view) {
  const x = view.external || {};
  if (!/^https?:\/\//i.test(x.uri || "")) return null;
  let domain = "";
  try { domain = new URL(x.uri).hostname.replace(/^www\./, ""); } catch { /* ignore */ }
  return el("a", { class: "ext", href: x.uri, target: "_blank", rel: "noopener nofollow", onclick: (e) => e.stopPropagation() },
    x.thumb ? el("img", { src: x.thumb, alt: "", loading: "lazy" }) : null,
    el("div", { class: "ext-body" },
      el("div", { class: "ext-domain", text: domain }),
      x.title ? el("div", { class: "ext-title", text: x.title }) : null,
      x.description ? el("div", { class: "ext-desc", text: x.description }) : null));
}

function quote(rec) {
  if (!rec || rec.$type !== "app.bsky.embed.record#viewRecord" || !rec.author) return null;
  if ((rec.labels || []).some((l) => HIDE_LABELS.has(l.val))) return null;
  const a = rec.author;
  const media = (rec.embeds || []).map(media1).filter(Boolean);
  return el("div", { class: "quote" },
    el("div", { class: "post-head" },
      a.avatar ? el("img", { class: "qavatar", src: a.avatar, alt: "", loading: "lazy" }) : null,
      el("span", { class: "post-name", text: a.displayName || a.handle }),
      el("span", { class: "post-handle", text: "@" + a.handle }),
      el("span", { class: "post-time", text: "· " + ago(rec.indexedAt || rec.value?.createdAt) })),
    richText(rec.value?.text, rec.value?.facets),
    media);
}

function media1(view) {
  switch (view?.$type) {
    case "app.bsky.embed.images#view": return images(view);
    case "app.bsky.embed.video#view": return video(view);
    case "app.bsky.embed.external#view": return external(view);
    default: return null;
  }
}

function embed(view) {
  if (!view) return null;
  switch (view.$type) {
    case "app.bsky.embed.record#view": return quote(view.record);
    case "app.bsky.embed.recordWithMedia#view":
      return [media1(view.media), quote(view.record?.record)];
    default: return media1(view);
  }
}

// ---------- the post card ----------

/** Renders a post card; `why` is the list of score chips (hidden unless scores are on). */
export function renderPost(p, why) {
  const a = p.author;
  const initial = (a.displayName || a.handle || "?").trim().charAt(0).toUpperCase();
  const avatar = a.avatar
    ? el("img", { class: "avatar", src: a.avatar, alt: "", loading: "lazy" })
    : el("div", { class: "avatar placeholder", text: initial });
  const url = postURL(p);
  return el("article", {
    class: "post", tabindex: "0", role: "link", "aria-label": `Post by ${a.displayName || a.handle}`,
    onclick: () => window.open(url, "_blank", "noopener"),
    onkeydown: (e) => { if (e.key === "Enter") window.open(url, "_blank", "noopener"); },
  },
    avatar,
    el("div", {},
      el("div", { class: "post-head" },
        el("span", { class: "post-name", text: a.displayName || a.handle }),
        el("span", { class: "post-handle", text: "@" + a.handle }),
        el("span", { class: "post-time", text: "· " + ago(p.indexedAt) })),
      richText(p.record?.text, p.record?.facets),
      embed(p.embed),
      el("div", { class: "post-foot" },
        el("span", { title: "Replies" }, svg(ICON_REPLY), compact(p.replyCount)),
        el("span", { title: "Reposts and quotes" }, svg(ICON_REPOST), compact((p.repostCount || 0) + (p.quoteCount || 0))),
        el("span", { title: "Likes" }, svg(ICON_LIKE), compact(p.likeCount))),
      why));
}
