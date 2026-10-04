package ingest

import (
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/jetstream"
)

const (
	collPost   = "app.bsky.feed.post"
	collLike   = "app.bsky.feed.like"
	collRepost = "app.bsky.feed.repost"
)

// Collections is the Jetstream collection filter for ingest. Account events come
// through regardless of the filter.
var Collections = []string{collPost, collLike, collRepost}

// Outcome of handling one post create, for metrics.
const (
	PostKept           = "kept"
	PostReply          = "reply"
	PostNotTaggedEn    = "not_tagged_en"
	PostDetectorReject = "detector_disagrees"
	PostStale          = "stale"
)

// maxRecordAge is how much older than its event a created record may be. When an
// account's repo is resynced, Jetstream re-sends all of its records as ordinary
// creates, with their original record keys and createdAt. Those (and imports with
// backdated createdAt) would otherwise look like fresh posts and likes.
const maxRecordAge = 24 * time.Hour

// stale reports whether a created record is much older than the event carrying it,
// judged by its record key (a TID, which encodes its creation time) and createdAt.
func stale(rkey, createdAt string, at time.Time) bool {
	cutoff := at.Add(-maxRecordAge)
	if t, ok := tidTime(rkey); ok && t.Before(cutoff) {
		return true
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil && t.Before(cutoff) {
		return true
	}
	return false
}

const tidAlphabet = "234567abcdefghijklmnopqrstuvwxyz"

// tidTime decodes the timestamp in an atproto TID record key: 13 base32-sortable
// characters whose top 53 bits are microseconds since the Unix epoch.
func tidTime(rkey string) (time.Time, bool) {
	if len(rkey) != 13 {
		return time.Time{}, false
	}
	var v uint64
	for i := 0; i < len(rkey); i++ {
		idx := strings.IndexByte(tidAlphabet, rkey[i])
		if idx < 0 {
			return time.Time{}, false
		}
		v = v<<5 | uint64(idx)
	}
	return time.UnixMicro(int64(v >> 10)).UTC(), true
}

// Parser turns Jetstream events into table rows.
type Parser struct {
	Lang *LangChecker
	// Texts, when set, receives the text of every post create (any language, replies
	// included) so quoted posts can be resolved without a database round trip.
	Texts *TextCache
}

// Result describes what Handle did with one event, for metrics.
type Result struct {
	Collection  string // commit collection, or "account"/"identity"/"sync"
	Operation   string // create/update/delete for commits
	PostOutcome string // set for post creates
	Stale       bool   // a created record dropped as much older than its event
}

// Handle appends the rows produced by ev to rows.
func (p *Parser) Handle(ev *jetstream.Event, rows *Rows) Result {
	at := time.UnixMicro(ev.TimeUS).UTC()
	switch ev.Kind {
	case jetstream.KindAccount:
		if a := ev.Account; a != nil {
			rows.Accounts = append(rows.Accounts, AccountRow{DID: ev.DID, Active: b2u(a.Active), Status: a.Status, IndexedAt: at})
		}
		return Result{Collection: "account"}
	case jetstream.KindCommit:
		// handled below
	default:
		return Result{Collection: string(ev.Kind)}
	}

	c := ev.Commit
	if c == nil {
		return Result{}
	}
	res := Result{Collection: c.Collection, Operation: string(c.Operation)}
	uri := "at://" + ev.DID + "/" + c.Collection + "/" + c.Rkey

	switch c.Operation {
	case jetstream.OpDelete:
		switch c.Collection {
		case collPost, collLike, collRepost:
			rows.Deletions = append(rows.Deletions, DeletionRow{DID: ev.DID, Collection: c.Collection, Rkey: c.Rkey, URI: uri, IndexedAt: at})
		}
		return res
	case jetstream.OpCreate:
		// handled below
	default:
		// Updates are rare for these collections and are ignored for now.
		return res
	}

	rec := c.Record
	switch c.Collection {
	case collLike, collRepost:
		if stale(c.Rkey, str(rec, "createdAt"), at) {
			res.Stale = true
			return res
		}
		row := LikeRow{
			ActorDID:   ev.DID,
			Rkey:       c.Rkey,
			SubjectURI: str(mapv(rec, "subject"), "uri"),
			CreatedAt:  parseTime(str(rec, "createdAt"), at),
			IndexedAt:  at,
		}
		if row.SubjectURI == "" {
			return res
		}
		if c.Collection == collLike {
			rows.Likes = append(rows.Likes, row)
		} else {
			rows.Reposts = append(rows.Reposts, row)
		}
	case collPost:
		res.PostOutcome = p.handlePost(ev.DID, c, uri, at, rec, rows)
		res.Stale = res.PostOutcome == PostStale
	}
	return res
}

func (p *Parser) handlePost(did string, c *jetstream.Commit, uri string, at time.Time, rec map[string]any, rows *Rows) string {
	text := str(rec, "text")
	rows.PostTexts = append(rows.PostTexts, PostTextRow{URI: uri, Text: text, IndexedAt: at})
	if p.Texts != nil {
		p.Texts.Put(uri, text)
	}

	// The text is kept above even for stale posts: quotes of old posts still need it.
	if stale(c.Rkey, str(rec, "createdAt"), at) {
		return PostStale
	}
	addRefs(did, uri, at, rec, rows)
	if rec["reply"] != nil {
		return PostReply
	}
	langs := strs(rec, "langs")
	verdict := p.Lang.Check(text, langs)
	if !verdict.Keep {
		return verdict.Reason
	}

	row := PostRow{
		URI:           uri,
		DID:           did,
		Rkey:          c.Rkey,
		CID:           c.CID,
		CreatedAt:     parseTime(str(rec, "createdAt"), at),
		IndexedAt:     at,
		Text:          text,
		Langs:         langs,
		DetectedLang:  verdict.Detected,
		EmbedType:     "none",
		MediaAlts:     []string{},
		Tags:          []string{},
		LinkDomains:   []string{},
		SelfLabels:    []string{},
		MediaKinds:    []string{},
		MediaCIDs:     []string{},
		MediaAltTexts: []string{},
	}
	extractEmbed(mapv(rec, "embed"), &row)
	extractFacets(rec, &row)
	row.SelfLabels = selfLabels(rec)
	if len(row.SelfLabels) > 0 {
		row.HasLabels = 1
	}
	rows.Posts = append(rows.Posts, row)
	return PostKept
}

// extractEmbed fills the embed columns from a post's embed (plan §9.1 step 3).
func extractEmbed(embed map[string]any, row *PostRow) {
	if embed == nil {
		return
	}
	typ := str(embed, "$type")
	switch typ {
	case "app.bsky.embed.images":
		row.EmbedType = "images"
		addImageAlts(embed, row)
	case "app.bsky.embed.video":
		row.EmbedType = "video"
		addVideo(embed, row)
	case "app.bsky.embed.gallery":
		row.EmbedType = "gallery"
		addGalleryImages(embed, row)
	case "app.bsky.embed.external":
		row.EmbedType = "external"
		addExternal(mapv(embed, "external"), row)
	case "app.bsky.embed.record":
		row.EmbedType = "record"
		setQuote(mapv(embed, "record"), row)
	case "app.bsky.embed.recordWithMedia":
		row.EmbedType = "recordWithMedia"
		// The quoted record is nested one level deeper: {record: {record: {uri, cid}}}.
		setQuote(mapv(mapv(embed, "record"), "record"), row)
		media := mapv(embed, "media")
		switch str(media, "$type") {
		case "app.bsky.embed.images":
			addImageAlts(media, row)
		case "app.bsky.embed.video":
			addVideo(media, row)
		case "app.bsky.embed.gallery":
			addGalleryImages(media, row)
		case "app.bsky.embed.external":
			addExternal(mapv(media, "external"), row)
		}
	default:
		if typ != "" {
			row.EmbedType = "other"
		}
	}
}

func addImageAlts(embed map[string]any, row *PostRow) {
	for _, img := range slice(embed, "images") {
		if m, ok := img.(map[string]any); ok {
			addAlt(str(m, "alt"), row)
			addMedia("image", blobCID(mapv(m, "image")), str(m, "alt"), row)
		}
	}
}

// addGalleryImages records the images of an app.bsky.embed.gallery the way addImageAlts records an
// images embed's: same blob, same alt text. A gallery's items are a union of kinds and only the
// image kind exists so far; items of a kind added later are left alone rather than guessed at.
func addGalleryImages(embed map[string]any, row *PostRow) {
	for _, item := range slice(embed, "items") {
		m, ok := item.(map[string]any)
		if !ok || str(m, "$type") != "app.bsky.embed.gallery#image" {
			continue
		}
		addAlt(str(m, "alt"), row)
		addMedia("image", blobCID(mapv(m, "image")), str(m, "alt"), row)
	}
}

func addVideo(embed map[string]any, row *PostRow) {
	addAlt(str(embed, "alt"), row)
	addMedia("video", blobCID(mapv(embed, "video")), str(embed, "alt"), row)
}

// addMedia records one attached image or video. Entries without a blob CID are
// skipped: there is nothing to fetch.
func addMedia(kind, cid, alt string, row *PostRow) {
	if cid == "" {
		return
	}
	row.MediaKinds = append(row.MediaKinds, kind)
	row.MediaCIDs = append(row.MediaCIDs, cid)
	row.MediaAltTexts = append(row.MediaAltTexts, strings.TrimSpace(alt))
}

// blobCID returns a blob's CID: {"ref": {"$link": cid}} in current records, or
// {"cid": cid} in the legacy blob format.
func blobCID(blob map[string]any) string {
	if ref := mapv(blob, "ref"); ref != nil {
		return str(ref, "$link")
	}
	return str(blob, "cid")
}

// selfLabels returns the post's self-label values, deduplicated, in order.
func selfLabels(rec map[string]any) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range slice(mapv(rec, "labels"), "values") {
		if val := strings.TrimSpace(str(asMap(v), "val")); val != "" && !seen[val] {
			seen[val] = true
			out = append(out, val)
		}
	}
	return out
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func addAlt(alt string, row *PostRow) {
	if alt = strings.TrimSpace(alt); alt != "" {
		row.MediaAlts = append(row.MediaAlts, alt)
	}
}

func addExternal(ext map[string]any, row *PostRow) {
	if ext == nil {
		return
	}
	row.LinkURI = str(ext, "uri")
	row.LinkDomain = domainOf(row.LinkURI)
	row.LinkTitle = str(ext, "title")
	row.LinkDescription = str(ext, "description")
}

// addRefs records what a post (any language, replies included) replies to and quotes,
// for engagement counts. Replies are counted for their direct parent. Replies and
// quotes of the author's own posts (threads, self-quotes) are left out.
func addRefs(did, uri string, at time.Time, rec map[string]any, rows *Rows) {
	add := func(kind, subject string) {
		if !strings.Contains(subject, "/"+collPost+"/") || authorOf(subject) == did {
			return
		}
		rows.PostRefs = append(rows.PostRefs, PostRefRow{SubjectURI: subject, Kind: kind, URI: uri, ActorDID: did, IndexedAt: at})
	}
	if reply := mapv(rec, "reply"); reply != nil {
		add("reply", str(mapv(reply, "parent"), "uri"))
	}
	embed := mapv(rec, "embed")
	switch str(embed, "$type") {
	case "app.bsky.embed.record":
		add("quote", str(mapv(embed, "record"), "uri"))
	case "app.bsky.embed.recordWithMedia":
		add("quote", str(mapv(mapv(embed, "record"), "record"), "uri"))
	}
}

// authorOf returns the DID in an at:// URI.
func authorOf(uri string) string {
	rest, _ := strings.CutPrefix(uri, "at://")
	did, _, _ := strings.Cut(rest, "/")
	return did
}

// setQuote records the quoted post's URI. Quotes of other record types (feeds,
// lists, starter packs) are not posts and are skipped.
func setQuote(ref map[string]any, row *PostRow) {
	uri := str(ref, "uri")
	if strings.Contains(uri, "/"+collPost+"/") {
		row.QuoteURI = uri
	}
}

// extractFacets collects hashtags and link domains from facets, plus the record's
// top-level `tags` field.
func extractFacets(rec map[string]any, row *PostRow) {
	seenTag := map[string]bool{}
	addTag := func(t string) {
		t = strings.TrimLeft(strings.TrimSpace(t), "#")
		if t != "" && !seenTag[strings.ToLower(t)] {
			seenTag[strings.ToLower(t)] = true
			row.Tags = append(row.Tags, t)
		}
	}
	seenDomain := map[string]bool{}
	for _, f := range slice(rec, "facets") {
		fm, _ := f.(map[string]any)
		for _, feat := range slice(fm, "features") {
			ft, _ := feat.(map[string]any)
			switch str(ft, "$type") {
			case "app.bsky.richtext.facet#tag":
				addTag(str(ft, "tag"))
			case "app.bsky.richtext.facet#link":
				if d := domainOf(str(ft, "uri")); d != "" && !seenDomain[d] {
					seenDomain[d] = true
					row.LinkDomains = append(row.LinkDomains, d)
				}
			}
		}
	}
	for _, t := range strs(rec, "tags") {
		addTag(t)
	}
}

// domainOf returns the lowercased host of a URL without a leading "www.".
func domainOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// parseTime parses a record's createdAt. Unparseable or missing values fall back to
// the event time; createdAt is stored but never trusted.
func parseTime(s string, fallback time.Time) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return fallback
}

// Helpers for the generic, JSON-shaped records the SDK decodes.

func mapv(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[key].(map[string]any)
	return v
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

func slice(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	v, _ := m[key].([]any)
	return v
}

func strs(m map[string]any, key string) []string {
	out := []string{}
	for _, v := range slice(m, key) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
