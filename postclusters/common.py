"""Shared settings: where files go, how to reach ClickHouse (read-only use) and the LLM gateway."""
import hashlib
import os
import pathlib
import re

import clickhouse_connect

HERE = pathlib.Path(__file__).parent
DATA = pathlib.Path(os.environ.get('POSTCLUSTERS_DATA', HERE / 'data'))
NSHARDS = 16  # posts are split by hash into this many shards, so a sample is whole shards


def load_env():
    """Read ~/.config/topic-feed/env (CLICKHOUSE_PASSWORD, TYPESAFE_API_KEY) into the environment."""
    p = pathlib.Path.home() / '.config/topic-feed/env'
    if p.exists():
        for line in p.read_text().splitlines():
            line = line.strip()
            if line and not line.startswith('#') and '=' in line:
                k, v = line.split('=', 1)
                os.environ.setdefault(k.strip(), v.strip().strip('"\''))


def clickhouse():
    load_env()
    return clickhouse_connect.get_client(
        host=os.environ.get('CLICKHOUSE_HOST', '127.0.0.1'),
        port=int(os.environ.get('CLICKHOUSE_PORT', '8123')),
        username=os.environ.get('CLICKHOUSE_USER', 'topicfeed'),
        password=os.environ['CLICKHOUSE_PASSWORD'],
        database=os.environ.get('CLICKHOUSE_DB', 'topicfeed'),
        settings={'max_execution_time': 900, 'max_bytes_before_external_group_by': 8 << 30},
    )


def shard_list(spec: str, nshards: int = NSHARDS):
    """'0-3' -> [0, 1, 2, 3]; '0,5,9' -> [0, 5, 9]; 'all' -> every shard."""
    if spec == 'all':
        return list(range(nshards))
    out = []
    for part in spec.split(','):
        if '-' in part:
            a, b = part.split('-')
            out += list(range(int(a), int(b) + 1))
        else:
            out.append(int(part))
    bad = [s for s in out if not 0 <= s < nshards]
    if bad:
        raise ValueError(f'shards {bad} are outside 0..{nshards - 1}')
    return out


def doc_fingerprint(docs) -> str:
    """A hash of the documents in order. embed.py stores it next to a shard's embeddings and cluster.py checks it,
    so that embeddings can never be paired with a differently ordered (re-pulled) list of posts."""
    h = hashlib.sha1()
    for d in docs:
        h.update(d.encode('utf-8', 'replace'))
        h.update(b'\x00')
    return h.hexdigest()


# Scripts the English-language posts the pipeline keeps are not written in: a post that is mostly these is
# in another language, and the embeddings would group it by language, not by subject.
NON_LATIN = re.compile('[\u0400-\u04ff\u0590-\u06ff\u0900-\u0dff\u0e00-\u0e7f\u3040-\u30ff\u3400-\u4dbf\u4e00-\u9fff\uac00-\ud7af]')


def non_latin_share(doc: str) -> float:
    return len(NON_LATIN.findall(doc)) / max(len(doc), 1)
