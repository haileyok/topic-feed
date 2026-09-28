// Package taxonomy loads and validates the versioned topic taxonomy (plan §8.2).
package taxonomy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Topic is a broad topic or a subtopic.
type Topic struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Examples    []string `yaml:"examples,omitempty"`
	Subtopics   []Topic  `yaml:"subtopics,omitempty"`
}

// Taxonomy is the two-level topic list.
type Taxonomy struct {
	Version string  `yaml:"version"`
	Broad   []Topic `yaml:"broad"`

	// Hash is a short content hash of the file, part of every label_config.
	Hash string `yaml:"-"`
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// maxOptions keeps every question portable to OpenJev's 52-option single pass.
const maxOptions = 52

// Load reads and validates a taxonomy YAML file.
func Load(path string) (*Taxonomy, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Taxonomy
	if err := yaml.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	t.Hash = hex.EncodeToString(sum[:])[:12]
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

// Validate checks IDs, descriptions, option counts, and that every broad topic with
// subtopics has an "other" subtopic.
func (t *Taxonomy) Validate() error {
	if t.Version == "" {
		return fmt.Errorf("missing version")
	}
	if len(t.Broad) == 0 || len(t.Broad) > maxOptions {
		return fmt.Errorf("need 1..%d broad topics, have %d", maxOptions, len(t.Broad))
	}
	seen := map[string]bool{}
	for _, b := range t.Broad {
		if err := checkTopic(b); err != nil {
			return err
		}
		if seen[b.ID] {
			return fmt.Errorf("duplicate broad topic %q", b.ID)
		}
		seen[b.ID] = true
		if len(b.Subtopics) == 0 {
			continue
		}
		if len(b.Subtopics) > maxOptions {
			return fmt.Errorf("%s: %d subtopics, max %d", b.ID, len(b.Subtopics), maxOptions)
		}
		subSeen := map[string]bool{}
		hasOther := false
		for _, s := range b.Subtopics {
			if err := checkTopic(s); err != nil {
				return fmt.Errorf("%s: %w", b.ID, err)
			}
			if subSeen[s.ID] {
				return fmt.Errorf("%s: duplicate subtopic %q", b.ID, s.ID)
			}
			subSeen[s.ID] = true
			hasOther = hasOther || s.ID == "other"
		}
		if !hasOther {
			return fmt.Errorf("%s: needs an \"other\" subtopic", b.ID)
		}
	}
	return nil
}

func checkTopic(tp Topic) error {
	if !idPattern.MatchString(tp.ID) {
		return fmt.Errorf("bad topic id %q (want snake_case)", tp.ID)
	}
	if tp.Description == "" {
		return fmt.Errorf("topic %q has no description", tp.ID)
	}
	return nil
}

// BroadByID returns a broad topic by ID.
func (t *Taxonomy) BroadByID(id string) (Topic, bool) {
	for _, b := range t.Broad {
		if b.ID == id {
			return b, true
		}
	}
	return Topic{}, false
}
