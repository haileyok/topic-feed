package feedgen

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// FilteredStore is what filtered feeds read and keep. *Store implements it.
type FilteredStore interface {
	// ScorePosts is what the model made of each of uris: a post it never scored is left out.
	ScorePosts(ctx context.Context, uris []string) (map[string]ScoredPost, error)
	// ViewerFilters is the filters a viewer chose for the feed rkey, or nil when they chose none.
	ViewerFilters(ctx context.Context, did, rkey string) (*Filters, error)
	// SaveViewerFilters stores a viewer's filters for the feed rkey; nil goes back to the feed's own.
	SaveViewerFilters(ctx context.Context, did, rkey string, fl *Filters) error
	// LeftOutPosts are the posts the feed rkey left out of the viewer's feed, newest first, after the
	// cursor (zero for the first page).
	LeftOutPosts(ctx context.Context, did, rkey string, after LeftOutCursor, limit int) ([]LeftOutPost, error)
}

var _ FilteredStore = (*Store)(nil)

func (s *Store) ScorePosts(ctx context.Context, uris []string) (map[string]ScoredPost, error) {
	out := make(map[string]ScoredPost, len(uris))
	if len(uris) == 0 {
		return out, nil
	}
	var rows []struct {
		URI        string             `ch:"uri"`
		PathProbs  map[string]float32 `ch:"path_probs"`
		BroadProbs map[string]float32 `ch:"broad_probs"`
		Tone       map[string]float32 `ch:"tone"`
		Signals    map[string]float32 `ch:"signals"`
		TopPath    string             `ch:"top_path"`
	}
	// The table is ordered by uri, so this reads only the parts' granules that hold these posts.
	// Posts the pipeline dropped have no model and count as never scored.
	if err := s.Conn.Select(ctx, &rows, `
		SELECT uri, path_probs, broad_probs, tone, signals, top_path
		FROM post_pipeline FINAL
		WHERE uri IN ? AND model != ''`, uris); err != nil {
		return nil, fmt.Errorf("select scores: %w", err)
	}
	for _, r := range rows {
		out[r.URI] = ScoredPost{URI: r.URI, Scored: true, PathProbs: r.PathProbs, BroadProbs: r.BroadProbs,
			Tone: r.Tone, Signals: r.Signals, TopPath: r.TopPath}
	}
	return out, nil
}

// LeftOutRow is a row of filtered_left_out: a post a filtered feed left out of a viewer's feed.
type LeftOutRow struct {
	ViewerDID string    `ch:"viewer_did"`
	Feed      string    `ch:"feed"`
	URI       string    `ch:"uri"`
	Reason    string    `ch:"reason"`
	Name      string    `ch:"name"`
	Value     float32   `ch:"value"`
	Cutoff    float32   `ch:"cutoff"`
	Bound     string    `ch:"bound"`
	Rule      string    `ch:"rule"`
	LeftOutAt time.Time `ch:"left_out_at"`
}

// LeftOutSink takes the rows to write; *RowWriter[LeftOutRow] is one.
type LeftOutSink interface {
	Add(rows []LeftOutRow) int
}

// NewLeftOutWriter writes left-out posts to filtered_left_out in the background.
func NewLeftOutWriter(conn driver.Conn, log *slog.Logger) *RowWriter[LeftOutRow] {
	return NewRowWriter[LeftOutRow](conn, "filtered_left_out", log, 100_000)
}

// LeftOutPost is a post left out of a viewer's feed, as the page shows it: the last time, and why.
type LeftOutPost struct {
	URI    string        `json:"uri"`
	At     time.Time     `json:"at"`
	Reason LeftOutReason `json:"reason"`
}

// LeftOutCursor is where a page of left-out posts ends: the last post's time and URI. Posts left out in
// one request share a time to the millisecond, so the URI breaks the tie. The zero cursor is the start.
// The time goes to the query as milliseconds: the driver would send a time.Time to the second.
type LeftOutCursor struct {
	At  time.Time
	URI string
}

