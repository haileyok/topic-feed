"""A page for judging, side by side and blind, a model's answer against its teacher's on posts where they disagree.

Each post (mm_disagree_sample.py draws them) is shown with two answers labelled A and B: the best three broad topics and
the best two subtopic paths of each, in an order fixed at random per post. You are not told which is the model and which
is the teacher, and no probabilities are shown (a teacher's sharper or flatter list would give it away). For each post you
say which answer is better, or both are fine, or neither, or you can't tell; if you like, what the topic should have been
and a note. Verdicts are appended to <dir>/verdicts.jsonl (the last one for a post counts). Which answer was the model's
is kept in <dir>/sample.json and only used when the verdicts are analysed.

    python3 mm_eval_pair.py --port 8753 --dir /data/models/mm2/eval-disagree --model /data/models/mm2
    -> http://<this machine>:8753/

Builds on mm_eval_app.py (same posts, pictures and adult blurring). Standard library only.
"""

import argparse
import html
import json
import os
import socket
import time
from http.server import ThreadingHTTPServer
from urllib.parse import urlparse

import mm_eval_app as E

PICKS = ("a", "b", "both", "neither", "unsure")


def names(d, k):
    return [n for n, _ in sorted(d.items(), key=lambda kv: -kv[1])[:k]]


class PairStore(E.Store):
    def __init__(self, a):
        sample = json.load(open(os.path.join(a.dir, "sample.json")))["posts"]
        self.pair = {p["uri"]: p for p in sample}  # needed by make_item, which the base constructor calls
        super().__init__(a)

    def make_item(self, uri):
        item = super().make_item(uri)
        p, r = self.pair[uri], self.pred[uri]
        answers = {"model": {"broad": names(r["broad"], 3), "paths": names(r["paths"], 2)},
                   "teacher": {"broad": names(p["teacher"]["broad"], 3), "paths": names(p["teacher"]["paths"], 2)}}
        item.pop("model")
        item["answers"] = {"a": answers[p["order"][0]], "b": answers[p["order"][1]]}  # the order itself is not sent
        return item

    def set_verdict(self, req):
        uri, pick = req.get("uri"), req.get("pick")
        if uri not in self.items or pick not in PICKS:
            return False
        should_be = req.get("should_be") or ""
        if should_be and should_be not in self.broad and should_be not in self.paths:
            return False
        v = {"uri": uri, "pick": pick, "should_be": should_be, "note": str(req.get("note") or "")[:500],
             "kind": "pictures" if self.items[uri]["pictures"] else "text", "at": int(time.time())}
        with self.lock:
            with open(self.verdicts_path, "a") as f:
                f.write(json.dumps(v) + "\n")
            self.verdicts[uri] = v
        return True

    def verdict_of(self, uri):
        v = self.verdicts.get(uri)
        return {k: v[k] for k in ("pick", "should_be", "note")} if v else {}

    def stats(self):
        out = {"total": len(self.order), "judged": 0, "by": {k: {"posts": 0, "judged": 0} for k in ("text", "pictures")},
               "adult_lookup_failed": self.adult_lookup_failed, **self.sample_info}
        with self.lock:
            for u in self.order:
                b = out["by"]["pictures" if self.items[u]["pictures"] else "text"]
                b["posts"] += 1
                if u in self.verdicts:
                    b["judged"] += 1
                    out["judged"] += 1
        return out


