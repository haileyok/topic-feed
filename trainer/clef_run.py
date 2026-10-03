"""Label the exported posts with Clef-flash, pictures included, on this machine's GPU.

Reads clef_export.py's file, labels each post the way clef_labels.py does (the same two passes and
questions as internal/labeler, the same post rendering), and appends one JSON line per post to
--out. Safe to stop and start again: posts already in --out (or in any --skip file, for results
made on another machine) are not done twice. A post that fails is written to <out>.errors.jsonl
and the run goes on; the run stops if --max-failures posts in a row fail.

Pictures come from the image archive's shrunk copies (<images>/1000/<sha[:2]>/<sha>.jpg, the long
side at most 1000 px), at most 4 per post. A post that has pictures is shown to the model with
them instead of the pipeline's text description of them; a post without any keeps the description.
Posts the archive holds attached pictures for but whose media_kinds were never stored (older
ingestion) get "1 image" / "1 video" filled in from the archive.

With --wait-for-pictures, a post is only labelled once all of its picture files are present, so the
run can start while the files are still being copied here; it goes back for the posts it skipped
until none are left.

Several GPUs or machines can split the work: the export is cut into n shards by line number, and
--shard 0,1,2/5 does shards 0, 1 and 2 of 5 (60% of the posts) while --shard 3,4/5 does the rest
on another machine. Every shard keeps the export's order, so each gets the same mix of posts.

    python clef_run.py --posts full_posts.jsonl --out clef-flash-full.jsonl [--shard 0/2]
    python clef_run.py --posts full_posts.jsonl --dry-run 5      # show the first posts, no GPU

Needs the Clef release directory (--model) and, for speed, the flash-linear-attention kernels.
"""

import argparse
import json
import os
import sys
import time
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent))
from clef_labels import jev_post, label_post, make_doc  # noqa: E402

MAX_PICTURES = 4  # per post, as Clef allows
VIDEO_KIND = {"image": "image", "video_thumb": "video"}
RESCAN_SECONDS = 120  # with --wait-for-pictures: how long to wait for more picture files


def picture_path(root: str, sha: str) -> Path:
    return Path(root) / "1000" / sha[:2] / f"{sha}.jpg"


def pictures_ready(root: str, shas: list[str]) -> bool:
    """Whether every picture the model would be shown for this post is present."""
    return all(picture_path(root, s).exists() for s in shas[:MAX_PICTURES])


def load_pictures(root: str, shas: list[str]):
    """The post's pictures as RGB PIL images (those whose file exists), at most MAX_PICTURES."""
    from PIL import Image

    out = []
    for sha in shas:
        if len(out) == MAX_PICTURES:
            break
        try:
            with Image.open(picture_path(root, sha)) as im:
                out.append(im.convert("RGB"))
        except OSError:
            continue
    return out


def prepare(row: dict, pictures: list) -> dict:
    """The row as the model should see it: pictures replace the pipeline's description of them."""
    row = dict(row)
    if pictures:
        row["image_texts"], row["image_text_sources"] = [], []
        if not row.get("media_kinds"):
            row["media_kinds"] = [VIDEO_KIND[k] for k in row.get("image_kinds") or [] if k in VIDEO_KIND]
    return row


def read_done(*paths) -> set[str]:
    done = set()
    for p in paths:
        if not p or not os.path.exists(p):
            continue
        with open(p) as f:
            for line in f:
                try:
                    done.add(json.loads(line)["uri"])
                except (ValueError, KeyError):
                    pass  # a partly written last line: that post is simply done again
    return done


def open_append(path: str):
    """Open for appending, first ending a last line that a crash left unfinished."""
    if os.path.exists(path) and os.path.getsize(path) > 0:
        with open(path, "rb") as f:
            f.seek(-1, os.SEEK_END)
            needs_newline = f.read(1) != b"\n"
        out = open(path, "a", buffering=1)
        if needs_newline:
            out.write("\n")
        return out
    return open(path, "a", buffering=1)


