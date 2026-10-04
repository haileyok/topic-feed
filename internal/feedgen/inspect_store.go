package feedgen

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// StoredPost is the post as ingest stored it.
type StoredPost struct {
	CID          string    `json:"cid"`
	CreatedAt    time.Time `json:"createdAt"` // what the author's client said; not trusted
	IndexedAt    time.Time `json:"indexedAt"` // when we saw it
	Text         string    `json:"text"`
	Langs        []string  `json:"langs"`
	DetectedLang string    `json:"detectedLang"`
	EmbedType    string    `json:"embedType"`
	LinkURI      string    `json:"linkUri,omitempty"`
	LinkDomain   string    `json:"linkDomain,omitempty"`
	LinkTitle    string    `json:"linkTitle,omitempty"`
	QuoteURI     string    `json:"quoteUri,omitempty"`
	QuoteText    string    `json:"quoteText,omitempty"`
	SelfLabels   []string  `json:"selfLabels"`
	Tags         []string  `json:"tags"`
	MediaKinds   []string  `json:"mediaKinds"`
	MediaCIDs    []string  `json:"-"`
}

// PipelineRow is what the pipeline made of the post: the model's scores and the label policy's decision.
type PipelineRow struct {
	IndexedAt      time.Time          `json:"indexedAt"`
	ProcessedAt    time.Time          `json:"processedAt"`
	FeedPolicy     string             `json:"feedPolicy"`
	Labels         []string           `json:"labels"`
	Model          string             `json:"model"`
	BroadProbs     map[string]float32 `json:"broadProbs"`
	PathProbs      map[string]float32 `json:"pathProbs"`
	Signals        map[string]float32 `json:"signals"`
	Tone           map[string]float32 `json:"tone"`
	TopBroad       string             `json:"topBroad"`
	TopPath        string             `json:"topPath"`
	TopPathP       float32            `json:"topPathP"`
	PicturesWanted uint8              `json:"picturesWanted"`
	PicturesUsed   uint8              `json:"picturesUsed"`
}

// PostFacts is everything stored about one post. A part that isn't there is nil or empty: a post may
// be seen and not stored (a reply), or stored and never processed.
type PostFacts struct {
	URI    string
	DID    string
	Stored *StoredPost
	// Pipeline is nil for a post the pipeline never processed.
	Pipeline *PipelineRow
	Post     Post // its engagement so far (the other fields are filled from Pipeline)
	// SeenText is whether we hold the post's text in post_texts, which is kept for every post we
	// see (replies and other languages included) for a week, and is how a post we don't store is told
	// from one we never saw.
	SeenText       bool
	Deleted        bool
	AuthorInactive bool
	// CurrentLabels are the labels the label policy's labelers have on the post and its author now.
	CurrentLabels []string
	// RetryStatus is the state of the post's entry in the picture retry queue ("" if it has none).
	RetryStatus   string
	RetryAttempts uint8
	RetryError    string
}

