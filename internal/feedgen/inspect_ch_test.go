package feedgen

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

type diffPipelineRow struct {
	URI         string             `ch:"uri"`
	DID         string             `ch:"did"`
	IndexedAt   time.Time          `ch:"indexed_at"`
	ProcessedAt time.Time          `ch:"processed_at"`
	FeedPolicy  string             `ch:"feed_policy"`
	Labels      []string           `ch:"labels"`
	Model       string             `ch:"model"`
	BroadProbs  map[string]float32 `ch:"broad_probs"`
	PathProbs   map[string]float32 `ch:"path_probs"`
	Signals     map[string]float32 `ch:"signals"`
	Tone        map[string]float32 `ch:"tone"`
	TopBroad    string             `ch:"top_broad"`
	TopPath     string             `ch:"top_path"`
	TopPathP    float32            `ch:"top_path_p"`
}

type diffDeletion struct {
	DID        string    `ch:"did"`
	Collection string    `ch:"collection"`
	RKey       string    `ch:"rkey"`
	URI        string    `ch:"uri"`
	IndexedAt  time.Time `ch:"indexed_at"`
}

type diffAccount struct {
	DID       string    `ch:"did"`
	Active    uint8     `ch:"active"`
	Status    string    `ch:"status"`
	IndexedAt time.Time `ch:"indexed_at"`
}

