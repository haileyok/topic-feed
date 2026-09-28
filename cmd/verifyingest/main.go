// Command verifyingest checks ingest for missing data: it re-reads a sequence range
// straight from the Jetstream archive and confirms every post create in it is in
// post_texts (which stores every post create ingest sees, of any language or age).
//
//	verifyingest -after 26373500000 -before 26375000000
//
// Environment: JETSTREAM_API_KEY, JETSTREAM_HOST (default jetstream.us-east.bsky.network),
// and the CLICKHOUSE_* variables read by chdb.FromEnv.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/bluesky-social/jetstream"

	"github.com/haileyok/topic-feed/internal/chdb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "verifyingest:", err)
		os.Exit(1)
	}
}

func run() error {
	after := flag.Uint64("after", 0, "exclusive lower sequence bound")
	before := flag.Uint64("before", 0, "inclusive upper sequence bound")
	flag.Parse()
	if *after == 0 || *before <= *after {
		return fmt.Errorf("need -after and -before with before > after")
	}
	host := os.Getenv("JETSTREAM_HOST")
	if host == "" {
		host = "jetstream.us-east.bsky.network"
	}
	ctx := context.Background()

	client, err := jetstream.Subscribe(host,
		jetstream.WithAPIKey(os.Getenv("JETSTREAM_API_KEY")),
		jetstream.WithCollections([]string{"app.bsky.feed.post"}),
		jetstream.WithKinds([]jetstream.Kind{jetstream.KindCommit}),
		jetstream.WithAfterSeq(*after),
		jetstream.WithBeforeSeq(*before),
		jetstream.WithSnapshotOnly(),
		jetstream.WithDownloadConcurrency(1),
		jetstream.WithSegmentStripes(1),
		jetstream.WithRawRecords(), // we only need URIs, not decoded records
	)
	if err != nil {
		return err
	}
	defer client.Close()

	uris := map[string]time.Time{}
	var minT, maxT time.Time
	for batch, err := range client.Events(ctx) {
		if err != nil {
			// Any archive error means the reference set is incomplete; don't guess.
			return fmt.Errorf("archive read failed, result would be unreliable: %w", err)
		}
		for _, ev := range batch.Events() {
			if ev.Commit == nil || ev.Commit.Operation != jetstream.OpCreate {
				continue
			}
			t := time.UnixMicro(ev.TimeUS).UTC()
			uris["at://"+ev.DID+"/app.bsky.feed.post/"+ev.Commit.Rkey] = t
			if minT.IsZero() || t.Before(minT) {
				minT = t
			}
			if t.After(maxT) {
				maxT = t
			}
		}
	}
	fmt.Printf("archive: %d post creates in seqs (%d, %d], event time %s .. %s\n",
		len(uris), *after, *before, minT.Format(time.RFC3339), maxT.Format(time.RFC3339))

	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()

	all := make([]string, 0, len(uris))
	for u := range uris {
		all = append(all, u)
	}
	found := map[string]bool{}
	// Chunks stay well under ClickHouse's default 256KB query size limit.
	for i := 0; i < len(all); i += 2000 {
		chunk := all[i:min(i+2000, len(all))]
		rows, err := conn.Query(ctx, "SELECT DISTINCT uri FROM post_texts WHERE uri IN (?)", chunk)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u string
			if err := rows.Scan(&u); err != nil {
				return err
			}
			found[u] = true
		}
		rows.Close()
	}

	var missing []string
	for _, u := range all {
		if !found[u] {
			missing = append(missing, u)
		}
	}
	fmt.Printf("post_texts: %d found, %d missing\n", len(found), len(missing))
	if len(missing) > 0 {
		// Group missing posts by minute to show whether they cluster (a gap) or scatter.
		byMinute := map[string]int{}
		for _, u := range missing {
			byMinute[uris[u].Truncate(time.Minute).Format("2006-01-02 15:04")]++
		}
		keys := make([]string, 0, len(byMinute))
		for k := range byMinute {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %s  %d missing\n", k, byMinute[k])
		}
		for _, u := range missing[:min(10, len(missing))] {
			fmt.Println("  e.g.", u)
		}
		os.Exit(2)
	}
	return nil
}
