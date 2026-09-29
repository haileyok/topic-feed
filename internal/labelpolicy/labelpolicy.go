// Package labelpolicy decides from a post's labels whether it may appear in feeds
// (config/label_policy.yaml), and looks up the labels currently in force from the
// labeler streams stored in mod_labels. The pipeline applies it when a post is
// processed; the feed generator applies it again when serving, to catch labels that
// arrived later.
package labelpolicy

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"gopkg.in/yaml.v3"
)

// Feed policy values.
const (
	OK        = "ok"
	AdultOnly = "adult_only"
	Drop      = "drop"
)

// Policy maps label values to a feed policy.
type Policy struct {
	Labelers  []string `yaml:"labelers"`
	AdultOnly []string `yaml:"adult_only"`
	Drop      []string `yaml:"drop"`
}

// Load reads and checks a policy file.
func Load(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Policy
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(p.Labelers) == 0 {
		return nil, fmt.Errorf("%s: no labelers", path)
	}
	for _, v := range p.AdultOnly {
		if slices.Contains(p.Drop, v) {
			return nil, fmt.Errorf("%s: %q is in both adult_only and drop", path, v)
		}
	}
	return &p, nil
}

// Decide returns the feed policy for a post with these label values.
func (p *Policy) Decide(labels []string) string {
	adult := false
	for _, l := range labels {
		if slices.Contains(p.Drop, l) {
			return Drop
		}
		if slices.Contains(p.AdultOnly, l) {
			adult = true
		}
	}
	if adult {
		return AdultOnly
	}
	return OK
}

// Current returns the labels the policy's labelers currently have on each subject (post
// URIs and account DIDs), keyed by subject. The newest row per (labeler, subject, value)
// decides: a removal or an expiry clears the label.
func (p *Policy) Current(ctx context.Context, conn driver.Conn, subjects []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(subjects) == 0 {
		return out, nil
	}
	var rows []struct {
		URI string `ch:"uri"`
		Val string `ch:"val"`
	}
	err := conn.Select(ctx, &rows, `
		SELECT uri, val FROM (
			SELECT uri, val, argMax(tuple(neg, exp), cts) AS last
			FROM mod_labels
			WHERE src IN ? AND uri IN ?
			GROUP BY src, uri, val
		)
		WHERE last.1 = 0 AND (last.2 IS NULL OR last.2 > now64(3))`, p.Labelers, subjects)
	if err != nil {
		return nil, fmt.Errorf("select labels: %w", err)
	}
	for _, r := range rows {
		out[r.URI] = append(out[r.URI], r.Val)
	}
	return out, nil
}
