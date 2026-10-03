"""CPU-only timing of what the labeller does on the CPU for each post (no GPU, no model weights):
tokenizing the questions, preparing the picture, and reading the picture from disk."""
import json
import sys
import time

sys.path.insert(0, '/data/clef/clef-flash')
sys.path.insert(0, '/home/penguin/bluesky/topic-feed/trainer')
import yaml  # noqa: E402
import joint_schema_model as jsm  # noqa: E402
import clef_labels as cl  # noqa: E402
import clef_run as cr  # noqa: E402
from transformers import AutoProcessor  # noqa: E402

processor = AutoProcessor.from_pretrained('/data/clef/clef-flash')
tok = processor.tokenizer
tax = yaml.safe_load(open('/home/penguin/bluesky/topic-feed/taxonomy/v2.1.yaml'))

# one real picture post from part 2
row = None
for line in open('/data/clef/v2/remote/clef-v21-pictures-part2.jsonl'):
    r = json.loads(line)
    break
uri = r['uri']
for line in open('/data/clef/full_posts.jsonl'):
    if line.startswith('{"uri":"') and json.loads('"' + line[8:line.index('"', 8)] + '"') == uri:
        row = json.loads(line)
        break
print('post:', uri[-14:], '| pictures:', len(row['image_shas']), '| text chars:', len(row.get('text') or ''))


def clock(f, n=10):
    f()
    t = time.perf_counter()
    for _ in range(n):
        f()
    return (time.perf_counter() - t) / n * 1000


# 1. reading the pictures from disk
t_load = clock(lambda: cr.load_pictures('/data/images', row['image_shas']), 10)
pics = cr.load_pictures('/data/images', row['image_shas'])
print(f'1. load {len(pics)} picture(s) from disk and convert: {t_load:.0f} ms   sizes {[p.size for p in pics]}')

doc = cl.jev_post(cl.make_doc(cr.prepare(row, pics)))
state = {'posts': [doc]}
req1 = {'model': 'clef-flash', 'state': state, 'questions': cl.pass1_questions(tax), 'images': pics}

# 2. what systemone does on the CPU before the GPU: encode_record (tokenizing questions + the picture processor)
t_enc = clock(lambda: jsm.encode_record(tok, req1, processor=processor), 5)
print(f'2. encode_record, pass 1 (questions + state + picture): {t_enc:.0f} ms')
req1_nopic = dict(req1)
req1_nopic.pop('images')
t_enc_nopic = clock(lambda: jsm.encode_record(tok, req1_nopic, processor=processor), 5)
print(f'   same without the picture: {t_enc_nopic:.0f} ms  -> the picture processor costs about {t_enc - t_enc_nopic:.0f} ms')
n_tokens = len(jsm.encode_record(tok, req1, processor=processor).input_ids)

# 3. pass 2 (sub-topic questions for 3 topics)
by_id = {b['id']: b for b in tax['broad']}
q2 = {f'p0_{b}_sub': cl.pass2_question(by_id[b]) for b in ('us_politics', 'humor', 'society')}
req2 = {'model': 'clef-flash', 'state': state, 'questions': q2, 'images': pics}
t_enc2 = clock(lambda: jsm.encode_record(tok, req2, processor=processor), 5)
print(f'3. encode_record, pass 2 (3 sub-topic questions + picture): {t_enc2:.0f} ms')

# 4. the same with the tokenizer calls remembered (the question text never changes)
orig = jsm._tokens
cache = {}


def cached_tokens(tokenizer, text):
    hit = cache.get(text)
    if hit is None:
        hit = cache[text] = orig(tokenizer, text)
    return list(hit)


jsm._tokens = cached_tokens
for _ in range(2):
    jsm.encode_record(tok, req1, processor=processor)
t_enc_c = clock(lambda: jsm.encode_record(tok, req1, processor=processor), 10)
ids_same = jsm.encode_record(tok, req1, processor=processor).input_ids == jsm.encode_record(tok, req1, processor=processor).input_ids
jsm._tokens = orig
ids_orig = jsm.encode_record(tok, req1, processor=processor).input_ids
jsm._tokens = cached_tokens
ids_cached = jsm.encode_record(tok, req1, processor=processor).input_ids
jsm._tokens = orig
print(f'4. encode_record, pass 1, with tokenizer calls remembered: {t_enc_c:.0f} ms (was {t_enc:.0f} ms); identical token ids: {ids_orig == ids_cached}; {n_tokens} tokens')
n_calls = []
jsm._tokens = lambda tk, tx: (n_calls.append(1), orig(tk, tx))[1]
jsm.encode_record(tok, req1, processor=processor)
jsm._tokens = orig
print(f'   tokenizer calls per pass-1 request: {len(n_calls)}')

per_post = t_load + t_enc + t_enc2
print(f'\nCPU work per post before the GPU gets it (load + pass 1 encode + pass 2 encode): about {per_post:.0f} ms')
print(f'  of a post that takes ~{1000 / 1.53:.0f} ms in total at 1.53 posts/s ({100 * per_post / (1000 / 1.53):.0f}% of it)')
print(f'  with the tokenizer calls remembered: about {t_load + t_enc_c + t_enc2 * t_enc_c / t_enc:.0f} ms')
