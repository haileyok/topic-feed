"""Classifier service: serves a trained student model on the GPU over HTTP.

Inputs are post documents already rendered by the Go code (internal/postdoc, Student()),
so training and serving can't format posts differently. The service only tokenizes.

    POST /classify   {"texts": ["...", ...]}
                  -> {"model": "v3-blend", "results": [{"broad": {id: p}, "paths": {path: p},
                                                        "signals": {name: p}, "tone": {name: p}}, ...]}
                     broad: top 5; paths: top 8 (probabilities with the model's temperatures)
    GET  /healthz -> {"model", "taxonomy_version", "postdoc_version", "device"}

    MODEL_DIR=/data/models/v3-blend uv run python serve.py   # listens on SERVE_ADDR (default 0.0.0.0:8700)
"""

import json
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import torch
import torch.nn.functional as F
from transformers import AutoTokenizer

import common
from train import Student

TOP_BROAD, TOP_PATHS = 5, 8
MAX_TEXTS = 4096


class Classifier:
    def __init__(self, model_dir: str, device: str = "cuda"):
        self.dir = model_dir.rstrip("/")
        self.name = os.path.basename(self.dir)
        self.cfg = json.load(open(f"{self.dir}/config.json"))
        self.device = device
        self.tok = AutoTokenizer.from_pretrained(self.dir)
        self.model = Student(self.cfg["base"], len(self.cfg["broad"]), len(self.cfg["paths"])).to(device)
        self.model.load_state_dict(torch.load(f"{self.dir}/model.pt", map_location=device))
        self.model.eval()
        self.lock = threading.Lock()  # one GPU batch at a time

    @torch.no_grad()
    def classify(self, texts: list[str], bs: int = 128) -> list[dict]:
        c = self.cfg
        out = []
        with self.lock:
            for i in range(0, len(texts), bs):
                enc = self.tok(texts[i:i + bs], padding=True, truncation=True, max_length=c["max_len"], return_tensors="pt")
                with torch.autocast(self.device, dtype=torch.bfloat16, enabled=self.device == "cuda"):
                    lb, lp, ls, lt = self.model(enc["input_ids"].to(self.device), enc["attention_mask"].to(self.device))
                pb = F.softmax(lb.float() / c["temperature_broad"], -1).cpu()
                pp = F.softmax(lp.float() / c["temperature_path"], -1).cpu()
                ps = torch.sigmoid(ls.float()).cpu()
                pt = F.softmax(lt.float(), -1).cpu()
                for j in range(pb.shape[0]):
                    bv, bi = pb[j].topk(TOP_BROAD)
                    pv, pidx = pp[j].topk(TOP_PATHS)
                    out.append({
                        "broad": {c["broad"][k]: round(float(v), 4) for v, k in zip(bv, bi.tolist())},
                        "paths": {c["paths"][k]: round(float(v), 4) for v, k in zip(pv, pidx.tolist())},
                        "signals": {n: round(float(ps[j][k]), 4) for k, n in enumerate(c["signals"])},
                        "tone": {n: round(float(pt[j][k]), 4) for k, n in enumerate(c["tones"])},
                    })
        return out

    def health(self) -> dict:
        return {"model": self.name, "taxonomy_version": self.cfg["taxonomy_version"],
                "postdoc_version": self.cfg["postdoc_version"], "device": self.device}


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
                req = json.loads(self.rfile.read(n))
                texts = req["texts"]
                if not isinstance(texts, list) or not all(isinstance(t, str) for t in texts) or len(texts) > MAX_TEXTS:
                    raise ValueError(f"texts must be a list of at most {MAX_TEXTS} strings")
            except (ValueError, KeyError, json.JSONDecodeError) as e:
                self._send(400, {"error": str(e)})
                return
            t0 = time.time()
            results = clf.classify(texts)
            self._send(200, {"model": clf.name, "results": results, "seconds": round(time.time() - t0, 3)})

        def log_message(self, fmt, *args):  # quiet: one line per request is too much at ~1 req/s
            pass

    return Handler


def main():
    model_dir = os.environ.get("MODEL_DIR", "/data/models/v3-blend")
    host, _, port = os.environ.get("SERVE_ADDR", "0.0.0.0:8700").rpartition(":")
    clf = Classifier(model_dir, "cuda" if torch.cuda.is_available() else "cpu")
    clf.classify(["warm up"])  # first batch compiles kernels
    if clf.cfg["postdoc_version"] != common_postdoc():
        print(f"warning: model expects post documents {clf.cfg['postdoc_version']}, trainer expects {common_postdoc()}", flush=True)
    print(f"serving {clf.name} ({clf.health()}) on {host}:{port}", flush=True)
    ThreadingHTTPServer((host, int(port)), make_handler(clf)).serve_forever()


def common_postdoc() -> str:
    from train import POSTDOC_VERSION
    return POSTDOC_VERSION


if __name__ == "__main__":
    main()
