// Package postdoc builds the one canonical representation of a post's content and
// renders it for both consumers: Jev (one entry of the state's "posts" array) and the
// student classifier (a single input string).
//
// Nothing else in the codebase may build either format. Jev and the student must see
// the same content, or the student learns to guess from information it never gets.
// Any change to the output of either rendering must bump Version, which is recorded in
// every label_config and in every model's config.json.
package postdoc

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Version identifies the rendering rules. Bump it whenever either rendering changes.
//
//   - pd1: text, alt text, link card, quoted text, tags.
//   - pd2: adds text read from images (OCR), descriptions of images written by an LLM,
//     the attachments ("2 images, 1 video"), and labels. A post without any of these
//     renders exactly as in pd1, so pd1 labels stay valid pd2 training data.
const Version = "pd2"

// Versions lists the renderings a classifier model may expect. The pipeline renders
// pd1 for older models (text found in images folded into the alt text, as pd1 models
// saw it in production) and pd2 otherwise.
var Versions = []string{"pd1", "pd2"}

const (
	maxLinkDescriptionRunes = 300
	maxQuoteRunes           = 500
	maxImageTextRunes       = 300
)

// Input holds the post's content: the columns of a posts row, plus what the pipeline
// found for its attachments and the labels on it.
type Input struct {
	Text            string
	MediaAlts       []string // image and video alt text written by the author
	LinkDomain      string
	LinkTitle       string
	LinkDescription string
	QuoteText       string
	Tags            []string

	// pd2
	ImageText         []string // words read from attachments without alt text (OCR)
	ImageDescriptions []string // LLM descriptions of attachments without alt text
	MediaKinds        []string // one entry per attachment: "image", "video"
	Labels            []string // self-labels and moderation labels on the post and its author
}

// AddImageTexts adds the text the pipeline found for attachments (post_pipeline's
// image_texts and image_text_sources, index-aligned): source "ocr" is text read from
// the image, source "luna" an LLM description. Other sources carry no text.
func (in *Input) AddImageTexts(texts, sources []string) {
	for i, t := range texts {
		if i >= len(sources) {
			break
		}
		switch sources[i] {
		case "ocr":
			in.ImageText = append(in.ImageText, t)
		case "luna":
			in.ImageDescriptions = append(in.ImageDescriptions, t)
		}
	}
}