// PostFacts reads what is stored about the post at uri. Every read is by the post's URI or its
// author, which the tables are sorted by.
func (s *Store) PostFacts(ctx context.Context, uri, did string) (PostFacts, error) {
	f := PostFacts{URI: uri, DID: did}

	var posts []struct {
		CID          string    `ch:"cid"`
		CreatedAt    time.Time `ch:"created_at"`
		IndexedAt    time.Time `ch:"indexed_at"`
		Text         string    `ch:"text"`
		Langs        []string  `ch:"langs"`
		DetectedLang string    `ch:"detected_lang"`
		EmbedType    string    `ch:"embed_type"`
		LinkURI      string    `ch:"link_uri"`
		LinkDomain   string    `ch:"link_domain"`
		LinkTitle    string    `ch:"link_title"`
		QuoteURI     string    `ch:"quote_uri"`
		QuoteText    string    `ch:"quote_text"`
		SelfLabels   []string  `ch:"self_labels"`
		Tags         []string  `ch:"tags"`
		MediaKinds   []string  `ch:"media_kinds"`
		MediaCIDs    []string  `ch:"media_cids"`
	}
	if err := s.Conn.Select(ctx, &posts, `
		SELECT cid, created_at, indexed_at, text, langs, detected_lang, embed_type, link_uri, link_domain, link_title,
		       quote_uri, quote_text, self_labels, tags, media_kinds, media_cids
		FROM posts FINAL WHERE uri = ? LIMIT 1`, uri); err != nil {
		return f, fmt.Errorf("select post: %w", err)
	}
	if len(posts) > 0 {
		p := posts[0]
		f.Stored = &StoredPost{CID: p.CID, CreatedAt: p.CreatedAt, IndexedAt: p.IndexedAt, Text: p.Text, Langs: p.Langs, DetectedLang: p.DetectedLang,
			EmbedType: p.EmbedType, LinkURI: p.LinkURI, LinkDomain: p.LinkDomain, LinkTitle: p.LinkTitle, QuoteURI: p.QuoteURI, QuoteText: p.QuoteText,
			SelfLabels: p.SelfLabels, Tags: p.Tags, MediaKinds: p.MediaKinds, MediaCIDs: p.MediaCIDs}
	}

	var pipe []struct {
		IndexedAt      time.Time          `ch:"indexed_at"`
		ProcessedAt    time.Time          `ch:"processed_at"`
		FeedPolicy     string             `ch:"feed_policy"`
		Labels         []string           `ch:"labels"`
		Model          string             `ch:"model"`
		BroadProbs     map[string]float32 `ch:"broad_probs"`
		PathProbs      map[string]float32 `ch:"path_probs"`
		Signals        map[string]float32 `ch:"signals"`
		Tone           map[string]float32 `ch:"tone"`
		TopBroad       string             `ch:"top_broad"`
		TopPath        string             `ch:"top_path"`
		TopPathP       float32            `ch:"top_path_p"`
		PicturesWanted uint8              `ch:"pictures_wanted"`
		PicturesUsed   uint8              `ch:"pictures_used"`
	}
	// Not perPartitionFinal: this reads one post by its key, and a post indexed again on another day
	// must be seen once.
	if err := s.Conn.Select(ctx, &pipe, `
		SELECT indexed_at, processed_at, feed_policy, labels, model, broad_probs, path_probs, signals, tone,
		       top_broad, top_path, top_path_p, pictures_wanted, pictures_used
		FROM post_pipeline FINAL WHERE uri = ? ORDER BY processed_at DESC LIMIT 1`, uri); err != nil {
		return f, fmt.Errorf("select pipeline row: %w", err)
	}
	if len(pipe) > 0 {
		r := pipe[0]
		f.Pipeline = &PipelineRow{IndexedAt: r.IndexedAt, ProcessedAt: r.ProcessedAt, FeedPolicy: r.FeedPolicy, Labels: r.Labels, Model: r.Model,
			BroadProbs: r.BroadProbs, PathProbs: r.PathProbs, Signals: r.Signals, Tone: r.Tone, TopBroad: r.TopBroad, TopPath: r.TopPath, TopPathP: r.TopPathP,
			PicturesWanted: r.PicturesWanted, PicturesUsed: r.PicturesUsed}
		f.Post = Post{URI: uri, DID: did, IndexedAt: r.IndexedAt, TopPath: r.TopPath, TopPathP: r.TopPathP, Tone: r.Tone, Signals: r.Signals, Labels: r.Labels}
	} else {
		f.Post = Post{URI: uri, DID: did}
	}

	// Engagement, with the same read the feeds use.
	one := []Post{f.Post}
	if err := s.engagement(ctx, one); err != nil {
		return f, err
	}
	f.Post = one[0]

	deleted, err := s.deleted(ctx, []string{did}, []string{uri})
	if err != nil {
		return f, err
	}
	f.Deleted = deleted[uri]
	inactive, err := s.inactive(ctx, []string{did})
	if err != nil {
		return f, err
	}
	f.AuthorInactive = inactive[did]
	if s.Policy != nil {
		labels, err := s.Policy.Current(ctx, s.Conn, []string{uri, did})
		if err != nil {
			return f, fmt.Errorf("select labels: %w", err)
		}
		f.CurrentLabels = slices.Concat(labels[uri], labels[did])
	}

	var seen uint64
	if err := s.Conn.QueryRow(ctx, `SELECT count() FROM post_texts WHERE uri = ?`, uri).Scan(&seen); err != nil {
		return f, fmt.Errorf("select post text: %w", err)
	}
	f.SeenText = seen > 0

	var retry []struct {
		Status   string `ch:"status"`
		Attempts uint8  `ch:"attempts"`
		LastErr  string `ch:"last_error"`
	}
	if err := s.Conn.Select(ctx, &retry, `SELECT status, attempts, last_error FROM image_retry_queue FINAL WHERE uri = ? LIMIT 1`, uri); err != nil {
		return f, fmt.Errorf("select retry queue: %w", err)
	}
	if len(retry) > 0 {
		f.RetryStatus, f.RetryAttempts, f.RetryError = retry[0].Status, retry[0].Attempts, retry[0].LastErr
	}
	return f, nil
}

// EvalInput is the post as the feeds judge it, from what was read.
func (f PostFacts) EvalInput(policy *labelpolicy.Policy) EvalInput {
	in := EvalInput{Post: f.Post, Deleted: f.Deleted, AuthorInactive: f.AuthorInactive, CurrentLabels: f.CurrentLabels, Policy: policy}
	if f.Pipeline != nil {
		in.Model, in.FeedPolicy = f.Pipeline.Model, f.Pipeline.FeedPolicy
		in.PathProbs, in.BroadProbs = f.Pipeline.PathProbs, f.Pipeline.BroadProbs
	}
	return in
}
