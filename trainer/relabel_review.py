"""Review page for an LLM relabel: Jev's original label, the LLM's relabel, and an even
blend of the two, side by side, with buttons to mark which answers are right.

Samples posts from the relabel (random, plus posts the LLM moved into --focus-broad),
since a page with every relabeled post would be too large.

    uv run python relabel_review.py --name v1-luna-le05
    -> /data/reports/<name>-review/index.html (http://192.168.88.155:8090/<name>-review/)
"""

import argparse
import gzip
import json
import os
import random

import yaml

PAGE = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Relabel review · __NAME__</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--fg:#1d2330;--mut:#667085;--line:#e4e7ec;--acc:#1570ef}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,sans-serif;color:var(--fg);background:#f8f9fb}
header{position:sticky;top:0;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px;z-index:2}
h1{font-size:16px;margin:0 0 6px}h1 small{color:var(--mut);font-weight:400}
.stats{font-size:12px;color:var(--mut);margin-bottom:8px}.stats b{color:var(--fg)}
.c{display:flex;flex-wrap:wrap;gap:8px;align-items:center}select,button{font:inherit;padding:4px 8px;border:1px solid var(--line);border-radius:6px;background:#fff}
button{cursor:pointer}.count{margin-left:auto;color:var(--mut)}
main{padding:12px 16px;max-width:1250px;margin:0 auto}
.card{background:#fff;border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin-bottom:10px}
.text{white-space:pre-wrap;word-break:break-word;margin-bottom:8px}
.cols{display:grid;grid-template-columns:1fr 1fr 1fr;gap:8px;font-size:13px}
.col{border:1px solid #f2f4f7;border-radius:6px;padding:6px 8px;background:#fcfcfd}.col b{font-size:12px;color:var(--mut)}
.col.same{background:#ecfdf3;border-color:#abefc6}.col.blend{background:#f5f8ff;border-color:#d1e0ff}
.path{font-weight:600}.p{color:var(--mut)}.why{color:#475467;font-style:italic;margin-top:3px}
.meta{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:8px;font-size:12px;color:var(--mut)}
.v button{font-size:12px;padding:2px 8px}.v button.on{background:#1570ef;color:#fff;border-color:#1570ef}
.tag{border:1px solid var(--line);border-radius:999px;padding:0 8px}a{color:var(--acc);text-decoration:none}
</style></head><body>
<header><h1>Relabel review <small>__NAME__ · posts where Jev's confidence was ≤ __MAXC__ · Jev, __LLM__, and an even blend of the two</small></h1>
<div class="stats" id="stats"></div>
<div class="c">
<select id="sample"><option value="">Both samples</option><option value="random">Random sample</option><option value="focus">Moved to __FOCUS__</option></select>
<select id="agree"><option value="">Any agreement</option><option value="same">Same broad topic</option><option value="diff">Different broad topic</option></select>
<select id="jb"><option value="">Any Jev topic</option></select>
<select id="lb"><option value="">Any __LLM__ topic</option></select>
<select id="rev"><option value="">Any review state</option><option value="none">Not reviewed</option><option value="done">Reviewed</option></select>
<button id="dl">Download marks</button><span class="count" id="count"></span></div></header>
<main id="list"></main>
<script>
const D=__DATA__;const KEY='relabelmarks:'+D.name;const M=JSON.parse(localStorage.getItem(KEY)||'{}');
const st={sample:'',agree:'',jb:'',lb:'',rev:''};const $=s=>document.querySelector(s);
function esc(s){return String(s).replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
for(const [id,k] of [['jb','jev'],['lb','llm']]){const s=[...new Set(D.rows.map(r=>r[k].broad))].sort();
 $('#'+id).innerHTML+=s.map(b=>'<option value="'+b+'">'+(id==='jb'?'Jev: ':D.llm+': ')+b+'</option>').join('')}
function col(title,x,cls,extra){return '<div class="col '+cls+'"><b>'+title+'</b><div class="path">'+esc(x.path)+' <span class="p">'+x.path_p.toFixed(2)+'</span></div><div class="p">'
 +x.top3.map(([k,p])=>esc(k)+' '+p.toFixed(2)).join(' · ')+'</div>'+(extra||'')+'</div>'}
function card(r){const m=M[r.uri]||{};const b=(k,l)=>'<button data-u="'+r.uri+'" data-k="'+k+'" class="'+(m[k]?'on':'')+'">'+l+'</button>';
 const same=r.jev.broad===r.llm.broad;
 const link=(()=>{const p=r.uri.replace('at://','').split('/');return p.length===3?'https://bsky.app/profile/'+p[0]+'/post/'+p[2]:''})();
 return '<div class="card"><div class="text">'+esc(r.text)+'</div><div class="cols">'
 +col('Jev (confidence '+r.jev.conf.toFixed(2)+')',r.jev,same?'same':'')
 +col(D.llm+' (confidence '+r.llm.conf.toFixed(2)+')',r.llm,same?'same':'','<div class="why">'+esc(r.llm.reason||'')+'</div>')
 +col('Even blend (what training would see)',r.blend,'blend')+'</div>'
 +'<div class="meta"><span class="tag">'+(r.sample==='focus'?'moved to '+D.focus:'random')+'</span><span class="tag">'+r.split+'</span><span>right:</span><span class="v">'
 +b('jev','Jev')+b('llm',D.llm)+b('none','Neither')+'</span>'
 +(link?'<a href="'+link+'" target="_blank" rel="noopener">open on bsky.app ↗</a>':'')+'</div></div>'}
function render(){$('#stats').innerHTML=D.stats;
 const f=D.rows.filter(r=>(!st.sample||r.sample===st.sample)&&(!st.agree||(st.agree==='same')===(r.jev.broad===r.llm.broad))
  &&(!st.jb||r.jev.broad===st.jb)&&(!st.lb||r.llm.broad===st.lb)&&(!st.rev||(st.rev==='none'?!M[r.uri]:!!M[r.uri])));
 $('#count').textContent=f.length+' of '+D.rows.length+' · '+Object.keys(M).length+' reviewed';
 $('#list').innerHTML=f.map(card).join('');
 for(const el of document.querySelectorAll('.v button'))el.onclick=()=>{const u=el.dataset.u,k=el.dataset.k;const m=M[u]||{};
  if(k==='none'){M[u]=m.none?{}:{none:true}}else{delete m.none;m[k]=!m[k];M[u]=m}
  if(!Object.values(M[u]).some(Boolean))delete M[u];localStorage.setItem(KEY,JSON.stringify(M));render()};}
for(const id of ['sample','agree','jb','lb','rev'])$('#'+id).onchange=e=>{st[id]=e.target.value;render()};
$('#dl').onclick=()=>{const out=D.rows.filter(r=>M[r.uri]).map(r=>({uri:r.uri,sample:r.sample,right:Object.keys(M[r.uri]).filter(k=>M[r.uri][k]),
  jev:r.jev.path,llm:r.llm.path,blend:r.blend.path,text:r.text}));
 const a=document.createElement('a');a.href=URL.createObjectURL(new Blob([JSON.stringify({relabel:D.name,llm_model:D.llm,marks:out},null,1)],{type:'application/json'}));
 a.download='relabel-marks-'+D.name+'.json';a.click()};
render();
</script></body></html>
"""


def joint(broad: dict, sub: dict, has_subs: set) -> dict:
    """Path distribution P(broad) * P(sub | broad), plus P(broad) for broad topics without
    subtopics, normalized (the same targets trainer/common.py builds)."""
    p = {k: broad.get(k.split("/", 1)[0], 0) * v for k, v in sub.items()}
    for b, v in broad.items():
        if b not in has_subs:
            p[b] = p.get(b, 0) + v
        elif not any(k.startswith(b + "/") for k in sub):
            p[b + "/other"] = p.get(b + "/other", 0) + v
    s = sum(p.values()) or 1
    return {k: v / s for k, v in p.items() if v > 0}


def view(broad: dict, path: dict) -> dict:
    s = sum(broad.values()) or 1
    b = {k: v / s for k, v in broad.items() if v > 0}
    top_b = max(b.items(), key=lambda kv: kv[1])
    top_p = max(path.items(), key=lambda kv: kv[1])
    return {"broad": top_b[0], "conf": round(top_b[1], 3), "path": top_p[0], "path_p": round(top_p[1], 3),
            "top3": [[k, round(v, 3)] for k, v in sorted(b.items(), key=lambda kv: -kv[1])[:3]]}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--name", required=True, help="relabel run name (files in /data/relabel)")
    ap.add_argument("--export", default="/data/exports/v1-final", help="export holding Jev's original labels")
    ap.add_argument("--taxonomy", default="../taxonomy/v1.yaml")
    ap.add_argument("--random", type=int, default=400)
    ap.add_argument("--focus", type=int, default=200, help="extra posts the LLM moved into --focus-broad")
    ap.add_argument("--focus-broad", default="humor")
    ap.add_argument("--seed", type=int, default=0)
    a = ap.parse_args()

    tax = yaml.safe_load(open(a.taxonomy))
    has_subs = {b["id"] for b in tax["broad"] if b.get("subtopics")}
    summary = json.load(open(f"/data/relabel/{a.name}.summary.json"))
    reasons = {}
    for line in open(f"/data/relabel/{a.name}.llm.jsonl"):
        x = json.loads(line)
        if "error" not in x["llm"]:
            reasons[x["uri"]] = x["llm"].get("reason", "")
    llm = {}
    for line in open(f"/data/relabel/{a.name}.rows.jsonl"):
        r = json.loads(line)
        llm[r["uri"]] = r
    jev = {}
    with gzip.open(f"{a.export}/labels.jsonl.gz", "rt") as f:
        for line in f:
            r = json.loads(line)
            if r["uri"] in llm:
                jev[r["uri"]] = r

    rows = []
    for uri, lr in llm.items():
        jr = jev.get(uri)
        if not jr:
            continue
        jb, jp = jr["broad_probs"], joint(jr["broad_probs"], jr["sub_probs"] or {}, has_subs)
        lb, lp = lr["broad_probs"], joint(lr["broad_probs"], lr["sub_probs"], has_subs)
        jsum, lsum = sum(jb.values()) or 1, sum(lb.values()) or 1
        bb = {k: 0.5 * jb.get(k, 0) / jsum + 0.5 * lb.get(k, 0) / lsum for k in set(jb) | set(lb)}
        bp = {k: 0.5 * jp.get(k, 0) + 0.5 * lp.get(k, 0) for k in set(jp) | set(lp)}
        w = jr["window_id"]
        rows.append({"uri": uri, "text": jr["model_input"],
                     "split": "test" if w in ("w21", "w22", "w23") else "validation" if w in ("w18", "w19", "w20") else "train",
                     "jev": view(jb, jp), "llm": {**view(lb, lp), "reason": reasons.get(uri, "")}, "blend": view(bb, bp)})

    n = len(rows)
    same = sum(r["jev"]["broad"] == r["llm"]["broad"] for r in rows)
    blend_is_llm = sum(r["blend"]["path"] == r["llm"]["path"] for r in rows)
    blend_is_jev = sum(r["blend"]["path"] == r["jev"]["path"] for r in rows)

    rng = random.Random(a.seed)
    rows.sort(key=lambda r: r["uri"])
    picked = rng.sample(rows, min(a.random, n))
    for r in picked:
        r["sample"] = "random"
    taken = {r["uri"] for r in picked}
    moved = [r for r in rows if r["uri"] not in taken and r["llm"]["broad"] == a.focus_broad and r["jev"]["broad"] != a.focus_broad]
    focus = rng.sample(moved, min(a.focus, len(moved)))
    for r in focus:
        r["sample"] = "focus"
    page_rows = picked + focus
    rng.shuffle(page_rows)

    model = summary["llm"]
    stats = (f"<b>All {n:,} relabeled posts:</b> same broad topic as Jev {same / n:.0%} · "
             f"blend's top answer is {model}'s {blend_is_llm / n:.0%}, Jev's {blend_is_jev / n:.0%} (subtopic) · "
             f"relabel cost ${summary['cost_usd_list_price']:.2f} at list price · "
             f"<b>This page:</b> {len(picked)} random + {len(focus)} moved to {a.focus_broad}")
    out_dir = f"/data/reports/{a.name}-review"
    os.makedirs(out_dir, exist_ok=True)
    data = json.dumps({"name": a.name, "llm": model, "focus": a.focus_broad, "rows": page_rows, "stats": stats},
                      ensure_ascii=False).replace("</", "<\\/")
    html = (PAGE.replace("__DATA__", data).replace("__NAME__", a.name).replace("__LLM__", model)
            .replace("__FOCUS__", a.focus_broad).replace("__MAXC__", str(summary["max_confidence"])))
    with open(f"{out_dir}/index.html", "w") as f:
        f.write(html)
    print(f"{n} relabeled posts; same broad {same / n:.1%}; blend top path = {model} {blend_is_llm / n:.1%}, = Jev {blend_is_jev / n:.1%}")
    print(f"wrote {out_dir}/index.html ({len(picked)} random + {len(focus)} moved to {a.focus_broad})")


if __name__ == "__main__":
    main()
