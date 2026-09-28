package labelreport

import (
	"encoding/json"
	"html/template"
	"math/rand/v2"
	"os"
	"strings"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// viewerRow is one post in the browsable HTML viewer.
type viewerRow struct {
	Link     string             `json:"link"`
	Text     string             `json:"text"`
	Broad    [][2]any           `json:"broad"` // top broad topics: [id, p]
	Best     string             `json:"best"`
	BestP    float32            `json:"bestP"`
	Conf     float32            `json:"conf"`
	ViaLower bool               `json:"via"`
	Signals  map[string]float32 `json:"sig"`
	Tone     string             `json:"tone"`
}

type viewerTopic struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"desc"`
	Subtopics   []viewerTopic `json:"subs,omitempty"`
}

// bskyLink turns at://did/app.bsky.feed.post/rkey into a bsky.app URL.
func bskyLink(uri string) string {
	parts := strings.Split(strings.TrimPrefix(uri, "at://"), "/")
	if len(parts) != 3 {
		return ""
	}
	return "https://bsky.app/profile/" + parts[0] + "/post/" + parts[2]
}

// maxViewerRows caps the posts embedded in the viewer page so it stays loadable in a
// browser. Larger runs show a fixed random sample; the report and CSV cover everything.
const maxViewerRows = 10000

func writeViewer(path string, tax *taxonomy.Taxonomy, labelConfig string, all []row) error {
	total := len(all)
	rows := all
	if len(rows) > maxViewerRows {
		rows = append([]row(nil), all...)
		rng := rand.New(rand.NewPCG(3, 4))
		rng.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
		rows = rows[:maxViewerRows]
	}
	vrows := make([]viewerRow, 0, len(rows))
	for _, r := range rows {
		tb := topBroad(r.label)
		var broad [][2]any
		for i, t := range tb {
			if i == 5 {
				break
			}
			broad = append(broad, [2]any{t.k, t.v})
		}
		best := r.label.BestPath()
		tone, tv := "", float32(-1)
		sig := map[string]float32{}
		for k, v := range r.label.Signals {
			if strings.HasPrefix(k, "tone.") {
				if v > tv {
					tone, tv = strings.TrimPrefix(k, "tone."), v
				}
				continue
			}
			sig[k] = v
		}
		vrows = append(vrows, viewerRow{
			Link: bskyLink(r.uri), Text: r.doc.Student(), Broad: broad, Best: best, BestP: r.label.PathScores[best],
			Conf: r.label.BroadConfidence, ViaLower: strings.SplitN(best, "/", 2)[0] != tb[0].k, Signals: sig, Tone: tone,
		})
	}
	var topics []viewerTopic
	for _, b := range tax.Broad {
		vt := viewerTopic{ID: b.ID, Name: b.Name, Description: b.Description}
		for _, s := range b.Subtopics {
			vt.Subtopics = append(vt.Subtopics, viewerTopic{ID: s.ID, Name: s.Name, Description: s.Description})
		}
		topics = append(topics, vt)
	}
	data, err := json.Marshal(map[string]any{"rows": vrows, "topics": topics})
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return viewerTmpl.Execute(f, map[string]any{
		"Title":       tax.Version + " · " + labelConfig,
		"Taxonomy":    tax.Version,
		"LabelConfig": labelConfig,
		"N":           len(rows),
		"Total":       total,
		"Data":        template.JS(data),
	})
}

var viewerTmpl = template.Must(template.New("viewer").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Labels · {{.Title}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
:root{--fg:#1d2330;--mut:#667085;--line:#e4e7ec;--bg:#f8f9fb;--acc:#1570ef}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,sans-serif;color:var(--fg);background:var(--bg)}
header{position:sticky;top:0;background:#fff;border-bottom:1px solid var(--line);padding:10px 16px;z-index:2}
h1{font-size:16px;margin:0 0 8px}h1 small{color:var(--mut);font-weight:400}
.controls{display:flex;flex-wrap:wrap;gap:8px;align-items:center}
select,input[type=search],input[type=number]{font:inherit;padding:4px 6px;border:1px solid var(--line);border-radius:6px;background:#fff}
input[type=search]{width:220px}input[type=number]{width:64px}
label{color:var(--mut)}button{font:inherit;padding:4px 10px;border:1px solid var(--line);border-radius:6px;background:#fff;cursor:pointer}
main{display:grid;grid-template-columns:300px 1fr;gap:0}
aside{border-right:1px solid var(--line);height:calc(100vh - 88px);overflow:auto;position:sticky;top:88px;background:#fff;padding:8px 0}
.t{padding:3px 12px;cursor:pointer;display:flex;justify-content:space-between;gap:6px}.t:hover{background:#f2f4f7}
.t.on{background:#eff4ff;color:var(--acc);font-weight:600}.t .n{color:var(--mut);font-variant-numeric:tabular-nums}
.sub{padding-left:26px;font-size:13px}.desc{padding:0 12px 6px 26px;color:var(--mut);font-size:12px;display:none}
.t.on+.desc{display:block}
#list{padding:12px 16px}.card{background:#fff;border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin-bottom:8px}
.text{white-space:pre-wrap;word-break:break-word;margin-bottom:6px}
.meta{display:flex;flex-wrap:wrap;gap:6px;font-size:12px;color:var(--mut);align-items:center}
.pill{border:1px solid var(--line);border-radius:999px;padding:1px 8px;background:#fcfcfd}
.pill.best{border-color:#b2ccff;background:#eff4ff;color:#1849a9}.pill.low{border-color:#fedf89;background:#fffaeb;color:#93370d}
.pill.via{border-color:#d9d6fe;background:#f4f3ff;color:#5925dc}
a{color:var(--acc);text-decoration:none}.count{color:var(--mut);margin-left:auto}
#more{margin:8px 0 24px}
</style></head><body>
<header>
<h1>Jev labels <small>taxonomy <b>{{.Taxonomy}}</b> · label_config <code>{{.LabelConfig}}</code> · {{if lt .N .Total}}random sample of {{.N}} of {{.Total}} labeled posts (counts below are within the sample; report.md covers all){{else}}{{.N}} posts{{end}}</small></h1>
<div class="controls">
<input type="search" id="q" placeholder="Search post text…">
<label>Confidence <input type="number" id="cmin" min="0" max="1" step="0.05" value="0"> – <input type="number" id="cmax" min="0" max="1" step="0.05" value="1"></label>
<label><input type="checkbox" id="via"> best path via 2nd/3rd broad topic</label>
<label>Sort <select id="sort"><option value="rand">random</option><option value="conf">confidence ↑</option><option value="confd">confidence ↓</option><option value="news">news ↓</option><option value="promo">promo ↓</option><option value="substance">substance ↓</option><option value="general_interest">general interest ↓</option></select></label>
<button id="clear">Clear filters</button>
<span class="count" id="count"></span>
</div></header>
<main><aside id="tax"></aside><section id="list"></section></main>
<script>
const D={{.Data}};
const rows=D.rows.map((r,i)=>({...r,i,rnd:Math.random(),top:r.broad[0][0],sub:r.best.includes('/')?r.best:''}));
const st={broad:'',sub:'',q:'',cmin:0,cmax:1,via:false,sort:'rand',shown:100};
const $=s=>document.querySelector(s);
function esc(s){return s.replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
function counts(){const b={},s={};for(const r of rows){b[r.top]=(b[r.top]||0)+1;if(r.sub)s[r.sub]=(s[r.sub]||0)+1}return{b,s}}
const C=counts();
function renderTax(){
 let h='<div class="t'+(!st.broad?' on':'')+'" data-b=""><span>All topics</span><span class="n">'+rows.length+'</span></div>';
 for(const t of D.topics){
  const on=st.broad===t.id&&!st.sub;
  h+='<div class="t'+(on?' on':'')+'" data-b="'+t.id+'"><span>'+esc(t.name)+' <code>'+t.id+'</code></span><span class="n">'+(C.b[t.id]||0)+'</span></div><div class="desc">'+esc(t.desc)+'</div>';
  if(st.broad===t.id)for(const s of (t.subs||[])){const k=t.id+'/'+s.id;
   h+='<div class="t sub'+(st.sub===k?' on':'')+'" data-b="'+t.id+'" data-s="'+k+'"><span>'+esc(s.name)+'</span><span class="n">'+(C.s[k]||0)+'</span></div><div class="desc">'+esc(s.desc)+'</div>'}
 }
 $('#tax').innerHTML=h;
 for(const el of document.querySelectorAll('#tax .t'))el.onclick=()=>{st.broad=el.dataset.b;st.sub=el.dataset.s||'';st.shown=100;render()};
}
function filtered(){
 const q=st.q.toLowerCase();
 let out=rows.filter(r=>(!st.broad||(st.sub?r.sub===st.sub:r.top===st.broad))&&r.conf>=st.cmin&&r.conf<=st.cmax&&(!st.via||r.via)&&(!q||r.text.toLowerCase().includes(q)));
 const key={rand:r=>r.rnd,conf:r=>r.conf,confd:r=>-r.conf}[st.sort]||(r=>-(r.sig[st.sort]||0));
 return out.sort((a,b)=>key(a)-key(b));
}
function card(r){
 const pills=r.broad.slice(0,3).map(([k,p])=>'<span class="pill">'+k+' '+p.toFixed(2)+'</span>').join('');
 const s=r.sig;
 return '<div class="card"><div class="text">'+esc(r.text)+'</div><div class="meta">'
  +'<span class="pill best">'+r.best+(r.bestP?' '+r.bestP.toFixed(2):'')+'</span>'+pills
  +'<span class="pill'+(r.conf<0.6?' low':'')+'">conf '+r.conf.toFixed(2)+'</span>'+(r.via?'<span class="pill via">via lower candidate</span>':'')
  +'<span>substance '+(s.substance||0).toFixed(2)+' · news '+(s.news||0).toFixed(2)+' · promo '+(s.promo||0).toFixed(2)+' · interest '+(s.general_interest||0).toFixed(2)+' · tone '+r.tone+'</span>'
  +(r.link?'<a href="'+r.link+'" target="_blank" rel="noopener">open on bsky.app ↗</a>':'')+'</div></div>';
}
function render(){
 renderTax();const f=filtered();
 $('#count').textContent=f.length+' of '+rows.length+' posts';
 $('#list').innerHTML=f.slice(0,st.shown).map(card).join('')+(f.length>st.shown?'<button id="more">Show 100 more ('+(f.length-st.shown)+' left)</button>':'');
 const m=$('#more');if(m)m.onclick=()=>{st.shown+=100;render()};
}
$('#q').oninput=e=>{st.q=e.target.value;st.shown=100;render()};
$('#cmin').oninput=e=>{st.cmin=+e.target.value||0;render()};
$('#cmax').oninput=e=>{st.cmax=e.target.value===''?1:+e.target.value;render()};
$('#via').onchange=e=>{st.via=e.target.checked;render()};
$('#sort').onchange=e=>{st.sort=e.target.value;render()};
$('#clear').onclick=()=>{Object.assign(st,{broad:'',sub:'',q:'',cmin:0,cmax:1,via:false,shown:100});$('#q').value='';$('#cmin').value=0;$('#cmax').value=1;$('#via').checked=false;render()};
render();
</script></body></html>
`))
