package ingest

import (
	"reflect"
	"testing"
	"time"

	"github.com/bluesky-social/jetstream"
)

var testLang = NewLangChecker()

const did = "did:plc:abc"

func commit(coll string, op jetstream.Operation, rkey string, rec map[string]any) *jetstream.Event {
	return &jetstream.Event{
		DID:    did,
		Seq:    1,
		TimeUS: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC).UnixMicro(),
		Kind:   jetstream.KindCommit,
		Commit: &jetstream.Commit{Operation: op, Collection: coll, Rkey: rkey, CID: "bafycid", Record: rec},
	}
}

func post(rec map[string]any) *jetstream.Event {
	if _, ok := rec["langs"]; !ok {
		rec["langs"] = []any{"en"}
	}
	rec["$type"] = collPost
	return commit(collPost, jetstream.OpCreate, "3lpost", rec)
}

func handle(t *testing.T, ev *jetstream.Event) (Rows, Result) {
	t.Helper()
	p := &Parser{Lang: testLang, Texts: NewTextCache(10)}
	var rows Rows
	res := p.Handle(ev, &rows)
	return rows, res
}

func TestTopLevelEnglishPostWithImages(t *testing.T) {
	rows, res := handle(t, post(map[string]any{
		"text":      "The ferry ride this morning was absolutely beautiful, look at this sky",
		"createdAt": "2026-09-27T11:59:58.123Z",
		"embed": map[string]any{
			"$type":  "app.bsky.embed.images",
			"images": []any{map[string]any{"alt": " Orange sky "}, map[string]any{"alt": ""}},
		},
	}))
	if res.PostOutcome != PostKept || len(rows.Posts) != 1 {
		t.Fatalf("outcome %q, %d posts", res.PostOutcome, len(rows.Posts))
	}
	p := rows.Posts[0]
	if p.URI != "at://did:plc:abc/app.bsky.feed.post/3lpost" || p.EmbedType != "images" {
		t.Errorf("uri %q embed %q", p.URI, p.EmbedType)
	}
	if !reflect.DeepEqual(p.MediaAlts, []string{"Orange sky"}) {
		t.Errorf("alts %v", p.MediaAlts)
	}
	if p.CreatedAt.Format(time.RFC3339Nano) != "2026-09-27T11:59:58.123Z" {
		t.Errorf("created_at %v", p.CreatedAt)
	}
	if p.DetectedLang != "en" {
		t.Errorf("detected %q", p.DetectedLang)
	}
	if len(rows.PostTexts) != 1 {
		t.Errorf("post_texts %d", len(rows.PostTexts))
	}
}

func TestRepliesAreRejectedButTextKept(t *testing.T) {
	rows, res := handle(t, post(map[string]any{
		"text":  "totally agree with this take",
		"reply": map[string]any{"root": map[string]any{"uri": "at://x"}, "parent": map[string]any{"uri": "at://x"}},
	}))
	if res.PostOutcome != PostReply || len(rows.Posts) != 0 {
		t.Fatalf("outcome %q, %d posts", res.PostOutcome, len(rows.Posts))
	}
	if len(rows.PostTexts) != 1 {
		t.Error("reply text should still go to post_texts for quote resolution")
	}
}

func TestLanguageFilter(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		langs []any
		want  string
	}{
		{"untagged", "This is a perfectly English sentence about the weather today.", []any{}, PostNotTaggedEn},
		{"tagged ja", "今日はとても良い天気ですね、散歩に行きましょう", []any{"ja"}, PostNotTaggedEn},
		{"en-US tag", "This is a perfectly English sentence about the weather today.", []any{"en-US"}, PostKept},
		{"portuguese tagged en", "Hoje o dia está muito bonito e eu vou passear com o meu cachorro no parque", []any{"en"}, PostDetectorReject},
		{"short text trusts tag", "bom dia!!", []any{"en"}, PostKept},
		{"multi tag with en", "Hoje o dia está muito bonito e eu vou passear com o meu cachorro no parque", []any{"pt", "en"}, PostDetectorReject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, res := handle(t, post(map[string]any{"text": c.text, "langs": c.langs}))
			if res.PostOutcome != c.want {
				t.Errorf("got %q, want %q", res.PostOutcome, c.want)
			}
		})
	}
}

