// Command profile shows what a personal feed would do for one viewer: the interests it reads
// from their likes and the first posts it would show, with how long each step took. It only
// reads from ClickHouse.
//
//	profile -did did:plc:...            [-feed for-you] [-config config/feeds.yaml] [-n 40]
//	profile -serve :8720 [-did ...]     [-feed for-you]
//
// -feed names a personal feed in the config whose settings to use (without it, or when
// the config has none, the defaults). CLICKHOUSE_ADDR, CLICKHOUSE_DB, CLICKHOUSE_USER and
// CLICKHOUSE_PASSWORD say how to reach the database.
//
// -serve starts a web page instead: it shows what a viewer's likes say they are interested
// in (the same interests their personal feed is built from), for any handle or DID, starting
// with -did, FEEDGEN_HANDLE or FEEDGEN_OWNER_DID. It only reads, and listens on every network
// interface of this machine unless the address says otherwise (127.0.0.1:8720).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func main() {
	did := flag.String("did", "", "the viewer's DID (with -serve, the account to start from: a handle works too)")
	feedName := flag.String("feed", "", "rkey of a personal feed in the config to take settings from")
	configPath := flag.String("config", "config/feeds.yaml", "feed config")
	policyPath := flag.String("label-policy", "config/label_policy.yaml", "label policy")
	taxPath := flag.String("taxonomy", "taxonomy/v2.1.yaml", "taxonomy")
	n := flag.Int("n", 40, "posts to print")
	pages := flag.Int("pages", 0, "then ask for the viewer's feed this many times, as the app does when it refreshes, and show what is repeated")
	serve := flag.String("serve", "", "serve the interests page on this address (for example :8720) instead of printing")
	flag.Parse()
	if *serve != "" {
		if err := runServe(context.Background(), *serve, *did, *feedName, *configPath, *policyPath, *taxPath); err != nil {
			fmt.Fprintln(os.Stderr, "profile:", err)
			os.Exit(1)
		}
		return
	}
	if !strings.HasPrefix(*did, "did:") {
		fmt.Fprintln(os.Stderr, "usage: profile -did did:plc:... [-feed rkey] [-n 40] [-pages 3]")
		os.Exit(2)
	}
	if err := run(context.Background(), *did, *feedName, *configPath, *policyPath, *taxPath, *n, *pages); err != nil {
		fmt.Fprintln(os.Stderr, "profile:", err)
		os.Exit(1)
	}
}

// loadSettings returns the personal feed settings to use: the named feed's from the config, or
// the defaults when no feed is named.
func loadSettings(feedName, configPath, taxPath string) (feedgen.PersonalConfig, feedgen.Ranking, error) {
	cfg := feedgen.PersonalConfigDefaults()
	ranking := feedgen.DefaultRanking
	if feedName == "" {
		return cfg, ranking, nil
	}
	tax, err := taxonomy.Load(taxPath)
	if err != nil {
		return cfg, ranking, err
	}
	c, err := feedgen.LoadConfig(configPath, tax)
	if err != nil {
		return cfg, ranking, err
	}
	for _, f := range c.Feeds {
		if f.Rkey == feedName && f.Personal != nil {
			return *f.Personal, f.Ranking, nil
		}
	}
	return cfg, ranking, fmt.Errorf("no personal feed %q in %s", feedName, configPath)
}

