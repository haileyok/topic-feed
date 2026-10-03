"""Takes CPU work off the GPU's critical path in clef_run.py.

For every post the labeller used to do, one step after another on one thread: load the pictures,
normalise and cut them into patches, tokenize the questions (about 230 small calls), run the model
for pass 1, then do the picture preparation and the tokenizing all over again for pass 2, and run the
model again. The GPU stood idle during every CPU step (about 17% of the time on the 5090).

This module changes three things and nothing the model sees:

  install_token_cache   the questions and their options are the same for every post, so their token
                        ids are remembered instead of computed again (a bounded cache, so the post
                        text itself, which is different every time, does not pile up).
  MediaCache            the prepared picture inputs of a post are computed once and used by both
                        passes. It keeps the loaded images alive until release() so that a reused
                        memory address can never hand one post another post's pictures.
  Prefetcher            a background thread loads and prepares the pictures of the next few posts
                        while the GPU works on the current one. It has its own processor object,
                        because a fast tokenizer must not be used from two threads at once.

The prepared tensors are the same ones the labeller computed itself, so the model's input is
identical; clef_fast_check.py proves that on real posts without a GPU.
"""

import collections
import queue
import threading


def install_token_cache(jsm, maxsize: int = 8192) -> dict:
    """Remember the token ids of text seen again and again. Only the main thread may call the
    patched function (the prefetch thread never tokenizes). Returns hit and miss counters."""
    orig = jsm._tokens
    cache: collections.OrderedDict = collections.OrderedDict()
    stats = {"hits": 0, "misses": 0}

    def cached(tokenizer, text):
        key = (id(tokenizer), text)
        hit = cache.get(key)
        if hit is None:
            stats["misses"] += 1
            hit = tuple(orig(tokenizer, text))
            cache[key] = hit
            if len(cache) > maxsize:
                cache.popitem(last=False)
        else:
            stats["hits"] += 1
            cache.move_to_end(key)
        return list(hit)  # callers extend and slice the list, so hand out a copy

    jsm._tokens = cached
    return stats


class MediaCache:
    """Prepared picture inputs, keyed by the identity of the loaded images of a post."""

    def __init__(self, jsm):
        self.orig = jsm._encode_media
        self._lock = threading.Lock()
        self._store: dict = {}
        self.stats = {"hits": 0, "misses": 0}
        jsm._encode_media = self._encode_media

    @staticmethod
    def _key(images) -> tuple:
        return tuple(id(i) for i in images)

    def put(self, images, ids, media) -> None:
        # Holding `images` keeps those objects alive, so their ids cannot be reused while the entry exists.
        with self._lock:
            self._store[self._key(images)] = (images, ids, media)

    def release(self, *image_lists) -> None:
        with self._lock:
            for images in image_lists:
                self._store.pop(self._key(images), None)

    def _encode_media(self, processor, record):
        images = list(record.get("images") or [])
        if not images or record.get("videos") or record.get("media_kwargs"):
            return self.orig(processor, record)
        with self._lock:
            hit = self._store.get(self._key(images))
        if hit is None:
            self.stats["misses"] += 1
            ids, media = self.orig(processor, record)
            self.put(images, ids, media)  # pass 2 of the same post reuses it
        else:
            self.stats["hits"] += 1
            _, ids, media = hit
        # encode_record adds a field to the media dict, so every caller gets its own shallow copy.
        return list(ids), dict(media)

    def __len__(self) -> int:
        with self._lock:
            return len(self._store)


class Prefetcher:
    """Loads and prepares the pictures of upcoming posts on a background thread.

    next() returns (row, pictures) in the order of `rows`, blocking until the post is ready; by then
    the prepared inputs are already in the cache. A failure while preparing is not raised here: the
    main thread meets the same error when it encodes that post and reports it for that post alone.
    """

    def __init__(self, rows, load, prepare, cache: MediaCache, depth: int = 3):
        self._q: queue.Queue = queue.Queue(maxsize=max(depth, 1))
        threading.Thread(target=self._run, args=(rows, load, prepare, cache), daemon=True, name="clef-prefetch").start()

    def _run(self, rows, load, prepare, cache) -> None:
        for row in rows:
            try:
                pictures = load(row)
            except Exception:  # noqa: BLE001
                pictures = []
            if pictures:
                try:
                    ids, media = prepare(pictures)
                    cache.put(pictures, ids, media)
                except Exception:  # noqa: BLE001
                    pass
            self._q.put((row, pictures))  # waits here while the queue is full
        self._q.put(None)

    def next(self):
        return self._q.get()
