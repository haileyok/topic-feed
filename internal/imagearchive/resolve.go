package imagearchive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// Resolve outcomes.
const (
	OutcomeFound    = "found"     // the AppView showed pictures; they are listed in post_images
	OutcomeNoImages = "no_images" // the AppView showed the post, but it has no pictures of its own
	OutcomeGone     = "gone"      // the AppView has no view of the post (deleted, taken down, hidden)
)

// Image statuses (schema/011_post_images.sql).
const (
	StatusPending       = "pending"        // waiting to be fetched
	StatusOK            = "ok"             // downloaded; sha256 names the file
	StatusImgGone       = "gone"           // 404/403/410/451: the picture is not there anymore
	StatusErrorRow      = "error"          // rate limited, server error, or network; one more pass may fix it
	StatusBadImage      = "bad_image"      // downloaded but not a decodable image
	StatusSkippedPolicy = "skipped_policy" // the post's policy is drop; never fetched
	StatusPurged        = "purged"         // ok once, but the post was deleted or the author deactivated
)

// MaxAttempts is how many fetches are tried before a picture's status stays error.
const MaxAttempts = 4

// Plan is the pure half of resolving a post: given what the AppView shows for it, the
// row to write to post_image_resolve and the rows to write to post_images. A nil view
// means the AppView has no view of the post.
func Plan(uri, did string, view *PostView, pol *labelpolicy.Policy, now time.Time) (ResolveRow, []ImageRow) {
	r := ResolveRow{URI: uri, DID: did, ResolvedAt: now, UpdatedAt: now}
	if view == nil {
		r.Outcome = OutcomeGone
		return r, nil
	}
	r.DID = view.Author.DID
	r.Labels = view.LabelValues()
	r.Policy = pol.Decide(r.Labels)
	refs := RefsFromEmbed(view.Embed)
	r.NImages = uint8(len(refs))
	if len(refs) == 0 {
		r.Outcome = OutcomeNoImages
		return r, nil
	}
	r.Outcome = OutcomeFound
	var images []ImageRow
	for _, ref := range refs {
		img := ImageRow{
			URI: uri, Idx: uint8(ref.Idx), Kind: ref.Kind, CID: ref.CID, URL: ref.URL,
			Policy: r.Policy, UpdatedAt: now,
		}
		if r.Policy == labelpolicy.Drop {
			img.Status = StatusSkippedPolicy // a drop post's pictures are never fetched
		} else {
			img.Status = StatusPending
		}
		images = append(images, img)
	}
	return r, images
}

// noImageEmbedTypes are stored embed types whose posts can be settled without an API
// call: a post with no embed, or a plain record (quote) embed, has no pictures of its
// own (a quote's pictures belong to the quoted post, which is resolved on its own).
var noImageEmbedTypes = map[string]bool{"none": true, "record": true}

// Resolver asks the AppView about labeled posts and records what it shows, batch by
// batch, until none are left. Safe to stop and rerun: resolved posts are skipped.
type Resolver struct {
	Store   *Store
	AppView *AppView
	Policy  *labelpolicy.Policy
	Log     *slog.Logger
	Batch   int     // posts per outer loop; default 5000
	Workers int     // parallel getPosts calls; default 4
	RPS     float64 // getPosts calls per second, shared by the workers; default 4
	Now     func() time.Time
}