// Link is a post's link card.
type Link struct {
	Domain      string `json:"domain,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// Doc is the normalized post document. Build it with New.
type Doc struct {
	Text              string
	AltText           []string
	ImageText         []string
	ImageDescriptions []string
	Media             string // "2 images, 1 video"; empty without attachments
	Labels            []string
	Link              *Link
	Quote             string
	Tags              []string
}

// JevPost is one entry of the "posts" array in a Jev request's state. Empty fields are
// omitted. Field order is fixed by the struct, so the JSON is deterministic.
type JevPost struct {
	Text              string   `json:"text,omitempty"`
	AltText           []string `json:"alt_text,omitempty"`
	Media             string   `json:"attachments,omitempty"`
	TextInImages      []string `json:"text_in_images,omitempty"`
	ImageDescriptions []string `json:"image_descriptions,omitempty"`
	Labels            []string `json:"labels,omitempty"`
	Link              *Link    `json:"link,omitempty"`
	Quote             string   `json:"quote,omitempty"`
	Tags              []string `json:"tags,omitempty"`
}

// New normalizes a post into a Doc.
//   - The post text is trimmed; its internal line breaks are kept.
//   - Alt text, image text, link fields, and quoted text are collapsed to single-line text.
//   - Empty alt texts and image texts are dropped.
//   - The link description and each image text are truncated to 300 characters, quoted
//     text to 500.
//   - Tags lose any leading '#' and are deduplicated case-insensitively, keeping the
//     first spelling seen.
//   - Labels are deduplicated; moderation actions ("!takedown", "!hide", ...) and
//     "needs-review" are left out, since they describe moderation state, not content.
func New(in Input) Doc {
	d := Doc{Text: strings.TrimSpace(in.Text)}

	d.AltText = lines(in.MediaAlts, 0)
	d.ImageText = lines(in.ImageText, maxImageTextRunes)
	d.ImageDescriptions = lines(in.ImageDescriptions, maxImageTextRunes)
	d.Media = mediaSummary(in.MediaKinds)

	for _, l := range in.Labels {
		l = oneLine(l)
		if l == "" || strings.HasPrefix(l, "!") || l == "needs-review" || slices.Contains(d.Labels, l) {
			continue
		}
		d.Labels = append(d.Labels, l)
	}

	link := Link{
		Domain:      oneLine(in.LinkDomain),
		Title:       oneLine(in.LinkTitle),
		Description: truncate(oneLine(in.LinkDescription), maxLinkDescriptionRunes),
	}
	if link != (Link{}) {
		d.Link = &link
	}

	d.Quote = truncate(oneLine(in.QuoteText), maxQuoteRunes)

	seen := map[string]bool{}
	for _, t := range in.Tags {
		t = strings.TrimLeft(oneLine(t), "#")
		key := strings.ToLower(t)
		if t == "" || seen[key] {
			continue
		}
		seen[key] = true
		d.Tags = append(d.Tags, t)
	}
	return d
}

// Empty reports whether the post has no text, no alt text, and no text found in its
// images. Such posts are not classified.
func (d Doc) Empty() bool {
	return d.Text == "" && len(d.AltText) == 0 && len(d.ImageText) == 0 && len(d.ImageDescriptions) == 0
}

// Jev renders the document as one entry of the state's "posts" array.
func (d Doc) Jev() JevPost {
	return JevPost{
		Text:              d.Text,
		AltText:           d.AltText,
		Media:             d.Media,
		TextInImages:      d.ImageText,
		ImageDescriptions: d.ImageDescriptions,
		Labels:            d.Labels,
		Link:              d.Link,
		Quote:             d.Quote,
		Tags:              d.Tags,
	}
}

// Student renders the document as the student classifier's input string:
//
//	{text}
//	[tags] #tag1 #tag2
//	[media] 2 images, 1 video
//	[labels] porn, nudity
//	[alt] {alt 1} | {alt 2}
//	[image text] {words read from image 1} | {image 2}
//	[image description] {description 1} | {description 2}
//	[link] {domain} | {title} | {description}
//	[quote] {quote text}
//
// Lines for empty fields are left out, as are empty parts of the link line. The short
// media and labels lines come early so truncation to the model's length cuts the
// longer fields first.
func (d Doc) Student() string {
	var out []string
	if d.Text != "" {
		out = append(out, d.Text)
	}
	if len(d.Tags) > 0 {
		tags := make([]string, len(d.Tags))
		for i, t := range d.Tags {
			tags[i] = "#" + t
		}
		out = append(out, "[tags] "+strings.Join(tags, " "))
	}
	if d.Media != "" {
		out = append(out, "[media] "+d.Media)
	}
	if len(d.Labels) > 0 {
		out = append(out, "[labels] "+strings.Join(d.Labels, ", "))
	}
	if len(d.AltText) > 0 {
		out = append(out, "[alt] "+strings.Join(d.AltText, " | "))
	}
	if len(d.ImageText) > 0 {
		out = append(out, "[image text] "+strings.Join(d.ImageText, " | "))
	}
	if len(d.ImageDescriptions) > 0 {
		out = append(out, "[image description] "+strings.Join(d.ImageDescriptions, " | "))
	}
	if d.Link != nil {
		var parts []string
		for _, p := range []string{d.Link.Domain, d.Link.Title, d.Link.Description} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		out = append(out, "[link] "+strings.Join(parts, " | "))
	}
	if d.Quote != "" {
		out = append(out, "[quote] "+d.Quote)
	}
	return strings.Join(out, "\n")
}

// lines collapses each entry to one line, truncates it to max characters (0: no
// limit), and drops empty entries.
func lines(in []string, max int) []string {
	var out []string
	for _, s := range in {
		if s = oneLine(s); s != "" {
			if max > 0 {
				s = truncate(s, max)
			}
			out = append(out, s)
		}
	}
	return out
}

// mediaSummary counts attachments by kind, in order of first appearance:
// ["image", "image", "video"] -> "2 images, 1 video".
func mediaSummary(kinds []string) string {
	var order []string
	count := map[string]int{}
	for _, k := range kinds {
		if k = oneLine(k); k == "" {
			continue
		}
		if count[k] == 0 {
			order = append(order, k)
		}
		count[k]++
	}
	parts := make([]string, len(order))
	for i, k := range order {
		if count[k] == 1 {
			parts[i] = "1 " + k
		} else {
			parts[i] = fmt.Sprintf("%d %ss", count[k], k)
		}
	}
	return strings.Join(parts, ", ")
}

// oneLine trims s and collapses every run of whitespace (including line breaks) into
// a single space.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// truncate shortens s to at most max characters, ending with "…" when it cuts.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRightFunc(string(r[:max-1]), unicode.IsSpace) + "…"
}
