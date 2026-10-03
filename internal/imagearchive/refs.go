// Package imagearchive downloads the pictures of posts once and keeps them on disk, so labeling
// and training can read the same pixels as often as they need. It works in three steps (run by
// cmd/images): resolve asks the AppView where each post's pictures are and what its labels say,
// fetch downloads them into a content-addressed directory, and purge removes the pictures of
// posts that have since been deleted.
package imagearchive

import (
	"slices"
	"strings"
)

// Kinds of picture the archive keeps.
const (
	KindImage      = "image"       // an attached image or gallery item, at full size
	KindVideoThumb = "video_thumb" // a video's poster frame
	KindLinkCard   = "link_card"   // the preview picture of a link card
)

// Ref is one picture of a post, as the AppView's post view names it.
type Ref struct {
	Idx  int
	Kind string
	URL  string // CDN link to fetch
	CID  string // blob CID the link carries; for a video thumbnail, the video's CID
}

// Label is a label on a post or an account, as the AppView shows it.
type Label struct {
	Val string `json:"val"`
	Neg bool   `json:"neg,omitempty"`
}

// PostView is the part of an app.bsky.feed.defs#postView the archive reads.
type PostView struct {
	URI    string `json:"uri"`
	Author struct {
		DID    string  `json:"did"`
		Labels []Label `json:"labels"`
	} `json:"author"`
	Labels []Label        `json:"labels"`
	Embed  map[string]any `json:"embed"`
}

// LabelValues returns the distinct label values in force on the post and on its author, in
// order. Self-labels are among them, as they are for the pipeline.
func (v PostView) LabelValues() []string {
	var out []string
	for _, group := range [][]Label{v.Labels, v.Author.Labels} {
		for _, l := range group {
			if l.Neg || l.Val == "" || slices.Contains(out, l.Val) {
				continue
			}
			out = append(out, l.Val)
		}
	}
	return out
}

// RefsFromEmbed lists a post's pictures: the images of an images or gallery embed, a video's
// thumbnail, a link card's preview picture, and the same for the media of a quote post with
// media. A quoted post's own pictures belong to that post and are not listed.
func RefsFromEmbed(embed map[string]any) []Ref {
	var refs []Ref
	add := func(kind, url, cid string) {
		if url == "" {
			return
		}
		refs = append(refs, Ref{Idx: len(refs), Kind: kind, URL: url, CID: cid})
	}
	var walk func(e map[string]any)
	walk = func(e map[string]any) {
		switch str(e, "$type") {
		case "app.bsky.embed.images#view":
			for _, it := range list(e, "images") {
				full := imageURL(str(asMap(it), "fullsize"))
				add(KindImage, full, cidFromURL(full))
			}
		case "app.bsky.embed.gallery#view":
			for _, it := range list(e, "items") {
				full := imageURL(str(asMap(it), "fullsize"))
				add(KindImage, full, cidFromURL(full))
			}
		case "app.bsky.embed.video#view":
			add(KindVideoThumb, str(e, "thumbnail"), str(e, "cid"))
		case "app.bsky.embed.external#view":
			thumb := imageURL(str(asMap(e["external"]), "thumb"))
			add(KindLinkCard, thumb, cidFromURL(thumb))
		case "app.bsky.embed.recordWithMedia#view":
			walk(asMap(e["media"]))
		}
	}
	walk(embed)
	return refs
}

// imageURL returns the link to fetch for a CDN image link. The view's links carry no format,
// and the pipeline asks the CDN for JPEG, so the archive does too.
func imageURL(u string) string {
	if u == "" || strings.Contains(u, "@") || !strings.Contains(u, "/img/") {
		return u
	}
	return u + "@jpeg"
}

// cidFromURL returns the blob CID at the end of a CDN image link.
func cidFromURL(u string) string {
	i := strings.LastIndex(u, "/")
	if i < 0 {
		return ""
	}
	s := u[i+1:]
	if j := strings.Index(s, "@"); j >= 0 {
		s = s[:j]
	}
	return s
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func list(m map[string]any, key string) []any {
	l, _ := m[key].([]any)
	return l
}
