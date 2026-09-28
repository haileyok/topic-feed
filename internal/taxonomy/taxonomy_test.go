package taxonomy

import (
	"path/filepath"
	"testing"
)

// Every taxonomy committed to the repo must load and validate.
func TestRepoTaxonomiesValid(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "taxonomy", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		tax, err := Load(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		subs := 0
		for _, b := range tax.Broad {
			subs += len(b.Subtopics)
		}
		t.Logf("%s: version %s, %d broad topics, %d subtopics, hash %s", f, tax.Version, len(tax.Broad), subs, tax.Hash)
	}
}
