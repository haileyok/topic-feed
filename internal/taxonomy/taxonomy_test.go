package taxonomy

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every taxonomy committed to the repo must load and validate.
func TestRepoTaxonomiesValid(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "taxonomy", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		// <version>-equivalences.yaml files hold scoring rules for a taxonomy, not a taxonomy.
		if strings.HasSuffix(f, "-equivalences.yaml") {
			continue
		}
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