// TestEvaluateFeedAgreesWithTheSQLAgainstClickHouse is what lets the inspector page be trusted: for
// every feed in testdata/feeds.yaml (the topic feeds config/feeds.yaml had, and its For you feed),
// the posts Store.Build's query admits are exactly the posts
// EvaluateFeed says match (and are fresh), over posts made to sit on every cutoff the feeds have. The
// same is checked for the personal feed's pool. Without it the two could drift apart one edit at a
// time. It runs only when TOPICFEED_CLICKHOUSE_TEST is set, in a database of its own.
func TestEvaluateFeedAgreesWithTheSQLAgainstClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	tax, err := taxonomy.Load("../../taxonomy/v2.1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig("testdata/feeds.yaml", tax)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := labelpolicy.Load("../../config/label_policy.yaml")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_inspect_%d", time.Now().UnixNano())
	if err := admin.Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+scratch); err != nil {
			t.Errorf("dropping %s: %v", scratch, err)
		}
	})
	dbcfg := chdb.FromEnv()
	dbcfg.Database = scratch
	conn, err := chdb.Open(ctx, dbcfg)
	if err != nil {
		t.Fatal(err)
	}
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
	for file, tables := range map[string][]string{
		"001_init.sql":       {"like_counts_hourly", "deletions", "account_status"},
		"003_mod_labels.sql": {"mod_labels"},
		"006_engagement.sql": {"engagement_hourly"},
	} {
		for _, s := range schemaStatements(t, file) {
			for _, table := range tables {
				if strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS "+table) {
					exec(s)
				}
			}
		}
	}

	// ----- the posts: values on every cutoff the feeds have, and a spread around them -----
	var (
		feeds       []Feed
		personal    *Feed
		thresholds  = []float32{0, 0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 1}
		feedPaths   []string // every subtopic a feed asks for, directly or through its broad topic
		allSubs     []string
		broadOf     = map[string]string{}
		subsOfBroad = map[string][]string{}
	)
	for _, b := range tax.Broad {
		for _, s := range b.Subtopics {
			path := b.ID + "/" + s.ID
			allSubs = append(allSubs, path)
			broadOf[path] = b.ID
			subsOfBroad[b.ID] = append(subsOfBroad[b.ID], path)
		}
	}
	for i := range cfg.Feeds {
		f := cfg.Feeds[i]
		if f.Personal != nil {
			personal = &cfg.Feeds[i]
			thresholds = append(thresholds, f.Personal.MinTopicProb, f.Personal.PoolMinTopicProb)
			continue
		}
		feeds = append(feeds, f)
		thresholds = append(thresholds, f.MinProb)
		for _, p := range f.Paths {
			switch {
			case p == AnyTopic:
			case isBroad(p):
				feedPaths = append(feedPaths, subsOfBroad[p]...)
			default:
				feedPaths = append(feedPaths, p)
			}
		}
		for _, v := range f.Exclude {
			thresholds = append(thresholds, v)
		}
		rules := []Rules{f.Tone, f.Signals}
		for p, o := range f.TopicRules {
			rules = append(rules, o.Tone, o.Signals)
			if o.MinProb > 0 {
				thresholds = append(thresholds, o.MinProb)
			}
			// Posts about the topics with rules of their own, too.
			if isBroad(p) {
				feedPaths = append(feedPaths, subsOfBroad[p]...)
			} else {
				feedPaths = append(feedPaths, p)
			}
		}
		for _, r := range rules {
			for _, m := range []map[string]float32{r.Max, r.Min} {
				for _, v := range m {
					thresholds = append(thresholds, v)
				}
			}
		}
	}
	if len(feeds) == 0 || personal == nil {
		t.Fatalf("testdata/feeds.yaml has %d topic feeds and a personal feed: %v", len(feeds), personal != nil)
	}
	slices.Sort(feedPaths)
	feedPaths = slices.Compact(feedPaths)

	rng := rand.New(rand.NewSource(20261004))
	value := func() float32 {
		if rng.Intn(10) < 4 {
			return thresholds[rng.Intn(len(thresholds))] // right on a cutoff
		}
		return float32(rng.Intn(1001)) / 1000
	}
	at := time.Now().UTC().Truncate(time.Second)
	window := 24 * time.Hour
	adultLabel, dropLabel := policy.AdultOnly[0], policy.Drop[0]

	const n = 700
	inputs := map[string]EvalInput{}
	var rows []diffPipelineRow
	var gone []diffDeletion
	inactive := map[string]bool{}
	for i := 0; i < n; i++ {
		did := fmt.Sprintf("did:plc:author%d", i%90)
		uri := fmt.Sprintf("at://%s/app.bsky.feed.post/p%d", did, i)
		age := time.Duration(rng.Int63n(int64(23 * time.Hour)))
		if rng.Intn(100) < 8 {
			age = window + time.Duration(1+rng.Int63n(int64(20*time.Hour))) // older than the window
		}
		pick := func() string {
			if rng.Intn(10) < 6 {
				return feedPaths[rng.Intn(len(feedPaths))]
			}
			return allSubs[rng.Intn(len(allSubs))]
		}
		path, broad := map[string]float32{}, map[string]float32{}
		var top string
		for k := 1 + rng.Intn(3); k > 0; k-- {
			p := pick()
			path[p] = value()
			if top == "" || path[p] > path[top] {
				top = p
			}
		}
		for p, v := range path {
			b := broadOf[p]
			broad[b] = min(1, broad[b]+v)
			if rng.Intn(10) == 0 {
				broad[b] = value()
			}
		}
		tone, signals := map[string]float32{}, map[string]float32{}
		for _, name := range Tones {
			tone[name] = value()
		}
		for _, name := range Signals {
			signals[name] = value()
		}
		row := diffPipelineRow{URI: uri, DID: did, IndexedAt: at.Add(-age), ProcessedAt: at, FeedPolicy: labelpolicy.OK, Labels: []string{}, Model: "test",
			BroadProbs: broad, PathProbs: path, Signals: signals, Tone: tone, TopBroad: broadOf[top], TopPath: top, TopPathP: path[top]}
		switch r := rng.Intn(100); {
		case r < 8:
			row.FeedPolicy = labelpolicy.AdultOnly
		case r < 12:
			row.FeedPolicy, row.Model = labelpolicy.Drop, ""
		case r < 16:
			row.Model = "" // never scored
		}
		switch r := rng.Intn(100); {
		case r < 6:
			row.Labels = []string{adultLabel}
		case r < 8:
			row.Labels = []string{dropLabel}
		}
		in := EvalInput{
			Post:  Post{URI: uri, DID: did, IndexedAt: row.IndexedAt, TopPath: top, TopPathP: path[top], Tone: tone, Signals: signals, Labels: row.Labels, Likes: uint64(rng.Intn(9))},
			Model: row.Model, FeedPolicy: row.FeedPolicy, PathProbs: path, BroadProbs: broad, Policy: policy,
		}
		if rng.Intn(100) < 4 {
			in.Deleted = true
			gone = append(gone, diffDeletion{DID: did, Collection: "app.bsky.feed.post", RKey: fmt.Sprintf("p%d", i), URI: uri, IndexedAt: at})
		}
		if i%90 == 7 || i%90 == 41 {
			inactive[did] = true
		}
		in.AuthorInactive = inactive[did]
		inputs[uri] = in
		rows = append(rows, row)
	}
	// An author who is inactive is inactive for every post of theirs.
	for uri, in := range inputs {
		in.AuthorInactive = inactive[in.Post.DID]
		inputs[uri] = in
	}
	if err := chdb.Insert(ctx, conn, "post_pipeline", rows); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "deletions", gone); err != nil {
		t.Fatal(err)
	}
	var accounts []diffAccount
	for did := range inactive {
		accounts = append(accounts, diffAccount{DID: did, Active: 0, Status: "suspended", IndexedAt: at})
	}
	if err := chdb.Insert(ctx, conn, "account_status", accounts); err != nil {
		t.Fatal(err)
	}

	store := &Store{Conn: conn, Policy: policy}
	since := at.Add(-window)
	uris := make([]string, 0, len(inputs))
	for uri := range inputs {
		uris = append(uris, uri)
	}
	sort.Strings(uris)

	// ----- the topic feeds: Build's SQL against EvaluateFeed -----
	reasons := map[string]int{}
	matchedAnywhere := 0
	for _, f := range feeds {
		built, _, err := store.Build(ctx, f, since, 100000)
		if err != nil {
			t.Fatalf("%s: %v", f.Rkey, err)
		}
		inBuild := map[string]bool{}
		for _, p := range built {
			inBuild[p.URI] = true
		}
		disagree := 0
		for _, uri := range uris {
			v := EvaluateFeed(f, inputs[uri], at, window)
			want := v.Matches && v.Fresh
			if want != inBuild[uri] {
				disagree++
				if disagree <= 3 {
					t.Errorf("feed %s, %s: the SQL says in=%v, EvaluateFeed says matches=%v fresh=%v (reason %q)\n  input: %+v", f.Rkey, uri, inBuild[uri], v.Matches, v.Fresh, v.Reason, inputs[uri])
				}
			}
			if v.Matches {
				matchedAnywhere++
			} else {
				reasons[strings.SplitN(v.Reason, ":", 2)[0]]++
			}
		}
		if disagree > 0 {
			t.Errorf("feed %s: %d posts disagree (of %d)", f.Rkey, disagree, len(uris))
		}
	}

	// The test has teeth only if posts really are in and out for each kind of reason.
	if matchedAnywhere == 0 {
		t.Error("no post matched any feed: the test checks nothing")
	}
	var names []string
	for r := range reasons {
		names = append(names, r)
	}
	sort.Strings(names)
	t.Logf("%d feeds x %d posts: %d matches; posts left out, by the first rule they fail: %v", len(feeds), len(uris), matchedAnywhere, reasons)
	for _, must := range []string{"Topic", "Scored by the topic model", "Label policy when it was processed", "Not deleted", "Author's account is active", "Labels on the post and its author"} {
		if reasons[must] == 0 {
			t.Errorf("no post was left out for %q: the test doesn't exercise that rule", must)
		}
	}
	for _, prefix := range []string{"Not about ", "Tone", "Signal"} {
		found := false
		for _, r := range names {
			found = found || strings.HasPrefix(r, prefix)
		}
		if !found {
			t.Errorf("no post was left out for a %q rule: the test doesn't exercise it", prefix)
		}
	}

	// ----- the personal feed: the pool's SQL against EvaluatePersonal (less the reaction minimum,
	// which is applied when a viewer's feed is shown, not in the pool) -----
	pool, err := store.TopicPool(ctx, PoolQuery{Since: since, Newest: 100000, Engaged: 0, MinProb: personal.Personal.PoolMinTopicProb})
	if err != nil {
		t.Fatal(err)
	}
	inPool := map[string]bool{}
	for _, ps := range pool {
		for _, p := range ps {
			inPool[p.URI] = true
		}
	}
	disagree, eligible := 0, 0
	for _, uri := range uris {
		v := EvaluatePersonal(*personal, inputs[uri], at)
		want := v.Fresh
		for _, c := range v.Checks {
			if !c.Soft && c.Name != "Enough reactions" && !c.Pass {
				want = false
			}
		}
		if want {
			eligible++
		}
		if want != inPool[uri] {
			disagree++
			if disagree <= 3 {
				t.Errorf("personal pool, %s: the SQL says in=%v, EvaluatePersonal says %v (%+v)", uri, inPool[uri], want, v.Checks)
			}
		}
	}
	if disagree > 0 {
		t.Errorf("personal pool: %d posts disagree", disagree)
	}
	if eligible == 0 || eligible == len(uris) {
		t.Errorf("%d of %d posts are eligible for the pool: the test needs both kinds", eligible, len(uris))
	}
}
