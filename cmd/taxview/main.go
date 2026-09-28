// Command taxview renders a taxonomy as a review page for the viewer service:
//
//	taxview -taxonomy taxonomy/draft2.yaml   # writes /data/reports/<version>-taxonomy/index.html
//
// The taxonomy is loaded (and validated) with the same loader the labeler uses.
package main

import (
	"flag"
	"fmt"
	"html/template"
	"os"
	"path/filepath"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func main() {
	taxPath := flag.String("taxonomy", "", "taxonomy YAML file")
	out := flag.String("out", "", "output directory (default /data/reports/<version>-taxonomy)")
	flag.Parse()
	if err := run(*taxPath, *out); err != nil {
		fmt.Fprintln(os.Stderr, "taxview:", err)
		os.Exit(1)
	}
}

func run(taxPath, out string) error {
	tax, err := taxonomy.Load(taxPath)
	if err != nil {
		return err
	}
	if out == "" {
		out = filepath.Join("/data/reports", tax.Version+"-taxonomy")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	subs := 0
	for _, b := range tax.Broad {
		subs += len(b.Subtopics)
	}
	f, err := os.Create(filepath.Join(out, "index.html"))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := page.Execute(f, map[string]any{"T": tax, "Subs": subs, "File": taxPath}); err != nil {
		return err
	}
	fmt.Println("wrote", filepath.Join(out, "index.html"))
	return nil
}

var page = template.Must(template.New("tax").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Taxonomy {{.T.Version}}</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
body{margin:0;font:14px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;color:#1d2330;background:#f8f9fb}
header{background:#fff;border-bottom:1px solid #e4e7ec;padding:14px 20px;position:sticky;top:0}
h1{font-size:18px;margin:0}header p{margin:4px 0 0;color:#667085}
main{max-width:1000px;margin:0 auto;padding:16px 20px 40px}
nav{columns:3;font-size:13px;margin-bottom:16px}nav a{display:block;color:#1570ef;text-decoration:none}
.b{background:#fff;border:1px solid #e4e7ec;border-radius:8px;padding:12px 16px;margin-bottom:12px}
.b h2{font-size:16px;margin:0 0 2px}.b h2 code{font-size:13px;color:#667085;font-weight:400}.n{color:#667085;font-size:13px;font-weight:400}
.d{margin:2px 0 6px}.ex{color:#475467;font-size:13px;margin:0 0 8px;padding-left:18px}
table{border-collapse:collapse;width:100%;font-size:13px}td{border-top:1px solid #f2f4f7;padding:4px 6px;vertical-align:top}
td:first-child{white-space:nowrap;width:1%}td code{color:#667085}
</style></head><body>
<header><h1>Taxonomy <code>{{.T.Version}}</code></h1>
<p>{{len .T.Broad}} broad topics · {{.Subs}} subtopics · file <code>{{.File}}</code> · hash <code>{{.T.Hash}}</code></p></header>
<main>
<nav>{{range .T.Broad}}<a href="#{{.ID}}">{{.Name}} ({{len .Subtopics}})</a>{{end}}</nav>
{{range .T.Broad}}<section class="b" id="{{.ID}}">
<h2>{{.Name}} <code>{{.ID}}</code> <span class="n">· {{len .Subtopics}} subtopics</span></h2>
<p class="d">{{.Description}}</p>
{{if .Examples}}<ul class="ex">{{range .Examples}}<li>{{.}}</li>{{end}}</ul>{{end}}
{{if .Subtopics}}<table>{{range .Subtopics}}<tr><td><b>{{.Name}}</b><br><code>{{.ID}}</code></td><td>{{.Description}}</td></tr>{{end}}</table>{{end}}
</section>{{end}}
</main></body></html>
`))
