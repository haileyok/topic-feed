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

import concurrent.futures as cf
import contextlib
import json
import os
import threading
import time
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
    def __init__(self, folder, device=None, image_encoder=None, decode_threads=None, prefetch=True):
        """decode_threads: threads that decode pictures (default the TOPIC_DECODE_THREADS environment variable, else 8;
        1 decodes on the calling thread). prefetch: decode the next batch while the current one runs on the GPU."""
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
        self._image_lock = threading.Lock()
        self._AutoModel, self._AutoProcessor = AutoModel, AutoProcessor
        self.t_broad, self.t_path = self.cfg["temperature_broad"], self.cfg["temperature_path"]
        n = decode_threads if decode_threads is not None else int(os.environ.get("TOPIC_DECODE_THREADS", "8"))
        self._decode_pool = cf.ThreadPoolExecutor(n, thread_name_prefix="decode") if n > 1 else None
        self._prefetch = prefetch
        # Optional timing hook, called as stage_timer(name, seconds, count=1) from whichever thread ran the stage
        # (the service sets it to log where the time goes). None costs nothing.
        self.stage_timer = None

    @contextlib.contextmanager
    def _stage(self, name):
        if self.stage_timer is None:
            yield
            return
        t0 = time.perf_counter()
        try:
            yield
        finally:
            self.stage_timer(name, time.perf_counter() - t0)

    def _count(self, name, n):
        if self.stage_timer is not None:
            self.stage_timer(name, 0.0, n)

    @classmethod
    def from_pretrained(cls, path_or_repo, device=None, image_encoder=None, **kwargs):
        """A local folder with the repository's files, or a Hugging Face repository id."""
        folder = path_or_repo
        if not os.path.isdir(folder):
            from huggingface_hub import snapshot_download

            folder = snapshot_download(path_or_repo)
        return cls(folder, device, image_encoder, **kwargs)

    # -- pictures
    def _load_image_encoder(self):
        if self._image_model is not None:
            return
        with self._image_lock:  # several threads may prepare batches at once
            if self._image_model is None:
                dtype = torch.bfloat16 if self.device.type == "cuda" else torch.float32
                m = self._AutoModel.from_pretrained(self._image_encoder_id, dtype=dtype)
                if hasattr(m, "text_model"):
                    del m.text_model  # only the picture tower is used
                processor = self._AutoProcessor.from_pretrained(self._image_encoder_id)
                self._image_processor = processor
                self._image_model = m.to(self.device).eval()  # set last: other threads treat it as "loaded"

    def load_image_encoder(self):
        """Load the picture encoder now (it is otherwise loaded when the first picture arrives)."""
        self._load_image_encoder()

    @staticmethod
    def _open(p):
        """A PIL RGB image from encoded bytes, a file path or a PIL image; None when it cannot be read."""
        import io

        from PIL import Image

        try:
            if isinstance(p, (bytes, bytearray, memoryview)):
                with Image.open(io.BytesIO(p)) as im:
                    return im.convert("RGB")
            if isinstance(p, (str, os.PathLike)):
                with Image.open(p) as im:
                    return im.convert("RGB")
            return p.convert("RGB")
        except Exception:  # noqa: BLE001  broken, truncated or hostile files must not fail a whole batch
            return None

    def embed_pictures(self, ims):
        """One embedding per PIL RGB image, as float16 (the encoder's pooled output, as stored in training)."""
        self._load_image_encoder()
        return self._embed_pixels(self._image_processor(images=ims, return_tensors="pt")["pixel_values"])

    @torch.no_grad()
    def _embed_pixels(self, pixel_values):
        """The picture encoder on the GPU, for pictures the image processor has already resized and normalised."""
        px = pixel_values.to(self.device, self._image_model.dtype)
        out = self._image_model.get_image_features(pixel_values=px)
        emb = out if torch.is_tensor(out) else out.pooler_output
        return emb.float().cpu().numpy().astype(np.float16)

    # -- predictions
    def _pixels(self, p):
        """One picture decoded, resized and normalised by the image processor: a float32 tensor [3, H, W], or None
        when the file cannot be read. Run on the decode threads; the result is exactly what the processor gives for
        the same picture inside a batch."""
        im = self._open(p)
        if im is None:
            return None
        with self._stage("prep.resize"):  # summed over threads
            return self._image_processor(images=[im], return_tensors="pt")["pixel_values"][0]

    def prepare_batch(self, items):
        """The CPU part of a batch: tokenize the texts and decode, resize and normalise the pictures (one picture per
        task on the decode threads). Thread safe; it can run for the next batches while the current one is on the GPU."""
        max_images = self.cfg["max_images"]
        t_prep = time.perf_counter()
        with self._stage("prep.tokenize"):
            enc = self.tok([(it["text"] or "")[:MAX_CHARS] for it in items], truncation=True, max_length=self.cfg["max_len"])["input_ids"]
        refs, sources = [], []
        for b, it in enumerate(items):
            for p in list(it.get("pictures") or [])[:max_images]:
                refs.append(b)
                sources.append(p)
        px, ok = None, []
        if sources:
            self._load_image_encoder()
            with self._stage("prep.pictures"):  # wall time of decoding and resizing the batch's pictures
                if self._decode_pool is not None and len(sources) > 1:
                    tensors = list(self._decode_pool.map(self._pixels, sources))
                else:
                    tensors = [self._pixels(p) for p in sources]
            ok = [t is not None for t in tensors]
            readable = [t for t in tensors if t is not None]
            if readable:
                px = torch.stack(readable)
        if self.stage_timer is not None:
            self.stage_timer("prep", time.perf_counter() - t_prep)
            self._count("pictures", sum(ok))
        return enc, refs, ok, px

    def _finish(self, n, prepared):
        """The GPU part: embed the decoded pictures and build the model's input tensors."""
        max_images, dim = self.cfg["max_images"], self.cfg["image_features"]["dim"]
        enc, refs, ok, px = prepared
        L = max(len(x) for x in enc)
        ids = torch.full((n, L), self.tok.pad_token_id, dtype=torch.long)
        att = torch.zeros((n, L), dtype=torch.long)
        for b, x in enumerate(enc):
            ids[b, : len(x)] = torch.tensor(x)
            att[b, : len(x)] = 1
        img = np.zeros((n, max_images, dim), np.float16)
        have = np.zeros((n, max_images), bool)
        where, slot = [], [0] * n
        for b, readable in zip(refs, ok):
            if not readable:  # a picture that cannot be read is skipped, as in training
                continue
            where.append((b, slot[b]))
            slot[b] += 1
        if px is not None:
            with self._stage("gpu.embed"):  # copy to the GPU, the picture encoder, and the copy back (waits for the GPU)
                emb = self._embed_pixels(px)
            for (b, k), e in zip(where, emb):
                img[b, k] = e
                have[b, k] = True
        d = self.device
        with self._stage("gpu.upload"):
            out = (ids.to(d), att.to(d), torch.from_numpy(img).to(d), torch.from_numpy(have).to(d))
        return out, have.sum(1)

    @torch.no_grad()
    def run_batch(self, n, prepared):
        """The GPU part for one batch of `n` posts that prepare_batch has prepared: ([broad, paths, signals, tone]
        probabilities as numpy arrays, pictures_used [n]). Call it from one thread at a time (the GPU)."""
        with self._stage("finish"):
            inputs, n_used = self._finish(n, prepared)
        with self._stage("gpu.forward"):  # text model and heads, up to the copy of the probabilities back
            with torch.autocast(self.device.type, dtype=torch.bfloat16, enabled=self.device.type == "cuda"):
                lb, lp, ls, lt = self.model(*inputs)
            probs = (torch.softmax(lb.float() / self.t_broad, -1), torch.softmax(lp.float() / self.t_path, -1),
                     torch.sigmoid(ls.float()), torch.softmax(lt.float(), -1))
            arrays = [p.cpu().numpy() for p in probs]
        self._count("batches", 1)
        self._count("batch.posts", n)
        return arrays, n_used

    @staticmethod
    def plan_batches(items, batch_size=32):
        """Splits the items into batches of similar text length: (order, batches), where batches holds the items in
        that order, `batch_size` at a time."""
        order = np.argsort([len(it["text"] or "") for it in items], kind="stable")  # similar lengths together
        return order, [[items[i] for i in order[s:s + batch_size]] for s in range(0, len(items), batch_size)]

    @staticmethod
    def assemble(order, parts, used):
        """The predict_arrays result from per-batch results (parts[k] = run_batch's arrays, used[k] = pictures_used),
        in batch order, put back in the input order."""
        inv = np.empty(len(order), int)
        inv[order] = np.arange(len(order))
        out = {name: np.concatenate([p[j] for p in parts])[inv] for j, name in enumerate(("broad", "paths", "signals", "tone"))}
        out["pictures_used"] = np.concatenate(used)[inv]
        return out

    @torch.no_grad()
    def predict_arrays(self, items, batch_size=32):
        """Arrays in the input order: broad [n, 25], paths [n, 118], signals [n, 11], tone [n, 6] (all
        probabilities) and pictures_used [n] (how many of each post's pictures could be read and were used)."""
        order, batches = self.plan_batches(items, batch_size)
        parts, used = [], []
        ahead = cf.ThreadPoolExecutor(1, thread_name_prefix="prefetch") if self._prefetch and len(batches) > 1 else None
        try:
            nxt = ahead.submit(self.prepare_batch, batches[0]) if ahead else None
            for k, batch in enumerate(batches):
                with self._stage("wait_prep.first" if k == 0 else "wait_prep"):  # the GPU is idle while this waits
                    prepared = nxt.result() if ahead else self.prepare_batch(batch)
                nxt = ahead.submit(self.prepare_batch, batches[k + 1]) if ahead and k + 1 < len(batches) else None
                arrays, n_used = self.run_batch(len(batch), prepared)
                parts.append(arrays)
                used.append(n_used)
        finally:
            if ahead:
                ahead.shutdown(wait=True)
        return self.assemble(order, parts, used)

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
