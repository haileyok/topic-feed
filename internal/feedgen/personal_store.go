package feedgen

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// maxPoolPosts bounds the posts kept across all subtopics: their URIs go into IN lists,
// which maxQuerySize allows up to 50,000 of (about 6 MB of query text with their authors). It is
// room for the newest and the most engaged posts of every subtopic in the taxonomy: about 120
// subtopics of up to 240 + 150 posts.
const maxPoolPosts = 50000

// PoolQuery says which posts a personal feed's pool holds, for each subtopic.
type PoolQuery struct {
	// Since is how far back posts count.
	Since time.Time
	// Newest is how many of the newest posts are kept.
	Newest int
	// Engaged is how many more are kept: the ones with the most engagement for their age
	// anywhere since Since. A busy subtopic's newest posts cover only minutes, so without
	// these the pool would hold nothing older, however well liked.
	Engaged int
	// MinProb is the least probability the model gives a post for the subtopic it is kept under.
	MinProb float32
	// Gravity and Weights are how the engaged posts are chosen, as ranking does (see Score):
	// engagement counts for less the older the post is, at this gravity, with each kind worth
	// its weight.
	Gravity float64
	Weights Weights
}

// PoolQuery is the pool a personal feed draws on as of now, with the feed's ranking
// deciding how the engaged posts are chosen. The gravity is the lowest any freshness setting
// uses, so the pool holds what each of them would pick.
func (c PersonalConfig) PoolQuery(now time.Time, r Ranking) PoolQuery {
	return PoolQuery{
		Since:   now.Add(-time.Duration(c.WindowHours) * time.Hour),
		Newest:  c.PerTopic,
		Engaged: c.TopPerTopic,
		MinProb: c.PoolMinTopicProb,
		Gravity: min(popularGravity, r.Gravity),
		Weights: r.Weights,
	}
}

// LikeData is what a viewer's likes and reposts say about them.
type LikeData struct {
	// Mass is how much of each subtopic the viewer liked: the sum, over the classified posts
	// they liked or reposted, of the model's probability for the subtopic, each like
	// weighted down by its age.
	Mass map[string]float64
	// Posts is how many distinct classified posts that rests on.
	Posts int
	// Liked is every post they liked or reposted in the period, classified or not: the
	// feed never shows a post the viewer already reacted to.
	Liked map[string]struct{}
}

// SeenData is what a viewer has already been shown.
type SeenData struct {
	// Confirmed are posts the viewer interacted with in one of our feeds (Bluesky reports
	// a post as seen, liked, shared, ...). Bluesky reports only some of what it shows.
	Confirmed map[string]struct{}
	// Serves counts how many times our personal feeds sent each post to the viewer.
	Serves map[string]int
}

// PersonalSource is what a personal feed reads from the database. *Store implements it.
type PersonalSource interface {
	// LikeProfile reads the viewer's likes and reposts since the given time; each is
	// weighted down by 0.5 for every halfLife it is older than now.
	LikeProfile(ctx context.Context, did string, since, now time.Time, halfLife time.Duration) (LikeData, error)
	// ViewerSeen reads what the viewer has been shown or reacted to since the given time.
	ViewerSeen(ctx context.Context, did string, since time.Time) (SeenData, error)
	// TopicPool returns, for each subtopic, the newest posts and the most engaged posts the
	// query asks for, among those the model gives at least MinProb for that subtopic: the
	// posts a personal feed draws on.
	TopicPool(ctx context.Context, q PoolQuery) (map[string][]Post, error)
}

// likesAndReposts is every post a viewer liked or reposted since a time. It takes four
// arguments: the viewer and the time, twice.
const likesAndReposts = `(
	SELECT subject_uri, indexed_at FROM likes WHERE actor_did = ? AND indexed_at >= ?
	UNION ALL
	SELECT subject_uri, indexed_at FROM reposts WHERE actor_did = ? AND indexed_at >= ?
)`