// LeftOutPosts are the posts the filtered feed rkey left out of the viewer's feed, each once (the
// last time), newest first (by time, then URI), after the cursor, at most limit.
func (s *Store) LeftOutPosts(ctx context.Context, did, rkey string, after LeftOutCursor, limit int) ([]LeftOutPost, error) {
	// The aliases differ from the columns: ClickHouse would read a column's name, inside another
	// aggregate, as the alias.
	var rows []struct {
		URI    string    `ch:"uri"`
		Reason string    `ch:"last_reason"`
		Name   string    `ch:"last_name"`
		Value  float32   `ch:"last_value"`
		Cutoff float32   `ch:"last_cutoff"`
		Bound  string    `ch:"last_bound"`
		Rule   string    `ch:"last_rule"`
		At     time.Time `ch:"last_at"`
	}
	if err := s.Conn.Select(ctx, &rows, `
		SELECT uri,
		       argMax(reason, left_out_at) AS last_reason, argMax(name, left_out_at) AS last_name,
		       argMax(value, left_out_at) AS last_value, argMax(cutoff, left_out_at) AS last_cutoff,
		       argMax(bound, left_out_at) AS last_bound, argMax(rule, left_out_at) AS last_rule,
		       max(left_out_at) AS last_at
		FROM filtered_left_out
		WHERE viewer_did = ? AND feed = ?
		GROUP BY uri
		HAVING ? OR (last_at, uri) < (fromUnixTimestamp64Milli(?, 'UTC'), ?)
		ORDER BY last_at DESC, uri DESC
		LIMIT ?`, did, rkey, after.At.IsZero(), after.At.UnixMilli(), after.URI, limit); err != nil {
		return nil, fmt.Errorf("select left-out posts (is schema/015_filtered_feeds.sql applied?): %w", err)
	}
	out := make([]LeftOutPost, len(rows))
	for i, r := range rows {
		out[i] = LeftOutPost{URI: r.URI, At: r.At, Reason: LeftOutReason{Kind: r.Reason, Name: r.Name,
			Value: r.Value, Cutoff: r.Cutoff, Bound: r.Bound, Rule: r.Rule}}
	}
	return out, nil
}

func (s *Store) ViewerFilters(ctx context.Context, did, rkey string) (*Filters, error) {
	var rows []struct {
		Filters string `ch:"filters"`
	}
	if err := s.Conn.Select(ctx, &rows, `
		SELECT filters FROM viewer_filters WHERE viewer_did = ? AND feed = ?
		ORDER BY updated_at DESC LIMIT 1`, did, rkey); err != nil {
		return nil, fmt.Errorf("select viewer filters (is schema/015_filtered_feeds.sql applied?): %w", err)
	}
	if len(rows) == 0 || rows[0].Filters == "" || rows[0].Filters == "null" {
		return nil, nil
	}
	var fl Filters
	if err := json.Unmarshal([]byte(rows[0].Filters), &fl); err != nil {
		return nil, fmt.Errorf("the filters of %s for %s are not valid: %w", did, rkey, err)
	}
	return &fl, nil
}

func (s *Store) SaveViewerFilters(ctx context.Context, did, rkey string, fl *Filters) error {
	b := []byte("null")
	if fl != nil {
		var err error
		if b, err = json.Marshal(fl); err != nil {
			return err
		}
	}
	return chdb.Insert(ctx, s.Conn, "viewer_filters", []struct {
		ViewerDID string    `ch:"viewer_did"`
		Feed      string    `ch:"feed"`
		Filters   string    `ch:"filters"`
		UpdatedAt time.Time `ch:"updated_at"`
	}{{ViewerDID: did, Feed: rkey, Filters: string(b), UpdatedAt: time.Now().UTC()}})
}

type loginRow struct {
	ViewerDID string    `ch:"viewer_did"`
	Sealed    string    `ch:"sealed"`
	Deleted   uint8     `ch:"deleted"`
	UpdatedAt time.Time `ch:"updated_at"`
}

// LoadLogin, SaveLogin and DeleteLogin keep viewers' sealed sign-ins for the filtered feeds
// (signin.LoginStore).
func (s *Store) LoadLogin(ctx context.Context, did string) ([]byte, error) {
	var rows []loginRow
	if err := s.Conn.Select(ctx, &rows, `
		SELECT viewer_did, sealed, deleted, updated_at FROM filter_logins WHERE viewer_did = ?
		ORDER BY updated_at DESC LIMIT 1`, did); err != nil {
		return nil, fmt.Errorf("select sign-in (is schema/015_filtered_feeds.sql applied?): %w", err)
	}
	if len(rows) == 0 || rows[0].Deleted == 1 || rows[0].Sealed == "" {
		return nil, nil
	}
	return []byte(rows[0].Sealed), nil
}

func (s *Store) SaveLogin(ctx context.Context, did string, sealed []byte) error {
	return s.insertLogin(ctx, loginRow{ViewerDID: did, Sealed: string(sealed)})
}

func (s *Store) DeleteLogin(ctx context.Context, did string) error {
	return s.insertLogin(ctx, loginRow{ViewerDID: did, Deleted: 1})
}

// insertLogin writes a row stamped now (to the millisecond: saves for one viewer are one at a time,
// and far apart).
func (s *Store) insertLogin(ctx context.Context, r loginRow) error {
	r.UpdatedAt = time.Now().UTC()
	return chdb.Insert(ctx, s.Conn, "filter_logins", []loginRow{r})
}
