"""A small page for checking the picture-capable student by hand, post by post.

Shows posts drawn at random from the student's held-out test set (mm_eval_sample.py), each with the
student's answer: the best broad topic and subtopic path with probabilities, the runners-up, the tone
and, for posts with pictures, its meme call (the meme answer was only ever trained on picture
posts). The teachers' answers are not shown, so the judging is blind to them. For each post you say
whether the topic is right, acceptable, wrong or can't be told, optionally what it should have been,
and whether the meme call was right. Verdicts are appended to <dir>/verdicts.jsonl, one JSON line
each (the last one for a post counts).

    python3 mm_eval_app.py --port 8750
    -> http://<this machine>:8750/

Standard library only. Reads the sample, the student's answers, the post export, the picture
archive, and ClickHouse (only to find adult-flagged pictures, which are blurred until clicked).
Writes only the verdicts file.
"""

import argparse
import html
import json
import os
import re
import socket
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

CH_CMD = 'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed'
URI_AT_START = re.compile(rb'^\{"uri":"((?:[^"\\]|\\.)*)"')
TOPIC_VERDICTS = ("right", "acceptable", "wrong", "unsure")
MEME_VERDICTS = ("right", "wrong")
KINDS = {0: "pictures", 1: "link card", 2: "text only"}


def clickhouse(query: str) -> str:
    return subprocess.run(["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c", CH_CMD],
                          input=query, capture_output=True, text=True, check=True, timeout=120).stdout


def in_list(uris) -> str:
    return ",".join("'" + u.replace("\\", "\\\\").replace("'", "\\'") + "'" for u in uris)


