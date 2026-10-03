"""Cloudflare's Clef as a Flumina server app on Fireworks.

Clef reads a post (and its pictures) and answers typed questions with a probability for
every option, in one forward pass. This app serves it with the same request and response
bodies as Jev's System One API (what internal/labeler already sends), plus two extras the
labeler needs: pictures, and many posts per request.

Routes (POST, JSON):
  /systemone        one request:  {model, state, questions, images?, max_image_side?}
                    answer:       {model, answers, usage}
  /systemone_batch  many at once: {requests: [<systemone request>, ...]}
                    answer:       {results: [{response} | {error}, ...]}, in order
  /info             what is loaded, and GPU memory in use

images are base64 strings (or data: URLs) of PNG, JPEG or WebP files, at most 4 per request.
Pictures longer than max_image_side (default 1000) pixels are shrunk first, which keeps a
picture to roughly 800 model tokens; the model accepts far larger ones at far greater cost.

The weights go in data/clef/ (see README.md). Run this file directly for an offline check:

    CLEF_MODEL_DIR=/data/clef/clef-flash python flumina.py
"""

import asyncio
import base64
import binascii
import io
import os
import sys
import time
from pathlib import Path
from typing import Any, Optional

import fireworks.flumina.route as route
import torch
from fireworks.flumina import FluminaModule, main as flumina_main
from pydantic import BaseModel

APP_DIR = Path(__file__).resolve().parent
MODEL_DIR = Path(os.environ.get("CLEF_MODEL_DIR", APP_DIR / "data" / "clef"))
HF_REPO = os.environ.get("CLEF_HF_REPO", "")  # only if the weights were not packaged into data/

MAX_IMAGES = 4  # per request, as Cloudflare's API allows
MAX_LENGTH = 16384  # model tokens per request, the loader's default
MAX_BATCH_TOKENS = int(os.environ.get("CLEF_MAX_BATCH_TOKENS", "16384"))  # padded tokens per forward pass
QUESTION_TYPES = ("noul", "choice", "score")


class SystemOneRequest(BaseModel):
    model: str = "clef"
    state: Any
    questions: dict[str, dict[str, Any]]
    images: Optional[list[str]] = None
    max_image_side: int = 1000


class SystemOneResponse(BaseModel):
    model: str
    answers: dict[str, Any]
    usage: dict[str, int]


class BatchRequest(BaseModel):
    requests: list[SystemOneRequest]


class BatchItem(BaseModel):
    response: Optional[SystemOneResponse] = None
    error: Optional[str] = None


class BatchResponse(BaseModel):
    results: list[BatchItem]


class InfoResponse(BaseModel):
    variant: str
    model_dir: str
    parameters_billions: float
    gpu: str
    gpu_memory_allocated_gib: float
    gpu_memory_peak_gib: float
    max_images: int
    max_batch_tokens: int
    uptime_seconds: float


def check_questions(questions: dict[str, dict[str, Any]]) -> None:
    """The checks joint_schema_model.systemone makes, so a bad request fails before the GPU."""
    if not questions:
        raise ValueError("at least one question is required")
    for question_id, question in questions.items():
        if question.get("type") not in QUESTION_TYPES:
            raise ValueError(f"{question_id}: type must be noul, choice, or score")
        if question["type"] != "noul" and not question.get("criteria"):
            raise ValueError(f"{question_id}: criteria must not be empty")


def decode_images(images: Optional[list[str]], max_side: int):
    """Base64 strings to RGB pictures, shrunk so their longer side is at most max_side."""
    if not images:
        return None
    if len(images) > MAX_IMAGES:
        raise ValueError(f"at most {MAX_IMAGES} images per request, got {len(images)}")
    from PIL import Image, ImageOps

    out = []
    for i, s in enumerate(images):
        if s.startswith("data:"):
            s = s.split(",", 1)[-1]
        try:
            raw = base64.b64decode(s, validate=False)
            im = ImageOps.exif_transpose(Image.open(io.BytesIO(raw))).convert("RGB")
        except (binascii.Error, OSError, ValueError) as e:
            raise ValueError(f"image {i} is not a readable PNG, JPEG or WebP: {e}") from e
        if max_side > 0 and max(im.size) > max_side:
            im.thumbnail((max_side, max_side), Image.LANCZOS)
        out.append(im)
    return out


