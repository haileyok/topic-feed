// Command labelreport writes the human review report for a labeling run (plan
// §10.2a): topic volumes, examples per topic, low-confidence posts, posts whose best
// path went through a 2nd or 3rd broad topic, ranking-signal distributions, and cost.
//
//	labelreport -taxonomy taxonomy/draft1.yaml -label-config <hash> -out reports/draft1-first-2500
//
// The report contains post text, so reports/ is gitignored.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labeler"
	"github.com/haileyok/topic-feed/internal/postdoc"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

const pricePerMTok = 0.042 // USD per million input tokens (jev-1.13.0)

type row struct {
	uri   string
	doc   postdoc.Doc
	label labeler.Label
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "labelreport:", err)
		os.Exit(1)
	}
}

func run() error {
	taxPath := flag.String("taxonomy", "taxonomy/draft1.yaml", "taxonomy YAML file")
	labelConfig := flag.String("label-config", "", "label_config hash of the run (printed by labeler)")
	out := flag.String("out", "", "output directory (default /data/reports/<version>-<label_config>, served by the viewer service)")
	perTopic := flag.Int("examples", 10, "examples per broad topic")
	flag.Parse()

	tax, err := taxonomy.Load(*taxPath)
	if err != nil {
		return err
	}
	if *labelConfig == "" {
		return fmt.Errorf("-label-config is required")
	}
	if *out == "" {
		*out = filepath.Join("/data/reports", tax.Version+"-"+*labelConfig)
	}
	ctx := context.Background()
	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()

	q, err := conn.Query(ctx, `
		SELECT l.uri, p.text, p.media_alts, p.link_domain, p.link_title, p.link_description, p.quote_text, p.tags,
		       l.broad_probs, l.broad_confidence, l.sub_probs, l.path_scores, l.signals, l.labeled_at
		FROM jev_labels AS l FINAL
		INNER JOIN posts AS p FINAL ON p.uri = l.uri
		WHERE l.taxonomy_version = ? AND l.label_config = ?`, tax.Version, *labelConfig)
	if err != nil {
		return err
	}
	var rows []row
	var first, last time.Time
	for q.Next() {
		var (
			r                                row
			text, domain, title, desc, quote string
			alts, tags                       []string
			broad, sub, paths, signals       map[string]float32
			conf                             float32
			at                               time.Time
		)
		if err := q.Scan(&r.uri, &text, &alts, &domain, &title, &desc, &quote, &tags, &broad, &conf, &sub, &paths, &signals, &at); err != nil {
			return err
		}
		r.doc = postdoc.New(postdoc.Input{Text: text, MediaAlts: alts, LinkDomain: domain, LinkTitle: title, LinkDescription: desc, QuoteText: quote, Tags: tags})
		r.label = labeler.Label{URI: r.uri, BroadProbs: broad, BroadConfidence: conf, SubProbs: sub, PathScores: paths, Signals: signals, LabeledAt: at}
		rows = append(rows, r)
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	if err := q.Err(); err != nil {
		return err
	}
	q.Close()
	if len(rows) == 0 {
		return fmt.Errorf("no labels for %s / %s", tax.Version, *labelConfig)
	}

	// Cost and request stats for the run's time window.
	var nReq, nOK, n429, tokens uint64
	var p50, p95 float64
	err = conn.QueryRow(ctx, `
		SELECT count(), countIf(status = 'ok'), countIf(status = 'rate_limited'), sum(input_tokens),
		       quantile(0.5)(latency_ms), quantile(0.95)(latency_ms)
		FROM jev_requests WHERE ts BETWEEN ? AND ?`,
		first.Add(-10*time.Minute), last.Add(time.Minute)).Scan(&nReq, &nOK, &n429, &tokens, &p50, &p95)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	if err := writeCSV(filepath.Join(*out, "labels.csv"), rows); err != nil {
		return err
	}
	md := report(tax, *labelConfig, rows, *perTopic, reqStats{nReq, nOK, n429, tokens, p50, p95})
	path := filepath.Join(*out, "report.md")
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	viewer := filepath.Join(*out, "index.html")
	if err := writeViewer(viewer, tax, *labelConfig, rows); err != nil {
		return err
	}
	fmt.Println("wrote", viewer)
	return nil
}

type reqStats struct {
	n, ok, rateLimited, tokens uint64
	p50, p95                   float64
}

func topBroad(l labeler.Label) []kv { return sorted(l.BroadProbs) }

type kv struct {
	k string
	v float32
}

func sorted(m map[string]float32) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].v > out[j].v || (out[i].v == out[j].v && out[i].k < out[j].k) })
	return out
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "|", "¦")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func pct(a, b int) string { return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(max(b, 1))) }