func (s *Store) LikeProfile(ctx context.Context, did string, since, now time.Time, halfLife time.Duration) (LikeData, error) {
	out := LikeData{Mass: map[string]float64{}, Liked: map[string]struct{}{}}
	react := []any{did, since, did, since}

	// The classified posts' subtopics, each like worth less the older it is. likes is
	// sorted by actor, so reading one viewer's is cheap; post_pipeline is sorted by uri.
	var mass []struct {
		Path string  `ch:"path"`
		Mass float64 `ch:"mass"`
	}
	args := append([]any{now, halfLife.Seconds()}, react...)
	args = append(args, react...)
	if err := s.Conn.Select(ctx, &mass, `
		SELECT path, sum(prob * pow(0.5, greatest(0, dateDiff('second', l.liked_at, ?)) / ?)) AS mass
		FROM (SELECT subject_uri, max(indexed_at) AS liked_at FROM `+likesAndReposts+` GROUP BY subject_uri) AS l
		INNER JOIN (
			SELECT uri, path_probs FROM post_pipeline FINAL
			WHERE model != '' AND uri IN (SELECT subject_uri FROM `+likesAndReposts+`)
		) AS pp ON l.subject_uri = pp.uri
		ARRAY JOIN mapKeys(pp.path_probs) AS path, mapValues(pp.path_probs) AS prob
		GROUP BY path`, args...); err != nil {
		return out, fmt.Errorf("select interests: %w", err)
	}
	for _, m := range mass {
		out.Mass[m.Path] = m.Mass
	}

	var n uint64
	args = append(append([]any{}, react...), react...)
	if err := s.Conn.QueryRow(ctx, `
		SELECT count() FROM (SELECT DISTINCT subject_uri FROM `+likesAndReposts+`) AS l
		INNER JOIN (
			SELECT uri FROM post_pipeline FINAL
			WHERE model != '' AND uri IN (SELECT subject_uri FROM `+likesAndReposts+`)
		) AS pp ON l.subject_uri = pp.uri`, args...).Scan(&n); err != nil {
		return out, fmt.Errorf("count classified likes: %w", err)
	}
	out.Posts = int(n)

	var liked []struct {
		URI string `ch:"subject_uri"`
	}
	if err := s.Conn.Select(ctx, &liked, `SELECT DISTINCT subject_uri FROM `+likesAndReposts, react...); err != nil {
		return out, fmt.Errorf("select liked posts: %w", err)
	}
	for _, l := range liked {
		out.Liked[l.URI] = struct{}{}
	}
	return out, nil
}

func (s *Store) ViewerSeen(ctx context.Context, did string, since time.Time) (SeenData, error) {
	out := SeenData{Confirmed: map[string]struct{}{}, Serves: map[string]int{}}
	// Any interaction with a post in any of our feeds (seen, liked, "show less", ...) means
	// the viewer has met it. feed_interactions isn't sorted by viewer, so this reads the
	// months in range; it runs once per viewer, not per request.
	var items []struct {
		Item string `ch:"item"`
	}
	if err := s.Conn.Select(ctx, &items, `
		SELECT DISTINCT item FROM feed_interactions WHERE viewer_did = ? AND received_at >= ?`, did, since); err != nil {
		return out, fmt.Errorf("select interactions: %w", err)
	}
	for _, it := range items {
		out.Confirmed[it.Item] = struct{}{}
	}
	var serves []struct {
		URI string `ch:"uri"`
		N   uint64 `ch:"n"`
	}
	if err := s.Conn.Select(ctx, &serves, `
		SELECT uri, count() AS n FROM viewer_served WHERE viewer_did = ? AND served_at >= ? GROUP BY uri`, did, since); err != nil {
		return out, fmt.Errorf("select served posts (is schema/009_viewer_served.sql applied?): %w", err)
	}
	for _, sv := range serves {
		out.Serves[sv.URI] = int(sv.N)
	}
	return out, nil
}