class Store:
    def __init__(self, a):
        self.a = a
        sample = json.load(open(os.path.join(a.dir, "sample.json")))
        self.order = [p["uri"] for p in sample["posts"]]
        self.sample_info = {k: sample[k] for k in ("seed", "n", "test_size")}
        if sample.get("description"):  # a batch that was not drawn at random says what it is instead
            self.sample_info["description"] = sample["description"]
        cfg = json.load(open(os.path.join(a.model, "config.json")))
        self.broad, self.paths = cfg["broad"], cfg["paths"]
        self.pred = {}
        for line in open(os.path.join(a.dir, "predictions.jsonl")):
            r = json.loads(line)
            self.pred[r["uri"]] = r
        missing = [u for u in self.order if u not in self.pred]
        if missing:
            raise SystemExit(f"{len(missing)} drawn posts have no answer from the student, e.g. {missing[0]}")
        self.posts = self.read_posts(set(self.order))
        self.adult, self.adult_lookup_failed = self.find_adult()
        self.lock = threading.Lock()
        self.verdicts: dict[str, dict] = {}
        self.verdicts_path = os.path.join(a.dir, "verdicts.jsonl") if not a.verdicts else a.verdicts
        self.load_verdicts()
        self.items = {u: self.make_item(u) for u in self.order}

    # --- loading -------------------------------------------------------------------------

    def read_posts(self, want):
        found = {}
        with open(self.a.posts, "rb") as f:
            for line in f:
                m = URI_AT_START.match(line)
                if m:
                    uri = json.loads(b'"' + m.group(1) + b'"')
                    if uri in want:
                        found[uri] = json.loads(line)
        missing = want - set(found)
        if missing:
            raise SystemExit(f"{len(missing)} drawn posts are not in {self.a.posts}, e.g. {sorted(missing)[0]}")
        return found

    def find_adult(self):
        """Posts whose pictures are adult-flagged. If ClickHouse cannot be asked, every picture is treated as flagged."""
        try:
            rows = clickhouse("SELECT DISTINCT uri FROM post_images FINAL WHERE policy = 'adult_only' "
                              f"AND uri IN ({in_list(self.order)}) FORMAT TSV")
            return set(rows.split()), False
        except (subprocess.SubprocessError, OSError):
            return set(self.order), True

    def load_verdicts(self):
        if os.path.exists(self.verdicts_path):
            for line in open(self.verdicts_path):
                try:
                    v = json.loads(line)
                    self.verdicts[v["uri"]] = v
                except (ValueError, KeyError):
                    pass

    def make_item(self, uri):
        p, r = self.posts[uri], self.pred[uri]
        did, rkey = uri[5:].split("/app.bsky.feed.post/")
        # Only posts the student saw pictures for show them: link-card previews were never part of its input.
        shas = p.get("image_shas", [])[:4] if r["images_shown"] > 0 else []
        top = lambda d, k: sorted(d.items(), key=lambda kv: -kv[1])[:k]  # noqa: E731
        signals = dict(r.get("signals") or {})  # a model that saved only topic probabilities has none
        if not shas:
            signals.pop("meme", None)  # the meme answer was trained on picture posts only
        return {
            "uri": uri, "link": f"https://bsky.app/profile/{did}/post/{rkey}", "kind": KINDS.get(p.get("priority"), "text only"),
            "text": p.get("text", ""), "alt": p.get("media_alts") or [], "quote": p.get("quote_text") or "",
            "card": {k: p.get(f"link_{k}") for k in ("domain", "title", "description") if p.get(f"link_{k}")},
            "shas": shas, "adult": uri in self.adult, "pictures": bool(shas), "images_shown": r["images_shown"],
            "read": r["model_input"],
            "model": {"broad": top(r["broad"], 3), "paths": top(r["paths"], 3), "signals": signals, "tone": top(r.get("tone") or {}, 2),
                      "meme": (r.get("signals") or {}).get("meme") if shas else None},
        }

    # --- verdicts ------------------------------------------------------------------------

    def set_verdict(self, req):
        uri, topic = req.get("uri"), req.get("topic")
        if uri not in self.items or topic not in TOPIC_VERDICTS:
            return False
        should_be = req.get("should_be") or ""
        if should_be and should_be not in self.broad and should_be not in self.paths:
            return False
        meme = req.get("meme") or ""
        if meme and (meme not in MEME_VERDICTS or not self.items[uri]["pictures"]):
            return False
        note = str(req.get("note") or "")[:500]
        v = {"uri": uri, "topic": topic, "should_be": should_be, "meme": meme, "note": note,
             "kind": "pictures" if self.items[uri]["pictures"] else "text", "at": int(time.time())}
        with self.lock:
            with open(self.verdicts_path, "a") as f:
                f.write(json.dumps(v) + "\n")
            self.verdicts[uri] = v
        return True

    def verdict_of(self, uri):
        v = self.verdicts.get(uri)
        return {k: v[k] for k in ("topic", "should_be", "meme", "note")} if v else {}

    def stats(self):
        out = {"total": len(self.order), "judged": 0, "by": {}, "meme": {"right": 0, "wrong": 0},
               "adult_lookup_failed": self.adult_lookup_failed, **self.sample_info}
        for kind in ("text", "pictures"):
            out["by"][kind] = {"posts": 0, "judged": 0, **{t: 0 for t in TOPIC_VERDICTS}}
        with self.lock:
            for u in self.order:
                kind = "pictures" if self.items[u]["pictures"] else "text"
                b = out["by"][kind]
                b["posts"] += 1
                v = self.verdicts.get(u)
                if v:
                    out["judged"] += 1
                    b["judged"] += 1
                    b[v["topic"]] += 1
                    if v.get("meme") in out["meme"]:
                        out["meme"][v["meme"]] += 1
        return out

    def query(self, q):
        kind, hide = q.get("kind", "all"), q.get("hide_judged") == "1"
        page, per = max(int(q.get("page", 0)), 0), min(max(int(q.get("per", 10)), 1), 50)
        uris = [u for u in self.order
                if (kind == "all" or (kind == "pictures") == self.items[u]["pictures"])
                and not (hide and u in self.verdicts)]
        chunk = uris[page * per:(page + 1) * per]
        return {"total": len(uris), "items": [{**self.items[u], "verdict": self.verdict_of(u), "number": self.order.index(u) + 1} for u in chunk]}


