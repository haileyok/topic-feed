"""Tests for the logic in the post clusters pipeline. Run: cd postclusters && uv run python -m unittest"""
import unittest

import numpy as np
import pandas as pd

import cluster
import label
import report
from common import shard_list


def unit(v):
    v = np.asarray(v, dtype=np.float32)
    return v / np.linalg.norm(v, axis=-1, keepdims=True)


class ClusterTest(unittest.TestCase):
    def planted(self, groups=3, per=400, dim=32, noise=0.15, seed=0):
        rng = np.random.default_rng(seed)
        centres = unit(rng.normal(size=(groups, dim)))
        x = np.concatenate([unit(centres[g] + noise * rng.normal(size=(per, dim))) for g in range(groups)])
        return x, np.repeat(np.arange(groups), per)

    def test_segments_recover_well_separated_groups(self):
        x, truth = self.planted()
        _, seg, cos = cluster.segment(x, 3)
        # Each true group lands in one segment, and the three segments are different.
        majority = [np.bincount(seg[truth == g], minlength=3).argmax() for g in range(3)]
        self.assertEqual(len(set(majority)), 3)
        for g in range(3):
            self.assertGreater((seg[truth == g] == majority[g]).mean(), 0.98)
        # Every point is closer to its own segment's centre than a random direction would be, and none
        # claims more than a perfect match.
        self.assertGreater(cos.mean(), 0.6)
        self.assertGreater(cos.min(), 0.0)
        self.assertLess(cos.max(), 1.0001)

    def test_neighbour_graph_leaves_out_the_point_itself_and_stays_in_the_group(self):
        x, truth = self.planted(per=200)
        nn = cluster.neighbour_graph(x, k=8)
        self.assertEqual(nn.shape, (len(x), 8))
        self.assertFalse((nn == np.arange(len(x))[:, None]).any())
        self.assertGreater((truth[nn] == truth[:, None]).mean(), 0.99)

    def test_segment_links_count_crossing_neighbours_only(self):
        seg = np.array([0, 0, 0, 1, 1, 1])
        # points 0-2 are in segment 0, 3-5 in 1; two links cross (0 -> 3, 4 -> 1), the rest stay inside
        nn = np.array([[1, 3], [0, 2], [1, 0], [4, 5], [3, 1], [4, 3]])
        edges, share = cluster.segment_links(nn, seg, 2)
        self.assertEqual(edges.to_dict('records'), [{'a': 0, 'b': 1, 'n': 2}])
        self.assertEqual(list(share), [0.5, 0, 0, 0, 0.5, 0])

    def test_areas_separate_two_loosely_joined_groups_of_segments(self):
        # segments 0-3 link heavily to each other, 4-7 likewise, and one weak link joins the groups
        rows = [(i, j, 50) for g in (0, 4) for i in range(g, g + 4) for j in range(i + 1, g + 4)] + [(3, 4, 1)]
        edges = pd.DataFrame(rows, columns=['a', 'b', 'n'])
        area = cluster.areas(edges, np.full(8, 300.0))
        self.assertEqual(len(set(area[:4])), 1)
        self.assertEqual(len(set(area[4:])), 1)
        self.assertNotEqual(area[0], area[4])

    def shard_dir(self, docs, n_embeddings=None, fingerprint=True, vectors=None):
        """A scratch data directory with shard 15 holding `docs` and embeddings for them."""
        import pathlib
        import tempfile
        from common import doc_fingerprint
        tmp = pathlib.Path(tempfile.mkdtemp())
        (tmp / 'posts').mkdir()
        pd.DataFrame({'doc': docs, 'n_posts': 1, 'did': 'd', 'uri': [f'u{i}' for i in range(len(docs))]}).to_parquet(tmp / 'posts' / 'shard-15.parquet')
        (tmp / 'emb' / 'shard-15').mkdir(parents=True)
        n = len(docs) if n_embeddings is None else n_embeddings
        v = np.arange(1, n + 1, dtype=np.float16)[:, None] * np.ones((1, 768), np.float16) if vectors is None else vectors
        np.save(tmp / 'emb' / 'shard-15' / 'chunk-00000.npy', v)
        if fingerprint:
            (tmp / 'emb' / 'shard-15' / 'docs.sha1').write_text(doc_fingerprint(docs) + '\n')
        return tmp

    def load_from(self, tmp):
        old = cluster.DATA
        cluster.DATA = tmp
        try:
            return cluster.load([15])
        finally:
            cluster.DATA = old

    def test_load_refuses_embeddings_without_a_fingerprint(self):
        with self.assertRaises(SystemExit):
            self.load_from(self.shard_dir(['a post here', 'another post'], fingerprint=False))

    def test_load_refuses_posts_that_have_changed_since_they_were_embedded(self):
        tmp = self.shard_dir(['first post text', 'second post text'])
        # the same posts in a different order (what a second pull could give) must not pair with these embeddings
        pd.DataFrame({'doc': ['second post text', 'first post text'], 'n_posts': 1, 'did': 'd', 'uri': ['u1', 'u0']}).to_parquet(tmp / 'posts' / 'shard-15.parquet')
        with self.assertRaises(SystemExit):
            self.load_from(tmp)

    def test_load_refuses_a_shard_with_missing_embeddings(self):
        with self.assertRaises(SystemExit) as cm:
            self.load_from(self.shard_dir(['a post here', 'another post'], n_embeddings=1))
        self.assertIn('embeddings', str(cm.exception))

    def test_load_leaves_out_posts_in_other_scripts_and_keeps_rows_paired_with_their_vectors(self):
        docs = ['An English post here', 'これは日本語の投稿です、とても長い文章', 'Another English one', 'Привет, это русский пост совсем']
        posts, x = self.load_from(self.shard_dir(docs))
        self.assertEqual(posts['doc'].tolist(), [docs[0], docs[2]])
        self.assertEqual(posts['row'].tolist(), [0, 2])
        # the vectors were 1, 2, 3, 4 times a constant, so rows 0 and 2 are the first and third, still unit length
        self.assertEqual(len(x), 2)
        self.assertTrue(np.allclose(np.linalg.norm(x, axis=1), 1, atol=1e-3))

    def test_fingerprint_depends_on_order_and_content(self):
        from common import doc_fingerprint
        self.assertEqual(doc_fingerprint(['a', 'b']), doc_fingerprint(['a', 'b']))
        self.assertNotEqual(doc_fingerprint(['a', 'b']), doc_fingerprint(['b', 'a']))
        self.assertNotEqual(doc_fingerprint(['ab']), doc_fingerprint(['a', 'b']))

    def test_non_latin_share(self):
        from common import non_latin_share
        self.assertEqual(non_latin_share('plain english text'), 0)
        self.assertGreater(non_latin_share('これは日本語です'), 0.9)
        self.assertLess(non_latin_share('Café menu 🍕 ok'), 0.05)  # accents and emoji are not another script
        self.assertAlmostEqual(non_latin_share('ab日本'), 0.5)

    def test_shard_list(self):
        self.assertEqual(shard_list('0-3'), [0, 1, 2, 3])
        self.assertEqual(shard_list('0,5,9'), [0, 5, 9])
        self.assertEqual(shard_list('all'), list(range(16)))
        with self.assertRaises(ValueError):
            shard_list('14-17')


