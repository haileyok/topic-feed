// Package pipeline processes live posts shortly after ingest: it applies the label
// policy, finds text in images (tesseract first, then an LLM description), and records
// the result in post_pipeline.
package pipeline

import (
	"fmt"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// Feed policy values.
const (
	PolicyOK        = "ok"
	PolicyAdultOnly = "adult_only"
	PolicyDrop      = "drop"
)

// Policy maps label values to a feed policy (config/label_policy.yaml).
type Policy struct {
	Labelers  []string `yaml:"labelers"`
	AdultOnly []string `yaml:"adult_only"`
	Drop      []string `yaml:"drop"`
}

// LoadPolicy reads and checks a policy file.
func LoadPolicy(path string) (*Policy, error) {
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
			return PolicyDrop
		}
		if slices.Contains(p.AdultOnly, l) {
			adult = true
		}
	}
	if adult {
		return PolicyAdultOnly
	}
	return PolicyOK
}