PAGE = r"""<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Checking the new model</title><style>
:root{--bg:#f4f6f8;--line:#d9dee4;--mut:#5d6874;--acc:#1b6ef3;--mod:#7a3fd1;--bad:#c0362c;--ok:#18794e}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,sans-serif;background:var(--bg);color:#1c2530}
header{position:sticky;top:0;z-index:5;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px}
h1{font-size:16px;margin:0 0 6px}h1 small{font-weight:400;color:var(--mut);font-size:12px;margin-left:8px}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:center;margin-top:6px}
select,input,button{font:inherit;padding:4px 8px;border:1px solid var(--line);border-radius:6px;background:#fff}button{cursor:pointer}
button:disabled,select:disabled,input:disabled{opacity:.45;cursor:not-allowed}button.pri{background:var(--acc);color:#fff;border-color:var(--acc)}
label{color:var(--mut)}.mut{color:var(--mut)}main{padding:14px 16px;max-width:900px;margin:0 auto}
.card{background:#fff;border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px}
.meta{display:flex;gap:10px;flex-wrap:wrap;color:var(--mut);font-size:12px;margin-bottom:6px}.meta a{color:var(--acc);text-decoration:none}
.text{white-space:pre-wrap;word-break:break-word;font-size:15px}.alt,.quote,.cardl{color:var(--mut);font-size:13px;margin-top:4px;white-space:pre-wrap}
details{margin-top:6px;color:var(--mut);font-size:12px}details pre{white-space:pre-wrap;word-break:break-word;margin:4px 0}
.pics{display:flex;gap:8px;flex-wrap:wrap;margin:8px 0}.pics img{max-height:240px;max-width:100%;border-radius:6px;border:2px solid var(--line)}
.pics img.seen{border-color:var(--mod)}.pics img.adult{filter:blur(22px)}.pics img.adult.shown{filter:none}
.answer{border:1px solid var(--mod);border-radius:8px;padding:8px 10px;margin-top:10px;background:#faf7ff}
.answer .path{font-weight:600;font-size:15px}.answer .path small{font-weight:400;color:var(--mut)}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:6px}@media(max-width:700px){.cols{grid-template-columns:1fr}}
.bar{display:flex;align-items:center;gap:6px;font-size:12px;margin:1px 0}.bar span.n{width:170px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.bar i{display:block;height:8px;border-radius:3px;min-width:1px;background:var(--mod)}
.tag{background:#eef2f6;border-radius:4px;padding:0 6px}.tag.adult{background:#fee4e2;color:var(--bad)}.tag.sig{background:#ece4fb}
.verd{display:flex;gap:6px;margin-top:10px;flex-wrap:wrap;align-items:center}
.verd button.on{color:#fff}.verd button.on.right{background:var(--ok);border-color:var(--ok)}.verd button.on.acceptable{background:#5a8f1c;border-color:#5a8f1c}
.verd button.on.wrong{background:var(--bad);border-color:var(--bad)}.verd button.on.unsure{background:#6b7480;border-color:#6b7480}
</style></head><body>
<header><h1>Checking the new model <small id="sum"></small></h1>
<div class="row"><label>Show <select id="kind"><option value="all">all posts</option><option value="text">text and link cards</option><option value="pictures">posts with pictures</option></select></label>
 <label><input type="checkbox" id="hide"> hide judged</label><button id="next" class="pri">Next unjudged</button>
 <span class="mut" style="margin-left:auto" id="count"></span><button id="prev">&larr;</button><span id="pg" class="mut"></span><button id="fwd">&rarr;</button></div>
<div class="row mut" id="by"></div></header>
<main id="list"></main>
<script>
const $=id=>document.getElementById(id);let page=0,total=0,per=10,meta=null;
function h(t,a,...k){const e=document.createElement(t);for(const[x,v]of Object.entries(a||{})){if(x==='class')e.className=v;else if(x.startsWith('on'))e[x]=v;else if(x==='disabled'){if(v)e.disabled=true}else e.setAttribute(x,v)}
 for(const c of k.flat()){if(c==null||c===false)continue;e.append(c.nodeType?c:document.createTextNode(c))}return e}
const pct=p=>Math.round(p*100)+'%';
const bars=rows=>rows.map(([n,p])=>h('div',{class:'bar'},h('span',{class:'n',title:n},n),h('i',{style:`width:${Math.max(1,p*140)}px`}),pct(p)));
async function post(u,b){const r=await fetch(u,{method:'POST',body:JSON.stringify(b||{})});return r.json()}
async function save(it){const r=await post('/api/verdict',{uri:it.uri,...it.verdict});if(!r.ok)alert('not saved');stats()}
function card(it){
 it.verdict=it.verdict||{};const v=it.verdict,m=it.model,decided=!!v.topic;
 const pics=it.shas.length?h('div',{class:'pics'},it.shas.map((s,i)=>{const seen=i<it.images_shown;
   const im=h('img',{src:'/img/'+s,loading:'lazy',class:(it.adult?'adult ':'')+(seen?'seen':''),title:seen?'the model saw this picture':'the model did not see this picture'});
   if(it.adult)im.onclick=()=>im.classList.toggle('shown');return im})):null;
 const topic=(k,l)=>h('button',{class:(v.topic===k?'on ':'')+k,onclick:async()=>{v.topic=k;await save(it);c.replaceWith(card(it))}},l);
 const memeBtn=(k,l)=>h('button',{class:v.meme===k?'on '+(k==='right'?'right':'wrong'):'',disabled:!decided,onclick:async()=>{v.meme=k;await save(it);c.replaceWith(card(it))}},l);
 const sel=h('select',{disabled:!decided,onchange:async e=>{v.should_be=e.target.value;await save(it)}},h('option',{value:''},'what it should be (optional)'),
   h('optgroup',{label:'broad topic'},meta.broad.map(b=>h('option',{value:b},b))),h('optgroup',{label:'subtopic path'},meta.paths.map(p=>h('option',{value:p},p))));
 sel.value=v.should_be||'';
 const note=h('input',{placeholder:'note (optional)',size:34,value:v.note||'',disabled:!decided,onchange:async e=>{v.note=e.target.value;await save(it)}});
 const sig=Object.entries(m.signals).filter(([k,p])=>p>=.5&&k!=='meme').map(([k,p])=>h('span',{class:'tag sig'},k+' '+pct(p)));
 const c=h('div',{class:'card'},
  h('div',{class:'meta'},h('b',null,'#'+it.number),h('a',{href:it.link,target:'_blank'},'open on Bluesky'),h('span',{class:'tag'},it.kind),
    it.adult?h('span',{class:'tag adult'},'adult-flagged pictures (click to reveal)'):null,
    it.shas.length?h('span',null,`the model saw ${it.images_shown} of ${it.shas.length} picture(s) (outlined)`):null),
  h('div',{class:'text'},it.text||'(no text)'),
  it.alt.length?h('div',{class:'alt'},'Alt text: '+it.alt.join(' | ')):null,
  Object.keys(it.card).length?h('div',{class:'cardl'},'Link: '+Object.values(it.card).join(' — ')):null,
  it.quote?h('div',{class:'quote'},'Quoted: '+it.quote):null,pics,
  h('details',null,h('summary',null,'The text the model read'),h('pre',null,it.read)),
  h('div',{class:'answer'},
   h('div',{class:'path'},m.broad[0][0]+'  →  '+m.paths[0][0]+'  ',h('small',null,'topic '+pct(m.broad[0][1])+' · subtopic '+pct(m.paths[0][1]))),
   h('div',{class:'cols'},h('div',null,h('div',{class:'mut'},'broad topics'),bars(m.broad)),h('div',null,h('div',{class:'mut'},'subtopic paths'),bars(m.paths))),
   h('div',{class:'mut',style:'margin-top:6px'},'tone: '+m.tone.map(([n,p])=>n+' '+pct(p)).join(', ')),
   sig.length?h('div',{style:'margin-top:4px'},h('span',{class:'mut'},'signals at 50% or more: '),sig):null,
   m.meme!=null?h('div',{style:'margin-top:4px'},h('span',{class:'mut'},'meme call: '),h('b',null,m.meme>=.5?'a meme':'not a meme'),' ('+(m.meme*100).toFixed(1)+'%; the line is 50%)'):null),
  h('div',{class:'verd'},h('span',{class:'mut'},'Is the topic right?'),topic('right','Right'),topic('acceptable','Acceptable'),topic('wrong','Wrong'),topic('unsure',"Can't tell"),sel,note),
  m.meme!=null?h('div',{class:'verd'},h('span',{class:'mut'},'Is the meme call right?'),memeBtn('right','Yes'),memeBtn('wrong','No')):null);
 return c}
async function stats(){const s=await (await fetch('/api/stats')).json();const d=s.by;
 const rate=b=>{const n=b.right+b.acceptable+b.wrong;return n?`${b.right} right, ${b.acceptable} acceptable, ${b.wrong} wrong`+(b.unsure?`, ${b.unsure} can't tell`:''):'none judged yet'};
 $('sum').textContent=`${s.judged} of ${s.total} judged · `+(s.description||`drawn at random (seed ${s.seed}) from ${s.test_size} posts the model never trained on`);
 $('by').textContent=`Text and link cards (${d.text.judged}/${d.text.posts}): ${rate(d.text)}  ·  Pictures (${d.pictures.judged}/${d.pictures.posts}): ${rate(d.pictures)}`+
   (s.meme.right+s.meme.wrong?`  ·  Meme calls: ${s.meme.right} right, ${s.meme.wrong} wrong`:'')+(s.adult_lookup_failed?'  ·  could not look up adult flags: all pictures are blurred':'')}
async function load(){const r=await (await fetch('/api/items?'+new URLSearchParams({kind:$('kind').value,hide_judged:$('hide').checked?1:0,page,per}))).json();
 total=r.total;$('list').replaceChildren(...r.items.map(card));if(!r.items.length)$('list').append(h('p',{class:'mut'},'Nothing to show.'));
 $('count').textContent=r.total+' posts';$('pg').textContent=`page ${page+1} of ${Math.max(1,Math.ceil(total/per))}`;window.scrollTo(0,0)}
for(const id of['kind','hide'])$(id).onchange=()=>{page=0;load()};
$('prev').onclick=()=>{if(page>0){page--;load()}};$('fwd').onclick=()=>{if((page+1)*per<total){page++;load()}};
$('next').onclick=()=>{$('hide').checked=true;page=0;load()};
fetch('/api/meta').then(r=>r.json()).then(m=>{meta=m;stats();load()});
</script></body></html>
"""