def fmt_hours(seconds: float) -> str:
    return f"{seconds / 3600:.1f} h" if seconds >= 3600 else f"{seconds / 60:.0f} min"


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--posts", required=True, help="clef_export.py's file")
    ap.add_argument("--out", help="results, JSON lines, appended to")
    ap.add_argument("--images", default="/data/images", help="image archive root (holds 1000/)")
    ap.add_argument("--model", default="/data/clef/clef-flash", help="the Clef release directory")
    ap.add_argument("--taxonomy", default=str(Path(__file__).resolve().parent.parent / "taxonomy" / "v1.yaml"))
    ap.add_argument("--shard", default="0/1", help="i[,j...]/n: do the posts whose line number mod n is one of the i")
    ap.add_argument("--skip", action="append", default=[], help="result files from other machines; their posts are done")
    ap.add_argument("--limit", type=int, default=0, help="stop after this many new posts (0: all)")
    ap.add_argument("--priorities", default="0,1,2",
                    help="which posts: 0 attached pictures, 1 link-card picture only, 2 no picture (default all)")
    ap.add_argument("--wait-for-pictures", action="store_true",
                    help="only label a post once its picture files are present; keep going back for the rest")
    ap.add_argument("--top-k", type=int, default=3)
    ap.add_argument("--min-broad-p", type=float, default=0.05)
    ap.add_argument("--max-failures", type=int, default=25, help="stop after this many failures in a row")
    ap.add_argument("--gpu-fraction", type=float, default=0.86, help="cap on this process's share of GPU memory")
    ap.add_argument("--prefetch", type=int, default=3,
                    help="prepare the pictures of this many upcoming posts on a background thread, and remember the "
                         "question tokens (clef_fast.py); 0 turns it off")
    ap.add_argument("--dry-run", type=int, default=0, help="show how this many posts would be sent, and exit")
    a = ap.parse_args()

    mine, _, count = a.shard.partition("/")
    mine, shards = {int(x) for x in mine.split(",")}, int(count)
    assert all(0 <= i < shards for i in mine), "--shard must be i[,j...]/n with every i in 0..n-1"
    wanted = {int(x) for x in a.priorities.split(",")}
    tax = yaml.safe_load(open(a.taxonomy))

    def build():
        """The posts still to do, in export order, and how many wait for their picture files."""
        done = read_done(a.out, *a.skip)
        todo, waiting = [], 0
        with open(a.posts) as f:
            for n, line in enumerate(f):
                if n % shards not in mine:
                    continue
                row = json.loads(line)
                if row["priority"] not in wanted or row["uri"] in done:
                    continue
                if a.wait_for_pictures and not pictures_ready(a.images, row["image_shas"]):
                    waiting += 1
                    continue
                todo.append(row)
        if a.limit:
            todo = todo[: a.limit]
        by_priority = [sum(1 for r in todo if r["priority"] == p) for p in range(3)]
        print(f"shard {a.shard}: {len(todo)} posts to do now ({len(done)} done"
              f"{f', {waiting} waiting for their picture files' if waiting else ''}): "
              f"pictures {by_priority[0]}, link card {by_priority[1]}, text only {by_priority[2]}", flush=True)
        return todo, waiting

    todo, waiting = build()
    if a.dry_run:
        shown = [r for p in range(3) for r in [r for r in todo if r["priority"] == p][: a.dry_run]]
        for row in shown:
            pics = load_pictures(a.images, row["image_shas"])
            state = {"posts": [jev_post(make_doc(prepare(row, pics)))]}
            print(f"--- priority {row['priority']}, {len(pics)}/{len(row['image_shas'])} pictures loaded, "
                  f"sizes {[im.size for im in pics]}\n{json.dumps(state, ensure_ascii=False)[:700]}")
        return
    if not a.out:
        sys.exit("--out is required")

    os.environ.setdefault("PYTORCH_CUDA_ALLOC_CONF", "expandable_segments:True")  # long runs, many shapes
    import torch

    torch.cuda.set_per_process_memory_fraction(a.gpu_fraction)
    sys.path.insert(0, a.model)
    from joint_schema_model import load_release_model, systemone

    t0 = time.time()
    model, processor = load_release_model(a.model, device="cuda")
    print(f"{torch.cuda.get_device_name(0)}: loaded in {time.time() - t0:.0f}s, "
          f"{torch.cuda.memory_allocated() / 2**30:.1f} GiB in use", flush=True)

    cache = prepare_media = token_stats = None
    if a.prefetch:
        import joint_schema_model as jsm
        from transformers import AutoProcessor

        from clef_fast import MediaCache, Prefetcher, install_token_cache

        token_stats = install_token_cache(jsm)
        cache = MediaCache(jsm)
        prep_processor = AutoProcessor.from_pretrained(a.model)  # its own tokenizer: two threads must not share one

        def prepare_media(pictures):
            return cache.orig(prep_processor, {"images": pictures})

        print(f"picture prefetch on (depth {a.prefetch}), question tokens remembered", flush=True)

    out = open_append(a.out)
    errors = open_append(a.out + ".errors.jsonl")
    failures_in_a_row, failed, total = 0, 0, 0
    while True:
        if not todo:
            if waiting and a.wait_for_pictures:
                print(f"{waiting} posts wait for their picture files; looking again in {RESCAN_SECONDS}s", flush=True)
                time.sleep(RESCAN_SECONDS)
                todo, waiting = build()
                continue
            print("nothing left to do", flush=True)
            return
        started, window_start, window_n = time.time(), time.time(), 0
        prefetcher = (Prefetcher(todo, lambda r: load_pictures(a.images, r["image_shas"]), prepare_media, cache, a.prefetch)
                      if a.prefetch else None)
        for n, row in enumerate(todo, 1):
            if prefetcher:
                got, pictures = prefetcher.next()
                assert got is row, "the prefetcher fell out of step with the posts"
            else:
                pictures = load_pictures(a.images, row["image_shas"])
            loaded = pictures
            try:
                try:
                    res = label_post(model, processor, systemone, tax,
                                     {"posts": [jev_post(make_doc(prepare(row, pictures)))]},
                                     a.top_k, a.min_broad_p, pictures or None)
                except torch.cuda.OutOfMemoryError:
                    # A post with several big pictures: once more with the first picture only.
                    torch.cuda.empty_cache()
                    pictures = pictures[:1]
                    res = label_post(model, processor, systemone, tax,
                                     {"posts": [jev_post(make_doc(prepare(row, pictures)))]},
                                     a.top_k, a.min_broad_p, pictures or None)
                res.update(uri=row["uri"], priority=row["priority"], images=len(pictures), used_pictures=bool(pictures))
                out.write(json.dumps(res, ensure_ascii=False) + "\n")
                failures_in_a_row = 0
            except Exception as e:  # noqa: BLE001 - one bad post must not end a day-long run
                failed += 1
                failures_in_a_row += 1
                errors.write(json.dumps({"uri": row["uri"], "error": f"{type(e).__name__}: {str(e)[:300]}"}) + "\n")
                torch.cuda.empty_cache()
                if failures_in_a_row >= a.max_failures:
                    sys.exit(f"{failures_in_a_row} posts failed in a row, last: {type(e).__name__}: {str(e)[:200]}")
            if cache is not None:
                cache.release(loaded, pictures)  # the post is done, whichever pictures it ended up using
            if n % 200 == 0 or n == len(todo):
                now = time.time()
                rate_now, rate_all = (n - window_n) / max(now - window_start, 1e-9), n / (now - started)
                window_start, window_n = now, n
                print(f"{n}/{len(todo)}  {rate_now:.2f} posts/s now, {rate_all:.2f} this round  "
                      f"ETA {fmt_hours((len(todo) - n) / rate_all)} for this round  failed {failed}  "
                      f"peak GPU {torch.cuda.max_memory_allocated() / 2**30:.1f} GiB"
                      + (f"  cache hits: tokens {token_stats['hits']}/{token_stats['hits'] + token_stats['misses']}, "
                         f"pictures {cache.stats['hits']}/{cache.stats['hits'] + cache.stats['misses']}" if cache else ""),
                      flush=True)
        total += len(todo)
        if a.limit:
            return
        todo, waiting = build()


if __name__ == "__main__":
    main()
