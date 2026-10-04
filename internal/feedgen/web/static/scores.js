// What the model's tone and signal scores are called on the page at /me, and small helpers for
// showing a range or a boost. (The feed builder at / has the same names.)

export const TONES = {
  informative: ["📰", "Informative", "facts, news, how-tos"],
  humorous: ["😂", "Funny", "jokes and bits"],
  personal: ["💬", "Personal", "life updates, feelings"],
  outraged: ["😡", "Outraged", "angry, heated"],
  supportive: ["🤗", "Supportive", "warm, encouraging"],
  other: ["✨", "Other", "none of the above"],
};

export const SIGNALS = {
  substance: ["🧠", "Substance", "has something to say"],
  general_interest: ["🌍", "Broad appeal", "interesting to most people"],
  news: ["🗞️", "Newsy", "reports what happened"],
  sentiment: ["😊", "Positive", "upbeat rather than negative"],
  critical: ["👎", "Critical", "against or mocking what it's about"],
  promo: ["📣", "Promotional", "any kind of promotion"],
  ad: ["🏷️", "Ad", "selling a product, service, or deal"],
  self_promo: ["🎨", "Own work", "sharing their own art, writing, stream…"],
  engagement_bait: ["🎣", "Engagement bait", "asks for likes, follows, reposts"],
  spam: ["🚫", "Spam", "scams, schemes, junk"],
  meme: ["😂", "Meme", "a captioned or edited joke image, a reaction image"],
};

/** The emoji, name and description of a score, with sensible stand-ins for one we don't know. */
export function describe(kind, key) {
  const meta = (kind === "tone" ? TONES : SIGNALS)[key];
  return meta ? { icon: meta[0], name: meta[1], hint: meta[2] } : { icon: "", name: String(key), hint: "" };
}

/** rangeText says what a cutoff range lets through: "any", "≤ 40%", "≥ 20%", or "20–40%". */
export function rangeText(d) {
  if (d.min <= 0 && d.max >= 1) return "any";
  if (d.min <= 0) return `≤ ${Math.round(d.max * 100)}%`;
  if (d.max >= 1) return `≥ ${Math.round(d.min * 100)}%`;
  return `${Math.round(d.min * 100)}–${Math.round(d.max * 100)}%`;
}

/** boostText says how much a score moves a post: "none", "+1", "-2.5". */
export function boostText(w) {
  return w === 0 ? "none" : (w > 0 ? "+" : "") + w;
}

/** A dial is a score's cutoffs and boost: leaving posts out, and lifting or sinking the rest. */
export const dialActive = (d) => d.min > 0 || d.max < 1 || d.w !== 0;
export const newDial = () => ({ min: 0, max: 1, w: 0 });
