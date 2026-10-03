"""Shows that clef_fast.py leaves the model's input unchanged, on real posts, without a GPU.

For each post it builds the pass-1 and pass-2 requests exactly as clef_labels.label_post does (pass 2
for the topics the post's real result put on top), encodes them the old way (nothing cached, pictures
prepared inside each request), then the new way (question tokens remembered, pictures prepared ahead
by the background thread and shared by both passes), and compares everything the model would
receive: token ids, question spans, and the picture tensors. It also times the encoding step.

    CUDA_VISIBLE_DEVICES= python clef_fast_check.py --results clef-v21-pictures.jsonl --n 60
"""

import argparse
import json
import sys
import time
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))


def same(a, b, torch) -> bool:
    if a.input_ids != b.input_ids or a.questions != b.questions or a.record_id != b.record_id:
        return False
    ma, mb = a.media, b.media
    if (ma is None) != (mb is None):
        return False
    if ma is None:
        return True
    if set(ma) != set(mb):
        return False
    for k in ma:
        if torch.is_tensor(ma[k]):
            if not torch.equal(ma[k], mb[k]):
                return False
        elif ma[k] != mb[k]:
            return False
    return True


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--model", default="/data/clef/clef-flash")
    ap.add_argument("--results", required=True, help="clef_run.py results (for each post's top topics)")
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl")
    ap.add_argument("--images", default="/data/images")
    ap.add_argument("--taxonomy", default=str(HERE.parent / "taxonomy" / "v2.1.yaml"))
    ap.add_argument("--n", type=int, default=60)
    a = ap.parse_args()

    sys.path.insert(0, a.model)
    import torch
    import joint_schema_model as jsm
    from transformers import AutoProcessor

    import clef_fast
    import clef_labels as cl
    import clef_run as cr

    tax = yaml.safe_load(open(a.taxonomy))
    by_id = {b["id"]: b for b in tax["broad"]}
    proc = AutoProcessor.from_pretrained(a.model)
    prep_proc = AutoProcessor.from_pretrained(a.model)
    tok = proc.tokenizer

    # a mix of posts with 1, 2, 3 and 4 pictures
    results, per_count = {}, {}
    for line in open(a.results):
        try:
            r = json.loads(line)
        except ValueError:
            continue
        k = min(r.get("images", 0), 4)
        if k and per_count.get(k, 0) < max(a.n // 4, 1):
            per_count[k] = per_count.get(k, 0) + 1
            results[r["uri"]] = r
    rows = []
    for line in open(a.posts):
        if not line.startswith('{"uri":"'):
            continue
        uri = json.loads('"' + line[8:line.index('"', 8)] + '"')
        if uri in results:
            rows.append(json.loads(line))
    print(f"{len(rows)} posts; pictures per post: {dict(sorted(per_count.items()))}")

    def requests(row, pics, res):
        state = {"posts": [cl.jev_post(cl.make_doc(cr.prepare(row, pics)))]}
        r1 = {"model": "clef-flash", "state": state, "questions": cl.pass1_questions(tax), "images": pics}
        pairs = []
        for rank, (b, p) in enumerate(sorted(res["broad_probs"].items(), key=lambda kv: -kv[1])):
            if rank >= 3 or p < 0.05:
                break
            if by_id.get(b, {}).get("subtopics"):
                pairs.append(by_id[b])
        r2 = ({"model": "clef-flash", "state": state, "images": pics,
               "questions": {f"p0_{b['id']}_sub": cl.pass2_question(b) for b in pairs}} if pairs else None)
        return r1, r2

    # the old way
    base, t_old = {}, 0.0
    for row in rows:
        pics = cr.load_pictures(a.images, row["image_shas"])
        r1, r2 = requests(row, pics, results[row["uri"]])
        t = time.perf_counter()
        e1 = jsm.encode_record(tok, r1, processor=proc)
        e2 = jsm.encode_record(tok, r2, processor=proc) if r2 else None
        t_old += time.perf_counter() - t
        base[row["uri"]] = (e1, e2)

    # the new way
    token_stats = clef_fast.install_token_cache(jsm)
    cache = clef_fast.MediaCache(jsm)
    pre = clef_fast.Prefetcher(rows, lambda r: cr.load_pictures(a.images, r["image_shas"]),
                               lambda pics: cache.orig(prep_proc, {"images": pics}), cache, depth=3)
    ok1 = ok2 = n2 = 0
    t_new = 0.0
    for row in rows:
        got, pics = pre.next()
        assert got is row
        r1, r2 = requests(row, pics, results[row["uri"]])
        t = time.perf_counter()
        e1 = jsm.encode_record(tok, r1, processor=proc)
        e2 = jsm.encode_record(tok, r2, processor=proc) if r2 else None
        t_new += time.perf_counter() - t
        b1, b2 = base[row["uri"]]
        ok1 += same(e1, b1, torch)
        if r2:
            n2 += 1
            ok2 += same(e2, b2, torch)
        cache.release(pics)
    print(f"pass 1 requests identical to the old way: {ok1} of {len(rows)}")
    print(f"pass 2 requests identical to the old way: {ok2} of {n2}")
    print(f"picture inputs: {cache.stats['hits']} served from the prefetch/shared cache, {cache.stats['misses']} computed on the main thread; "
          f"entries left after release: {len(cache)} (must be 0)")
    print(f"question tokens: {token_stats['hits']} remembered, {token_stats['misses']} computed")
    print(f"encoding time on the main thread, both passes, {len(rows)} posts: old {t_old:.2f} s, new {t_new:.2f} s "
          f"({1000 * t_old / len(rows):.0f} -> {1000 * t_new / len(rows):.0f} ms per post)")
    all_ok = ok1 == len(rows) and ok2 == n2 and len(cache) == 0
    print("ALL IDENTICAL" if all_ok else "MISMATCH")
    sys.exit(0 if all_ok else 1)


if __name__ == "__main__":
    main()
