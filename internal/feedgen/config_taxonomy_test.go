package feedgen

import (
	"testing"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// The live classifier predicts taxonomy v2.1, so every topic a feed asks for in config/feeds.yaml
// has to exist in it. (v2.1 keeps all of v1's topic and subtopic IDs, so older feeds stay valid.)
func TestFeedsConfigUsesTopicsTheLiveTaxonomyHas(t *testing.T) {
	tax, err := taxonomy.Load("../../taxonomy/v2.1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig("../../config/feeds.yaml", tax)
	if err != nil {
		t.Fatalf("config/feeds.yaml against taxonomy v2.1: %v", err)
	}
	if len(cfg.Feeds) == 0 {
		t.Fatal("no feeds loaded")
	}
}
