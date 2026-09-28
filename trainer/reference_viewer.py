"""Review page for a reference set: Jev, the student, and the LLM side by side, with
buttons to mark which answers are right. Marks are saved in the browser and can be
downloaded as JSON."""

import json

PAGE = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Reference set · __NAME__</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--fg:#1d2330;--mut:#667085;--line:#e4e7ec;--acc:#1570ef}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,sans-serif;color:var(--fg);background:#f8f9fb}
header{position:sticky;top:0;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px;z-index:2}
h1{font-size:16px;margin:0 0 6px}h1 small{color:var(--mut);font-weight:400}
.stats{font-size:12px;color:var(--mut);margin-bottom:8px}.stats b{color:var(--fg)}
.c{display:flex;flex-wrap:wrap;gap:8px;align-items:center}select,input,button{font:inherit;padding:4px 8px;border:1px solid var(--line);border-radius:6px;background:#fff}
button{cursor:pointer}.count{margin-left:auto;color:var(--mut)}
main{padding:12px 16px;max-width:1250px;margin:0 auto}
.card{background:#fff;border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin-bottom:10px}
.text{white-space:pre-wrap;word-break:break-word;margin-bottom:8px}
.cols{display:grid;grid-template-columns:1fr 1fr 1fr;gap:8px;font-size:13px}
.col{border:1px solid #f2f4f7;border-radius:6px;padding:6px 8px;background:#fcfcfd}.col b{font-size:12px;color:var(--mut)}
.col.same{background:#ecfdf3;border-color:#abefc6}.path{font-weight:600}.p{color:var(--mut)}.why{color:#475467;font-style:italic;margin-top:3px}
.meta{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:8px;font-size:12px;color:var(--mut)}
.v button{font-size:12px;padding:2px 8px}.v button.on{background:#1570ef;color:#fff;border-color:#1570ef}
.tag{border:1px solid var(--line);border-radius:999px;padding:0 8px}a{color:var(--acc);text-decoration:none}
</style></head><body>
<header><h1>Reference set <small>__NAME__ · __N__ test posts · Jev, the student model, and __LLM__ (which never saw the other two answers)</small></h1>
<div class="stats" id="stats"></div>
<div class="c">
<select id="stratum"><option value="">Both samples</option><option value="random">Random sample</option><option value="disagree">Student and Jev disagree</option></select>
<select id="pattern"><option value="">Any agreement</option><option value="all3">All three agree (broad)</option><option value="llm=student">LLM sides with student</option><option value="llm=jev">LLM sides with Jev</option><option value="llm-alone">LLM matches neither</option></select>
<select id="rev"><option value="">Any review state</option><option value="none">Not reviewed</option><option value="done">Reviewed</option></select>
<button id="dl">Download marks</button><span class="count" id="count"></span></div></header>
<main id="list"></main>
<script>
const D=__DATA__;const KEY='refmarks:'+D.name;const M=JSON.parse(localStorage.getItem(KEY)||'{}');
const st={stratum:'',pattern:'',rev:''};const $=s=>document.querySelector(s);
function esc(s){return String(s).replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
function statsHtml(){const f=(k,s)=>{const x=D.stats[s][k];return x==null?'–':(x*100).toFixed(0)+'%'};
 return ['random','disagree'].map(s=>'<b>'+(s==='random'?'Random':'Disagreements')+' ('+D.stats[s].n+'):</b> LLM = Jev '+f('broad: llm == jev',s)+' · LLM = student '+f('broad: llm == student',s)+' · LLM matches neither '+f('broad: llm matches neither',s)+(s==='random'?' · Jev = student '+f('broad: jev == student',s):'')).join(' &nbsp;|&nbsp; ')+' <span>(broad topic)</span>'}
function pat(r){const j=r.jev.broad,s=r.student.broad,l=r.llm.broad;if(j===s&&s===l)return'all3';if(l===s)return'llm=student';if(l===j)return'llm=jev';return'llm-alone'}
function col(title,x,extra,same){return '<div class="col'+(same?' same':'')+'"><b>'+title+'</b><div class="path">'+esc(x.path)+'</div><div class="p">'+(x.top3?x.top3.map(([k,p])=>esc(k)+' '+p.toFixed(2)).join(' · '):'confidence '+(x.confidence??'?'))+'</div>'+(extra||'')+'</div>'}
function card(r){const m=M[r.uri]||{};const b=(k,l)=>'<button data-u="'+r.uri+'" data-k="'+k+'" class="'+(m[k]?'on':'')+'">'+l+'</button>';
 const ag=pat(r);const lsame=r.llm.broad===r.jev.broad||r.llm.broad===r.student.broad;
 const link=(()=>{const p=r.uri.replace('at://','').split('/');return p.length===3?'https://bsky.app/profile/'+p[0]+'/post/'+p[2]:''})();
 return '<div class="card"><div class="text">'+esc(r.text)+'</div><div class="cols">'
 +col('Jev (conf '+r.jev.broad_p.toFixed(2)+')',r.jev,'',r.jev.broad===r.llm.broad)
 +col('Student (conf '+r.student.broad_p.toFixed(2)+')',r.student,'',r.student.broad===r.llm.broad)
 +col(D.llm,r.llm,'<div class="why">'+esc(r.llm.reason||'')+'</div>',lsame)+'</div>'
 +'<div class="meta"><span class="tag">'+r.stratum+'</span><span>right:</span><span class="v">'+b('jev','Jev')+b('student','Student')+b('llm',D.llm)+b('none','None of them')+'</span>'
 +(link?'<a href="'+link+'" target="_blank" rel="noopener">open on bsky.app ↗</a>':'')+'</div></div>'}
function render(){$('#stats').innerHTML=statsHtml();
 const f=D.rows.filter(r=>(!st.stratum||r.stratum===st.stratum)&&(!st.pattern||pat(r)===st.pattern)&&(!st.rev||(st.rev==='none'?!M[r.uri]:!!M[r.uri])));
 $('#count').textContent=f.length+' of '+D.rows.length+' · '+Object.keys(M).length+' reviewed';
 $('#list').innerHTML=f.map(card).join('');
 for(const el of document.querySelectorAll('.v button'))el.onclick=()=>{const u=el.dataset.u,k=el.dataset.k;const m=M[u]||{};
  if(k==='none'){M[u]=m.none?{}:{none:true}}else{delete m.none;m[k]=!m[k];M[u]=m}
  if(!Object.values(M[u]).some(Boolean))delete M[u];localStorage.setItem(KEY,JSON.stringify(M));render()};}
for(const id of ['stratum','pattern','rev'])$('#'+id).onchange=e=>{st[id]=e.target.value;render()};
$('#dl').onclick=()=>{const out=D.rows.filter(r=>M[r.uri]).map(r=>({uri:r.uri,stratum:r.stratum,right:Object.keys(M[r.uri]).filter(k=>M[r.uri][k]),jev:r.jev.path,student:r.student.path,llm:r.llm.path,text:r.text}));
 const a=document.createElement('a');a.href=URL.createObjectURL(new Blob([JSON.stringify({reference:D.name,marks:out},null,1)],{type:'application/json'}));a.download='marks-'+D.name+'.json';a.click()};
render();
</script></body></html>
"""


def write(path, name, rows, stats):
    llm = rows[0]["llm"]["model"] if rows else "LLM"
    data = json.dumps({"name": name, "llm": llm, "rows": rows, "stats": stats}, ensure_ascii=False).replace("</", "<\\/")
    html = PAGE.replace("__DATA__", data).replace("__NAME__", name).replace("__N__", str(len(rows))).replace("__LLM__", llm)
    with open(path, "w") as f:
        f.write(html)