func report(tax *taxonomy.Taxonomy, labelConfig string, rows []row, perTopic int, rs reqStats) string {
	var b strings.Builder
	n := len(rows)
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("# Labeling review: %s, first %d posts\n\n", tax.Version, n)
	w("- Taxonomy `%s` (file hash `%s`), label_config `%s`, questions `%s`, post document `%s`\n", tax.Version, tax.Hash, labelConfig, labeler.QuestionsVersion, postdoc.Version)
	w("- Generated %s\n\n", time.Now().UTC().Format(time.RFC3339))

	// Cost.
	w("## Cost and throughput\n\n")
	w("| Requests | OK | Rate limited (429) | Input tokens | Tokens per post | Cost | Cost per 1k posts | Latency p50 / p95 |\n|---|---|---|---|---|---|---|---|\n")
	w("| %d | %d | %d | %d | %.0f | $%.3f | $%.3f | %.0f / %.0f ms |\n\n", rs.n, rs.ok, rs.rateLimited, rs.tokens,
		float64(rs.tokens)/float64(n), float64(rs.tokens)/1e6*pricePerMTok, float64(rs.tokens)/float64(n)/1e3*pricePerMTok, rs.p50, rs.p95)

	// Volumes.
	broadCount := map[string]int{}
	pathCount := map[string]int{}
	viaLower := []row{}
	for _, r := range rows {
		tb := topBroad(r.label)
		broadCount[tb[0].k]++
		best := r.label.BestPath()
		pathCount[best]++
		if bb := strings.SplitN(best, "/", 2)[0]; bb != tb[0].k {
			viaLower = append(viaLower, r)
		}
	}
	w("## Broad topics (top-1)\n\n| Broad topic | Posts | Share |\n|---|---|---|\n")
	for _, bt := range tax.Broad {
		w("| `%s` %s | %d | %s |\n", bt.ID, bt.Name, broadCount[bt.ID], pct(broadCount[bt.ID], n))
	}
	w("\n## Subtopics (best path)\n\nBest path = highest `sqrt(P(broad) × P(sub | broad))` over the top-3 broad candidates.\n\n| Path | Posts | Share |\n|---|---|---|\n")
	for _, p := range sortedCounts(pathCount) {
		w("| `%s` | %d | %s |\n", p.k, int(p.v), pct(int(p.v), n))
	}
	var empty []string
	for _, bt := range tax.Broad {
		for _, s := range bt.Subtopics {
			if pathCount[bt.ID+"/"+s.ID] == 0 {
				empty = append(empty, bt.ID+"/"+s.ID)
			}
		}
	}
	if len(empty) > 0 {
		w("\nSubtopics with no posts in this sample (%d): %s\n", len(empty), "`"+strings.Join(empty, "`, `")+"`")
	}

	// Confidence overview.
	var confs []float64
	for _, r := range rows {
		confs = append(confs, float64(r.label.BroadConfidence))
	}
	sort.Float64s(confs)
	w("\n## Broad-topic confidence\n\n")
	w("Median %.2f; 10th percentile %.2f; share below 0.6: %s.\n\n", confs[len(confs)/2], confs[len(confs)/10], pct(countBelow(confs, 0.6), n))
	w("Best path went through the 2nd or 3rd broad candidate for %d posts (%s).\n", len(viaLower), pct(len(viaLower), n))

	// Signals.
	w("\n## Ranking signals\n\nShare of posts in each 0.2-wide bucket. A question that's nearly always 0 or 1 doesn't discriminate.\n\n")
	w("| Signal | 0–0.2 | 0.2–0.4 | 0.4–0.6 | 0.6–0.8 | 0.8–1 | Mean |\n|---|---|---|---|---|---|---|\n")
	for _, s := range []string{"substance", "news", "promo", "general_interest"} {
		var buckets [5]int
		var sum float64
		for _, r := range rows {
			v := r.label.Signals[s]
			sum += float64(v)
			buckets[min(int(v*5), 4)]++
		}
		w("| %s | %s | %s | %s | %s | %s | %.2f |\n", s, pct(buckets[0], n), pct(buckets[1], n), pct(buckets[2], n), pct(buckets[3], n), pct(buckets[4], n), sum/float64(n))
	}
	toneCount := map[string]int{}
	for _, r := range rows {
		best, bv := "", float32(-1)
		for k, v := range r.label.Signals {
			if strings.HasPrefix(k, "tone.") && v > bv {
				best, bv = strings.TrimPrefix(k, "tone."), v
			}
		}
		toneCount[best]++
	}
	w("\nTone (top choice): ")
	var tones []string
	for _, t := range sortedCounts(toneCount) {
		tones = append(tones, fmt.Sprintf("%s %s", t.k, pct(int(t.v), n)))
	}
	w("%s\n", strings.Join(tones, ", "))

	// Examples per broad topic.
	rng := rand.New(rand.NewPCG(1, 2))
	w("\n## Examples per broad topic\n\nRandom posts whose top broad topic is this one. Columns: post (as the student sees it), top-3 broad topics, best path.\n")
	for _, bt := range tax.Broad {
		var mine []row
		for _, r := range rows {
			if topBroad(r.label)[0].k == bt.ID {
				mine = append(mine, r)
			}
		}
		rng.Shuffle(len(mine), func(i, j int) { mine[i], mine[j] = mine[j], mine[i] })
		w("\n### `%s` %s (%d posts)\n\n> %s\n\n", bt.ID, bt.Name, len(mine), bt.Description)
		if len(mine) == 0 {
			continue
		}
		w("| Post | Top-3 broad | Best path |\n|---|---|---|\n")
		for _, r := range mine[:min(perTopic, len(mine))] {
			w("| %s | %s | %s |\n", oneLine(r.doc.Student(), 220), top3(r.label), bestPathCell(r.label))
		}
	}

	// Low confidence.
	low := append([]row(nil), rows...)
	sort.Slice(low, func(i, j int) bool { return low[i].label.BroadConfidence < low[j].label.BroadConfidence })
	w("\n## 50 lowest-confidence posts\n\n| Confidence | Post | Top-3 broad | Best path |\n|---|---|---|---|\n")
	for _, r := range low[:min(50, len(low))] {
		w("| %.2f | %s | %s | %s |\n", r.label.BroadConfidence, oneLine(r.doc.Student(), 200), top3(r.label), bestPathCell(r.label))
	}

	// Via 2nd/3rd candidate.
	w("\n## Best path through the 2nd or 3rd broad candidate (up to 40)\n\n| Post | Top-3 broad | Best path |\n|---|---|---|\n")
	for _, r := range viaLower[:min(40, len(viaLower))] {
		w("| %s | %s | %s |\n", oneLine(r.doc.Student(), 200), top3(r.label), bestPathCell(r.label))
	}
	return b.String()
}

