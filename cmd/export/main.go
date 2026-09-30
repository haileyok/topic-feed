// Command export writes the training set for the classifier (plan §11.2): exactly
// one Jev label per post, never `eval` labels, and never any post that has an `eval`
// label (Phase 0 test posts and the reference set). Each row carries the student's
// input rendered by internal/postdoc, so training and serving see identical text, and
// the labeling window the post falls in (empty for posts outside the windows).
//
//	export -taxonomy taxonomy/v1.yaml -label-configs 5697660f73fc -out /data/exports/v1-v0
//	export -label-configs 5697660f73fc,6a350cf6d994,f101541647c7 -full-context-configs f101541647c7 -out /data/exports/v4
//
// Output: labels.jsonl.gz (one JSON object per post) and manifest.json.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/postdoc"
	"github.com/haileyok/topic-feed/internal/taxonomy"
	"github.com/haileyok/topic-feed/internal/windows"
)

type rowOut struct {
	URI             string             `json:"uri"`
	IndexedAt       time.Time          `json:"indexed_at"`
	WindowID        string             `json:"window_id"`
	Source          string             `json:"source"`
	LabelConfig     string             `json:"label_config"`
	ModelInput      string             `json:"model_input"`
	BroadProbs      map[string]float32 `json:"broad_probs"`
	BroadConfidence float32            `json:"broad_confidence"`
	SubProbs        map[string]float32 `json:"sub_probs"`
	PathScores      map[string]float32 `json:"path_scores"`
	Signals         map[string]float32 `json:"signals"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "export:", err)
		os.Exit(1)
	}
}

func run() error {
	taxPath := flag.String("taxonomy", "taxonomy/v1.yaml", "taxonomy YAML file")
	configs := flag.String("label-configs", "", "comma-separated label_config hashes accepted for this training set")
	windowsPath := flag.String("windows", "config/labeling_windows.yaml", "labeling windows file")
	out := flag.String("out", "", "output directory")
	fullContext := flag.String("full-context-configs", "",
		"comma-separated label_configs whose posts Jev saw with attachments, image text, and labels (pd2 live labels); "+
			"posts labeled under other configs are rendered without them, as Jev saw them")
	flag.Parse()
	if *configs == "" || *out == "" {
		return fmt.Errorf("-label-configs and -out are required")
	}
	tax, err := taxonomy.Load(*taxPath)
	if err != nil {
		return err
	}
	ws, err := windows.Load(*windowsPath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()

	accepted := strings.Split(*configs, ",")
	full := map[string]bool{}
	for _, c := range strings.Split(*fullContext, ",") {
		if c != "" {
			full[c] = true
		}
	}
	// Live posts carry what the pipeline found (post_pipeline); labeling-window posts
	// have no row there and render as before.
	q, err := conn.Query(ctx, `
		WITH
		    eval_uris AS (SELECT DISTINCT uri FROM jev_labels WHERE source = 'eval'),
		    labels AS (
		        SELECT uri, broad_probs, broad_confidence, sub_probs, path_scores, signals, source, label_config
		        FROM jev_labels FINAL
		        WHERE taxonomy_version = ? AND label_config IN ? AND source IN ('window', 'sample', 'uncertain')
		        ORDER BY uri, labeled_at DESC
		        LIMIT 1 BY uri
		    )
		SELECT p.uri, p.indexed_at, p.text, p.media_alts, p.link_domain, p.link_title, p.link_description, p.quote_text, p.tags,
		       p.media_kinds, pp.image_texts, pp.image_text_sources, pp.labels,
		       l.broad_probs, l.broad_confidence, l.sub_probs, l.path_scores, l.signals, l.source, l.label_config
		FROM posts AS p FINAL
		INNER JOIN labels AS l ON l.uri = p.uri
		LEFT JOIN (SELECT uri, image_texts, image_text_sources, labels FROM post_pipeline FINAL
		           WHERE uri IN (SELECT uri FROM labels)) AS pp ON pp.uri = p.uri
		WHERE p.uri NOT IN (SELECT uri FROM eval_uris)
		  AND p.uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
		  AND p.did NOT IN (SELECT did FROM account_status FINAL WHERE active = 0)`,
		tax.Version, accepted)
	if err != nil {
		return err
	}
	defer q.Close()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(*out, "labels.jsonl.gz"))
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)

	seen := map[string]bool{}
	perWindow := map[string]int{}
	n := 0
	fullRows := 0
	for q.Next() {
		var (
			r                                    row
			text, domain, title, desc, quote     string
			alts, tags, kinds, imgTexts, imgSrcs []string
			labels                               []string
		)
		if err := q.Scan(&r.URI, &r.IndexedAt, &text, &alts, &domain, &title, &desc, &quote, &tags,
			&kinds, &imgTexts, &imgSrcs, &labels,
			&r.BroadProbs, &r.BroadConfidence, &r.SubProbs, &r.PathScores, &r.Signals, &r.Source, &r.LabelConfig); err != nil {
			return err
		}
		if seen[r.URI] {
			return fmt.Errorf("duplicate uri %s in export; the one-label-per-post rule failed", r.URI)
		}
		seen[r.URI] = true
		in := postdoc.Input{Text: text, MediaAlts: alts, LinkDomain: domain, LinkTitle: title,
			LinkDescription: desc, QuoteText: quote, Tags: tags}
		if full[r.LabelConfig] {
			// Jev saw attachments, image text, and labels for this label: so does the student.
			in.MediaKinds, in.Labels = kinds, labels
			in.AddImageTexts(imgTexts, imgSrcs)
			fullRows++
		}
		r.ModelInput = postdoc.New(in).Student()
		r.WindowID = windows.Of(ws, r.IndexedAt)
		perWindow[r.WindowID]++
		if err := enc.Encode(rowOut(r)); err != nil {
			return err
		}
		n++
	}
	if err := q.Err(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	manifest := map[string]any{
		"created_at":        time.Now().UTC(),
		"taxonomy_version":  tax.Version,
		"taxonomy_hash":     tax.Hash,
		"label_configs":     accepted,
		"full_context":      *fullContext,
		"full_context_rows": fullRows,
		"postdoc_version":   postdoc.Version,
		"rows":              n,
		"rows_per_window":   perWindow,
		"windows_file":      *windowsPath,
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(*out, "manifest.json"), mb, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d rows to %s (%d outside windows)\n", n, *out, perWindow[""])
	return nil
}

type row rowOut