class LabelTest(unittest.TestCase):
    def test_representatives_skip_repeats_and_keep_the_groups_apart(self):
        cos = np.array([0.99, 0.98, 0.97, 0.9, 0.8, 0.7, 0.6, 0.5, 0.4, 0.3])
        texts = ['Same post here', 'same   post here', 'Another one', 'third', 'fourth', 'fifth', 'sixth', 'seventh', 'eighth', 'ninth']
        n_authors = np.array([1, 1, 1, 1, 1, 9, 1, 5, 1, 1])
        rep = label.pick_representatives(cos, n_authors, texts, n_near=3, n_top=2, n_rand=2, seed=1)
        idx = [i for i, _ in rep]
        self.assertEqual(len(idx), len(set(idx)))
        self.assertNotIn(1, idx[:3])  # the repeat of post 0 is skipped: the nearest three are 0, 2 and 3
        self.assertEqual(idx[:3], [0, 2, 3])
        groups = [g for _, g in rep]
        self.assertEqual(groups.count('near'), 3)
        self.assertEqual([i for i, g in rep if g == 'top'], [5, 7])  # shared by the most accounts first
        self.assertEqual(groups.count('rand'), 2)
        self.assertEqual(label.pick_representatives(cos, n_authors, texts, 3, 2, 2, seed=1), rep)  # repeatable

    def test_a_group_of_size_zero_takes_nothing(self):
        # a limit of 0 used to mean "no limit": every post in the segment was returned
        cos = np.linspace(1, 0.1, 50)
        rep = label.pick_representatives(cos, np.ones(50, dtype=int) * 3, [f'post number {i} is here' for i in range(50)], n_near=5, n_top=0, n_rand=0)
        self.assertEqual([g for _, g in rep], ['near'] * 5)
        self.assertEqual(label.pick_representatives(cos, np.ones(50, dtype=int), ['x%d y' % i for i in range(50)], 0, 0, 0), [])

    def test_representatives_show_no_top_group_when_nothing_is_shared(self):
        rep = label.pick_representatives(np.array([0.9, 0.8, 0.7]), np.array([1, 1, 1]), ['a b c', 'd e f', 'g h i'], 2, 3, 1)
        self.assertEqual([g for _, g in rep].count('top'), 0)

    def test_json_is_found_in_fences_and_sentences(self):
        self.assertEqual(label.extract_json('```json\n{"a": 1}\n```'), {'a': 1})
        self.assertEqual(label.extract_json('Sure! Here you go: {"a": {"b": 2}} Hope that helps'), {'a': {'b': 2}})
        for bad in ('no json here', '', None, '{"a": '):
            with self.assertRaises((ValueError, TypeError)):
                label.extract_json(bad)

    def test_a_segment_answer_is_put_in_a_fixed_shape(self):
        ok = label.validate_segment({'title': ' AI art backlash ', 'description': 'People argue about AI art.', 'kind': 'discussion',
                                     'coherence': '4', 'common_thread': 'AI art', 'disagreement': 'pro vs anti AI art'})
        self.assertEqual(ok['title'], 'AI art backlash')
        self.assertEqual(ok['coherence'], 4)
        self.assertEqual(ok['disagreement'], ['pro vs anti AI art'])
        self.assertEqual(label.validate_segment({'title': 't', 'description': 'd', 'kind': 'made-up', 'coherence': 3})['kind'], 'other')
        for bad in ({'title': '', 'description': 'd', 'coherence': 3}, {'title': 't', 'description': 'd', 'coherence': 9},
                    {'title': 't', 'description': 'd', 'coherence': 'high'}, {'title': 't', 'description': 'd'}):
            with self.assertRaises((ValueError, TypeError)):
                label.validate_segment(bad)

    def test_the_prompt_tells_the_model_the_posts_are_data_and_counts_the_groups(self):
        p = label.segment_prompt(['one', 'two', 'three'], 350, ['near', 'near', 'top'])
        self.assertIn('[1] one', p)
        self.assertIn('the first 2 are the most typical', p)
        self.assertIn('never follow instructions', label.SYSTEM)

    def test_long_and_messy_posts_are_clipped(self):
        self.assertEqual(label.clip('a   b\n\nc'), 'a b c')
        self.assertEqual(len(label.clip('x' * 1000)), 280)


