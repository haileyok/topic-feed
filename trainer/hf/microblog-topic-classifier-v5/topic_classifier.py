"""Standalone inference for microblog-topic-classifier-v5.

The model reads one post (its text, plus up to two pictures) and returns probabilities for

  broad    25 broad topics ("sports", "us_politics", ... "unclear")
  paths    118 subtopics ("sports/baseball", ...); a broad topic with no subtopics has one entry named like itself
  signals  11 scores between 0 and 1 (substance, news, promo, general_interest, sentiment, critical, ad,
           engagement_bait, spam, self_promo, meme)
  tone     6 tones (informative, humorous, personal, outraged, supportive, other)

Architecture: the post text goes through a fine-tuned Ettin-150M text encoder (mean pooled). Each picture goes through a
frozen SigLIP 2 so400m (512 px) image encoder; the pictures' embeddings are projected and averaged (one learned vector
stands in for "no picture"). Text vector, picture vector and their product go through a small network to the heads.
Only the Ettin weights, the projection and the heads are in this repository (model.safetensors); the SigLIP 2 image
encoder is downloaded from google/siglip2-so400m-patch16-512 the first time it is needed.

    from topic_classifier import TopicClassifier, render_post

    clf = TopicClassifier.from_pretrained("haileyok/microblog-topic-classifier-v5")   # or a local folder
    post = {"text": "Great win for the Braves tonight", "tags": [], "media_kinds": ["image"]}
    out = clf.predict([{"text": render_post(post, with_pictures=True), "pictures": ["braves.jpg"]}])
    print(out[0]["top_broad"], out[0]["broad"]["sports"])

Tested with torch 2.14.0 and transformers 5.17.0 (an NVIDIA GPU is strongly recommended for posts with pictures).
Probabilities for the broad topic and subtopic heads are temperature scaled (config.json: temperature_broad,
temperature_path), fitted on held-out validation posts so that the confidence is not over- or under-stated.
"""

import json
import os
from pathlib import Path

import numpy as np
import torch
import torch.nn as nn

MAX_CHARS = 1800  # the post text is cut here, before tokenizing (as in training)
MAX_LINK_DESCRIPTION, MAX_QUOTE, MAX_IMAGE_TEXT = 300, 500, 300
VIDEO_KIND = {"image": "image", "video_thumb": "video"}


# --- turning a post into the text the model reads ----------------------------------------------------------------


def _one_line(s):
    return " ".join(s.split())


def _truncate(s, n):
    return s if len(s) <= n else s[: n - 1].rstrip() + "…"


def _lines(xs, n=0):
    out = []
    for s in xs or []:
        s = _one_line(s)
        if s:
            out.append(_truncate(s, n) if n else s)
    return out


def _media_summary(kinds):
    order, count = [], {}
    for k in kinds or []:
        k = _one_line(k)
        if not k:
            continue
        if k not in count:
            order.append(k)
        count[k] = count.get(k, 0) + 1
    return ", ".join(f"1 {k}" if count[k] == 1 else f"{count[k]} {k}s" for k in order)


def render_post(post, with_pictures=False):
    """The text the model reads for a post.

    `post` is a dict with any of these keys (all optional):
      text                  the post text
      tags                  list of hashtags
      media_kinds           list of "image" / "video", one per attachment
      labels                list of moderation labels on the post
      media_alts            list of alt texts
      link_domain, link_title, link_description   the link card, if any
      quote_text            the quoted post's text, if any
      image_texts, image_text_sources   text found in the pictures by OCR ("ocr") or a picture-description service
                            ("luna"), one source per text
      image_kinds           "image" / "video_thumb" per picture (only used to fill in media_kinds, see below)

    With `with_pictures=True` (you will pass the pictures themselves) any text found in the pictures is dropped, as
    it was in training for picture posts, and an empty media_kinds is filled in from image_kinds.
    """
    row = dict(post)
    if with_pictures:
        row["image_texts"], row["image_text_sources"] = [], []
        if not row.get("media_kinds"):
            row["media_kinds"] = [VIDEO_KIND[k] for k in row.get("image_kinds") or [] if k in VIDEO_KIND]
    ocr, desc = [], []
    for t, src in zip(row.get("image_texts") or [], row.get("image_text_sources") or []):
        if src == "ocr":
            ocr.append(t)
        elif src == "luna":
            desc.append(t)
    labels = []
    for l in row.get("labels") or []:
        l = _one_line(l)
        if l and not l.startswith("!") and l != "needs-review" and l not in labels:
            labels.append(l)
    link = {
        "domain": _one_line(row.get("link_domain") or ""),
        "title": _one_line(row.get("link_title") or ""),
        "description": _truncate(_one_line(row.get("link_description") or ""), MAX_LINK_DESCRIPTION),
    }
    tags, seen = [], set()
    for t in row.get("tags") or []:
        t = _one_line(t).lstrip("#")
        if t and t.lower() not in seen:
            seen.add(t.lower())
            tags.append(t)
    text = (row.get("text") or "").strip()
    alt, ocr, desc = _lines(row.get("media_alts")), _lines(ocr, MAX_IMAGE_TEXT), _lines(desc, MAX_IMAGE_TEXT)
    media = _media_summary(row.get("media_kinds"))
    quote = _truncate(_one_line(row.get("quote_text") or ""), MAX_QUOTE)
    out = []
    if text:
        out.append(text)
    if tags:
        out.append("[tags] " + " ".join("#" + t for t in tags))
    if media:
        out.append("[media] " + media)
    if labels:
        out.append("[labels] " + ", ".join(labels))
    if alt:
        out.append("[alt] " + " | ".join(alt))
    if ocr:
        out.append("[image text] " + " | ".join(ocr))
    if desc:
        out.append("[image description] " + " | ".join(desc))
    if any(link.values()):
        out.append("[link] " + " | ".join(p for p in (link["domain"], link["title"], link["description"]) if p))
    if quote:
        out.append("[quote] " + quote)
    return "\n".join(out)