PAGE = r"""<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Judging answers side by side</title><style>
:root{--bg:#f4f6f8;--line:#d9dee4;--mut:#5d6874;--acc:#1b6ef3;--bad:#c0362c}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,sans-serif;background:var(--bg);color:#1c2530}
header{position:sticky;top:0;z-index:5;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px}
h1{font-size:16px;margin:0 0 6px}h1 small{display:block;font-weight:400;color:var(--mut);font-size:12px;margin-top:2px}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:center;margin-top:6px}
select,input,button{font:inherit;padding:4px 8px;border:1px solid var(--line);border-radius:6px;background:#fff}button{cursor:pointer}
button:disabled,select:disabled,input:disabled{opacity:.45;cursor:not-allowed}button.pri{background:var(--acc);color:#fff;border-color:var(--acc)}
label{color:var(--mut)}.mut{color:var(--mut)}main{padding:14px 16px;max-width:900px;margin:0 auto}
.card{background:#fff;border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px}
.meta{display:flex;gap:10px;flex-wrap:wrap;color:var(--mut);font-size:12px;margin-bottom:6px}.meta a{color:var(--acc);text-decoration:none}
.text{white-space:pre-wrap;word-break:break-word;font-size:15px}.alt,.quote,.cardl{color:var(--mut);font-size:13px;margin-top:4px;white-space:pre-wrap}
details{margin-top:6px;color:var(--mut);font-size:12px}details pre{white-space:pre-wrap;word-break:break-word;margin:4px 0}
.pics{display:flex;gap:8px;flex-wrap:wrap;margin:8px 0}.pics img{max-height:260px;max-width:100%;border-radius:6px;border:2px solid var(--line)}
.pics img.adult{filter:blur(22px)}.pics img.adult.shown{filter:none}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:10px}@media(max-width:700px){.cols{grid-template-columns:1fr}}
.ans{border:1px solid var(--line);border-radius:8px;padding:8px 12px;background:#fafbfc}.ans h3{margin:0 0 4px;font-size:13px;color:var(--mut)}
.ans .top{font-size:16px;font-weight:600}.ans .sub{font-size:14px;margin-top:2px}
.tag{background:#eef2f6;border-radius:4px;padding:0 6px}.tag.adult{background:#fee4e2;color:var(--bad)}
.verd{display:flex;gap:6px;margin-top:10px;flex-wrap:wrap;align-items:center}.verd button.on{background:var(--acc);color:#fff;border-color:var(--acc)}
</style></head><body>
<header><h1>Judging answers side by side<small id="sum"></small></h1>
<div class="row"><label>Show <select id="kind"><option value="all">all posts</option><option value="text">text and link cards</option><option value="pictures">posts with pictures</option></select></label>
 <label><input type="checkbox" id="hide"> hide judged</label><button id="next" class="pri">Next unjudged</button>
 <span class="mut" style="margin-left:auto" id="count"></span><button id="prev">&larr;</button><span id="pg" class="mut"></span><button id="fwd">&rarr;</button></div>
<div class="row mut" id="by"></div></header>
<main id="list"></main>
<script>
const $=id=>document.getElementById(id);let page=0,total=0,per=10,meta=null;
function h(t,a,...k){const e=document.createElement(t);for(const[x,v]of Object.entries(a||{})){if(x==='class')e.className=v;else if(x.startsWith('on'))e[x]=v;else if(x==='disabled'){if(v)e.disabled=true}else e.setAttribute(x,v)}
 for(const c of k.flat()){if(c==null||c===false)continue;e.append(c.nodeType?c:document.createTextNode(c))}return e}
async function post(u,b){const r=await fetch(u,{method:'POST',body:JSON.stringify(b||{})});return r.json()}
async function save(it){const r=await post('/api/verdict',{uri:it.uri,...it.verdict});if(!r.ok)alert('not saved');stats()}
function answer(label,a){return h('div',{class:'ans'},h('h3',null,'Answer '+label),h('div',{class:'top'},a.broad[0]),h('div',{class:'sub'},'subtopic: '+a.paths[0]),
  h('div',{class:'mut',style:'margin-top:4px'},'next most likely: '+a.broad.slice(1).join(', ')+(a.paths[1]?' · subtopic '+a.paths[1]:'')))}
function card(it){
 it.verdict=it.verdict||{};const v=it.verdict,decided=!!v.pick;
 const pics=it.shas.length?h('div',{class:'pics'},it.shas.map(s=>{const im=h('img',{src:'/img/'+s,loading:'lazy',class:it.adult?'adult':''});
   if(it.adult)im.onclick=()=>im.classList.toggle('shown');return im})):null;
 const pick=(k,l)=>h('button',{class:v.pick===k?'on':'',onclick:async()=>{v.pick=k;await save(it);c.replaceWith(card(it))}},l);
 const sel=h('select',{disabled:!decided,onchange:async e=>{v.should_be=e.target.value;await save(it)}},h('option',{value:''},'what it should be (optional)'),
   h('optgroup',{label:'broad topic'},meta.broad.map(b=>h('option',{value:b},b))),h('optgroup',{label:'subtopic path'},meta.paths.map(p=>h('option',{value:p},p))));
 sel.value=v.should_be||'';
 const note=h('input',{placeholder:'note (optional)',size:34,value:v.note||'',disabled:!decided,onchange:async e=>{v.note=e.target.value;await save(it)}});
 const c=h('div',{class:'card'},
  h('div',{class:'meta'},h('b',null,'#'+it.number),h('a',{href:it.link,target:'_blank'},'open on Bluesky'),h('span',{class:'tag'},it.kind),
    it.adult?h('span',{class:'tag adult'},'adult-flagged pictures (click to reveal)'):null,
    it.shas.length?h('span',null,`${it.shas.length} picture(s)`):null),
  h('div',{class:'text'},it.text||'(no text)'),
  it.alt.length?h('div',{class:'alt'},'Alt text: '+it.alt.join(' | ')):null,
  Object.keys(it.card).length?h('div',{class:'cardl'},'Link: '+Object.values(it.card).join(' — ')):null,
  it.quote?h('div',{class:'quote'},'Quoted: '+it.quote):null,pics,
  h('details',null,h('summary',null,'The text one of the answerers read'),h('pre',null,it.read)),
  h('div',{class:'cols'},answer('A',it.answers.a),answer('B',it.answers.b)),
  h('div',{class:'verd'},h('span',{class:'mut'},'Which topic is better?'),pick('a','A is better'),pick('both','Both fine'),pick('b','B is better'),pick('neither','Neither'),pick('unsure',"Can't tell"),sel,note));
 return c}
async function stats(){const s=await (await fetch('/api/stats')).json();const d=s.by;
 $('sum').textContent=`${s.judged} of ${s.total} judged · ${s.description||''}`;
 $('by').textContent=`Text and link cards: ${d.text.judged} of ${d.text.posts} judged  ·  Pictures: ${d.pictures.judged} of ${d.pictures.posts} judged`+(s.adult_lookup_failed?'  ·  could not look up adult flags: all pictures are blurred':'')}
async function load(){const r=await (await fetch('/api/items?'+new URLSearchParams({kind:$('kind').value,hide_judged:$('hide').checked?1:0,page,per}))).json();
 total=r.total;$('list').replaceChildren(...r.items.map(card));if(!r.items.length)$('list').append(h('p',{class:'mut'},'Nothing to show.'));
 $('count').textContent=r.total+' posts';$('pg').textContent=`page ${page+1} of ${Math.max(1,Math.ceil(total/per))}`;window.scrollTo(0,0)}
for(const id of['kind','hide'])$(id).onchange=()=>{page=0;load()};
$('prev').onclick=()=>{if(page>0){page--;load()}};$('fwd').onclick=()=>{if((page+1)*per<total){page++;load()}};
$('next').onclick=()=>{$('hide').checked=true;page=0;load()};
fetch('/api/meta').then(r=>r.json()).then(m=>{meta=m;stats();load()});
</script></body></html>
"""


class PairHandler(E.Handler):
    title = "Judging answers side by side"

    def do_GET(self):
        if urlparse(self.path).path == "/":
            return self.send(200, PAGE.replace("Judging answers side by side", html.escape(self.title)), "text/html")
        super().do_GET()


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", type=int, default=8753)
    ap.add_argument("--host", default="::")
    ap.add_argument("--dir", required=True, help="holds sample.json and predictions.jsonl from mm_disagree_sample.py")
    ap.add_argument("--model", required=True, help="holds config.json (the topic lists)")
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl")
    ap.add_argument("--images", default="/data/images")
    ap.add_argument("--verdicts")
    ap.add_argument("--title", default="Judging answers side by side")
    a = ap.parse_args()
    PairHandler.title = a.title
    t0 = time.time()
    PairHandler.store = PairStore(a)
    s = PairHandler.store.stats()
    print(f"loaded {s['total']} posts, {s['judged']} already judged, in {time.time() - t0:.1f}s; serving on http://[{a.host}]:{a.port}/", flush=True)

    class Server(ThreadingHTTPServer):
        address_family = socket.AF_INET6 if ":" in a.host else socket.AF_INET

    Server((a.host, a.port), PairHandler).serve_forever()


if __name__ == "__main__":
    main()