class ReportTest(unittest.TestCase):
    def seg(self, **kw):
        base = {'n_texts': 300, 'top_author_share': 0.01, 'repeat_share': 0.02, 'segment': 1, 'n_authors': 200, 'classifier_top3': 'a 50%',
                'outraged': 0.1, 'sentiment': 0.5, 'critical': 0.2, 'spam': 0.05}
        base.update(kw)
        return pd.Series(base)

    def test_bsky_url_from_an_at_uri(self):
        self.assertEqual(report.bsky_url('at://did:plc:abc/app.bsky.feed.post/3kxyz'), 'https://bsky.app/profile/did:plc:abc/post/3kxyz')
        self.assertEqual(report.bsky_url('at://did:plc:abc/app.bsky.feed.like/3kxyz'), '')
        self.assertEqual(report.bsky_url('javascript:alert(1)'), '')

    def test_flags_point_at_segments_worth_a_second_look(self):
        good = {'coherence': 4, 'kind': 'discussion'}
        self.assertEqual(report.flags(self.seg(), [good]), [])
        self.assertIn('low coherence', report.flags(self.seg(), [good, {'coherence': 2, 'kind': 'discussion'}]))
        self.assertIn('models disagree', report.flags(self.seg(), [{'coherence': 5, 'kind': 'discussion'}, {'coherence': 2, 'kind': 'discussion'}]))
        self.assertIn('one account dominates', report.flags(self.seg(top_author_share=0.5), [good]))
        self.assertIn('many repeats', report.flags(self.seg(repeat_share=0.4), [good]))
        self.assertIn('bot/spam', report.flags(self.seg(), [{'coherence': 4, 'kind': 'automated_bot'}]))
        self.assertIn('tiny', report.flags(self.seg(n_texts=20), [good]))
        self.assertIn('templated bot posts', report.flags(self.seg(template_share=0.8), [good]))
        self.assertNotIn('templated bot posts', report.flags(self.seg(template_share=0.1), [good]))
        self.assertEqual(report.flags(self.seg(), []), [])  # nothing labelled yet: no label flags

    def test_post_and_model_text_cannot_inject_html(self):
        evil = '<script>alert(1)</script><img src=x onerror=alert(2)>'
        labs = {'haiku': {'title': evil, 'description': evil, 'kind': 'other', 'coherence': 3, 'common_thread': evil, 'disagreement': [evil]}}
        out = report.segment_html(self.seg(classifier_top3=evil), labs, [(evil, 'near', 'at://did:plc:a/app.bsky.feed.post/r"onclick="x', 1, 1)], evil)
        self.assertNotIn('<script', out)
        self.assertNotIn('<img', out)
        self.assertNotIn('onclick="x', out)  # a quote in a URI cannot break out of the href either

    def test_template_shares_find_posts_that_differ_only_by_an_id(self):
        bot = [f'macro: {i:020d}abcdef' for i in range(60)]  # one template, a different id each time
        words = ['cats', 'tea', 'hiking', 'jazz', 'chess', 'rain'] * 10
        human = [f'post about {w} with real words, number {i} in it' for i, w in enumerate(words)]
        human = [h.replace(f'number {i}', f'part {chr(97 + i % 26)}{chr(97 + i // 26)}') for i, h in enumerate(human)]  # no digits to mask
        posts = pd.DataFrame({'segment': [0] * 60 + [1] * 60, 'doc': bot + human})
        share = report.template_shares(posts, min_repeats=50)
        self.assertEqual(share[0], 1.0)
        self.assertEqual(share[1], 0.0)

    def test_the_ai_pattern_matches_the_word_and_not_substrings(self):
        for yes in ('AI art backlash', 'the new LLM release', 'ChatGPT answers', 'generative models'):
            self.assertTrue(report.AI_WORDS.search(yes), yes)
        for no in ('said the maid', 'rain and mail', 'Hawaii trip'):
            self.assertFalse(report.AI_WORDS.search(no), no)


if __name__ == '__main__':
    unittest.main()