func (s *Store) TopicPool(ctx context.Context, q PoolQuery) (map[string][]Post, error) {
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_query_size": maxQuerySize}))
	cands, err := s.poolCandidates(ctx, q)
	if err != nil {
		return nil, err
	}
	posts, _, err := s.finish(ctx, Feed{}, cands)
	if err != nil {
		return nil, err
	}
	pool := map[string][]Post{}
	for _, p := range posts {
		pool[p.TopPath] = append(pool[p.TopPath], p)
	}
	return pool, nil
}

// poolFilter is which processed posts can be in a pool: classified, allowed by the label policy,
// and confidently in a subtopic. It takes four arguments: the time, the policy, the unclear
// topic's name, and the least probability.
const poolFilter = `indexed_at >= ? AND model != '' AND feed_policy = ? AND top_path != ? AND top_path_p >= ?`

// poolCandidates reads the pool's posts, a post counting for its most likely subtopic: the newest
// of each subtopic, and the most engaged for their age.
func (s *Store) poolCandidates(ctx context.Context, q PoolQuery) ([]candidate, error) {
	var newest []candidate
	err := s.Conn.Select(ctx, &newest, `
		SELECT `+candidateColumns("top_path_p")+`
		FROM post_pipeline FINAL
		WHERE `+poolFilter+`
		ORDER BY indexed_at DESC, uri DESC
		LIMIT ? BY top_path
		LIMIT ?`+perPartitionFinal, q.Since, labelpolicy.OK, unclearTopic, q.MinProb, q.Newest, maxPoolPosts)
	if err != nil {
		return nil, fmt.Errorf("select topic pool: %w", err)
	}
	if q.Engaged <= 0 {
		return newest, nil
	}

	// The same score ranking gives a post, without its prior (what the model says of the post
	// itself): (1 + engagement) / (age in hours + 2) ^ gravity. Counts are summed over the hours
	// since the window began, which is all the engagement a post in the window can have had.
	// Only posts with some engagement are candidates, so quiet subtopics add nothing here.
	var engaged []candidate
	err = s.Conn.Select(ctx, &engaged, `
		SELECT `+candidateColumns("top_path_p")+`
		FROM post_pipeline FINAL
		WHERE `+poolFilter+` AND uri IN (
			SELECT p.uri
			FROM (SELECT uri, top_path, indexed_at FROM post_pipeline FINAL WHERE `+poolFilter+`) AS p
			INNER JOIN (
				SELECT subject_uri, sum(w) AS eng FROM (
					SELECT subject_uri, likes * ? AS w FROM like_counts_hourly WHERE hour >= ?
					UNION ALL
					SELECT subject_uri, n * multiIf(kind = 'repost', ?, kind = 'reply', ?, kind = 'quote', ?, 0) AS w
					FROM engagement_hourly WHERE hour >= ?
				)
				GROUP BY subject_uri
			) AS e ON p.uri = e.subject_uri
			ORDER BY (1 + e.eng) / pow(dateDiff('minute', p.indexed_at, now()) / 60 + 2, ?) DESC, p.uri DESC
			LIMIT ? BY p.top_path
		)
		LIMIT ?`+perPartitionFinal,
		q.Since, labelpolicy.OK, unclearTopic, q.MinProb, // the posts to read
		q.Since, labelpolicy.OK, unclearTopic, q.MinProb, // the posts to choose from
		q.Weights.Like, q.Since.Truncate(time.Hour),
		q.Weights.Repost, q.Weights.Reply, q.Weights.Quote, q.Since.Truncate(time.Hour),
		q.Gravity, q.Engaged, maxPoolPosts)
	if err != nil {
		return nil, fmt.Errorf("select engaged topic pool: %w", err)
	}
	return mergeCandidates(newest, engaged, maxPoolPosts), nil
}

