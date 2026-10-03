package pipeline

import (
	"reflect"
	"testing"
)

func TestThumbnailURL(t *testing.T) {
	if u := ThumbnailURL("image", "did:plc:abc", "bafyimg"); u != "https://cdn.bsky.app/img/feed_thumbnail/plain/did:plc:abc/bafyimg@jpeg" {
		t.Errorf("image %s", u)
	}
	if u := ThumbnailURL("video", "did:plc:abc", "bafyvid"); u != "https://video.bsky.app/watch/did:plc:abc/bafyvid/thumbnail.jpg" {
		t.Errorf("video %s", u)
	}
}

func TestModelInput(t *testing.T) {
	ps := post{Text: "Denver been wide open two plays in a row", MediaAlts: []string{"Author's own alt"}, Tags: []string{"Broncos"},
		MediaKinds: []string{"image", "image", "image"}}
	r := Row{Labels: []string{"graphic-media", "!hide"}}
	// The pictures go to the model separately: no text read from them is added to the document.
	got, ok := ModelInput(ps, r, true)
	want := "Denver been wide open two plays in a row\n[tags] #Broncos\n[media] 3 images\n[labels] graphic-media\n[alt] Author's own alt"
	if !ok || got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	// Attachments and labels alone are not content, unless the model gets to look at a picture.
	bare := post{MediaKinds: []string{"image"}}
	if _, ok := ModelInput(bare, Row{Labels: []string{"porn"}}, false); ok {
		t.Error("attachments and labels alone are not content to classify")
	}
	got, ok = ModelInput(bare, Row{}, true)
	if !ok || got != "[media] 1 image" {
		t.Errorf("a post with only a picture is classified from the picture: %q %v", got, ok)
	}
}

func TestPictureRefsTakeTheFirstAttachmentsInOrder(t *testing.T) {
	// A carousel (gallery embed) and a quote post's media reach ingest as one media list, like images do.
	ps := post{MediaCIDs: []string{"a", "b", "c"}, MediaKinds: []string{"image", "video", "image"}}
	got := pictureRefs(ps, 2)
	want := []pictureRef{{"image", "a"}, {"video", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := pictureRefs(ps, 0); len(got) != 0 {
		t.Errorf("a model that looks at no pictures: %v", got)
	}
	// Kinds missing for an attachment default to an image.
	if got := pictureRefs(post{MediaCIDs: []string{"x"}}, 2); !reflect.DeepEqual(got, []pictureRef{{"image", "x"}}) {
		t.Errorf("default kind: %v", got)
	}
	if got := pictureRefs(post{}, 2); len(got) != 0 {
		t.Errorf("no media: %v", got)
	}
}

func TestChunkItemsKeepsRequestsSmall(t *testing.T) {
	pic := make([]byte, 5<<20)
	items := []Item{{Text: "a"}, {Text: "b", Pictures: [][]byte{pic}}, {Text: "c", Pictures: [][]byte{pic}}, {Text: "d", Pictures: [][]byte{pic, pic}}, {Text: "e"}}
	// 5 + 5 MB fit one request (limit 12 MB), the next 10 MB item starts another.
	want := [][2]int{{0, 3}, {3, 5}}
	if got := chunkItems(items); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// One post alone over the limit still goes out, in its own request.
	big := []Item{{Text: "x", Pictures: [][]byte{make([]byte, 13<<20)}}, {Text: "y"}}
	if got := chunkItems(big); !reflect.DeepEqual(got, [][2]int{{0, 1}, {1, 2}}) {
		t.Errorf("oversized item: %v", got)
	}
	// Many text-only posts are split by count.
	many := make([]Item, maxRequestPosts*2+1)
	if got := chunkItems(many); len(got) != 3 || got[0] != [2]int{0, maxRequestPosts} {
		t.Errorf("by count: %v", got)
	}
	if got := chunkItems(nil); len(got) != 0 {
		t.Errorf("no items: %v", got)
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
