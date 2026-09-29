package pipeline

import (
	"reflect"
	"testing"
	"time"
)

func TestRepoPolicy(t *testing.T) {
	p, err := LoadPolicy("../../config/label_policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		labels []string
		want   string
	}{
		{nil, PolicyOK},
		{[]string{"!warn"}, PolicyOK}, // not in either list
		{[]string{"porn"}, PolicyAdultOnly},
		{[]string{"sexual", "nudity"}, PolicyAdultOnly},
		{[]string{"porn", "!takedown"}, PolicyDrop}, // drop beats adult_only
		{[]string{"spam"}, PolicyDrop},
		{[]string{"graphic-media"}, PolicyDrop},
	}
	for _, c := range cases {
		if got := p.Decide(c.labels); got != c.want {
			t.Errorf("Decide(%v) = %s, want %s", c.labels, got, c.want)
		}
	}
}

func TestParseTSV(t *testing.T) {
	tsv := "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n" +
		"1\t1\t0\t0\t0\t0\t0\t0\t640\t480\t-1\t\n" +
		"5\t1\t1\t1\t1\t1\t10\t10\t50\t20\t96.5\tVOTE\n" +
		"5\t1\t1\t1\t1\t2\t70\t10\t50\t20\t91.0\tBLUE\n" +
		"5\t1\t1\t1\t1\t3\t130\t10\t20\t20\t95.0\t&\n" + // no letters
		"5\t1\t1\t1\t1\t4\t160\t10\t20\t20\t40.2\tMIDTERMS\n" + // low confidence
		"5\t1\t1\t1\t1\t5\t190\t10\t20\t20\t88.0\tI\n" + // one letter
		"5\t1\t1\t1\t1\t6\t210\t10\t40\t20\t77.0\tMéxico\n"
	got := ParseTSV([]byte(tsv), 70)
	if !reflect.DeepEqual(got, []string{"VOTE", "BLUE", "México"}) {
		t.Errorf("words %v", got)
	}
}

func TestThumbnailURL(t *testing.T) {
	if u := ThumbnailURL("image", "did:plc:abc", "bafyimg"); u != "https://cdn.bsky.app/img/feed_thumbnail/plain/did:plc:abc/bafyimg@jpeg" {
		t.Errorf("image %s", u)
	}
	if u := ThumbnailURL("video", "did:plc:abc", "bafyvid"); u != "https://video.bsky.app/watch/did:plc:abc/bafyvid/thumbnail.jpg" {
		t.Errorf("video %s", u)
	}
}

func TestModelInputAddsImageTextAsAlt(t *testing.T) {
	ps := post{Text: "Denver been wide open two plays in a row", MediaAlts: []string{"Author's own alt"}, Tags: []string{"Broncos"}}
	got, ok := ModelInput(ps, []string{"", "A Broncos receiver catches a pass"})
	want := "Denver been wide open two plays in a row\n[tags] #Broncos\n[alt] Author's own alt | A Broncos receiver catches a pass"
	if !ok || got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if _, ok := ModelInput(post{}, []string{""}); ok {
		t.Error("a post with no content should not be classified")
	}
}

func TestTop(t *testing.T) {
	if k, p := top(map[string]float32{"a": 0.2, "b": 0.7, "c": 0.1}); k != "b" || p != 0.7 {
		t.Errorf("top %s %v", k, p)
	}
	if k, _ := top(map[string]float32{}); k != "" {
		t.Error("empty map")
	}
}

func TestBudget(t *testing.T) {
	b := NewBudget(1.0, 0.9)
	if !b.Allow() {
		t.Fatal("under budget")
	}
	b.Add(0.2)
	if b.Allow() {
		t.Error("over budget should not allow")
	}
	b.day = time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly) // a new day resets
	if !b.Allow() {
		t.Error("new day should allow")
	}
}
