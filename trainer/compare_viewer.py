"""HTML page for checking the student against Jev by hand, served by the viewer.

Each test post shows Jev's answer next to the student's, with filters (agree or
disagree, Jev's confidence, topic, text) and verdict buttons (Jev right, student
right, both fine, neither). Verdicts are kept in the browser's local storage and can
be downloaded as JSON: the start of the human-reviewed reference set (plan §8.3).
"""

import json

PAGE = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Student vs Jev · __TITLE__</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--fg:#1d2330;--mut:#667085;--line:#e4e7ec;--acc:#1570ef}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,sans-serif;color:var(--fg);background:#f8f9fb}
header{position:sticky;top:0;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px;z-index:2}
h1{font-size:16px;margin:0 0 8px}h1 small{color:var(--mut);font-weight:400}
.c{display:flex;flex-wrap:wrap;gap:8px;align-items:center}select,input,button{font:inherit;padding:4px 8px;border:1px solid var(--line);border-radius:6px;background:#fff}
input[type=search]{width:200px}button{cursor:pointer}label{color:var(--mut)}.count{margin-left:auto;color:var(--mut)}
main{padding:12px 16px;max-width:1200px;margin:0 auto}
.card{background:#fff;border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin-bottom:10px}
.card.dis{border-left:4px solid #f79009}.card.agr{border-left:4px solid #12b76a}
.text{white-space:pre-wrap;word-break:break-word;margin-bottom:8px}
.cols{display:grid;grid-template-columns:1fr 1fr;gap:10px;font-size:13px}
.col{background:#fcfcfd;border:1px solid #f2f4f7;border-radius:6px;padding:6px 8px}.col b{font-size:12px;color:var(--mut)}
.top{font-weight:600}.row{display:flex;justify-content:space-between;gap:8px}.p{color:var(--mut);font-variant-numeric:tabular-nums}
.bar{height:4px;background:#e4e7ec;border-radius:2px;margin:1px 0 3px}.bar i{display:block;height:4px;border-radius:2px;background:var(--acc)}
.meta{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:8px;font-size:12px;color:var(--mut)}
.v button{font-size:12px;padding:2px 8px}.v button.on{background:#1570ef;color:#fff;border-color:#1570ef}
a{color:var(--acc);text-decoration:none}#more{margin:8px 0 24px}
</style></head><body>
<header><h1>Student vs Jev <small>__TITLE__ · __N__ test posts from windows the model never trained on</small></h1>
<div class="c">
<select id="agree"><option value="">All</option><option value="dis">Disagree on broad topic</option><option value="agr">Agree on broad topic</option><option value="pathdis">Agree on broad, disagree on subtopic</option></select>
<select id="conf"><option value="">Any Jev confidence</option><option value="hi">Jev ≥ 0.9</option><option value="mid">Jev 0.6–0.9</option><option value="lo">Jev &lt; 0.6</option></select>
<select id="topic"><option value="">Any topic (Jev's or student's)</option></select>
<input type="search" id="q" placeholder="Search text…">
<select id="verd"><option value="">Any verdict</option><option value="none">Not reviewed</option><option value="jev">Jev right</option><option value="student">Student right</option><option value="both">Both fine</option><option value="neither">Neither</option></select>
<button id="dl">Download verdicts</button>
<span class="count" id="count"></span></div></header>
<main id="list"></main>
<script>
const D=__DATA__;
const KEY='verdicts:'+D.title;
const V=JSON.parse(localStorage.getItem(KEY)||'{}');
const st={agree:'',conf:'',topic:'',q:'',verd:'',shown:50};
const $=s=>document.querySelector(s);
function esc(s){return s.replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
for(const t of D.topics){const o=document.createElement('option');o.value=t;o.textContent=t;$('#topic').appendChild(o)}
const rows=D.rows.map((r,i)=>({...r,i,rnd:Math.random()}));
function side(title,broad,path,pathP){
 return '<div class="col"><b>'+title+'</b>'+broad.map(([k,p],i)=>'<div class="row"><span'+(i==0?' class="top"':'')+'>'+k+'</span><span class="p">'+p.toFixed(2)+'</span></div><div class="bar"><i style="width:'+(p*100).toFixed(0)+'%"></i></div>').join('')
  +'<div class="row"><span>path <span class="top">'+path+'</span></span><span class="p">'+pathP.toFixed(2)+'</span></div></div>';
}
function card(r){
 const v=V[r.uri]||'';
 const btn=(k,l)=>'<button data-u="'+r.uri+'" data-v="'+k+'" class="'+(v===k?'on':'')+'">'+l+'</button>';
 return '<div class="card '+(r.agree?'agr':'dis')+'"><div class="text">'+esc(r.text)+'</div><div class="cols">'
  +side('Jev (confidence '+r.jconf.toFixed(2)+')',r.jb,r.jpath,r.jpathP)+side('Student',r.sb,r.spath,r.spathP)+'</div>'
  +'<div class="meta"><span class="v">'+btn('jev','Jev right')+btn('student','Student right')+btn('both','Both fine')+btn('neither','Neither')+'</span>'
  +'<span>signals Jev/student: substance '+r.jsig[0].toFixed(2)+'/'+r.ssig[0].toFixed(2)+' · news '+r.jsig[1].toFixed(2)+'/'+r.ssig[1].toFixed(2)+' · promo '+r.jsig[2].toFixed(2)+'/'+r.ssig[2].toFixed(2)+'</span>'
  +(r.link?'<a href="'+r.link+'" target="_blank" rel="noopener">open on bsky.app ↗</a>':'')+'</div></div>';
}
function filtered(){
 const q=st.q.toLowerCase();
 return rows.filter(r=>(!st.agree||(st.agree==='dis'&&!r.agree)||(st.agree==='agr'&&r.agree)||(st.agree==='pathdis'&&r.agree&&!r.pathAgree))
  &&(!st.conf||(st.conf==='hi'&&r.jconf>=0.9)||(st.conf==='mid'&&r.jconf>=0.6&&r.jconf<0.9)||(st.conf==='lo'&&r.jconf<0.6))
  &&(!st.topic||r.jb[0][0]===st.topic||r.sb[0][0]===st.topic)
  &&(!st.verd||(st.verd==='none'?!V[r.uri]:V[r.uri]===st.verd))
  &&(!q||r.text.toLowerCase().includes(q))).sort((a,b)=>a.rnd-b.rnd);
}
function render(){
 const f=filtered();const nv=Object.keys(V).length;
 $('#count').textContent=f.length+' of '+rows.length+' posts · '+nv+' reviewed';
 $('#list').innerHTML=f.slice(0,st.shown).map(card).join('')+(f.length>st.shown?'<button id="more">Show 50 more ('+(f.length-st.shown)+' left)</button>':'');
 const m=$('#more');if(m)m.onclick=()=>{st.shown+=50;render()};
 for(const b of document.querySelectorAll('.v button'))b.onclick=()=>{const u=b.dataset.u;V[u]=V[u]===b.dataset.v?undefined:b.dataset.v;if(!V[u])delete V[u];localStorage.setItem(KEY,JSON.stringify(V));render()};
}
for(const id of ['agree','conf','topic','verd'])$('#'+id).onchange=e=>{st[id]=e.target.value;st.shown=50;render()};
$('#q').oninput=e=>{st.q=e.target.value;st.shown=50;render()};
$('#dl').onclick=()=>{const byUri=Object.fromEntries(rows.map(r=>[r.uri,r]));
 const out=Object.entries(V).map(([u,v])=>({uri:u,verdict:v,jev_broad:byUri[u]?.jb[0][0],jev_path:byUri[u]?.jpath,student_broad:byUri[u]?.sb[0][0],student_path:byUri[u]?.spath,text:byUri[u]?.text}));
 const a=document.createElement('a');a.href=URL.createObjectURL(new Blob([JSON.stringify({model:D.title,verdicts:out},null,1)],{type:'application/json'}));a.download='verdicts-'+D.title+'.json';a.click()};
render();
</script></body></html>
"""


def top(names, probs, k):
    idx = probs.argsort()[::-1][:k]
    return [[names[i], round(float(probs[i]), 3)] for i in idx]


def link(uri: str) -> str:
    parts = uri.removeprefix("at://").split("/")
    return f"https://bsky.app/profile/{parts[0]}/post/{parts[2]}" if len(parts) == 3 else ""


def write(path, title, space, te, sb, sp, sig):
    rows = []
    for i in range(len(te)):
        jb, jp = te.broad[i], te.path[i]
        rows.append({
            "uri": te.uris[i], "link": link(te.uris[i]), "text": te.texts[i],
            "jconf": round(float(jb.max()), 3),
            "jb": top(space.broad, jb, 3), "sb": top(space.broad, sb[i], 3),
            "jpath": space.paths[int(jp.argmax())], "jpathP": round(float(jp.max()), 3),
            "spath": space.paths[int(sp[i].argmax())], "spathP": round(float(sp[i].max()), 3),
            "agree": bool(jb.argmax() == sb[i].argmax()), "pathAgree": bool(jp.argmax() == sp[i].argmax()),
            "jsig": [round(float(x), 2) for x in te.signals[i][:3]], "ssig": [round(float(x), 2) for x in sig[i][:3]],
        })
    data = json.dumps({"title": title, "rows": rows, "topics": space.broad}, ensure_ascii=False).replace("</", "<\\/")
    html = PAGE.replace("__DATA__", data).replace("__TITLE__", title).replace("__N__", str(len(rows)))
    with open(path, "w") as f:
        f.write(html)