# --- the model ---------------------------------------------------------------------------------------------------


class FusionModel(nn.Module):
    def __init__(self, text_config, img_dim, n_broad, n_paths, n_signals, n_tones):
        super().__init__()
        from transformers import AutoModel

        self.text = AutoModel.from_config(text_config)
        h = self.text.config.hidden_size
        self.img = nn.Sequential(nn.LayerNorm(img_dim), nn.Linear(img_dim, h), nn.GELU())
        self.no_img = nn.Parameter(torch.zeros(h))
        self.fuse = nn.Sequential(nn.LayerNorm(3 * h), nn.Linear(3 * h, h), nn.GELU(), nn.Dropout(0.1))
        self.broad, self.path = nn.Linear(h, n_broad), nn.Linear(h, n_paths)
        self.signals, self.tone = nn.Linear(h, n_signals), nn.Linear(h, n_tones)

    def forward(self, input_ids, attention_mask, img, img_mask):
        hs = self.text(input_ids=input_ids, attention_mask=attention_mask).last_hidden_state
        m = attention_mask.unsqueeze(-1).to(hs.dtype)
        t = (hs * m).sum(1) / m.sum(1).clamp(min=1)
        v = self.img(img.to(t.dtype))  # [batch, pictures, hidden]
        w = img_mask.unsqueeze(-1).to(v.dtype)
        have = w.sum(1)
        pooled = (v * w).sum(1) / have.clamp(min=1)
        v = torch.where(have > 0, pooled, self.no_img.to(pooled.dtype).expand_as(pooled))
        z = self.fuse(torch.cat([t, v, t * v], -1))
        return self.broad(z), self.path(z), self.signals(z), self.tone(z)


