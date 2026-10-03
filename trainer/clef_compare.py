"""A small page for comparing Clef-flash's answers with Jev's, post by post.

Shows each post (text, pictures, what each model was given) next to both models' answers: top
topics with probabilities, the best subtopic path, and the ranking signals. Filter to the posts
where they disagree, sort by how much, and mark who was right; verdicts are appended to
--verdicts, one JSON line each (the last one for a post counts).

Jev's answers come from ClickHouse (jev_labels: taxonomy v1, jev-1.13.0; the main run first, then
samples, then the uncertain set). Only posts with such a row can be compared. Clef's answers are the
JSON lines clef_run.py writes. "Pull latest" copies the results file down from the labelling machine.

    python3 clef_compare.py --port 8730
    -> http://<this machine>:8730/

Standard library plus PyYAML. Reads files and ClickHouse; writes only the verdicts file and the
local copy of the results file.
"""

import argparse
import json
import math
import os
import re
import socket
import subprocess
import threading
import time
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
CH_CMD = 'clickhouse-client --user topicfeed --password "$CLICKHOUSE_PASSWORD" --database topicfeed'
JEV_RANK = {"window": 0, "sample": 1, "uncertain": 2}
VERDICTS = ("jev", "clef", "both", "neither", "skip")
URI_AT_START = re.compile(rb'^\{"uri":"((?:[^"\\]|\\.)*)"')


def clickhouse(query: str) -> str:
    return subprocess.run(["docker", "exec", "-i", "topic-feed-clickhouse", "sh", "-c", CH_CMD],
                          input=query, capture_output=True, text=True, check=True, timeout=300).stdout


def in_list(uris) -> str:
    return ",".join("'" + u.replace("\\", "\\\\").replace("'", "\\'") + "'" for u in uris)