class ClefModule(FluminaModule):
    def __init__(self):
        super().__init__()
        self.started = time.time()
        if not MODEL_DIR.exists():
            if not HF_REPO:
                raise RuntimeError(f"no weights in {MODEL_DIR}: put the Clef release there (see README.md)")
            from huggingface_hub import snapshot_download

            snapshot_download(HF_REPO, local_dir=str(MODEL_DIR))
        # The release's own loader goes first, so it always matches the weights beside it.
        sys.path.insert(0, str(MODEL_DIR))
        sys.path.insert(1, str(APP_DIR))
        import joint_schema_model as jsm

        self.jsm = jsm
        self.variant = (MODEL_DIR / "variant.txt").read_text().strip() if (MODEL_DIR / "variant.txt").exists() else "clef"
        self.device = torch.device("cuda")
        self.model, self.processor = jsm.load_release_model(MODEL_DIR, device="cuda")
        self.params_b = sum(p.numel() for p in self.model.parameters()) / 1e9
        self._lock = asyncio.Lock()  # one forward pass at a time on the GPU
        self._warm_up()

    # --- inference -----------------------------------------------------------------------

    def _warm_up(self) -> None:
        """One text request and one with a picture, so kernels are built before real traffic."""
        from PIL import Image

        buf = io.BytesIO()
        Image.new("RGB", (640, 480), (120, 60, 200)).save(buf, "JPEG")
        picture = base64.b64encode(buf.getvalue()).decode()
        q = {"q": {"type": "choice", "instructions": "Pick one.", "criteria": {"a": "first", "b": "second"}}}
        self._run([SystemOneRequest(model=self.variant, state="warm up", questions=q),
                   SystemOneRequest(model=self.variant, state="warm up", questions=q, images=[picture])])

    def _run(self, requests: list[SystemOneRequest]) -> list[SystemOneResponse]:
        """Answer requests in order, packing them into forward passes of bounded size."""
        jsm = self.jsm
        encoded = []
        for r in requests:
            check_questions(r.questions)
            record = {"state": r.state, "questions": r.questions, "images": decode_images(r.images, r.max_image_side)}
            encoded.append(jsm.encode_record(self.processor.tokenizer, record, max_length=MAX_LENGTH, processor=self.processor))

        responses: list[Optional[SystemOneResponse]] = [None] * len(requests)
        start = 0
        while start < len(encoded):
            end, widest = start, 0
            while end < len(encoded):
                widest_next = max(widest, len(encoded[end].input_ids))
                if end > start and widest_next * (end - start + 1) > MAX_BATCH_TOKENS:
                    break
                widest, end = widest_next, end + 1
            batch = jsm.collate_records(encoded[start:end], self.processor.tokenizer.pad_token_id, self.device)
            with torch.inference_mode():
                logits = self.model(batch)
            for k, per_question in enumerate(logits):
                i = start + k
                answers = {
                    q.question_id: jsm.systemone_answer(
                        requests[i].questions[q.question_id],
                        dict(zip(q.option_ids, ql.float().softmax(-1).tolist())),
                    )
                    for q, ql in zip(encoded[i].questions, per_question)
                }
                responses[i] = SystemOneResponse(
                    model=requests[i].model, answers=answers,
                    usage={"input_tokens": len(encoded[i].input_ids), "output_tokens": 0})
            start = end
        return responses  # type: ignore[return-value]

    async def _locked_run(self, requests: list[SystemOneRequest]) -> list[SystemOneResponse]:
        async with self._lock:
            return await asyncio.to_thread(self._run, requests)

    # --- routes --------------------------------------------------------------------------

    @route.post("/systemone")
    async def systemone(self, body: SystemOneRequest) -> SystemOneResponse:
        return (await self._locked_run([body]))[0]

    @route.post("/systemone_batch")
    async def systemone_batch(self, body: BatchRequest) -> BatchResponse:
        """Each request is checked on its own, so one bad request does not fail the rest."""
        items: list[BatchItem] = [BatchItem() for _ in body.requests]
        good: list[int] = []
        for i, r in enumerate(body.requests):
            try:
                check_questions(r.questions)
                decode_images(r.images, r.max_image_side)
                good.append(i)
            except ValueError as e:
                items[i].error = str(e)
        if good:
            try:
                for i, resp in zip(good, await self._locked_run([body.requests[i] for i in good])):
                    items[i].response = resp
            except Exception as e:  # a whole-batch failure (for example out of memory) is reported per request
                for i in good:
                    items[i].error = f"{type(e).__name__}: {e}"
        return BatchResponse(results=items)

    @route.post("/info")
    async def info(self) -> InfoResponse:
        gib = 2**30
        return InfoResponse(
            variant=self.variant, model_dir=str(MODEL_DIR), parameters_billions=round(self.params_b, 2),
            gpu=torch.cuda.get_device_name(0), gpu_memory_allocated_gib=round(torch.cuda.memory_allocated() / gib, 2),
            gpu_memory_peak_gib=round(torch.cuda.max_memory_allocated() / gib, 2), max_images=MAX_IMAGES,
            max_batch_tokens=MAX_BATCH_TOKENS, uptime_seconds=round(time.time() - self.started, 1))


