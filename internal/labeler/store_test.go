package labeler

import (
	"reflect"
	"strings"
	"testing"
)

func TestReadURIList(t *testing.T) {
	in := "# test set\n" +
		"at://did:plc:a/app.bsky.feed.post/1\n" +
		"\n" +
		"at://did:plc:b/app.bsky.feed.post/2\tplain\n" +
		"  at://did:plc:c/app.bsky.feed.post/3 \t full \n" +
		"at://did:plc:a/app.bsky.feed.post/1\tplain\n" // a repeat keeps the first setting
	got, err := ReadURIList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []URIItem{
		{URI: "at://did:plc:a/app.bsky.feed.post/1"},
		{URI: "at://did:plc:b/app.bsky.feed.post/2", Plain: true},
		{URI: "at://did:plc:c/app.bsky.feed.post/3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadURIListRejects(t *testing.T) {
	for name, in := range map[string]string{
		"not a URI":    "https://bsky.app/profile/x/post/1\n",
		"unknown mode": "at://did:plc:a/app.bsky.feed.post/1\tsparse\n",
	} {
		if _, err := ReadURIList(strings.NewReader(in)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
