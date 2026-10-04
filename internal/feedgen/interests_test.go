package feedgen

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func TestToSampleTruncates(t *testing.T) {
	long := strings.Repeat("é", 500)
	s := toSample(LikedPost{URI: "at://did:plc:x/app.bsky.feed.post/abc", Text: long, LikedAt: now})
	if n := utf8.RuneCountInString(s.Text); n != maxSampleRunes+1 || !strings.HasSuffix(s.Text, "…") {
		t.Errorf("%d runes", n)
	}
	if !utf8.ValidString(s.Text) {
		t.Error("cut in the middle of a character")
	}
	if s.URL != "https://bsky.app/profile/did:plc:x/post/abc" {
		t.Errorf("url %q", s.URL)
	}
	if bad := toSample(LikedPost{URI: "not a uri", Text: "  a \n b  "}); bad.URL != "" || bad.Text != "a b" {
		t.Errorf("a bad URI gets no link, whitespace is collapsed: %+v", bad)
	}
}

func TestTopicNamesFromTheTaxonomy(t *testing.T) {
	tax, err := taxonomy.Load("../../taxonomy/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	n := TopicNames(tax)
	if got := n["technology/ai"]; got.Name == "" || got.Broad == "" || got.Name == got.Broad {
		t.Errorf("technology/ai: %+v", got)
	}
	for _, b := range tax.Broad {
		for _, s := range b.Subtopics {
			if n[b.ID+"/"+s.ID].Name == "" {
				t.Errorf("%s/%s has no name", b.ID, s.ID)
			}
		}
	}
}