// mergeCandidates is the candidates of first, then those of second that first doesn't have, at
// most limit. A post that is among both the newest and the most engaged is kept once.
func mergeCandidates(first, second []candidate, limit int) []candidate {
	out := make([]candidate, 0, min(len(first)+len(second), limit))
	seen := make(map[string]struct{}, cap(out))
	for _, list := range [][]candidate{first, second} {
		for _, c := range list {
			if len(out) >= limit {
				return out
			}
			if _, dup := seen[c.URI]; dup {
				continue
			}
			seen[c.URI] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// ServedRow is one row of viewer_served: a post a personal feed sent to a viewer.
type ServedRow struct {
	ViewerDID string    `ch:"viewer_did"`
	URI       string    `ch:"uri"`
	Feed      string    `ch:"feed"`
	ServedAt  time.Time `ch:"served_at"`
}

// ServedSink accepts served posts for storage. *RowWriter[ServedRow] implements it.
type ServedSink interface {
	// Add queues rows and returns how many were dropped because the queue was full.
	Add(rows []ServedRow) (dropped int)
}

// NewServedWriter makes the writer for viewer_served, counting its rows in the service's
// metrics. Call Run.
func NewServedWriter(conn driver.Conn, log *slog.Logger) *RowWriter[ServedRow] {
	w := NewRowWriter[ServedRow](conn, "viewer_served", log, 200_000)
	w.Written, w.Dropped, w.Errors = metricServedWritten, metricServedDropped, metricServedWriteErrors
	return w
}

// RowWriter batches rows into a ClickHouse table in the background, so requests never wait
// on the database.
type RowWriter[T any] struct {
	Conn       driver.Conn
	Table      string
	Log        *slog.Logger
	FlushEvery time.Duration
	FlushRows  int
	// Written, Dropped and Errors count rows written, rows dropped, and failed writes; any
	// may be nil.
	Written, Dropped, Errors prometheus.Counter

	queue chan T
	done  chan struct{}
}

// NewRowWriter makes a writer with room for queueSize rows. Call Run.
func NewRowWriter[T any](conn driver.Conn, table string, log *slog.Logger, queueSize int) *RowWriter[T] {
	return &RowWriter[T]{Conn: conn, Table: table, Log: log, FlushEvery: 2 * time.Second, FlushRows: 5000,
		queue: make(chan T, queueSize), done: make(chan struct{})}
}

func (w *RowWriter[T]) Add(rows []T) int {
	dropped := 0
	for _, r := range rows {
		select {
		case w.queue <- r:
		default:
			dropped++
		}
	}
	if dropped > 0 && w.Dropped != nil {
		w.Dropped.Add(float64(dropped))
	}
	return dropped
}

// Run writes queued rows until ctx ends, then writes what's left and returns. Wait returns
// once it has.
func (w *RowWriter[T]) Run(ctx context.Context) {
	defer close(w.done)
	t := time.NewTicker(w.FlushEvery)
	defer t.Stop()
	var buf []T
	flush := func(ctx context.Context) {
		if len(buf) == 0 {
			return
		}
		if err := chdb.Insert(ctx, w.Conn, w.Table, buf); err != nil {
			if w.Errors != nil {
				w.Errors.Inc()
			}
			w.Log.Error("write rows", "table", w.Table, "rows", len(buf), "err", err)
			if len(buf) > 100_000 { // ClickHouse is down for a while: don't grow without bound
				if w.Dropped != nil {
					w.Dropped.Add(float64(len(buf)))
				}
				buf = buf[:0]
			}
			return
		}
		if w.Written != nil {
			w.Written.Add(float64(len(buf)))
		}
		buf = buf[:0]
	}
	for {
		select {
		case r := <-w.queue:
			buf = append(buf, r)
			if len(buf) >= w.FlushRows {
				flush(ctx)
			}
		case <-t.C:
			flush(ctx)
		case <-ctx.Done():
		drain:
			for {
				select {
				case r := <-w.queue:
					buf = append(buf, r)
				default:
					break drain
				}
			}
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(fctx)
			cancel()
			return
		}
	}
}

// Wait blocks until Run has returned.
func (w *RowWriter[T]) Wait() { <-w.done }