class TopicClassifier:
    def __init__(self, folder, device=None, image_encoder=None):
        from safetensors.torch import load_file
        from transformers import AutoConfig, AutoModel, AutoProcessor, AutoTokenizer

        folder = Path(folder)
        self.cfg = json.load(open(folder / "config.json"))
        self.device = torch.device(device or ("cuda" if torch.cuda.is_available() else "cpu"))
        self.tok = AutoTokenizer.from_pretrained(folder / "tokenizer")
        self.model = FusionModel(AutoConfig.from_pretrained(folder / "text_encoder"), self.cfg["image_features"]["dim"],
                                 len(self.cfg["broad"]), len(self.cfg["paths"]), len(self.cfg["signals"]), len(self.cfg["tones"]))
        self.model.load_state_dict(load_file(folder / "model.safetensors"), strict=True)
        self.model.to(self.device).eval()
        self._image_encoder_id = image_encoder or self.cfg["image_features"]["model"]
        self._image_model = self._image_processor = None
        self._AutoModel, self._AutoProcessor = AutoModel, AutoProcessor
        self.t_broad, self.t_path = self.cfg["temperature_broad"], self.cfg["temperature_path"]

    @classmethod
    def from_pretrained(cls, path_or_repo, device=None, image_encoder=None):
        """A local folder with the repository's files, or a Hugging Face repository id."""
        folder = path_or_repo
        if not os.path.isdir(folder):
            from huggingface_hub import snapshot_download

            folder = snapshot_download(path_or_repo)
        return cls(folder, device, image_encoder)

    # -- pictures
    def _load_image_encoder(self):
        if self._image_model is None:
            dtype = torch.bfloat16 if self.device.type == "cuda" else torch.float32
            m = self._AutoModel.from_pretrained(self._image_encoder_id, dtype=dtype)
            if hasattr(m, "text_model"):
                del m.text_model  # only the picture tower is used
            self._image_model = m.to(self.device).eval()
            self._image_processor = self._AutoProcessor.from_pretrained(self._image_encoder_id)

    @staticmethod
    def _open(p):
        """A PIL RGB image from a file path or a PIL image; None when the file cannot be read."""
        from PIL import Image

        try:
            if isinstance(p, (str, os.PathLike)):
                with Image.open(p) as im:
                    return im.convert("RGB")
            return p.convert("RGB")
        except OSError:
            return None

    @torch.no_grad()
    def embed_pictures(self, ims):
        """One embedding per PIL RGB image, as float16 (the encoder's pooled output, as stored in training)."""
        self._load_image_encoder()
        px = self._image_processor(images=ims, return_tensors="pt")["pixel_values"].to(self.device, self._image_model.dtype)
        out = self._image_model.get_image_features(pixel_values=px)
        emb = out if torch.is_tensor(out) else out.pooler_output
        return emb.float().cpu().numpy().astype(np.float16)

    # -- predictions
    def _batch(self, items):
        n, max_images, dim = len(items), self.cfg["max_images"], self.cfg["image_features"]["dim"]
        enc = self.tok([(it["text"] or "")[:MAX_CHARS] for it in items], truncation=True, max_length=self.cfg["max_len"])["input_ids"]
        L = max(len(x) for x in enc)
        ids = torch.full((n, L), self.tok.pad_token_id, dtype=torch.long)
        att = torch.zeros((n, L), dtype=torch.long)
        img = np.zeros((n, max_images, dim), np.float16)
        have = np.zeros((n, max_images), bool)
        ims, where = [], []
        for b, (x, it) in enumerate(zip(enc, items)):
            ids[b, : len(x)] = torch.tensor(x)
            att[b, : len(x)] = 1
            k = 0
            for p in list(it.get("pictures") or [])[:max_images]:
                im = self._open(p)
                if im is None:  # a picture that cannot be read is skipped, as in training
                    continue
                ims.append(im)
                where.append((b, k))
                k += 1
        if ims:
            for (b, k), e in zip(where, self.embed_pictures(ims)):
                img[b, k] = e
                have[b, k] = True
        d = self.device
        return ids.to(d), att.to(d), torch.from_numpy(img).to(d), torch.from_numpy(have).to(d)

    @torch.no_grad()
    def predict_arrays(self, items, batch_size=32):
        """Arrays in the input order: broad [n, 25], paths [n, 118], signals [n, 11], tone [n, 6] (all probabilities)."""
        order = np.argsort([len(it["text"] or "") for it in items], kind="stable")  # similar lengths together
        parts = [[], [], [], []]
        for s in range(0, len(items), batch_size):
            batch = [items[i] for i in order[s:s + batch_size]]
            with torch.autocast(self.device.type, dtype=torch.bfloat16, enabled=self.device.type == "cuda"):
                lb, lp, ls, lt = self.model(*self._batch(batch))
            probs = (torch.softmax(lb.float() / self.t_broad, -1), torch.softmax(lp.float() / self.t_path, -1),
                     torch.sigmoid(ls.float()), torch.softmax(lt.float(), -1))
            for k in range(4):
                parts[k].append(probs[k].cpu().numpy())
        inv = np.empty(len(order), int)
        inv[order] = np.arange(len(order))
        return {name: np.concatenate(p)[inv] for name, p in zip(("broad", "paths", "signals", "tone"), parts)}

    def predict(self, items, batch_size=32, top_k=None):
        """items: list of {"text": str, "pictures": [PIL image or file path, ...]}  ("pictures" optional).

        Returns one dict per item: broad, paths, signals, tone (name -> probability, broad/paths sorted high to low,
        cut to top_k when given) and top_broad, top_path.
        """
        a = self.predict_arrays(items, batch_size)
        out = []
        for i in range(len(items)):
            r = {}
            for key, names in (("broad", self.cfg["broad"]), ("paths", self.cfg["paths"])):
                pairs = sorted(zip(names, a[key][i].tolist()), key=lambda kv: -kv[1])
                r[key] = dict(pairs[:top_k] if top_k else pairs)
            r["signals"] = dict(zip(self.cfg["signals"], a["signals"][i].tolist()))
            r["tone"] = dict(zip(self.cfg["tones"], a["tone"][i].tolist()))
            r["top_broad"], r["top_path"] = next(iter(r["broad"])), next(iter(r["paths"]))
            out.append(r)
        return out
