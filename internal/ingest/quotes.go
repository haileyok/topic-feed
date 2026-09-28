package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// getPostsBatch is the most URIs app.bsky.feed.getPosts accepts per call.
const getPostsBatch = 25

// QuoteResolver fills in the text of quoted posts (plan §9.1 step 5): first from
// memory, then from post_texts in ClickHouse, then from the public AppView. Texts
// fetched from the AppView are returned so the caller can store them in post_texts.
type QuoteResolver struct {
	Cache   *TextCache
	Conn    driver.Conn
	AppView string // e.g. https://public.api.bsky.app
	HTTP    *http.Client
	Log     *slog.Logger

	// minInterval spaces AppView calls to stay well inside public rate limits.
	minInterval time.Duration
	lastCall    time.Time
}

func NewQuoteResolver(cache *TextCache, conn driver.Conn, appView string, log *slog.Logger) *QuoteResolver {
	return &QuoteResolver{
		Cache:       cache,
		Conn:        conn,
		AppView:     appView,
		HTTP:        &http.Client{Timeout: 15 * time.Second},
		Log:         log,
		minInterval: 100 * time.Millisecond, // at most ~10 calls/s
	}
}

// Resolve sets QuoteText on every post that quotes another post. Quoted posts that
// can't be found (deleted, or hidden by the AppView) keep an empty QuoteText.
// Resolution errors are logged, not returned: a missing quote shouldn't stop ingest.
func (q *QuoteResolver) Resolve(ctx context.Context, rows *Rows) {
	need := map[string]bool{}
	for i := range rows.Posts {
		p := &rows.Posts[i]
		if p.QuoteURI == "" {
			continue
		}
		if t, ok := q.Cache.Get(p.QuoteURI); ok {
			p.QuoteText = t
			metricQuotes.WithLabelValues("memory").Inc()
		} else {
			need[p.QuoteURI] = true
		}
	}
	if len(need) == 0 {
		return
	}

	found := map[string]string{}
	if err := q.fromClickHouse(ctx, need, found); err != nil {
		q.Log.Warn("quote lookup in post_texts failed", "err", err)
	}
	var missing []string
	for uri := range need {
		if _, ok := found[uri]; ok {
			metricQuotes.WithLabelValues("clickhouse").Inc()
		} else {
			missing = append(missing, uri)
		}
	}

	now := time.Now().UTC()
	for start := 0; start < len(missing); start += getPostsBatch {
		end := min(start+getPostsBatch, len(missing))
		texts, err := q.fromAppView(ctx, missing[start:end])
		if err != nil {
			q.Log.Warn("quote lookup via AppView failed", "err", err, "uris", end-start)
		}
		for uri, t := range texts {
			found[uri] = t
			rows.PostTexts = append(rows.PostTexts, PostTextRow{URI: uri, Text: t, IndexedAt: now})
			metricQuotes.WithLabelValues("appview").Inc()
		}
	}

	for uri, t := range found {
		q.Cache.Put(uri, t)
	}
	for i := range rows.Posts {
		p := &rows.Posts[i]
		if p.QuoteURI == "" || p.QuoteText != "" {
			continue
		}
		if t, ok := found[p.QuoteURI]; ok {
			p.QuoteText = t
		} else {
			metricQuotes.WithLabelValues("missing").Inc()
		}
	}
}

// lookupChunk keeps each IN (...) list well under ClickHouse's default 256KB query
// size limit (a post URI is ~70 bytes).
const lookupChunk = 2000

func (q *QuoteResolver) fromClickHouse(ctx context.Context, need map[string]bool, found map[string]string) error {
	uris := make([]string, 0, len(need))
	for u := range need {
		uris = append(uris, u)
	}
	for i := 0; i < len(uris); i += lookupChunk {
		if err := q.lookupChunk(ctx, uris[i:min(i+lookupChunk, len(uris))], found); err != nil {
			return err
		}
	}
	return nil
}

func (q *QuoteResolver) lookupChunk(ctx context.Context, uris []string, found map[string]string) error {
	rows, err := q.Conn.Query(ctx, "SELECT uri, argMax(text, indexed_at) FROM post_texts WHERE uri IN (?) GROUP BY uri", uris)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var uri, text string
		if err := rows.Scan(&uri, &text); err != nil {
			return err
		}
		found[uri] = text
	}
	return rows.Err()
}

func (q *QuoteResolver) fromAppView(ctx context.Context, uris []string) (map[string]string, error) {
	if wait := q.minInterval - time.Since(q.lastCall); wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	q.lastCall = time.Now()

	v := url.Values{}
	for _, u := range uris {
		v.Add("uris", u)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.AppView+"/xrpc/app.bsky.feed.getPosts?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "topic-feed-ingest (haileyok)")
	resp, err := q.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getPosts: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Posts []struct {
			URI    string `json:"uri"`
			Record struct {
				Text string `json:"text"`
			} `json:"record"`
		} `json:"posts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(body.Posts))
	for _, p := range body.Posts {
		out[p.URI] = p.Record.Text
	}
	return out, nil
}
