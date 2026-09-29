"""Blind labeling page for a human gold set: random test-window posts, shown with no
model answers, where a person picks every acceptable topic path (and the best one).

The sample is drawn once and saved to reference/gold/<name>.jsonl, so it stays the same
across runs. Posts people already judged (reference/human) are left out. The page holds
only the post text and link: no model output, confidence, window or label source.

    uv run python gold_page.py --name v1-gold-100 --n 100
    -> /data/reports/<name>/index.html (http://192.168.88.155:8090/<name>/)
"""

import argparse
import glob
import gzip
import json
import os
import random

import yaml

import human_check

HERE = os.path.dirname(os.path.abspath(__file__))
TEST_WINDOWS = {"w21", "w22", "w23"}

PAGE = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Topic labeling · __NAME__</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--fg:#1d2330;--mut:#667085;--line:#e4e7ec;--acc:#1570ef;--ok:#067647}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,sans-serif;color:var(--fg);background:#f8f9fb}
header{position:sticky;top:0;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px;z-index:3}
h1{font-size:16px;margin:0 0 6px}h1 small{color:var(--mut);font-weight:400}
.bar{height:6px;background:#eef2f6;border-radius:3px;overflow:hidden;margin:6px 0 8px}.bar div{height:100%;background:var(--ok)}
.c{display:flex;flex-wrap:wrap;gap:8px;align-items:center}button,input,textarea{font:inherit;padding:4px 10px;border:1px solid var(--line);border-radius:6px;background:#fff}
button{cursor:pointer}button.pri{background:var(--acc);color:#fff;border-color:var(--acc)}.count{margin-left:auto;color:var(--mut)}
main{padding:14px 16px;max-width:980px;margin:0 auto}
.card{background:#fff;border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:12px}
.pmeta{display:flex;gap:10px;color:var(--mut);font-size:12px;margin-bottom:8px}.pmeta a{color:var(--acc);text-decoration:none}
.text{white-space:pre-wrap;word-break:break-word;font-size:15px}
.chosen{display:flex;flex-wrap:wrap;gap:6px;min-height:34px;align-items:center}
.chip{display:inline-flex;gap:6px;align-items:center;border:1px solid #b2ccff;background:#eff4ff;border-radius:999px;padding:3px 6px 3px 10px}
.chip.primary{background:#1570ef;color:#fff;border-color:#1570ef}.chip button{border:0;background:transparent;padding:0 4px;color:inherit}
.hint{color:var(--mut);font-size:12px}
#q{width:100%;padding:8px 12px;font-size:15px;margin-top:10px}
.res{margin-top:4px;border:1px solid var(--line);border-radius:8px;overflow:hidden}
.r{padding:6px 10px;cursor:pointer;border-top:1px solid #f2f4f7}.r:first-child{border-top:0}.r.hl{background:#eff4ff}.r.on{background:#ecfdf3}
.r .n{font-weight:600}.r .id{color:var(--mut);font-size:12px;margin-left:6px}.r .d{color:#475467;font-size:12px}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(170px,1fr));gap:6px;margin-top:10px}
.grid button{text-align:left;font-size:13px}.grid button.has{border-color:var(--ok);background:#ecfdf3}.grid button.open{border-color:var(--acc)}
.subs{margin-top:8px}
.row{display:flex;gap:10px;align-items:center;margin-top:10px;flex-wrap:wrap}.row textarea{flex:1;min-width:240px;height:34px}
label.cant{display:inline-flex;gap:6px;align-items:center}
</style></head><body>
<header><h1>Topic labeling <small>__NAME__ · __N__ posts · pick every topic that fits; ★ marks the best one</small></h1>
<div class="bar"><div id="prog"></div></div>
<div class="c"><button id="prev">◀ Prev</button><button id="next" class="pri">Next ▶</button><button id="nextun">Next unlabeled</button>
<button id="dl">Download labels</button><label><input type="file" id="load" accept=".json" style="display:none"><span style="cursor:pointer;border:1px solid var(--line);border-radius:6px;padding:4px 10px">Load labels…</span></label>
<span class="count" id="count"></span></div></header>
<main>
<div class="card"><div class="pmeta"><span id="pos"></span><a id="link" target="_blank" rel="noopener">open on bsky.app ↗</a></div><div class="text" id="text"></div></div>
<div class="card"><div class="chosen" id="chosen"></div>
<input id="q" placeholder="Search topics: type a few letters (e.g. basketball, fan art, unclear) · ↑↓ to pick · Enter adds" autocomplete="off">
<div class="res" id="res"></div>
<div class="hint" style="margin-top:8px">Or browse: pick a broad topic, then a subtopic. With the search box empty, ← and → move between posts.</div>
<div class="grid" id="grid"></div><div class="subs" id="subs"></div>
<div class="row"><label class="cant"><input type="checkbox" id="cant"> Can't judge this post (language, missing context, broken)</label>
<textarea id="note" placeholder="Optional note"></textarea></div></div>
</main>
<script>
const D=__DATA__;const KEY='gold:'+D.name;const M=JSON.parse(localStorage.getItem(KEY)||'{}');
let cur=+(localStorage.getItem(KEY+':cur')||0),hl=0,open=null;const $=s=>document.querySelector(s);
function esc(s){return String(s).replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
const P=[];for(const b of D.topics){if(b.subs.length){for(const s of b.subs)P.push({path:s.path,name:s.name,bname:b.name,desc:s.description,broad:b.id})}
 else P.push({path:b.id,name:b.name,bname:'',desc:b.description,broad:b.id})}
const byPath=Object.fromEntries(P.map(p=>[p.path,p]));
function save(){for(const u of Object.keys(M))clean(u);localStorage.setItem(KEY,JSON.stringify(M));localStorage.setItem(KEY+':cur',cur)}
function st(){const u=D.posts[cur].uri;return M[u]||(M[u]={paths:[],primary:null,cant:false,note:''})}
function labeled(u){const m=M[u];return m&&(m.paths.length>0||m.cant)}
function clean(u){const m=M[u];if(m&&!m.paths.length&&!m.cant&&!m.note)delete M[u]}
function toggle(path){const m=st();const i=m.paths.indexOf(path);if(i>=0){m.paths.splice(i,1);if(m.primary===path)m.primary=m.paths[0]||null}
 else{m.paths.push(path);if(!m.primary)m.primary=path}clean(D.posts[cur].uri);save();render()}
function label(p){const x=byPath[p];return x?(x.bname?x.bname+' › ':'')+x.name:p}
function search(q){const t=q.toLowerCase().split(/\s+/).filter(Boolean);if(!t.length)return[];
 const sc=P.map((p,i)=>{const hi=(p.path+' '+p.name+' '+p.bname).toLowerCase(),lo=p.desc.toLowerCase();let s=0;
  for(const w of t){if(hi.includes(w))s+=3;else if(lo.includes(w))s+=1;else return[-1,i]}return[s,i]}).filter(x=>x[0]>0);
 sc.sort((a,b)=>b[0]-a[0]||a[1]-b[1]);return sc.slice(0,10).map(x=>P[x[1]])}
function renderRes(){const q=$('#q').value;const r=search(q);if(hl>=r.length)hl=Math.max(0,r.length-1);const m=st();
 $('#res').style.display=r.length?'':'none';
 $('#res').innerHTML=r.map((p,i)=>'<div class="r'+(i===hl?' hl':'')+(m.paths.includes(p.path)?' on':'')+'" data-p="'+p.path+'"><span class="n">'+esc(label(p.path))+'</span><span class="id">'+esc(p.path)+'</span><div class="d">'+esc(p.desc)+'</div></div>').join('');
 for(const el of document.querySelectorAll('.r'))el.onclick=()=>{toggle(el.dataset.p);$('#q').focus()};return r}
function render(){const p=D.posts[cur],m=M[p.uri]||{paths:[],primary:null,cant:false,note:''};
 $('#pos').textContent='Post '+(cur+1)+' of '+D.posts.length+(labeled(p.uri)?' · labeled':'');$('#text').textContent=p.text;$('#link').href=p.link;
 $('#chosen').innerHTML=m.paths.length?m.paths.map(x=>'<span class="chip'+(x===m.primary?' primary':'')+'"><button data-star="'+x+'" title="mark as the best topic">'+(x===m.primary?'★':'☆')+'</button>'+esc(label(x))+'<button data-rm="'+x+'" title="remove">×</button></span>').join(''):'<span class="hint">No topics yet: search below or browse the broad topics.</span>';
 for(const el of document.querySelectorAll('[data-star]'))el.onclick=()=>{st().primary=el.dataset.star;save();render()};
 for(const el of document.querySelectorAll('[data-rm]'))el.onclick=()=>toggle(el.dataset.rm);
 const chosenB=new Set(m.paths.map(x=>byPath[x]?.broad));
 $('#grid').innerHTML=D.topics.map(b=>'<button data-b="'+b.id+'" class="'+(chosenB.has(b.id)?'has ':'')+(open===b.id?'open':'')+'" title="'+esc(b.description)+'">'+esc(b.name)+(b.subs.length?' ›':'')+'</button>').join('');
 for(const el of document.querySelectorAll('[data-b]'))el.onclick=()=>{const b=D.topics.find(x=>x.id===el.dataset.b);if(!b.subs.length){toggle(b.id);return}open=open===b.id?null:b.id;render()};
 const ob=D.topics.find(x=>x.id===open);
 $('#subs').innerHTML=ob?'<div class="res">'+ob.subs.map(s=>'<div class="r'+(m.paths.includes(s.path)?' on':'')+'" data-p="'+s.path+'"><span class="n">'+esc(s.name)+'</span><span class="id">'+esc(s.path)+'</span><div class="d">'+esc(s.description)+'</div></div>').join('')+'</div>':'';
 for(const el of document.querySelectorAll('#subs .r'))el.onclick=()=>toggle(el.dataset.p);
 $('#cant').checked=!!m.cant;$('#note').value=m.note||'';
 const n=D.posts.filter(x=>labeled(x.uri)).length;$('#count').textContent=n+' of '+D.posts.length+' labeled';$('#prog').style.width=(100*n/D.posts.length)+'%';
 renderRes()}
function go(i){cur=Math.max(0,Math.min(D.posts.length-1,i));open=null;$('#q').value='';hl=0;save();render();window.scrollTo(0,0);$('#q').focus()}
$('#q').oninput=()=>{hl=0;renderRes()};
$('#q').onkeydown=e=>{const r=search($('#q').value);
 if(e.key==='ArrowDown'){hl=Math.min(hl+1,r.length-1);renderRes();e.preventDefault()}
 else if(e.key==='ArrowUp'){hl=Math.max(hl-1,0);renderRes();e.preventDefault()}
 else if(e.key==='Enter'&&r.length){toggle(r[hl].path);$('#q').value='';hl=0;renderRes();e.preventDefault()}
 else if(e.key==='Escape'){$('#q').value='';renderRes()}
 else if(!$('#q').value&&e.key==='ArrowRight'){go(cur+1);e.preventDefault()}
 else if(!$('#q').value&&e.key==='ArrowLeft'){go(cur-1);e.preventDefault()}};
$('#cant').onchange=e=>{st().cant=e.target.checked;clean(D.posts[cur].uri);save();render()};
$('#note').oninput=e=>{st().note=e.target.value;clean(D.posts[cur].uri);save()};
$('#prev').onclick=()=>go(cur-1);$('#next').onclick=()=>go(cur+1);
$('#nextun').onclick=()=>{for(let k=1;k<=D.posts.length;k++){const i=(cur+k)%D.posts.length;if(!labeled(D.posts[i].uri)){go(i);return}}};
$('#dl').onclick=()=>{save();const out=D.posts.filter(p=>M[p.uri]&&(labeled(p.uri)||M[p.uri].note)).map(p=>({uri:p.uri,acceptable:M[p.uri].paths,primary:M[p.uri].primary,
  cant_judge:!!M[p.uri].cant,note:M[p.uri].note||'',text:p.text}));
 const a=document.createElement('a');a.href=URL.createObjectURL(new Blob([JSON.stringify({gold:D.name,taxonomy:D.taxonomy,labels:out},null,1)],{type:'application/json'}));
 a.download='gold-'+D.name+'.json';a.click()};
$('#load').onchange=async e=>{const f=e.target.files[0];if(!f)return;const doc=JSON.parse(await f.text());
 if(doc.gold!==D.name&&!confirm('This file is for "'+doc.gold+'", not "'+D.name+'". Load anyway?'))return;
 let n=0;for(const l of doc.labels||[]){M[l.uri]={paths:l.acceptable||[],primary:l.primary||null,cant:!!l.cant_judge,note:l.note||''};n++}
 save();render();alert('Loaded labels for '+n+' posts.')};
render();$('#q').focus();
</script></body></html>
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--name", default="v1-gold-100")
    ap.add_argument("--n", type=int, default=100)
    ap.add_argument("--export", default="/data/exports/v1-final", help="export to draw test-window posts from")
    ap.add_argument("--taxonomy", default="../taxonomy/v1.yaml")
    ap.add_argument("--seed", type=int, default=7)
    a = ap.parse_args()

    sample_path = os.path.join(HERE, "..", "reference", "gold", f"{a.name}.jsonl")
    if os.path.exists(sample_path):
        posts = [json.loads(line) for line in open(sample_path)]
        print(f"reusing the saved sample {sample_path} ({len(posts)} posts)")
    else:
        judged = {u for u, _ in human_check.judgments(sorted(glob.glob(os.path.join(HERE, "..", "reference", "human", "*.json"))))}
        pool = []
        with gzip.open(f"{a.export}/labels.jsonl.gz", "rt") as f:
            for line in f:
                r = json.loads(line)
                if r["window_id"] in TEST_WINDOWS and r["uri"] not in judged:
                    pool.append({"uri": r["uri"], "text": r["model_input"], "window_id": r["window_id"]})
        pool.sort(key=lambda r: r["uri"])
        posts = random.Random(a.seed).sample(pool, a.n)
        os.makedirs(os.path.dirname(sample_path), exist_ok=True)
        with open(sample_path, "w") as f:
            for p in posts:
                f.write(json.dumps(p, ensure_ascii=False) + "\n")
        print(f"drew {len(posts)} posts from {len(pool)} test-window posts (excluding {len(judged)} already judged); saved {sample_path}")

    tax = yaml.safe_load(open(a.taxonomy))
    topics = [{"id": b["id"], "name": b["name"], "description": b["description"],
               "subs": [{"path": f"{b['id']}/{s['id']}", "name": s["name"], "description": s["description"]}
                        for s in b.get("subtopics") or []]} for b in tax["broad"]]

    def link(uri):
        p = uri.replace("at://", "").split("/")
        return f"https://bsky.app/profile/{p[0]}/post/{p[2]}" if len(p) == 3 else ""

    # Blind: the page gets only each post's text and link.
    page_posts = [{"uri": p["uri"], "text": p["text"], "link": link(p["uri"])} for p in posts]
    data = json.dumps({"name": a.name, "taxonomy": tax["version"], "topics": topics, "posts": page_posts},
                      ensure_ascii=False).replace("</", "<\\/")
    out_dir = f"/data/reports/{a.name}"
    os.makedirs(out_dir, exist_ok=True)
    with open(f"{out_dir}/index.html", "w") as f:
        f.write(PAGE.replace("__DATA__", data).replace("__NAME__", a.name).replace("__N__", str(len(posts))))
    print(f"wrote {out_dir}/index.html")


if __name__ == "__main__":
    main()
