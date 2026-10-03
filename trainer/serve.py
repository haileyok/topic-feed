"""Classifier service: serves the topic model on the GPU over HTTP.

The model reads a post's text (already rendered by the Go code, internal/postdoc Student(), so training and serving
cannot format posts differently) and up to two of its pictures (any image format Pillow reads, base64 in the JSON),
and returns broad topic, subtopic, signals (including meme) and tone. See topic_classifier.py for the model.

    POST /classify   {"posts": [{"text": "...", "pictures": ["<base64 image>", ...]}, ...]}
                  -> {"model": "v5", "seconds": 0.12,
                      "results": [{"broad": {id: p}, "paths": {path: p}, "signals": {name: p}, "tone": {name: p},
                                   "pictures_used": 1}, ...]}
                     broad: top 5; paths: top 8 (probabilities with the model's temperatures); pictures_used is how
                     many of the post's pictures could be read and went into the answer
    GET  /healthz -> {"model", "taxonomy_version", "postdoc_version", "device", "max_images", "signals"}

    MODEL_DIR=/data/models/v5 uv run python serve.py   # listens on SERVE_ADDR (default 0.0.0.0:8700)

Environment: MODEL_DIR, SERVE_ADDR, IMAGE_ENCODER (picture encoder weights: a folder or Hugging Face id; default the
one named in the model's config.json).
"""

import base64
import binascii
import io
import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np

from topic_classifier import TopicClassifier

TOP_BROAD, TOP_PATHS = 5, 8
MAX_POSTS = 1024
MAX_BODY = 96 << 20  # bytes; the Go client keeps requests well under this


class Classifier:
    def __init__(self, model_dir: str):
        self.dir = model_dir.rstrip("/")
        self.name = os.path.basename(self.dir)
        self.model = TopicClassifier(self.dir, image_encoder=os.environ.get("IMAGE_ENCODER") or None)
        self.cfg = self.model.cfg
        self.model.load_image_encoder()
        self.lock = threading.Lock()  # one GPU batch at a time

    def classify(self, posts: list[dict]) -> list[dict]:
        if not posts:
            return []
        with self.lock:
            a = self.model.predict_arrays(posts)
        c, out = self.cfg, []
        for i in range(len(posts)):
            bi = np.argsort(-a["broad"][i])[:TOP_BROAD]
            pi = np.argsort(-a["paths"][i])[:TOP_PATHS]
            out.append({
                "broad": {c["broad"][k]: round(float(a["broad"][i][k]), 4) for k in bi},
                "paths": {c["paths"][k]: round(float(a["paths"][i][k]), 4) for k in pi},
                "signals": {n: round(float(a["signals"][i][k]), 4) for k, n in enumerate(c["signals"])},
                "tone": {n: round(float(a["tone"][i][k]), 4) for k, n in enumerate(c["tones"])},
                "pictures_used": int(a["pictures_used"][i]),
            })
        return out

    def health(self) -> dict:
        return {"model": self.name, "taxonomy_version": self.cfg["taxonomy_version"],
                "postdoc_version": self.cfg.get("postdoc_version", "pd2"), "device": str(self.model.device),
                "max_images": self.cfg["max_images"], "signals": self.cfg["signals"]}


def parse_posts(req: dict, max_images: int) -> list[dict]:
    """Validates a /classify request body; pictures are decoded from base64 to bytes."""
    posts = req["posts"]
    if not isinstance(posts, list) or len(posts) > MAX_POSTS:
        raise ValueError(f"posts must be a list of at most {MAX_POSTS} objects")
    out = []
    for p in posts:
        if not isinstance(p, dict) or not isinstance(p.get("text", ""), str):
            raise ValueError("each post must be an object with a text string")
        pics = p.get("pictures") or []
        if not isinstance(pics, list) or not all(isinstance(x, str) for x in pics):
            raise ValueError("pictures must be a list of base64 strings")
        try:
            raw = [base64.b64decode(x, validate=True) for x in pics[:max_images]]
        except binascii.Error as e:
            raise ValueError(f"pictures must be base64: {e}") from e
        out.append({"text": p.get("text", ""), "pictures": raw})
    return out


def make_handler(clf: Classifier):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def _send(self, code: int, obj: dict):
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path == "/healthz":
                self._send(200, clf.health())
            else:
                self._send(404, {"error": "not found"})

        def do_POST(self):
            if self.path != "/classify":
                self._send(404, {"error": "not found"})
                return
            try:
                n = int(self.headers.get("Content-Length", "0"))
                if n > MAX_BODY:
                    raise ValueError(f"request body over {MAX_BODY} bytes")
                posts = parse_posts(json.loads(self.rfile.read(n)), clf.cfg["max_images"])
            except (ValueError, KeyError, TypeError, json.JSONDecodeError) as e:
                self._send(400, {"error": str(e)})
                return
            t0 = time.time()
            try:
                results = clf.classify(posts)
            except Exception as e:  # noqa: BLE001  report instead of dropping the connection; the client retries
                self._send(500, {"error": f"{type(e).__name__}: {e}"})
                return
            self._send(200, {"model": clf.name, "results": results, "seconds": round(time.time() - t0, 3)})

        def log_message(self, fmt, *args):  # quiet: one line per request is too much at ~1 req/s
            pass

    return Handler


def warm_up(clf: Classifier):
    """The first batches compile kernels: run a text post and a picture post once before serving."""
    from PIL import Image

    buf = io.BytesIO()
    Image.new("RGB", (640, 480), (120, 130, 140)).save(buf, "JPEG")
    clf.classify([{"text": "warm up", "pictures": []}, {"text": "warm up", "pictures": [buf.getvalue()]}])


def main():
    model_dir = os.environ.get("MODEL_DIR", "/data/models/v5")
    host, _, port = os.environ.get("SERVE_ADDR", "0.0.0.0:8700").rpartition(":")
    clf = Classifier(model_dir)
    warm_up(clf)
    print(f"serving {clf.name} ({clf.health()}) on {host}:{port}", flush=True)
    ThreadingHTTPServer((host, int(port)), make_handler(clf)).serve_forever()


if __name__ == "__main__":
    main()
