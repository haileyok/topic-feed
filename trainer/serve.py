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
import queue
import threading
import time
import traceback
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import numpy as np

from topic_classifier import TopicClassifier

TOP_BROAD, TOP_PATHS = 5, 8
MAX_POSTS = 1024
MAX_BODY = 96 << 20  # bytes; the Go client keeps requests well under this


class Profile:
    """Where the service's time goes: seconds, call counts and the longest call per named stage, logged as one JSON
    line every `every` seconds (journalctl --user -u topic-feed-classifier | grep '"profile"'). Stage times from
    different threads are summed, so a stage's total can exceed the wall time. The GPU worker's time is split into
    "worker_idle" (nothing queued: waiting for requests) and "worker_busy" (running batches, of which "wait_prep" and
    "wait_prep.first" are waits for the CPU to finish preparing a batch, i.e. the GPU idle)."""

    def __init__(self, every: float):
        self.every = every
        self.mu = threading.Lock()
        self.stages: dict[str, list] = {}
        self.t0 = time.perf_counter()

    def add(self, name: str, seconds: float, count: int = 1):
        with self.mu:
            s = self.stages.get(name)
            if s is None:
                s = self.stages[name] = [0, 0.0, 0.0]
            s[0] += count
            s[1] += seconds
            s[2] = max(s[2], seconds)

    def flush(self) -> str:
        with self.mu:
            now = time.perf_counter()
            stages, wall = self.stages, now - self.t0
            self.stages, self.t0 = {}, now
        return json.dumps({"profile": {"wall": round(wall, 2), "stages": {
            k: {"n": v[0], "sum": round(v[1], 3), "max": round(v[2], 3)} for k, v in sorted(stages.items())}}})

    def run(self):
        while True:
            time.sleep(self.every)
            print(self.flush(), flush=True)


class _Request:
    """One /classify request's batches in the GPU pipeline: results are filled in per batch."""

    def __init__(self, n_batches: int):
        self.parts = [None] * n_batches
        self.used = [None] * n_batches
        self.error = None
        self.left = n_batches
        self.mu = threading.Lock()
        self.done = threading.Event()

    def finish_one(self):
        with self.mu:
            self.left -= 1
            if self.left == 0:
                self.done.set()


class GpuPipeline:
    """Feeds the GPU from every request, so it never waits for the CPU work of the next batch or the next request.

    Each request is cut into batches (the model's own plan_batches, so a request's results do not depend on what
    else is in flight). Its batches are prepared (tokenizing, decoding and resizing pictures) on `prep_threads`
    threads and run, in order, by ONE worker thread, the only one that touches the GPU. At most `depth` batches are
    being prepared, waiting or running at a time (memory is bounded), and one request hands out its batches at a time,
    so the first batches of the next request are prepared while the last ones of the current request run."""

    def __init__(self, model: TopicClassifier, batch_size: int = 32, depth: int = 4, prep_threads: int = 2):
        self.model, self.batch_size = model, batch_size
        self.profile = None  # a Profile when profiling is on
        self._slots = threading.Semaphore(depth)
        self._hand_out = threading.Lock()
        self._queue: queue.Queue = queue.Queue()
        self._prep = ThreadPoolExecutor(prep_threads, thread_name_prefix="prep")
        threading.Thread(target=self._worker, name="gpu-worker", daemon=True).start()

    def run(self, items: list[dict]) -> dict:
        """Blocks until every item is classified; returns what TopicClassifier.predict_arrays returns."""
        if not items:
            raise ValueError("no items")
        order, batches = self.model.plan_batches(items, self.batch_size)
        req = _Request(len(batches))
        t0 = time.perf_counter()
        with self._hand_out:
            for k, batch in enumerate(batches):
                self._slots.acquire()
                self._queue.put((req, k, len(batch), self._prep.submit(self.model.prepare_batch, batch)))
        if self.profile:
            self.profile.add("hand_out", time.perf_counter() - t0)  # waiting behind earlier requests' batches
        req.done.wait()
        if req.error is not None:
            raise req.error
        return self.model.assemble(order, req.parts, req.used)

    def _worker(self):
        while True:
            t_idle = time.perf_counter()
            req, k, n, fut = self._queue.get()
            prof = self.profile
            t_got = time.perf_counter()
            try:
                if req.error is None:
                    prepared = fut.result()  # the GPU is idle while this waits for the CPU
                    if prof:
                        prof.add("wait_prep.first" if k == 0 else "wait_prep", time.perf_counter() - t_got)
                    req.parts[k], req.used[k] = self.model.run_batch(n, prepared)
                else:
                    fut.cancel()  # an earlier batch of this request failed; the request is reported as failed
            except BaseException as e:  # noqa: BLE001  the worker must survive; the request reports the error
                if req.error is None:
                    req.error = e
                traceback.print_exc()
            finally:
                self._slots.release()
                req.finish_one()
                if prof:
                    prof.add("worker_idle", t_got - t_idle)  # nothing was queued: waiting for requests
                    prof.add("worker_busy", time.perf_counter() - t_got)


class Classifier:
    def __init__(self, model_dir: str):
        self.dir = model_dir.rstrip("/")
        self.name = os.path.basename(self.dir)
        self.model = TopicClassifier(self.dir, image_encoder=os.environ.get("IMAGE_ENCODER") or None)
        self.cfg = self.model.cfg
        self.model.load_image_encoder()
        self.pipeline = GpuPipeline(self.model, depth=int(os.environ.get("PIPELINE_DEPTH", "4")),
                                    prep_threads=int(os.environ.get("PREP_THREADS", "2")))
        self.profile = None  # a Profile when profiling is on

    def classify(self, posts: list[dict]) -> list[dict]:
        if not posts:
            return []
        prof = self.profile
        t_req = time.perf_counter()
        if prof:
            prof.add("requests", 0.0)
            prof.add("posts", 0.0, len(posts))
        a = self.pipeline.run(posts)
        t_build = time.perf_counter()
        if prof:
            prof.add("request_wall", t_build - t_req)  # from parsed to classified, including queueing
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
        if prof:
            prof.add("build", time.perf_counter() - t_build)
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
            t0 = time.perf_counter()
            body = json.dumps(obj).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            if clf.profile and code == 200 and obj.get("results") is not None:
                clf.profile.add("respond", time.perf_counter() - t0)

        def do_GET(self):
            if self.path == "/healthz":
                self._send(200, clf.health())
            else:
                self._send(404, {"error": "not found"})

        def do_POST(self):
            if self.path != "/classify":
                self._send(404, {"error": "not found"})
                return
            prof = clf.profile
            try:
                n = int(self.headers.get("Content-Length", "0"))
                if n > MAX_BODY:
                    raise ValueError(f"request body over {MAX_BODY} bytes")
                t_read = time.perf_counter()
                raw = self.rfile.read(n)  # includes waiting for the client to finish sending
                t_parse = time.perf_counter()
                posts = parse_posts(json.loads(raw), clf.cfg["max_images"])
                if prof:
                    prof.add("read_body", t_parse - t_read)
                    prof.add("parse", time.perf_counter() - t_parse)
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
    every = float(os.environ.get("TOPIC_PROFILE_SECONDS", "30"))  # 0 turns the timing log off
    if every > 0:
        clf.profile = clf.pipeline.profile = Profile(every)
        clf.model.stage_timer = clf.profile.add
        threading.Thread(target=clf.profile.run, name="profile", daemon=True).start()
    print(f"serving {clf.name} ({clf.health()}) on {host}:{port}", flush=True)
    ThreadingHTTPServer((host, int(port)), make_handler(clf)).serve_forever()


if __name__ == "__main__":
    main()
