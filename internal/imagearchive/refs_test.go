package imagearchive

import (
	"reflect"
	"testing"
)

func embed(m map[string]any) map[string]any { return m }

func TestRefsFromEmbed(t *testing.T) {
	uri := "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy123/jpeg"
	tests := []struct {
		name  string
		embed map[string]any
		want  []Ref
	}{
		{
			name: "images",
			embed: map[string]any{
				"$type": "app.bsky.embed.images#view",
				"images": []any{
					map[string]any{"fullsize": "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy111"},
					map[string]any{"fullsize": "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy222"},
				},
			},
			want: []Ref{
				{0, KindImage, "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy111@jpeg", "bafy111"},
				{1, KindImage, "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy222@jpeg", "bafy222"},
			},
		},
		{
			name: "gallery",
			embed: map[string]any{
				"$type": "app.bsky.embed.gallery#view",
				"items": []any{
					map[string]any{"fullsize": "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy333"},
				},
			},
			want: []Ref{{0, KindImage, "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy333@jpeg", "bafy333"}},
		},
		{
			name: "video",
			embed: map[string]any{
				"$type":     "app.bsky.embed.video#view",
				"thumbnail": "https://video.cdn.bsky.app/hls/did:plc:a/bafyvid/thumbnail.jpg",
				"cid":       "bafyvid",
			},
			want: []Ref{{0, KindVideoThumb, "https://video.cdn.bsky.app/hls/did:plc:a/bafyvid/thumbnail.jpg", "bafyvid"}},
		},
		{
			name: "external",
			embed: map[string]any{
				"$type":    "app.bsky.embed.external#view",
				"external": map[string]any{"thumb": "https://cdn.bsky.app/img/feed_thumbnail/plain/did:plc:a/bafyext"},
			},
			want: []Ref{{0, KindLinkCard, "https://cdn.bsky.app/img/feed_thumbnail/plain/did:plc:a/bafyext@jpeg", "bafyext"}},
		},
		{
			name: "recordWithMedia",
			embed: map[string]any{
				"$type":  "app.bsky.embed.recordWithMedia#view",
				"record": map[string]any{"$type": "app.bsky.embed.record#view"}, // the quote's own text; not walked
				"media": map[string]any{
					"$type": "app.bsky.embed.images#view",
					"images": []any{
						map[string]any{"fullsize": "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafyqwm"},
					},
				},
			},
			want: []Ref{{0, KindImage, "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafyqwm@jpeg", "bafyqwm"}},
		},
		{
			name:  "quote only",
			embed: map[string]any{"$type": "app.bsky.embed.record#view", "record": map[string]any{"$type": "app.bsky.embed.record#view"}},
			want:  nil,
		},
		{
			name:  "nil",
			embed: nil,
			want:  nil,
		},
		{
			name:  "empty fullsize",
			embed: map[string]any{"$type": "app.bsky.embed.video#view", "thumbnail": "", "cid": "bafy"},
			want:  nil,
		},
	}
	_ = uri
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RefsFromEmbed(tt.embed)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RefsFromEmbed = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestImageURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy1", "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy1@jpeg"},
		{"https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy1@png", "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy1@png"}, // format already there
		{"", ""},
		{"https://video.cdn.bsky.app/hls/did:plc:a/bafyv/thumbnail.jpg", "https://video.cdn.bsky.app/hls/did:plc:a/bafyv/thumbnail.jpg"}, // not an /img/ link
	}
	for _, tt := range tests {
		if got := imageURL(tt.in); got != tt.want {
			t.Errorf("imageURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCidFromURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy1@jpeg", "bafy1"},
		{"https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy1", "bafy1"},
		{"https://video.cdn.bsky.app/hls/did:plc:a/bafyv/thumbnail.jpg", "thumbnail.jpg"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := cidFromURL(tt.in); got != tt.want {
			t.Errorf("cidFromURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLabelValues(t *testing.T) {
	v := PostView{}
	v.Labels = []Label{{Val: "porn"}, {Val: "porn"}, {Val: "gore", Neg: true}, {Val: ""}}
	v.Author.Labels = []Label{{Val: "spam"}, {Val: "gore"}}
	got := v.LabelValues()
	want := []string{"porn", "spam", "gore"} // dedupe across post and author, neg skipped, empty skipped
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LabelValues = %v, want %v", got, want)
	}
}