func top3(l labeler.Label) string {
	var parts []string
	for i, t := range topBroad(l) {
		if i == 3 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.2f", t.k, t.v))
	}
	return strings.Join(parts, ", ")
}

func bestPathCell(l labeler.Label) string {
	best := l.BestPath()
	if v, ok := l.PathScores[best]; ok {
		return fmt.Sprintf("`%s` %.2f", best, v)
	}
	return "`" + best + "`"
}

func sortedCounts(m map[string]int) []kv {
	f := make(map[string]float32, len(m))
	for k, v := range m {
		f[k] = float32(v)
	}
	return sorted(f)
}

func countBelow(xs []float64, t float64) int {
	n := 0
	for _, x := range xs {
		if x < t {
			n++
		}
	}
	return n
}

func writeCSV(path string, rows []row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"uri", "post", "broad_1", "p_1", "broad_2", "p_2", "broad_3", "p_3", "broad_confidence", "best_path", "best_path_score", "substance", "news", "promo", "general_interest"})
	for _, r := range rows {
		tb := topBroad(r.label)
		rec := []string{r.uri, r.doc.Student()}
		for i := 0; i < 3; i++ {
			if i < len(tb) {
				rec = append(rec, tb[i].k, fmt.Sprintf("%.3f", tb[i].v))
			} else {
				rec = append(rec, "", "")
			}
		}
		best := r.label.BestPath()
		rec = append(rec, fmt.Sprintf("%.3f", r.label.BroadConfidence), best, fmt.Sprintf("%.3f", r.label.PathScores[best]))
		for _, s := range []string{"substance", "news", "promo", "general_interest"} {
			rec = append(rec, fmt.Sprintf("%.3f", r.label.Signals[s]))
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return w.Error()
}
