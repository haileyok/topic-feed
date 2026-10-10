"""Exact k nearest neighbours by cosine on the GPU.

Needs torch with a GPU, which this project's own environment does not have: run it with the python of
~/bluesky/topic-feed/trainer (torch ROCm), as a member of the `render` group:

    sg render -c "~/bluesky/topic-feed/trainer/.venv/bin/python gpu_knn.py in.npy out_prefix [k]"

in.npy is an (n, d) array of unit vectors. Writes out_prefix.idx.npy (int32, n x k, each row's own index first,
then its k-1 nearest others, nearest first) and out_prefix.dist.npy (float32, cosine distance 1 - similarity,
0 for the point itself), in the shape umap's `precomputed_knn` takes.
"""
import sys
import time

import numpy as np
import torch


def knn(x, k, batch=None, dev='cuda'):
    n = len(x)
    batch = batch or (4096 if n < 400_000 else 1024)  # the scores of a batch against every point must fit in memory
    xt = torch.from_numpy(x).to(dev, torch.float16)
    xf = torch.from_numpy(x).to(dev, torch.float32)
    idx = np.empty((n, k), dtype=np.int32)
    dist = np.empty((n, k), dtype=np.float32)
    for lo in range(0, n, batch):
        hi = min(lo + batch, n)
        # fp16 scores to rank a few more than k, then exact fp32 similarities for the ones kept
        top = (xt[lo:hi] @ xt.T).topk(k + 4, dim=1).indices
        sim = (xf[top] * xf[lo:hi, None, :]).sum(-1)
        order = sim.argsort(dim=1, descending=True)
        top, sim = top.gather(1, order), sim.gather(1, order)
        own = torch.arange(lo, hi, device=dev)[:, None]
        notself = (top != own).to(torch.int8)
        # put the point itself first, then its neighbours; a point missing from its own top list is still first
        keep = notself.argsort(dim=1, descending=True, stable=True)[:, : k - 1]
        nb, ns = top.gather(1, keep), sim.gather(1, keep)
        idx[lo:hi] = torch.cat([own, nb], dim=1).cpu().numpy()
        dist[lo:hi] = torch.cat([torch.zeros_like(own, dtype=torch.float32), (1 - ns).clamp_min(0)], dim=1).cpu().numpy()
    return idx, dist


def search(queries, base, k, batch=1024, dev='cuda'):
    """For each query vector, the k most similar rows of `base` (the query itself is not special): (indices, similarities)."""
    bt = torch.from_numpy(np.ascontiguousarray(base)).to(dev, torch.float16)
    idx = np.empty((len(queries), k), dtype=np.int32)
    sim = np.empty((len(queries), k), dtype=np.float32)
    for lo in range(0, len(queries), batch):
        q = torch.from_numpy(np.ascontiguousarray(queries[lo:lo + batch])).to(dev, torch.float16)
        v, i = (q @ bt.T).topk(k, dim=1)
        idx[lo:lo + batch], sim[lo:lo + batch] = i.cpu().numpy(), v.float().cpu().numpy()
    return idx, sim


if __name__ == '__main__' and sys.argv[1] == 'search':
    # gpu_knn.py search queries.npy base.npy out_prefix k  ->  out_prefix.idx.npy, out_prefix.sim.npy
    t = time.time()
    idx, sim = search(np.load(sys.argv[2], mmap_mode='r'), np.load(sys.argv[3]), int(sys.argv[5]))
    print(f'{len(idx):,} queries against {len(np.load(sys.argv[3], mmap_mode="r")):,}: {time.time() - t:.0f}s', flush=True)
    np.save(sys.argv[4] + '.idx.npy', idx)
    np.save(sys.argv[4] + '.sim.npy', sim)
elif __name__ == '__main__':
    src, prefix = sys.argv[1], sys.argv[2]
    k = int(sys.argv[3]) if len(sys.argv) > 3 else 20
    x = np.load(src).astype(np.float32)
    t = time.time()
    idx, dist = knn(x, k)
    print(f'{len(x):,} points, k={k}: {time.time() - t:.0f}s on {torch.cuda.get_device_name(0)}', flush=True)
    np.save(prefix + '.idx.npy', idx)
    np.save(prefix + '.dist.npy', dist)
