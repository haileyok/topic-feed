"""Tests for serve.GpuPipeline with a fake model (no GPU, no weights).

    cd trainer && uv run python -m unittest test_serve_pipeline -v
"""

import random
import threading
import time
import unittest

import numpy as np

from serve import GpuPipeline
from topic_classifier import TopicClassifier


class FakeModel:
    """Same interface the pipeline uses. Each item is {"text": str, "id": (request, index), "pictures": []}; run_batch
    returns the ids as the "broad" probabilities so the tests can see who got what."""

    plan_batches = staticmethod(TopicClassifier.plan_batches)
    assemble = staticmethod(TopicClassifier.assemble)

    def __init__(self, prep_s=0.0, run_s=0.0, fail_prep=(), fail_run=()):
        self.prep_s, self.run_s = prep_s, run_s
        self.fail_prep, self.fail_run = set(fail_prep), set(fail_run)
        self.mu = threading.Lock()
        self.in_flight = 0  # batches prepared, waiting or running
        self.max_in_flight = 0
        self.running = 0
        self.max_running = 0
        self.events = []  # (time, "prep_start"/"run_end", request, batch's first item index)

    def _log(self, what, batch):
        rid = batch[0]["id"][0]
        with self.mu:
            self.events.append((time.perf_counter(), what, rid))

    def prepare_batch(self, items):
        with self.mu:
            self.in_flight += 1
            self.max_in_flight = max(self.max_in_flight, self.in_flight)
        self._log("prep_start", items)
        time.sleep(self.prep_s)
        if any(it["id"] in self.fail_prep for it in items):
            raise RuntimeError("prepare failed")
        return items

    def run_batch(self, n, prepared):
        try:
            with self.mu:
                self.running += 1
                self.max_running = max(self.max_running, self.running)
            time.sleep(self.run_s)
            if any(it["id"] in self.fail_run for it in prepared):
                raise RuntimeError("run failed")
            ids = np.array([it["id"] for it in prepared], dtype=np.float32)
            return [ids, np.zeros((n, 3), np.float32), np.zeros((n, 2), np.float32), np.zeros((n, 1), np.float32)], np.ones(n, int)
        finally:
            with self.mu:
                self.running -= 1
                self.in_flight -= 1
            self._log("run_end", prepared)


def make_items(rid, n, rng):
    return [{"text": "x" * rng.randint(0, 200), "id": (rid, i), "pictures": []} for i in range(n)]


