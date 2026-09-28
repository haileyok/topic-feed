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
	"strings"
	"unicode"
)

// Version identifies the rendering rules. Bump it whenever either rendering changes.
const Version = "pd1"

const (
	maxLinkDescriptionRunes = 300
	maxQuoteRunes           = 500
)

// Input holds the columns of a posts row that the post document uses.
type Input struct {
	Text            string
	MediaAlts       []string // image and video alt text
	LinkDomain      string
	LinkTitle       string
	LinkDescription string
	QuoteText       string
	Tags            []string
}

// Link is a post's link card.
type Link struct {
	Domain      string `json:"domain,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// Doc is the normalized post document. Build it with New.
type Doc struct {
	Text    string
	AltText []string
	Link    *Link
	Quote   string
	Tags    []string
}

// JevPost is one entry of the "posts" array in a Jev request's state. Empty fields are
// omitted. Field order is fixed by the struct, so the JSON is deterministic.
type JevPost struct {
	Text    string   `json:"text,omitempty"`
	AltText []string `json:"alt_text,omitempty"`
	Link    *Link    `json:"link,omitempty"`
	Quote   string   `json:"quote,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// New normalizes a posts row into a Doc.
//   - The post text is trimmed; its internal line breaks are kept.
//   - Alt text, link fields, and quoted text are collapsed to single-line text.
//   - Empty alt texts are dropped.
//   - The link description is truncated to 300 characters and quoted text to 500.
//   - Tags lose any leading '#' and are deduplicated case-insensitively, keeping the
//     first spelling seen.
func New(in Input) Doc {
	d := Doc{Text: strings.TrimSpace(in.Text)}

	for _, a := range in.MediaAlts {
		if a = oneLine(a); a != "" {
			d.AltText = append(d.AltText, a)
		}
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

// Empty reports whether the post has no text and no alt text. Such posts are still
// labeled; "unclear" is the expected answer.
func (d Doc) Empty() bool {
	return d.Text == "" && len(d.AltText) == 0
}

// Jev renders the document as one entry of the state's "posts" array.
func (d Doc) Jev() JevPost {
	return JevPost{
		Text:    d.Text,
		AltText: d.AltText,
		Link:    d.Link,
		Quote:   d.Quote,
		Tags:    d.Tags,
	}
}

// Student renders the document as the student classifier's input string:
//
//	{text}
//	[tags] #tag1 #tag2
//	[alt] {alt 1} | {alt 2}
//	[link] {domain} | {title} | {description}
//	[quote] {quote text}
//
// Lines for empty fields are left out, as are empty parts of the link line.
func (d Doc) Student() string {
	var lines []string
	if d.Text != "" {
		lines = append(lines, d.Text)
	}
	if len(d.Tags) > 0 {
		tags := make([]string, len(d.Tags))
		for i, t := range d.Tags {
			tags[i] = "#" + t
		}
		lines = append(lines, "[tags] "+strings.Join(tags, " "))
	}
	if len(d.AltText) > 0 {
		lines = append(lines, "[alt] "+strings.Join(d.AltText, " | "))
	}
	if d.Link != nil {
		var parts []string
		for _, p := range []string{d.Link.Domain, d.Link.Title, d.Link.Description} {
			if p != "" {
				parts = append(parts, p)
			}
		}
		lines = append(lines, "[link] "+strings.Join(parts, " | "))
	}
	if d.Quote != "" {
		lines = append(lines, "[quote] "+d.Quote)
	}
	return strings.Join(lines, "\n")
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