class Handler(BaseHTTPRequestHandler):
    store: Store
    title = "Checking the new model"

    def log_message(self, *args):
        pass

    def send(self, code, body, ctype="application/json"):
        data = body if isinstance(body, bytes) else (body if isinstance(body, str) else json.dumps(body)).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype + ("; charset=utf-8" if ctype.startswith("text") or "json" in ctype else ""))
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store" if ctype != "image/jpeg" else "max-age=86400")
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/":
            self.send(200, PAGE.replace("Checking the new model", html.escape(self.title)), "text/html")
        elif u.path == "/api/meta":
            self.send(200, {"broad": self.store.broad, "paths": self.store.paths})
        elif u.path == "/api/stats":
            self.send(200, self.store.stats())
        elif u.path == "/api/items":
            try:
                q = {k: v[0] for k, v in parse_qs(u.query).items()}
                self.send(200, self.store.query(q))
            except ValueError:
                self.send(400, {"error": "bad query"})
        elif u.path.startswith("/img/"):
            sha = u.path[5:]
            if not re.fullmatch(r"[0-9a-f]{64}", sha):
                return self.send(404, {"error": "not found"})
            path = os.path.join(self.store.a.images, "1000", sha[:2], sha + ".jpg")
            if not os.path.exists(path):
                return self.send(404, {"error": "no such picture here"})
            self.send(200, open(path, "rb").read(), "image/jpeg")
        else:
            self.send(404, {"error": "not found"})

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        try:
            req = json.loads(body or b"{}")
        except ValueError:
            return self.send(400, {"error": "bad json"})
        if self.path == "/api/verdict" and isinstance(req, dict):
            ok = self.store.set_verdict(req)
            self.send(200 if ok else 400, {"ok": ok})
        else:
            self.send(404, {"error": "not found"})


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", type=int, default=8750)
    ap.add_argument("--host", default="::", help="address to listen on (default: every interface)")
    ap.add_argument("--dir", default="/data/models/mm1/eval", help="holds sample.json and predictions.jsonl")
    ap.add_argument("--model", default="/data/models/mm1", help="holds config.json")
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl")
    ap.add_argument("--images", default="/data/images")
    ap.add_argument("--verdicts", help="where verdicts are written (default: <dir>/verdicts.jsonl)")
    ap.add_argument("--title", default="Checking the new model", help="heading of the page, to tell batches apart")
    a = ap.parse_args()
    Handler.title = a.title

    t0 = time.time()
    Handler.store = Store(a)
    s = Handler.store.stats()
    print(f"loaded {s['total']} posts, {s['judged']} already judged, in {time.time() - t0:.1f}s; "
          f"serving on http://[{a.host}]:{a.port}/", flush=True)

    class Server(ThreadingHTTPServer):
        address_family = socket.AF_INET6 if ":" in a.host else socket.AF_INET

    Server((a.host, a.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