func TestQuoteWithMediaAndFacets(t *testing.T) {
	rows, _ := handle(t, post(map[string]any{
		"text": "This thread about #Transit funding is worth reading https://example.com/a",
		"embed": map[string]any{
			"$type": "app.bsky.embed.recordWithMedia",
			"record": map[string]any{"$type": "app.bsky.embed.record",
				"record": map[string]any{"uri": "at://did:plc:q/app.bsky.feed.post/3lq", "cid": "bafyq"}},
			"media": map[string]any{"$type": "app.bsky.embed.video", "alt": "A bus pulling in"},
		},
		"facets": []any{
			map[string]any{"features": []any{map[string]any{"$type": "app.bsky.richtext.facet#tag", "tag": "Transit"}}},
			map[string]any{"features": []any{map[string]any{"$type": "app.bsky.richtext.facet#link", "uri": "https://WWW.Example.com/a"}}},
		},
		"tags":   []any{"transit", "cities"},
		"labels": map[string]any{"$type": "com.atproto.label.defs#selfLabels", "values": []any{map[string]any{"val": "graphic-media"}}},
	}))
	if len(rows.Posts) != 1 {
		t.Fatalf("%d posts", len(rows.Posts))
	}
	p := rows.Posts[0]
	if p.EmbedType != "recordWithMedia" || p.QuoteURI != "at://did:plc:q/app.bsky.feed.post/3lq" {
		t.Errorf("embed %q quote %q", p.EmbedType, p.QuoteURI)
	}
	if !reflect.DeepEqual(p.MediaAlts, []string{"A bus pulling in"}) {
		t.Errorf("alts %v", p.MediaAlts)
	}
	if !reflect.DeepEqual(p.Tags, []string{"Transit", "cities"}) {
		t.Errorf("tags %v", p.Tags)
	}
	if !reflect.DeepEqual(p.LinkDomains, []string{"example.com"}) {
		t.Errorf("link domains %v", p.LinkDomains)
	}
	if p.HasLabels != 1 {
		t.Error("expected has_labels")
	}
}

func TestExternalLinkAndNonPostQuote(t *testing.T) {
	rows, _ := handle(t, post(map[string]any{
		"text": "New paper on sparse attention kernels is out today, worth a look",
		"embed": map[string]any{"$type": "app.bsky.embed.external",
			"external": map[string]any{"uri": "https://www.arXiv.org/abs/1", "title": "Faster Sparse Attention", "description": "We present..."}},
	}))
	p := rows.Posts[0]
	if p.EmbedType != "external" || p.LinkDomain != "arxiv.org" || p.LinkTitle != "Faster Sparse Attention" {
		t.Errorf("%+v", p)
	}

	rows, _ = handle(t, post(map[string]any{
		"text":  "Check out this feed I made for people who love birds and birdwatching",
		"embed": map[string]any{"$type": "app.bsky.embed.record", "record": map[string]any{"uri": "at://did:plc:f/app.bsky.feed.generator/birds"}},
	}))
	if rows.Posts[0].QuoteURI != "" {
		t.Error("quoting a feed generator is not a post quote")
	}
}

func TestLikesRepostsDeletesAccounts(t *testing.T) {
	var rows Rows
	p := &Parser{Lang: testLang}
	p.Handle(commit(collLike, jetstream.OpCreate, "3llike", map[string]any{
		"subject": map[string]any{"uri": "at://did:plc:x/app.bsky.feed.post/1", "cid": "c"}, "createdAt": "2026-09-27T12:00:00Z"}), &rows)
	p.Handle(commit(collRepost, jetstream.OpCreate, "3lrp", map[string]any{
		"subject": map[string]any{"uri": "at://did:plc:x/app.bsky.feed.post/1", "cid": "c"}, "createdAt": "bogus"}), &rows)
	p.Handle(commit(collLike, jetstream.OpDelete, "3llike", nil), &rows)
	p.Handle(commit("app.bsky.graph.follow", jetstream.OpDelete, "3lf", nil), &rows)
	p.Handle(&jetstream.Event{DID: did, Kind: jetstream.KindAccount, TimeUS: 1, Account: &jetstream.Account{DID: did, Active: false, Status: "deactivated"}}, &rows)

	if len(rows.Likes) != 1 || rows.Likes[0].SubjectURI != "at://did:plc:x/app.bsky.feed.post/1" {
		t.Errorf("likes %+v", rows.Likes)
	}
	if len(rows.Reposts) != 1 || rows.Reposts[0].CreatedAt != rows.Reposts[0].IndexedAt {
		t.Errorf("repost with bad createdAt should fall back to event time: %+v", rows.Reposts)
	}
	if len(rows.Deletions) != 1 || rows.Deletions[0].URI != "at://did:plc:abc/app.bsky.feed.like/3llike" {
		t.Errorf("deletions %+v", rows.Deletions)
	}
	if len(rows.Accounts) != 1 || rows.Accounts[0].Active != 0 || rows.Accounts[0].Status != "deactivated" {
		t.Errorf("accounts %+v", rows.Accounts)
	}
}

