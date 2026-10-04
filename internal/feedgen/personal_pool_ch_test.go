package feedgen

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// schemaStatements returns the statements of a schema file, without its comments.
func schemaStatements(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile("../../schema/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		lines = append(lines, l)
	}
	var out []string
	for _, s := range strings.Split(strings.Join(lines, "\n"), ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

type pipelineRow struct {
	URI         string             `ch:"uri"`
	DID         string             `ch:"did"`
	IndexedAt   time.Time          `ch:"indexed_at"`
	ProcessedAt time.Time          `ch:"processed_at"`
	FeedPolicy  string             `ch:"feed_policy"`
	Labels      []string           `ch:"labels"`
	Model       string             `ch:"model"`
	PathProbs   map[string]float32 `ch:"path_probs"`
	Signals     map[string]float32 `ch:"signals"`
	Tone        map[string]float32 `ch:"tone"`
	TopPath     string             `ch:"top_path"`
	TopPathP    float32            `ch:"top_path_p"`
}

type likeHour struct {
	URI   string    `ch:"subject_uri"`
	Hour  time.Time `ch:"hour"`
	Likes uint64    `ch:"likes"`
}

type engagementHour struct {
	URI  string    `ch:"subject_uri"`
	Kind string    `ch:"kind"`
	Hour time.Time `ch:"hour"`
	N    uint64    `ch:"n"`
}

// TestPoolCandidatesAgainstClickHouse runs the pool's SQL on a real server. It needs one, so it
// only runs when TOPICFEED_CLICKHOUSE_TEST is set (plus the CLICKHOUSE_ADDR, CLICKHOUSE_USER and
// CLICKHOUSE_PASSWORD that reach it). It works in a database of its own and drops it
// afterwards, so it never touches real data.
func TestPoolCandidatesAgainstClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_pool_%d", time.Now().UnixNano())
	if err := admin.Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+scratch); err != nil {
			t.Errorf("dropping %s: %v", scratch, err)
		}
	})
	cfg := chdb.FromEnv()
	cfg.Database = scratch
	conn, err := chdb.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	// The tables the pool reads, made from the schema files the service is deployed with.
	exec := func(stmt string) {
		t.Helper()
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	for _, s := range schemaStatements(t, "004_post_pipeline.sql") {
		exec(s)
	}
	for _, s := range schemaStatements(t, "005_predictions.sql") {
		if strings.HasPrefix(s, "ALTER TABLE post_pipeline") {
			exec(s)
		}
	}
	for file, table := range map[string]string{"001_init.sql": "like_counts_hourly", "006_engagement.sql": "engagement_hourly"} {
		for _, s := range schemaStatements(t, file) {
			if strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS "+table) {
				exec(s)
			}
		}
	}

	at := time.Now().UTC()
	hour := at.Truncate(time.Hour)
	ago := func(d time.Duration) time.Time { return at.Add(-d) }
	var posts []pipelineRow
	var likes []likeHour
	var other []engagementHour
	post := func(name, topic string, age time.Duration, mutate func(*pipelineRow)) {
		r := pipelineRow{URI: "at://did:plc:" + name + "/app.bsky.feed.post/1", DID: "did:plc:" + name, IndexedAt: ago(age), ProcessedAt: at,
			FeedPolicy: "ok", Labels: []string{}, Model: "test", TopPath: topic, TopPathP: 0.9,
			PathProbs: map[string]float32{topic: 0.9}, Signals: map[string]float32{}, Tone: map[string]float32{}}
		if mutate != nil {
			mutate(&r)
		}
		posts = append(posts, r)
	}
	uri := func(name string) string { return "at://did:plc:" + name + "/app.bsky.feed.post/1" }
	liked := func(name string, n uint64) { likes = append(likes, likeHour{uri(name), hour, n}) }
	engaged := func(name, kind string, n uint64) { other = append(other, engagementHour{uri(name), kind, hour, n}) }

	const busy, quiet = "sports/baseball", "technology/ai"
	// A busy subtopic: 40 recent posts nobody has reacted to yet (one minute apart), and older ones.
	for i := 1; i <= 40; i++ {
		post(fmt.Sprintf("n%02d", i), busy, time.Duration(i)*time.Minute, nil)
	}
	post("old-hot", busy, 5*time.Hour, nil)        // very well liked, hours old
	post("old-warm", busy, 8*time.Hour, nil)       // reposted and quoted
	post("old-lukewarm", busy, 20*time.Hour, nil)  // a few likes, a long time ago
	post("old-cold", busy, 6*time.Hour, nil)       // nobody reacted
	post("young-liked", busy, 30*time.Minute, nil) // new, but not among the newest ten
	post("ancient-hot", busy, 30*time.Hour, nil)   // best liked of all, but outside the window
	post("reposted-only", busy, 12*time.Hour, nil) // reposts and nothing else
	liked("old-hot", 100)
	engaged("old-warm", "repost", 5)
	engaged("old-warm", "quote", 4)
	liked("old-lukewarm", 3)
	liked("young-liked", 5)
	liked("n05", 10) // among the newest ten, and engaged: it must be in the pool once
	liked("ancient-hot", 10000)
	engaged("reposted-only", "repost", 12)
	engaged("reposted-only", "reply", 7)

	// Another subtopic, with its own limits: twelve recent posts nobody reacted to, and two older
	// ones that were.
	for i := 1; i <= 12; i++ {
		post(fmt.Sprintf("qf%02d", i), quiet, time.Duration(i)*time.Minute, nil)
	}
	post("q-hot", quiet, 3*time.Hour, nil)
	post("q-old", quiet, 12*time.Hour, nil)
	liked("q-hot", 50)
	liked("q-old", 5)

	// Posts that must never be in a pool, however much they are liked.
	post("unclear-hot", "unclear", time.Hour, nil)
	post("lowprob-hot", busy, time.Hour, func(r *pipelineRow) { r.TopPathP = 0.3 })
	post("adult-hot", busy, time.Hour, func(r *pipelineRow) { r.FeedPolicy = "adult_only" })
	post("unclassified-hot", busy, time.Hour, func(r *pipelineRow) { r.Model = "" })
	for _, name := range []string{"unclear-hot", "lowprob-hot", "adult-hot", "unclassified-hot"} {
		liked(name, 1000)
	}

	if err := chdb.Insert(ctx, conn, "post_pipeline", posts); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "like_counts_hourly", likes); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "engagement_hourly", other); err != nil {
		t.Fatal(err)
	}

	store := &Store{Conn: conn}
	query := func(q PoolQuery) []string {
		t.Helper()
		cs, err := store.poolCandidates(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]string, len(cs))
		for i, c := range cs {
			got[i] = strings.TrimSuffix(strings.TrimPrefix(c.URI, "at://did:plc:"), "/app.bsky.feed.post/1")
		}
		slices.Sort(got)
		return got
	}
	// names is the sorted, de-duplicated post names in these space-separated lists.
	names := func(parts ...string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, strings.Fields(p)...)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	newestTen := "n01 n02 n03 n04 n05 n06 n07 n08 n09 n10"
	quietNewest := "qf01 qf02 qf03 qf04 qf05 qf06 qf07 qf08 qf09 qf10"
	base := PoolQuery{Since: ago(24 * time.Hour), Newest: 10, MinProb: 0.5, Gravity: popularGravity, Weights: DefaultRanking.Weights}
	check := func(what string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Errorf("%s:\n got  %v\n want %v", what, got, want)
		}
	}

	// Only the newest, as the pool was before it had the engaged ones.
	check("no engaged posts asked for", query(base), names(newestTen, quietNewest))

	// The three best-engaged posts of each subtopic join the newest ten. In the busy one:
	// old-hot (101 / 7^1.2 = 9.8), n05 (11 / 2.08^1.2 = 4.6, already among the newest: kept once)
	// and young-liked (6 / 2.5^1.2 = 2.0). Next would come reposted-only (39 / 14^1.2 = 1.6) and
	// old-warm (23 / 10^1.2 = 1.5), which the limit leaves out; old-cold and the newest nobody
	// reacted to aren't candidates, and ancient-hot is outside the window. The quiet subtopic has
	// a limit of its own: both its engaged posts fit.
	q := base
	q.Engaged = 3
	check("three engaged posts per subtopic", query(q), names(newestTen, "old-hot young-liked", quietNewest, "q-hot q-old"))

	// Each kind of engagement is worth its own weight: with only one kind counting, the post with
	// the most of it comes first. (With likes at zero a liked post still has a score, from its
	// age alone, so the quiet subtopic's best is its newest engaged post.)
	q.Engaged = 1
	q.Weights = Weights{Repost: 100}
	check("only reposts count", query(q), names(newestTen, "reposted-only", quietNewest, "q-hot"))
	q.Weights = Weights{Reply: 100}
	check("only replies count", query(q), names(newestTen, "reposted-only", quietNewest, "q-hot"))
	q.Weights = Weights{Quote: 100}
	check("only quotes count", query(q), names(newestTen, "old-warm", quietNewest, "q-hot"))
	q.Weights = Weights{Like: 1}
	check("only likes count", query(q), names(newestTen, "old-hot", quietNewest, "q-hot"))

	// A gravity that lets age count for much more favours the newer posts: in the busy subtopic
	// n05 (11 / 2.08^4 = 0.59) beats young-liked and old-hot, and it is already in the pool.
	q.Weights = DefaultRanking.Weights
	q.Gravity = 4
	check("a high gravity", query(q), names(newestTen, quietNewest, "q-hot"))

	// A shorter window leaves out what is older, however well liked: old-hot, q-old and the others
	// are more than four hours old.
	q = base
	q.Engaged = 3
	q.Since = ago(4 * time.Hour)
	check("a four hour window", query(q), names(newestTen, "young-liked", quietNewest, "q-hot"))
}