if __name__ == "__flumina_main__":
    flumina_main(ClefModule())


if __name__ == "__main__":
    # Offline check: load the weights, answer a few requests, and make sure packing posts into
    # one forward pass gives the same answers as asking one at a time.
    module = ClefModule()
    ticket = {"type": "choice", "instructions": "Which team should handle this request?",
              "criteria": {"billing": "Payments, invoices, and refunds", "technical": "Outages, errors, and configuration",
                           "sales": "Plans and upgrades"}}
    urgent = {"type": "noul", "instructions": "Is this support request urgent?"}
    colour = {"type": "choice", "instructions": "What is the main colour of the attached picture?",
              "criteria": {"red": "Mostly red.", "green": "Mostly green.", "blue": "Mostly blue."}}

    def jpeg(rgb):
        from PIL import Image

        buf = io.BytesIO()
        Image.new("RGB", (1500, 900), rgb).save(buf, "JPEG")
        return base64.b64encode(buf.getvalue()).decode()

    requests = [
        SystemOneRequest(model=module.variant, state="Checkout has been failing for every customer for the last hour.",
                         questions={"team": ticket, "urgent": urgent}),
        SystemOneRequest(model=module.variant, state="Please send me last month's invoice again.",
                         questions={"team": ticket, "urgent": urgent}),
        SystemOneRequest(model=module.variant, state="Describe the picture.", questions={"colour": colour},
                         images=[jpeg((220, 20, 20))]),
        SystemOneRequest(model=module.variant, state="Describe the picture.", questions={"colour": colour},
                         images=[jpeg((20, 30, 220))]),
    ]
    singles = [asyncio.run(module.systemone(r)) for r in requests]
    batch = asyncio.run(module.systemone_batch(BatchRequest(requests=requests)))
    bad = asyncio.run(module.systemone_batch(BatchRequest(requests=[
        requests[0], SystemOneRequest(questions={"x": {"type": "choice", "criteria": {}}}, state="x")])))

    for r, one, many in zip(requests, singles, batch.results):
        assert many.error is None, many.error
        for qid, a in one.answers.items():
            b = many.response.answers[qid]
            if a["type"] == "noul":
                assert abs(a["noul"] - b["noul"]) < 0.05, (qid, a, b)
            else:
                assert a["choice"] == b["choice"], (qid, a, b)
        print(str(r.state)[:60], "->", {q: (a.get("choice") or a.get("noul")) for q, a in one.answers.items()},
              f"({one.usage['input_tokens']} tokens)")
    assert bad.results[0].response is not None and "criteria" in (bad.results[1].error or ""), bad
    assert singles[0].answers["team"]["choice"] == "technical", singles[0].answers["team"]
    assert singles[2].answers["colour"]["choice"] == "red" and singles[3].answers["colour"]["choice"] == "blue"
    info = asyncio.run(module.info())
    print("ok:", info.model_dump())