func TestTIDTime(t *testing.T) {
	// Encode a known time as a TID and decode it back.
	want := time.Date(2026, 9, 27, 11, 59, 0, 0, time.UTC)
	v := uint64(want.UnixMicro())<<10 | 7 // clock id bits
	b := make([]byte, 13)
	for i := 12; i >= 0; i-- {
		b[i] = tidAlphabet[v&31]
		v >>= 5
	}
	got, ok := tidTime(string(b))
	if !ok || !got.Equal(want) {
		t.Fatalf("tidTime(%s) = %v %v, want %v", b, got, ok, want)
	}
	if _, ok := tidTime("self"); ok {
		t.Error("non-TID rkey should not decode")
	}
}

func tidAt(tm time.Time) string {
	v := uint64(tm.UnixMicro()) << 10
	b := make([]byte, 13)
	for i := 12; i >= 0; i-- {
		b[i] = tidAlphabet[v&31]
		v >>= 5
	}
	return string(b)
}

func TestStaleResyncRecordsAreDropped(t *testing.T) {
	eventTime := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	// A resynced post: old record key and old createdAt, delivered today.
	ev := post(map[string]any{"text": "An old post from last year that is being re-sent by a resync", "createdAt": "2025-01-15T15:05:04Z"})
	ev.Commit.Rkey = tidAt(time.Date(2025, 1, 15, 15, 5, 4, 0, time.UTC))
	rows, res := handle(t, ev)
	if res.PostOutcome != PostStale || len(rows.Posts) != 0 || len(rows.PostTexts) != 1 {
		t.Errorf("resynced post: outcome %q, posts %d, texts %d", res.PostOutcome, len(rows.Posts), len(rows.PostTexts))
	}

	// Fresh key but backdated createdAt (an import) is also dropped.
	ev = post(map[string]any{"text": "An imported post with a backdated creation time from long ago", "createdAt": "2020-05-01T00:00:00Z"})
	ev.Commit.Rkey = tidAt(eventTime.Add(-time.Second))
	if _, res = handle(t, ev); res.PostOutcome != PostStale {
		t.Errorf("backdated post: outcome %q", res.PostOutcome)
	}

	// A normal post a few minutes old is kept.
	ev = post(map[string]any{"text": "A perfectly normal post written a few minutes ago about lunch", "createdAt": "2026-09-27T11:55:00Z"})
	ev.Commit.Rkey = tidAt(eventTime.Add(-5 * time.Minute))
	if _, res = handle(t, ev); res.PostOutcome != PostKept {
		t.Errorf("fresh post: outcome %q", res.PostOutcome)
	}

	// A resynced like is dropped.
	var likeRows Rows
	p := &Parser{Lang: testLang}
	res = p.Handle(commit(collLike, jetstream.OpCreate, tidAt(time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)), map[string]any{
		"subject": map[string]any{"uri": "at://did:plc:x/app.bsky.feed.post/1"}, "createdAt": "2025-03-01T00:00:00Z"}), &likeRows)
	if !res.Stale || len(likeRows.Likes) != 0 {
		t.Errorf("resynced like kept: %+v", likeRows.Likes)
	}
}

func TestTextCacheEvictsOldest(t *testing.T) {
	c := NewTextCache(2)
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("c", "3")
	if _, ok := c.Get("a"); ok {
		t.Error("a should be evicted")
	}
	if v, _ := c.Get("c"); v != "3" || c.Len() != 2 {
		t.Error("c missing or wrong size")
	}
}
