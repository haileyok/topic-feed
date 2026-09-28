package postdoc

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run `go test ./internal/postdoc -update` to rewrite the golden files after an
// intentional rendering change, and bump Version in the same commit.
var update = flag.Bool("update", false, "rewrite golden files")

type goldenCase struct {
	Name  string `json:"name"`
	Input Input  `json:"input"`
}

func TestGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			d := New(c.Input)

			jev, err := json.MarshalIndent(d.Jev(), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, filepath.Join("testdata", "golden", c.Name+".jev.json"), append(jev, '\n'))
			checkGolden(t, filepath.Join("testdata", "golden", c.Name+".student.txt"), []byte(d.Student()+"\n"))
		})
	}
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s (run with -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed; if intentional, run -update and bump Version.\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestTruncation(t *testing.T) {
	d := New(Input{LinkDomain: "x.com", LinkDescription: strings.Repeat("a", 400), QuoteText: strings.Repeat("é", 600)})
	if n := len([]rune(d.Link.Description)); n != maxLinkDescriptionRunes {
		t.Errorf("description has %d runes, want %d", n, maxLinkDescriptionRunes)
	}
	if n := len([]rune(d.Quote)); n != maxQuoteRunes {
		t.Errorf("quote has %d runes, want %d", n, maxQuoteRunes)
	}
	if !strings.HasSuffix(d.Quote, "…") {
		t.Error("truncated quote should end with an ellipsis")
	}
}

func TestEmpty(t *testing.T) {
	if !New(Input{Text: "  ", MediaAlts: []string{"", " "}}).Empty() {
		t.Error("whitespace-only post should be empty")
	}
	if New(Input{MediaAlts: []string{"a cat"}}).Empty() {
		t.Error("post with alt text is not empty")
	}
}