class Store:
    def __init__(self, a):
        self.a = a
        self.jev_config_clause = f"AND label_config = '{a.jev_label_config}' " if a.jev_label_config else ""
        tax = yaml.safe_load(open(a.taxonomy))
        self.has_sub = {b["id"]: bool(b.get("subtopics")) for b in tax["broad"]}
        self.topics = sorted(self.has_sub)
        self.lock = threading.Lock()
        self.items: dict[str, dict] = {}  # uri -> item, in the order results arrived
        self.jev_checked: set[str] = set()
        self.verdicts: dict[str, str] = {}
        self.offsets: dict[str, int] = {}
        self.load_verdicts()
        self.index_posts()
        self.reload()

    # --- loading -------------------------------------------------------------------------

    def load_verdicts(self):
        if os.path.exists(self.a.verdicts):
            for line in open(self.a.verdicts):
                try:
                    v = json.loads(line)
                    self.verdicts[v["uri"]] = v["verdict"]
                except (ValueError, KeyError):
                    pass

    def index_posts(self):
        """Byte offset of every post in the export, so a post can be read without loading the file."""
        pos = 0
        with open(self.a.posts, "rb") as f:
            for line in f:
                m = URI_AT_START.match(line)
                if m:
                    self.offsets[json.loads(b'"' + m.group(1) + b'"')] = pos
                pos += len(line)

    def read_post(self, uri):
        with open(self.a.posts, "rb") as f:
            f.seek(self.offsets[uri])
            return json.loads(f.readline())

    def top_path(self, broad, paths):
        cand = dict(paths)
        for b, sub in self.has_sub.items():
            if not sub:
                cand[b] = math.sqrt(broad.get(b, 0.0))
        return max(cand.items(), key=lambda kv: kv[1]) if cand else (None, 0.0)

    def reload(self):
        with self.lock:
            fresh = {}
            if os.path.exists(self.a.results):
                for line in open(self.a.results):
                    try:
                        r = json.loads(line)
                        fresh[r["uri"]] = r
                    except (ValueError, KeyError):
                        pass  # a line cut off by a copy in progress
            new = [u for u in fresh if u not in self.items and u in self.offsets]
            for u in new:
                self.items[u] = self.make_item(fresh[u])
            self.fill_jev([u for u in new if u not in self.jev_checked])
            return len(new)

    def make_item(self, r):
        p = self.read_post(r["uri"])
        did, rkey = r["uri"][5:].split("/app.bsky.feed.post/")
        descriptions = [t for t, s in zip(p.get("image_texts") or [], p.get("image_text_sources") or []) if s == "luna"]
        ocr = [t for t, s in zip(p.get("image_texts") or [], p.get("image_text_sources") or []) if s == "ocr"]
        clef_path, clef_score = self.top_path(r["broad_probs"], r["path_scores"])
        return {
            "uri": r["uri"], "link": f"https://bsky.app/profile/{did}/post/{rkey}", "kind": p["priority"],
            "text": p["text"], "alt": p.get("media_alts") or [], "quote": p.get("quote_text") or "",
            "card": {k: p.get(f"link_{k}") for k in ("domain", "title", "description") if p.get(f"link_{k}")},
            "pipeline_desc": descriptions, "pipeline_ocr": ocr, "shas": p.get("image_shas", [])[:4], "adult": False,
            "images_shown": r.get("images", 0), "clef_raw": r,
            "clef": self.summary(r["broad_probs"], clef_path, clef_score, r["signals"], r["path_scores"]),
            "jev": None, "jev_source": None,
        }

    def summary(self, broad, path, score, signals, paths):
        top = sorted(broad.items(), key=lambda kv: -kv[1])[:3]
        subs = sorted(paths.items(), key=lambda kv: -kv[1])[:3]
        return {"top": top, "path": path, "path_score": score, "signals": signals, "subs": subs, "broad": broad}

    def fill_jev(self, uris):
        """Jev's answers (and whether the post's pictures are adult-flagged) for posts not yet looked up."""
        for i in range(0, len(uris), 800):
            chunk = uris[i:i + 800]
            lst = in_list(chunk)
            best = {}
            q = ("SELECT uri, source, broad_probs, path_scores, signals FROM jev_labels FINAL "
                 f"WHERE taxonomy_version = '{self.a.jev_version}' {self.jev_config_clause}"
                 f"AND jev_model = 'jev-1.13.0' AND uri IN ({lst}) FORMAT JSONEachRow")
            for line in clickhouse(q).splitlines():
                r = json.loads(line)
                if r["uri"] not in best or JEV_RANK.get(r["source"], 9) < JEV_RANK.get(best[r["uri"]]["source"], 9):
                    best[r["uri"]] = r
            adult = set()
            q = ("SELECT DISTINCT uri FROM post_images FINAL "
                 f"WHERE policy = 'adult_only' AND uri IN ({lst}) FORMAT TSV")
            adult = set(clickhouse(q).split())
            for u in chunk:
                self.jev_checked.add(u)
                it = self.items[u]
                it["adult"] = u in adult
                if u in best:
                    r = best[u]
                    path, score = self.top_path(r["broad_probs"], r["path_scores"])
                    it["jev"] = self.summary(r["broad_probs"], path, score, r["signals"], r["path_scores"])
                    it["jev_source"] = r["source"]
                    it["tv"] = 0.5 * sum(abs(r["broad_probs"].get(k, 0.0) - it["clef"]["broad"].get(k, 0.0))
                                         for k in set(r["broad_probs"]) | set(it["clef"]["broad"]))
                    common = set(r["signals"]) & set(it["clef"]["signals"])
                    it["sig_diff"] = max((abs(r["signals"][k] - it["clef"]["signals"][k]) for k in common), default=0.0)

    # --- queries -------------------------------------------------------------------------

    def stats(self):
        both = [it for it in self.items.values() if it["jev"]]
        n = len(both)
        tally = {v: 0 for v in VERDICTS}
        for v in self.verdicts.values():
            if v in tally:
                tally[v] += 1
        return {
            "results": len(self.items), "with_jev": n,
            "same_broad": round(100 * sum(i["jev"]["top"][0][0] == i["clef"]["top"][0][0] for i in both) / n, 1) if n else None,
            "same_path": round(100 * sum(i["jev"]["path"] == i["clef"]["path"] for i in both) / n, 1) if n else None,
            "verdicts": tally, "topics": self.topics,
        }

    def query(self, q):
        view, kind, source = q.get("view", "all"), q.get("kind", "any"), q.get("source", "any")
        topic, text, sort = q.get("topic", "any"), q.get("q", "").lower(), q.get("sort", "different")
        hide_judged = q.get("hide_judged") == "1"
        need_jev = q.get("only_jev", "1") == "1"
        out = []
        for it in self.items.values():
            j, c = it["jev"], it["clef"]
            if need_jev and not j:
                continue
            if kind != "any" and str(it["kind"]) != kind:
                continue
            if source != "any" and it["jev_source"] != source:
                continue
            if hide_judged and it["uri"] in self.verdicts:
                continue
            if topic != "any" and not ((j and j["top"][0][0] == topic) or c["top"][0][0] == topic):
                continue
            if text and text not in (it["text"] + " " + " ".join(it["alt"])).lower():
                continue
            if view != "all":
                if not j:
                    continue
                ok = {"broad": j["top"][0][0] != c["top"][0][0], "path": j["path"] != c["path"],
                      "signals": it.get("sig_diff", 0) >= 0.3, "clef_unclear": c["top"][0][0] == "unclear",
                      "jev_unclear": j["top"][0][0] == "unclear", "same": j["path"] == c["path"]}[view]
                if not ok:
                    continue
            out.append(it)
        keys = {"different": lambda i: -i.get("tv", 0), "random": lambda i: zlib.crc32(i["uri"].encode()),
                "arrival": None, "clef_unsure": lambda i: i["clef"]["top"][0][1],
                "jev_unsure": lambda i: i["jev"]["top"][0][1] if i["jev"] else 2}
        if keys.get(sort):
            out.sort(key=keys[sort])
        per, page = int(q.get("per", 20)), int(q.get("page", 0))
        shown = []
        for it in out[page * per:(page + 1) * per]:
            d = {k: v for k, v in it.items() if k != "clef_raw"}
            d["verdict"] = self.verdicts.get(it["uri"])
            d["clef"] = {k: v for k, v in it["clef"].items() if k != "broad"}
            d["jev"] = {k: v for k, v in it["jev"].items() if k != "broad"} if it["jev"] else None
            shown.append(d)
        return {"total": len(out), "page": page, "per": per, "items": shown}

    def set_verdict(self, uri, verdict):
        if uri not in self.items or verdict not in VERDICTS:
            return False
        with self.lock:
            self.verdicts[uri] = verdict
            with open(self.a.verdicts, "a") as f:
                f.write(json.dumps({"uri": uri, "verdict": verdict, "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}) + "\n")
        return True

    def sync(self):
        a = self.a
        if not a.remote:
            return {"error": "no --remote configured"}
        ssh = f"ssh -i {a.remote_key} -p {a.remote_port} -o BatchMode=yes -o IdentitiesOnly=yes -o ConnectTimeout=15"
        p = subprocess.run(["rsync", "-t", "-e", ssh, f"{a.remote}:{a.remote_file}", a.results],
                           capture_output=True, text=True, timeout=600)
        if p.returncode:
            return {"error": (p.stderr or "rsync failed")[-300:]}
        return {"new": self.reload()}


PAGE = r"""<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Clef vs Jev</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--fg:#1d2330;--mut:#667085;--line:#e4e7ec;--acc:#1570ef;--jev:#b54708;--clef:#0e7090;--bad:#b42318}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,sans-serif;color:var(--fg);background:#f8f9fb}
header{position:sticky;top:0;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px;z-index:3}
h1{font-size:16px;margin:0 0 6px}h1 small{color:var(--mut);font-weight:400}
.row{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:6px}
select,input,button{font:inherit;padding:4px 8px;border:1px solid var(--line);border-radius:6px;background:#fff}button{cursor:pointer}
button.pri{background:var(--acc);color:#fff;border-color:var(--acc)}label{color:var(--mut)}.mut{color:var(--mut)}
main{padding:14px 16px;max-width:1040px;margin:0 auto}
.card{background:#fff;border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px}
.meta{display:flex;gap:10px;flex-wrap:wrap;color:var(--mut);font-size:12px;margin-bottom:6px}.meta a{color:var(--acc);text-decoration:none}
.text{white-space:pre-wrap;word-break:break-word;font-size:15px}.alt,.quote,.cardl{color:var(--mut);font-size:13px;margin-top:4px;white-space:pre-wrap}
details{margin-top:6px;color:var(--mut);font-size:12px}
.pics{display:flex;gap:8px;flex-wrap:wrap;margin:8px 0}.pics img{max-height:220px;max-width:100%;border-radius:6px;border:1px solid var(--line)}
.pics img.adult{filter:blur(22px)}.pics img.adult.shown{filter:none}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:12px;margin-top:10px}@media(max-width:760px){.cols{grid-template-columns:1fr}}
.col{border:1px solid var(--line);border-radius:8px;padding:8px 10px}.col h3{margin:0 0 4px;font-size:13px}
.jev h3{color:var(--jev)}.clef h3{color:var(--clef)}
.path{font-weight:600}.diff{color:var(--bad)}
.bar{display:flex;align-items:center;gap:6px;font-size:12px;margin:1px 0}.bar span.n{width:150px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.bar i{display:block;height:8px;border-radius:3px;min-width:1px}.jev .bar i{background:var(--jev)}.clef .bar i{background:var(--clef)}
.verd{display:flex;gap:6px;margin-top:10px;flex-wrap:wrap;align-items:center}.verd button.on{background:var(--acc);color:#fff;border-color:var(--acc)}
.tag{background:#eef2f6;border-radius:4px;padding:0 6px}.tag.adult{background:#fee4e2;color:var(--bad)}
</style></head><body>
<header><h1>Clef-flash vs Jev <small id="sum"></small></h1>
<div class="row">
 <label>Show <select id="view"><option value="broad">different broad topic</option><option value="path">different topic path</option>
  <option value="signals">signals far apart</option><option value="clef_unclear">Clef says unclear</option><option value="jev_unclear">Jev says unclear</option>
  <option value="same">same topic path</option><option value="all">all posts</option></select></label>
 <label>Sort <select id="sort"><option value="different">most different</option><option value="random">random</option><option value="arrival">order labelled</option>
  <option value="clef_unsure">Clef least sure</option><option value="jev_unsure">Jev least sure</option></select></label>
 <label>Posts <select id="kind"><option value="any">any</option><option value="0">with pictures</option><option value="1">link card only</option><option value="2">text only</option></select></label>
 <label>Jev set <select id="source"><option value="any">any</option><option value="window">main run</option><option value="sample">sample</option><option value="uncertain">uncertain</option></select></label>
 <label>Topic <select id="topic"><option value="any">any</option></select></label>
 <input id="q" placeholder="search text" size="16">
 <label><input type="checkbox" id="hide"> hide judged</label>
</div>
<div class="row"><button class="pri" id="pull">Pull latest from the 5090</button><button id="reload">Reload</button>
 <span id="msg" class="mut"></span><span class="mut" style="margin-left:auto" id="count"></span>
 <button id="prev">&larr;</button><span id="pg" class="mut"></span><button id="next">&rarr;</button></div></header>
<main id="list"></main>
<script>
const $=id=>document.getElementById(id);let page=0,total=0,per=20;
function h(t,a,...k){const e=document.createElement(t);for(const[x,v]of Object.entries(a||{})){if(x==='class')e.className=v;else if(x.startsWith('on'))e[x]=v;else e.setAttribute(x,v)}
 for(const c of k.flat()){if(c==null||c===false)continue;e.append(c.nodeType?c:document.createTextNode(c))}return e}
const pct=p=>Math.round(p*100)+'%';
function bars(rows,maxw){return rows.map(([n,p])=>h('div',{class:'bar'},h('span',{class:'n',title:n},n),h('i',{style:`width:${Math.max(1,p*maxw)}px`}),pct(p)))}
function side(cls,title,m,other,src){
 if(!m)return h('div',{class:'col '+cls},h('h3',null,title),h('div',{class:'mut'},'no answer'));
 const diff=other&&other.path!==m.path;
 const sig=Object.keys(m.signals).sort().map(k=>{const o=other&&other.signals[k];return h('div',{class:'bar'},h('span',{class:'n'},k),h('i',{style:`width:${m.signals[k]*100}px`}),m.signals[k].toFixed(2),
   o!=null&&Math.abs(o-m.signals[k])>=.3?h('span',{class:'diff'},' ≠'):null)});
 return h('div',{class:'col '+cls},h('h3',null,title+(src?' · '+src:'')),
  h('div',{class:'path'+(diff?' diff':'')},m.path+'  ',h('span',{class:'mut'},m.path_score.toFixed(2))),
  h('div',{class:'mut'},'broad topics'),bars(m.top,110),h('div',{class:'mut'},'subtopic paths'),bars(m.subs,110),h('div',{class:'mut'},'signals'),sig)}
function card(it){
 const kinds={0:'pictures',1:'link card',2:'text only'};
 const pics=it.shas.length?h('div',{class:'pics'},it.shas.map(s=>{const i=h('img',{src:'/img/'+s,loading:'lazy',class:it.adult?'adult':''});
   if(it.adult)i.onclick=()=>i.classList.toggle('shown');return i})):null;
 const given=[];if(it.pipeline_desc.length)given.push('Jev read this description of the pictures: '+it.pipeline_desc.join(' | '));
 if(it.pipeline_ocr.length)given.push('Text found in the pictures: '+it.pipeline_ocr.join(' | '));
 given.push(it.images_shown?`Clef was shown ${it.images_shown} picture(s) and no description.`:'Clef was shown no pictures.');
 const v=(k,l)=>h('button',{class:it.verdict===k?'on':'',onclick:async()=>{await post('/api/verdict',{uri:it.uri,verdict:k});it.verdict=k;
   c.replaceWith(card(it));stats()}},l);
 const c=h('div',{class:'card'},
  h('div',{class:'meta'},h('a',{href:it.link,target:'_blank'},'open on Bluesky'),h('span',{class:'tag'},kinds[it.kind]),
    it.adult?h('span',{class:'tag adult'},'adult-flagged pictures (click to reveal)'):null,h('span',null,'Jev set: '+(it.jev_source||'none')),
    it.tv!=null?h('span',null,'difference '+pct(it.tv)):null),
  h('div',{class:'text'},it.text||'(no text)'),
  it.alt.length?h('div',{class:'alt'},'Alt text: '+it.alt.join(' | ')):null,
  Object.keys(it.card).length?h('div',{class:'cardl'},'Link: '+Object.values(it.card).join(' — ')):null,
  it.quote?h('div',{class:'quote'},'Quoted: '+it.quote):null,pics,
  h('details',null,h('summary',null,'What each model was given'),given.map(g=>h('div',null,g))),
  h('div',{class:'cols'},side('jev','Jev',it.jev,it.clef,''),side('clef','Clef-flash',it.clef,it.jev,'')),
  h('div',{class:'verd'},h('span',{class:'mut'},'Who is closer to right?'),v('jev','Jev'),v('clef','Clef'),v('both','Both fine'),v('neither','Neither'),v('skip','Not sure')));
 return c}
async function post(u,b){const r=await fetch(u,{method:'POST',body:JSON.stringify(b||{})});return r.json()}
async function stats(){const s=await (await fetch('/api/stats')).json();const t=$('topic');
 if(t.options.length===1)s.topics.forEach(x=>t.append(h('option',{value:x},x)));
 const v=s.verdicts;$('sum').textContent=`${s.results} results, ${s.with_jev} comparable · same broad topic ${s.same_broad}% · same path ${s.same_path}% · judged: Jev ${v.jev}, Clef ${v.clef}, both ${v.both}, neither ${v.neither}`}
function params(){return new URLSearchParams({view:$('view').value,sort:$('sort').value,kind:$('kind').value,source:$('source').value,topic:$('topic').value,
 q:$('q').value,hide_judged:$('hide').checked?1:0,page,per})}
async function load(){const r=await (await fetch('/api/items?'+params())).json();total=r.total;$('list').replaceChildren(...r.items.map(card));
 if(!r.items.length)$('list').append(h('p',{class:'mut'},'Nothing matches.'));
 $('count').textContent=r.total+' posts';$('pg').textContent=`page ${page+1} of ${Math.max(1,Math.ceil(total/per))}`;window.scrollTo(0,0)}
for(const id of['view','sort','kind','source','topic','hide'])$(id).onchange=()=>{page=0;load()};
let tm;$('q').oninput=()=>{clearTimeout(tm);tm=setTimeout(()=>{page=0;load()},300)};
$('prev').onclick=()=>{if(page>0){page--;load()}};$('next').onclick=()=>{if((page+1)*per<total){page++;load()}};
$('reload').onclick=async()=>{$('msg').textContent='reloading…';const r=await post('/api/reload');$('msg').textContent=r.new+' new results';await stats();load()};
$('pull').onclick=async()=>{$('msg').textContent='copying from the 5090…';const r=await post('/api/sync');
 $('msg').textContent=r.error?('failed: '+r.error):(r.new+' new results');await stats();load()};
stats().then(load);
</script></body></html>
"""


class Handler(BaseHTTPRequestHandler):
    store: Store

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
            self.send(200, PAGE, "text/html")
        elif u.path == "/api/stats":
            self.send(200, self.store.stats())
        elif u.path == "/api/items":
            q = {k: v[0] for k, v in parse_qs(u.query).items()}
            self.send(200, self.store.query(q))
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
        if self.path == "/api/verdict":
            ok = self.store.set_verdict(req.get("uri"), req.get("verdict"))
            self.send(200 if ok else 400, {"ok": ok})
        elif self.path == "/api/reload":
            self.send(200, {"new": self.store.reload()})
        elif self.path == "/api/sync":
            self.send(200, self.store.sync())
        else:
            self.send(404, {"error": "not found"})


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--port", type=int, default=8730)
    ap.add_argument("--host", default="::", help="address to listen on (default: every interface)")
    ap.add_argument("--results", default="/data/clef/clef-flash-remote.jsonl", help="Clef results (clef_run.py output)")
    ap.add_argument("--posts", default="/data/clef/full_posts.jsonl", help="clef_export.py's file")
    ap.add_argument("--images", default="/data/images")
    ap.add_argument("--verdicts", default="/data/clef/verdicts.jsonl")
    ap.add_argument("--taxonomy", default=os.path.join(HERE, "..", "taxonomy", "v1.yaml"))
    ap.add_argument("--jev-version", default="v1", help="jev_labels.taxonomy_version to show next to Clef's answers")
    ap.add_argument("--jev-label-config", help="only Jev's labels with this label_config (default: any, main run first)")
    ap.add_argument("--remote", default=os.environ.get("CLEF_BOX", ""), help="where the results are made, for 'Pull latest'")
    ap.add_argument("--remote-port", type=int, default=9305)
    ap.add_argument("--remote-key", default=os.path.expanduser("~/.ssh/id_ed25519_clef"))
    ap.add_argument("--remote-file", default="/root/clef/out/clef-flash-full.jsonl")
    a = ap.parse_args()

    t0 = time.time()
    Handler.store = Store(a)
    s = Handler.store.stats()
    print(f"loaded {s['results']} results ({s['with_jev']} with Jev answers) in {time.time() - t0:.1f}s; "
          f"serving on http://[{a.host}]:{a.port}/", flush=True)

    class Server(ThreadingHTTPServer):
        address_family = socket.AF_INET6 if ":" in a.host else socket.AF_INET

    Server((a.host, a.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