func run(ctx context.Context, did, feedName, configPath, policyPath, taxPath string, n, pages int) error {
	cfg, ranking, err := loadSettings(feedName, configPath, taxPath)
	if err != nil {
		return err
	}
	policy, err := labelpolicy.Load(policyPath)
	if err != nil {
		return err
	}
	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()
	store := &feedgen.Store{Conn: conn, Policy: policy}
	now := time.Now().UTC()

	t0 := time.Now()
	likes, err := store.LikeProfile(ctx, did, now.AddDate(0, 0, -cfg.LookbackDays), now,
		time.Duration(cfg.HalfLifeDays*24*float64(time.Hour)))
	if err != nil {
		return err
	}
	fmt.Printf("likes and reposts in the last %d days: %d posts, %d of them classified (%s)\n",
		cfg.LookbackDays, len(likes.Liked), likes.Posts, since(t0))
	prof := feedgen.NewProfile(likes.Mass, likes.Posts, cfg.Topics, now)

	t0 = time.Now()
	raw, err := store.TopicPool(ctx, cfg.PoolQuery(now, ranking))
	if err != nil {
		return err
	}
	total, under1h := 0, 0
	var ages []time.Duration
	for _, ps := range raw {
		total += len(ps)
		for _, p := range ps {
			age := now.Sub(p.IndexedAt)
			ages = append(ages, age)
			if age < time.Hour {
				under1h++
			}
		}
	}
	fmt.Printf("candidate pool: %d posts in %d subtopics from the last %d hours (%s)\n", total, len(raw), cfg.WindowHours, since(t0))
	if total > 0 {
		slices.Sort(ages)
		fmt.Printf("  how old: median %s, 90th percentile %s, oldest %s; %d%% under an hour\n",
			ages[len(ages)/2].Round(time.Minute), ages[len(ages)*9/10].Round(time.Minute), ages[len(ages)-1].Round(time.Minute),
			under1h*100/total)
	}
	pool := feedgen.RankPool(raw, ranking, now)

	if prof.Personalized(cfg.MinLikes) {
		fmt.Printf("\ninterests (%d classified likes, half-life %.0f days):\n", prof.Likes, cfg.HalfLifeDays)
	} else {
		fmt.Printf("\nonly %d classified likes (need %d): the viewer would get an even mix of the busiest subtopics\n", prof.Likes, cfg.MinLikes)
		prof = feedgen.GenericProfile(pool, cfg.Topics, now)
	}
	for _, t := range prof.Topics {
		fmt.Printf("  %5.1f%%  %-40s %4d posts in the pool\n", t.Share*100, t.Path, len(pool[t.Path]))
	}

	skip := func(p feedgen.Post) bool {
		_, liked := likes.Liked[p.URI]
		return liked || p.DID == did
	}
	t0 = time.Now()
	feed := feedgen.Assemble(prof, pool, skip, cfg.ListSize, *cfg.AuthorGap)
	fmt.Printf("\nassembled %d posts (%s); the first %d:\n", len(feed), since(t0), min(n, len(feed)))

	shown := feed[:min(n, len(feed))]
	uris := make([]string, len(shown))
	for i, p := range shown {
		uris[i] = p.URI
	}
	texts, err := postTexts(ctx, store, uris)
	if err != nil {
		return err
	}
	byTopic := map[string]int{}
	for i, p := range shown {
		byTopic[p.TopPath]++
		fmt.Printf("%3d  %-34s %5s old  %3d likes  %s\n", i+1, p.TopPath, age(now.Sub(p.IndexedAt)), p.Likes, snippet(texts[p.URI]))
	}
	fmt.Println("\nslots by subtopic in those posts:")
	var paths []string
	for path := range byTopic {
		paths = append(paths, path)
	}
	slices.SortFunc(paths, func(a, b string) int { return byTopic[b] - byTopic[a] })
	for _, path := range paths {
		fmt.Printf("  %3d  %s\n", byTopic[path], path)
	}
	if pages > 0 {
		return simulate(ctx, store, did, cfg, ranking, pages)
	}
	return nil
}

// simulate asks the personal feed service for the viewer's feed several times, as the app
// does when it is refreshed, with what is sent kept in memory only (nothing is written).
func simulate(ctx context.Context, store *feedgen.Store, did string, cfg feedgen.PersonalConfig, ranking feedgen.Ranking, requests int) error {
	f := feedgen.Feed{Rkey: "sim", DisplayName: "sim", Ranking: ranking, Personal: &cfg}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := feedgen.NewPersonal([]feedgen.Feed{f}, store, log)
	p.Budget = 30 * time.Second
	p.Start(ctx)

	fmt.Printf("\nasking the service for the feed %d times, 30 posts each (max_serves %d):\n", requests, cfg.MaxServes)
	sent := map[string]int{}
	for i := 1; i <= requests; i++ {
		t0 := time.Now()
		pg, err := p.Page(ctx, "sim", did, "", 30)
		if err != nil {
			return err
		}
		again := 0
		for _, it := range pg.Items {
			if sent[it.URI] > 0 {
				again++
			}
			sent[it.URI]++
		}
		note := ""
		if i == 1 {
			note = " (includes reading the viewer's likes and what they have seen)"
		}
		fmt.Printf("  request %d: %-8s %2d posts, %2d of them sent before, next page: %v, %s%s\n",
			i, pg.State, len(pg.Items), again, pg.Cursor != "", since(t0), note)
	}
	return nil
}

func postTexts(ctx context.Context, s *feedgen.Store, uris []string) (map[string]string, error) {
	var rows []struct {
		URI  string `ch:"uri"`
		Text string `ch:"text"`
	}
	if err := s.Conn.Select(ctx, &rows, `SELECT uri, text FROM posts WHERE uri IN ?`, uris); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.URI] = r.Text
	}
	return out, nil
}

func snippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 90 {
		return string(r[:90]) + "…"
	}
	return s
}

func age(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1fh", d.Hours())
}

func since(t time.Time) string { return time.Since(t).Round(time.Millisecond).String() }
