"""Prepare derived copies of the image archive for training.

Scans <root>/raw (the image archive's content-addressed originals) and writes a
uniform derived copy of each picture to <root>/1000/<sha256[:2]>/<sha256>.jpg:
EXIF-transposed, converted to RGB, long side at most 1000 px, JPEG quality 90.
Pictures already small are re-encoded the same way, so every file the trainer
reads is the same shape. Idempotent: existing copies are skipped, so a rerun
only does what arrived since the last one.

    /data/clef/.venv/bin/python trainer/prepare_images.py --root /data/images
"""

import argparse
import os
import sys
from concurrent.futures import ProcessPoolExecutor
from pathlib import Path

try:
    from PIL import Image, ImageOps
except ImportError:
    print("Pillow is required (try /data/clef/.venv/bin/python)", file=sys.stderr)
    raise

MAX_SIDE = 1000
QUALITY = 90


def prepare(args: tuple[str, str]) -> tuple[str, str | None]:
    """Prepare one derived copy. Returns (source path, failure or None)."""
    src, dst = args
    if os.path.exists(dst):
        return src, None
    try:
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        with Image.open(src) as im:
            im = ImageOps.exif_transpose(im)
            if im.mode != "RGB":
                im = im.convert("RGB")
            if max(im.size) > MAX_SIDE:
                im.thumbnail((MAX_SIDE, MAX_SIDE), Image.LANCZOS)
            tmp = dst + ".tmp"
            im.save(tmp, "JPEG", quality=QUALITY)
            os.replace(tmp, dst)
        return src, None
    except Exception as e:  # decode or write failure: reported, not fatal
        return src, f"{type(e).__name__}: {e}"


def main() -> None:
    global MAX_SIDE, QUALITY
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--root", default="/data/images", help="archive root (raw/ and 1000/)")
    ap.add_argument("--workers", type=int, default=32, help="worker processes")
    ap.add_argument(
        "--max-side", type=int, default=MAX_SIDE, help="long side cap in pixels (default 1000)"
    )
    ap.add_argument("--quality", type=int, default=QUALITY, help="JPEG quality (default 90)")
    args = ap.parse_args()
    MAX_SIDE, QUALITY = args.max_side, args.quality

    raw = Path(args.root) / "raw"
    out = Path(args.root) / "1000"
    if not raw.is_dir():
        sys.exit(f"no such directory: {raw}")

    jobs = []
    for d in sorted(raw.iterdir()):
        if not d.is_dir():
            continue
        for f in sorted(d.iterdir()):
            if f.is_file():
                sha = f.stem
                jobs.append((str(f), str(out / sha[:2] / (sha + ".jpg"))))
    print(f"{len(jobs)} pictures in {raw}", flush=True)

    done, failed = 0, 0
    with ProcessPoolExecutor(max_workers=args.workers) as pool:
        for src, err in pool.map(prepare, jobs, chunksize=64):
            done += 1
            if err:
                failed += 1
                print(f"decode failure: {src}: {err}", file=sys.stderr, flush=True)
            if done % 5000 == 0:
                print(f"prepared {done}/{len(jobs)}", flush=True)
    print(f"done: {done} pictures, {failed} failures", flush=True)


if __name__ == "__main__":
    main()