// Run resolves labeled posts until none are left, at most limit (0 for all). Every
// batch is fully written before the next is read, so a stopped run leaves no holes.
func (r *Resolver) Run(ctx context.Context, limit int) error {
	batch := r.Batch
	if batch <= 0 {
		batch = 5000
	}
	workers := r.Workers
	if workers <= 0 {
		workers = 4
	}
	rps := r.RPS
	if rps <= 0 {
		rps = 4
	}
	limiter := rate.NewLimiter(rate.Limit(rps), int(rps))
	now := r.Now
	if now == nil {
		now = time.Now
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}

	var done int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		take := batch
		if limit > 0 {
			take = min(take, limit-done)
		}
		cands, err := r.Store.SelectUnresolved(ctx, take)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			log.Info("resolve done", "resolved", done)
			return nil
		}
		var settle, ask []Candidate
		for _, c := range cands {
			if noImageEmbedTypes[c.EmbedType] {
				settle = append(settle, c)
			} else {
				ask = append(ask, c)
			}
		}
		var resolves []ResolveRow
		var images []ImageRow
		for _, c := range settle {
			// No API call: the post's own embed holds no pictures.
			resolves = append(resolves, ResolveRow{
				URI: c.URI, DID: c.DID, Outcome: OutcomeNoImages,
				ResolvedAt: now(), UpdatedAt: now(),
			})
		}

		// Ask for the rest in chunks of 25, claimed by the workers as they finish.
		var chunks [][]Candidate
		for i := 0; i < len(ask); i += MaxURIsPerCall {
			chunks = append(chunks, ask[i:min(i+MaxURIsPerCall, len(ask))])
		}
		type chunkResult struct {
			rows     []ResolveRow
			images   []ImageRow
			fetchErr error
		}
		results := make([]chunkResult, len(chunks))
		var next atomic.Int64
		var mu sync.Mutex
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(next.Add(1)) - 1
					if i >= len(chunks) {
						return
					}
					if ctx.Err() != nil {
						return
					}
					rows, imgs, cerr := r.resolveChunk(ctx, chunks[i], limiter)
					mu.Lock()
					results[i] = chunkResult{rows: rows, images: imgs, fetchErr: cerr}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var werr error
		for _, res := range results {
			if res.fetchErr != nil {
				werr = errors.Join(werr, res.fetchErr)
			}
			resolves = append(resolves, res.rows...)
			images = append(images, res.images...)
		}
		if werr != nil {
			return fmt.Errorf("resolving batch: %w", werr)
		}

		if err := chdb.Insert(ctx, r.Store.Conn, "post_image_resolve", resolves); err != nil {
			return err
		}
		if err := chdb.Insert(ctx, r.Store.Conn, "post_images", images); err != nil {
			return err
		}
		done += len(cands)
		log.Info("resolve progress", "resolved", done, "batch", len(cands),
			"asked", len(ask), "settled_without_call", len(settle))
		if limit > 0 && done >= limit {
			log.Info("resolve reached limit", "resolved", done)
			return nil
		}
	}
}

// resolveChunk asks the AppView for one chunk (at most 25 posts) and plans each post
// from the answer. On a 400 for the whole chunk (one malformed or taken-down URI can
// poison the call), it asks for each URI on its own instead.
func (r *Resolver) resolveChunk(ctx context.Context, cands []Candidate, limiter *rate.Limiter) ([]ResolveRow, []ImageRow, error) {
	uris := make([]string, len(cands))
	for i, c := range cands {
		uris[i] = c.URI
	}
	views, err := r.getPosts(ctx, uris, limiter)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Code == 400 {
			var resolves []ResolveRow
			var images []ImageRow
			for _, c := range cands {
				rr, imgs, ferr := r.resolveChunk(ctx, []Candidate{c}, limiter)
				if ferr != nil {
					return nil, nil, ferr
				}
				resolves = append(resolves, rr...)
				images = append(images, imgs...)
			}
			return resolves, images, nil
		}
		return nil, nil, err
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	var resolves []ResolveRow
	var images []ImageRow
	for _, c := range cands {
		var view *PostView
		if v, ok := views[c.URI]; ok {
			view = &v
		}
		rr, imgs := Plan(c.URI, c.DID, view, r.Policy, now())
		resolves = append(resolves, rr)
		images = append(images, imgs...)
	}
	return resolves, images, nil
}

// getPosts is AppView.GetPosts, sharing limiter across calls. The limiter lives in Run
// so every worker draws from the same rate.
func (r *Resolver) getPosts(ctx context.Context, uris []string, limiter *rate.Limiter) (map[string]PostView, error) {
	av := *r.AppView // copy; the limiter is passed per call
	av.Limiter = limiter
	return av.GetPosts(ctx, uris)
}
