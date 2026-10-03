package imagearchive

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// Purge settles pictures whose post was deleted or whose author deactivated: their
// rows become purged, and their files are deleted when no other ok picture still
// needs them. Safe to rerun; it only ever touches rows that are still ok.
type Purger struct {
	Store *Store
	Root  string
	Log   *slog.Logger
	Now   func() time.Time
}

type purgedRow struct {
	URI    string `ch:"uri"`
	Idx    uint8  `ch:"idx"`
	Sha256 string `ch:"sha256"`
}

// Run purges deleted and deactivated posts' pictures. It returns how many rows were
// purged and how many files were deleted.
func (p *Purger) Run(ctx context.Context) (rows, files int, err error) {
	now := p.Now
	if now == nil {
		now = time.Now
	}
	log := p.Log
	if log == nil {
		log = slog.Default()
	}

	// Rows still ok whose post is deleted or whose author is deactivated. The author's
	// DID lives in post_image_resolve, so the deactivated check joins through it.
	var doomed []purgedRow
	err = p.Store.Conn.Select(ctx, &doomed, `
		SELECT uri, idx, sha256
		FROM
		(
		    SELECT uri, idx, sha256, status
		    FROM post_images FINAL
		) AS p
		WHERE p.status = 'ok'
		  AND (p.uri IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
		    OR p.uri IN (
		        SELECT r.uri
		        FROM (SELECT uri, did FROM post_image_resolve FINAL) AS r
		        WHERE r.did IN (SELECT did FROM account_status FINAL WHERE active = 0)
		    ))
		ORDER BY uri, idx`)
	if err != nil {
		return 0, 0, fmt.Errorf("select purge candidates: %w", err)
	}
	if len(doomed) == 0 {
		log.Info("purge done", "rows", 0, "files", 0)
		return 0, 0, nil
	}

	// The sha256 of every row that stays ok: a file dies only when it drops out of here.
	var keep []struct {
		Sha256 string `ch:"sha256"`
	}
	if err := p.Store.Conn.Select(ctx, &keep, `
		SELECT DISTINCT sha256
		FROM post_images FINAL
		WHERE status = 'ok' AND uri NOT IN ?`, urisOf(doomed)); err != nil {
		return 0, 0, fmt.Errorf("select kept shas: %w", err)
	}
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k.Sha256] = true
	}

	// Write the purged rows (a file's facts go with them).
	var updates []ImageRow
	seen := map[string]bool{}
	for _, d := range doomed {
		updates = append(updates, ImageRow{
			URI: d.URI, Idx: d.Idx, Status: StatusPurged,
			Sha256: d.Sha256, UpdatedAt: now(),
		})
		// One row per file, not one per post.
		if d.Sha256 == "" || seen[d.Sha256] || keepSet[d.Sha256] {
			continue
		}
		seen[d.Sha256] = true
		for _, sub := range []string{"raw", "1000"} {
			if p.removeFile(ctx, sub, d.Sha256) {
				files++
			}
		}
	}
	if err := chdb.Insert(ctx, p.Store.Conn, "post_images", updates); err != nil {
		return 0, 0, err
	}
	rows = len(updates)
	log.Info("purge done", "rows", rows, "files", files)
	return rows, files, nil
}

// removeFile deletes <root>/<sub>/<sha[:2]>/<sha>.* for a sha no ok row needs anymore.
// The extension is not in the row, so the sha's whole directory entry goes.
func (p *Purger) removeFile(ctx context.Context, sub, sha string) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	dir := filepath.Join(p.Root, sub, sha[:2])
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	removed := false
	for _, e := range entries {
		name := e.Name()
		if len(name) < len(sha) || name[:len(sha)] != sha {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed = true
		}
	}
	return removed
}

func urisOf(rows []purgedRow) []string {
	out := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.URI] {
			seen[r.URI] = true
			out = append(out, r.URI)
		}
	}
	return out
}

// Purge runs the purger (see Purger.Run). Kept as a thin entry point for callers that
// have a conn and do not want to build the struct themselves.
func Purge(ctx context.Context, conn driver.Conn, root string, log *slog.Logger) (int, int, error) {
	p := &Purger{Store: &Store{Conn: conn}, Root: root, Log: log}
	return p.Run(ctx)
}
