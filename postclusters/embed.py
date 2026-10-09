"""Embed the pulled posts with nomic-embed-text through the local Ollama.

For every shard asked for, reads data/posts/shard-XX.parquet and writes data/emb/shard-XX/chunk-NNNNN.npy,
2,000 rows each, float16, 768 dimensions, in the row order of the parquet file. A chunk that exists is
skipped, so an interrupted run resumes where it stopped (delete a shard's folder to redo it).

Documents get nomic's "clustering: " task prefix, which the model was trained to expect when the
embeddings are used to group texts. Embeddings come back from Ollama normalised to unit length.
Measured speed on this machine is about 150 posts per second whatever the batch size or number of
parallel requests, so a shard of 215,000 posts takes about 25 minutes.

Usage: embed.py [--shards 0-3] [--workers 2]
"""
import argparse
import concurrent.futures as cf
import json
import time
import urllib.request

import numpy as np
import pandas as pd

from common import DATA, doc_fingerprint, shard_list

OLLAMA = 'http://127.0.0.1:11434/api/embed'
MODEL = 'nomic-embed-text'
PREFIX = 'clustering: '
CHUNK = 2000
BATCH = 100
DIM = 768


def embed_batch(texts, retries=4):
    body = json.dumps({'model': MODEL, 'input': [PREFIX + t for t in texts], 'truncate': True}).encode()
    for attempt in range(retries):
        try:
            req = urllib.request.Request(OLLAMA, data=body, headers={'Content-Type': 'application/json'})
            with urllib.request.urlopen(req, timeout=300) as r:
                e = np.array(json.load(r)['embeddings'], dtype=np.float32)
            if e.shape != (len(texts), DIM) or not np.isfinite(e).all():
                raise ValueError(f'unexpected embeddings {e.shape}')
            return e
        except Exception:
            if attempt == retries - 1:
                raise
            time.sleep(2 * (attempt + 1))


def embed_chunk(texts):
    return np.concatenate([embed_batch(texts[i:i + BATCH]) for i in range(0, len(texts), BATCH)]).astype(np.float16)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--shards', default='0-3')
    ap.add_argument('--workers', type=int, default=2)
    a = ap.parse_args()
    for s in shard_list(a.shards):
        src = DATA / 'posts' / f'shard-{s:02d}.parquet'
        docs = pd.read_parquet(src, columns=['doc'])['doc'].tolist()
        out = DATA / 'emb' / f'shard-{s:02d}'
        out.mkdir(parents=True, exist_ok=True)
        fp = doc_fingerprint(docs)
        fp_file = out / 'docs.sha1'
        if fp_file.exists() and fp_file.read_text().strip() != fp:
            raise SystemExit(f'shard {s}: its posts have changed since its embeddings were made (the file was pulled again?); '
                             f'delete {out} to embed it afresh')
        fp_file.write_text(fp + '\n')
        todo = [(i // CHUNK, docs[i:i + CHUNK]) for i in range(0, len(docs), CHUNK) if not (out / f'chunk-{i // CHUNK:05d}.npy').exists()]
        done0 = (len(docs) + CHUNK - 1) // CHUNK - len(todo)
        print(f'shard {s:02d}: {len(docs):,} posts, {len(todo)} chunks to do ({done0} already done)', flush=True)
        t0, n = time.time(), 0
        with cf.ThreadPoolExecutor(a.workers) as ex:
            futs = {ex.submit(embed_chunk, texts): k for k, texts in todo}
            for f in cf.as_completed(futs):
                k = futs[f]
                np.save(out / f'chunk-{k:05d}.npy.tmp.npy', f.result())
                (out / f'chunk-{k:05d}.npy.tmp.npy').rename(out / f'chunk-{k:05d}.npy')
                n += CHUNK
                if (n // CHUNK) % 10 == 0:
                    rate = n / (time.time() - t0)
                    left = (len(todo) * CHUNK - n) / rate
                    print(f'  shard {s:02d}: {n:,} done, {rate:.0f} posts/s, about {left / 60:.0f} min left', flush=True)
        print(f'shard {s:02d} done in {(time.time() - t0) / 60:.1f} min', flush=True)


if __name__ == '__main__':
    main()
