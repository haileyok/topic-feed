package ingest

import "time"

// Row types mirror the ClickHouse tables in schema/001_init.sql. The `ch` tags are
// the column names used by clickhouse-go's AppendStruct.

type PostRow struct {
	URI             string    `ch:"uri"`
	DID             string    `ch:"did"`
	Rkey            string    `ch:"rkey"`
	CID             string    `ch:"cid"`
	CreatedAt       time.Time `ch:"created_at"`
	IndexedAt       time.Time `ch:"indexed_at"`
	Text            string    `ch:"text"`
	Langs           []string  `ch:"langs"`
	DetectedLang    string    `ch:"detected_lang"`
	EmbedType       string    `ch:"embed_type"`
	MediaAlts       []string  `ch:"media_alts"`
	LinkURI         string    `ch:"link_uri"`
	LinkDomain      string    `ch:"link_domain"`
	LinkTitle       string    `ch:"link_title"`
	LinkDescription string    `ch:"link_description"`
	QuoteURI        string    `ch:"quote_uri"`
	QuoteText       string    `ch:"quote_text"`
	Tags            []string  `ch:"tags"`
	LinkDomains     []string  `ch:"link_domains"`
	HasLabels       uint8     `ch:"has_labels"`
	// Self-label values the author set on the post (e.g. porn, sexual, nudity, graphic-media).
	SelfLabels []string `ch:"self_labels"`
	// One entry per attached image or video, aligned across the three columns: kind
	// ("image" or "video"), blob CID (the CDN URL is built from the author DID and this),
	// and its alt text ("" when the author wrote none).
	MediaKinds    []string `ch:"media_kinds"`
	MediaCIDs     []string `ch:"media_cids"`
	MediaAltTexts []string `ch:"media_alt_texts"`
}

type PostTextRow struct {
	URI       string    `ch:"uri"`
	Text      string    `ch:"text"`
	IndexedAt time.Time `ch:"indexed_at"`
}

// LikeRow is used for both the likes and reposts tables, which share a shape.
type LikeRow struct {
	ActorDID   string    `ch:"actor_did"`
	Rkey       string    `ch:"rkey"`
	SubjectURI string    `ch:"subject_uri"`
	CreatedAt  time.Time `ch:"created_at"`
	IndexedAt  time.Time `ch:"indexed_at"`
}

// PostRefRow is a post replying to or quoting another post (table post_refs).
type PostRefRow struct {
	SubjectURI string    `ch:"subject_uri"` // the post replied to or quoted
	Kind       string    `ch:"kind"`        // reply | quote
	URI        string    `ch:"uri"`         // the replying or quoting post
	ActorDID   string    `ch:"actor_did"`
	IndexedAt  time.Time `ch:"indexed_at"`
}

type DeletionRow struct {
	DID        string    `ch:"did"`
	Collection string    `ch:"collection"`
	Rkey       string    `ch:"rkey"`
	URI        string    `ch:"uri"`
	IndexedAt  time.Time `ch:"indexed_at"`
}

type AccountRow struct {
	DID       string    `ch:"did"`
	Active    uint8     `ch:"active"`
	Status    string    `ch:"status"`
	IndexedAt time.Time `ch:"indexed_at"`
}

// Rows is everything parsed from a run of events, waiting to be written.
type Rows struct {
	Posts     []PostRow
	PostTexts []PostTextRow
	Likes     []LikeRow
	Reposts   []LikeRow
	PostRefs  []PostRefRow
	Deletions []DeletionRow
	Accounts  []AccountRow
}

func (r *Rows) Len() int {
	return len(r.Posts) + len(r.PostTexts) + len(r.Likes) + len(r.Reposts) + len(r.PostRefs) + len(r.Deletions) + len(r.Accounts)
}

func (r *Rows) Reset() {
	r.Posts = r.Posts[:0]
	r.PostTexts = r.PostTexts[:0]
	r.Likes = r.Likes[:0]
	r.Reposts = r.Reposts[:0]
	r.PostRefs = r.PostRefs[:0]
	r.Deletions = r.Deletions[:0]
	r.Accounts = r.Accounts[:0]
}
