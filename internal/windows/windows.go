// Package windows loads the labeling windows (plan §10.1) from
// config/labeling_windows.yaml. The labeler labels every post in them, and the
// trainer splits training data by them.
package windows

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Window is one labeling window: posts with Start <= indexed_at < End.
type Window struct {
	ID    string
	Start time.Time
	End   time.Time
}

type file struct {
	Seed    int64 `yaml:"seed"`
	Windows []struct {
		ID    string `yaml:"id"`
		Start string `yaml:"start"`
		End   string `yaml:"end"`
	} `yaml:"windows"`
}

// Load reads and validates the windows file: IDs unique, times ordered, no overlaps.
func Load(path string) ([]Window, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f file
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []Window
	seen := map[string]bool{}
	for _, w := range f.Windows {
		s, err1 := time.Parse(time.RFC3339, w.Start)
		e, err2 := time.Parse(time.RFC3339, w.End)
		if err1 != nil || err2 != nil || !e.After(s) || w.ID == "" || seen[w.ID] {
			return nil, fmt.Errorf("%s: bad window %+v", path, w)
		}
		seen[w.ID] = true
		out = append(out, Window{ID: w.ID, Start: s.UTC(), End: e.UTC()})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no windows", path)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	for i := 1; i < len(out); i++ {
		if out[i].Start.Before(out[i-1].End) {
			return nil, fmt.Errorf("%s: windows %s and %s overlap", path, out[i-1].ID, out[i].ID)
		}
	}
	return out, nil
}

// Of returns the ID of the window containing t, or "" if none.
func Of(ws []Window, t time.Time) string {
	for _, w := range ws {
		if !t.Before(w.Start) && t.Before(w.End) {
			return w.ID
		}
	}
	return ""
}