class PipelineTest(unittest.TestCase):
    def check(self, rid, items, out):
        self.assertEqual(out["broad"].shape[0], len(items))
        for i, row in enumerate(out["broad"]):
            self.assertEqual(tuple(row), (rid, i), f"request {rid} item {i} got someone else's result")
        self.assertEqual(out["pictures_used"].tolist(), [1] * len(items))

    def test_results_come_back_in_input_order(self):
        rng = random.Random(1)
        p = GpuPipeline(FakeModel(), batch_size=7, depth=3)
        items = make_items(0, 50, rng)
        self.check(0, items, p.run(items))

    def test_single_item_and_exact_batch_multiple(self):
        rng = random.Random(2)
        p = GpuPipeline(FakeModel(), batch_size=4, depth=2)
        for n in (1, 3, 4, 8, 9):
            items = make_items(0, n, rng)
            self.check(0, items, p.run(items))

    def test_empty_request_is_refused(self):
        with self.assertRaises(ValueError):
            GpuPipeline(FakeModel()).run([])

    def test_many_concurrent_requests_each_get_their_own_results(self):
        model = FakeModel(prep_s=0.002, run_s=0.002)
        p = GpuPipeline(model, batch_size=5, depth=4, prep_threads=3)
        errors, done = [], []

        def client(rid):
            try:
                for rep in range(3):
                    r = random.Random(rid * 10 + rep)  # a different size and text lengths per request
                    items = make_items(rid, r.randint(1, 60), r)
                    self.check(rid, items, p.run(items))
                done.append(rid)
            except BaseException as e:  # noqa: BLE001
                errors.append((rid, e))

        threads = [threading.Thread(target=client, args=(r,)) for r in range(16)]
        [t.start() for t in threads]
        [t.join(timeout=60) for t in threads]
        self.assertEqual(errors, [])
        self.assertEqual(sorted(done), list(range(16)))

    def test_only_one_batch_runs_at_a_time_and_memory_is_bounded(self):
        rng = random.Random(4)
        model = FakeModel(prep_s=0.003, run_s=0.004)
        p = GpuPipeline(model, batch_size=3, depth=3, prep_threads=2)
        ts = [threading.Thread(target=lambda r=r: p.run(make_items(r, 40, rng))) for r in range(6)]
        [t.start() for t in ts]
        [t.join(timeout=60) for t in ts]
        self.assertEqual(model.max_running, 1, "the GPU must see one batch at a time")
        self.assertLessEqual(model.max_in_flight, 3, "no more than depth batches may be prepared or running")

    def test_a_failing_batch_fails_only_its_request(self):
        rng = random.Random(5)
        for where in ("prep", "run"):
            bad = {(1, 13)}
            model = FakeModel(fail_prep=bad if where == "prep" else (), fail_run=bad if where == "run" else ())
            p = GpuPipeline(model, batch_size=4, depth=3)
            results = {}

            def client(rid):
                items = make_items(rid, 30, random.Random(rid))
                try:
                    results[rid] = ("ok", p.run(items), items)
                except RuntimeError as e:
                    results[rid] = ("error", str(e), items)

            ts = [threading.Thread(target=client, args=(r,)) for r in range(4)]
            [t.start() for t in ts]
            [t.join(timeout=60) for t in ts]
            self.assertEqual(results[1][0], "error", where)
            for rid in (0, 2, 3):
                self.assertEqual(results[rid][0], "ok", f"{where}: request {rid} must not be affected")
                self.check(rid, results[rid][2], results[rid][1])
            # the pipeline still works afterwards, and no batch (run, failed or skipped) is left occupying a slot
            items = make_items(9, 20, rng)
            self.check(9, items, p.run(items))
            self.assertEqual(p._slots._value, 3, f"{where}: a slot leaked")

    def test_next_request_is_prepared_while_the_previous_one_runs(self):
        # 8 batches each, preparing takes half as long as running. Without overlap across requests, request B's
        # first batch would only start preparing after request A's last batch has finished running.
        model = FakeModel(prep_s=0.03, run_s=0.06)
        p = GpuPipeline(model, batch_size=2, depth=4, prep_threads=2)
        a = [{"text": "x", "id": (0, i), "pictures": []} for i in range(16)]
        b = [{"text": "x", "id": (1, i), "pictures": []} for i in range(16)]
        ta = threading.Thread(target=p.run, args=(a,))
        ta.start()
        time.sleep(0.05)
        tb = threading.Thread(target=p.run, args=(b,))
        tb.start()
        ta.join(30)
        tb.join(30)
        a_done = max(t for t, what, rid in model.events if what == "run_end" and rid == 0)
        b_first_prep = min(t for t, what, rid in model.events if what == "prep_start" and rid == 1)
        self.assertLess(b_first_prep, a_done, "request B's first batch should be prepared before A has finished")

    def test_worker_survives_an_error_in_assemble_inputs(self):
        # a request whose run raises must not kill the worker thread
        model = FakeModel(fail_run={(0, 0)})
        p = GpuPipeline(model, batch_size=2, depth=2)
        with self.assertRaises(RuntimeError):
            p.run([{"text": "x", "id": (0, 0), "pictures": []}, {"text": "x", "id": (0, 1), "pictures": []}])
        ok = [{"text": "x", "id": (5, 0), "pictures": []}]
        self.check(5, ok, p.run(ok))


if __name__ == "__main__":
    unittest.main()
