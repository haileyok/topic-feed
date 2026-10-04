package pipeline

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	fetchTimeout    = 15 * time.Second
	maxPictureBytes = 8 << 20
)

// ThumbnailURL returns a JPEG of an attachment: the CDN feed thumbnail (at most 1000 px on a side)
// for images, the poster frame for videos.
func ThumbnailURL(kind, did, cid string) string {
	if kind == "video" {
		return "https://video.bsky.app/watch/" + url.PathEscape(did) + "/" + cid + "/thumbnail.jpg"
	}
	return "https://cdn.bsky.app/img/feed_thumbnail/plain/" + did + "/" + cid + "@jpeg"
}

// Fetch downloads an image, up to maxBytes.
func Fetch(ctx context.Context, client *http.Client, u string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "topic-feed-pipeline")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBytes))
}

// pictureRef names one attachment whose picture the model looks at.
type pictureRef struct{ Kind, CID string }

// pictureRefs are the attachments the model sees: the first max of the post's media, in order. That
// covers images, the items of a carousel (gallery embed) and the media of a quote post, which ingest
// stores as one list, and a video's poster frame. It is the same choice training made (the first two).
func pictureRefs(ps post, max int) []pictureRef {
	var refs []pictureRef
	for i, cid := range ps.MediaCIDs {
		if len(refs) >= max {
			break
		}
		kind := "image"
		if i < len(ps.MediaKinds) {
			kind = ps.MediaKinds[i]
		}
		refs = append(refs, pictureRef{Kind: kind, CID: cid})
	}
	return refs
}

// Stages of pipeline_errors_total for failed picture downloads. They are separate so the live
// failure rate is not buried under retries: a picture that cannot be fetched (its post was
// deleted, say) fails once as stageFetch and then again at every retry as stageRetryFetch.
const (
	stageFetch      = "fetch"       // a post's pictures downloaded for the first time (or by a rescore)
	stageRetryFetch = "retry_fetch" // the retry worker trying a post's missing pictures again
)

// fetchPictures downloads the pictures for refs; a failed download counts as an error of the given
// stage. It returns the ones it got, in order, and how many it could not get (the retry worker
// tries those again later).
func (p *Pipeline) fetchPictures(ctx context.Context, stage, did string, refs []pictureRef) (pics [][]byte, failed int) {
	for _, ref := range refs {
		t0 := time.Now()
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		img, err := Fetch(fctx, p.HTTP, ThumbnailURL(ref.Kind, did, ref.CID), maxPictureBytes)
		cancel()
		metricFetchSeconds.Observe(time.Since(t0).Seconds())
		if err != nil || len(img) == 0 {
			failed++
			metricPictures.WithLabelValues("failed").Inc()
			metricErrors.WithLabelValues(stage).Inc()
			p.Log.Debug("picture fetch failed", "err", err, "did", did, "cid", ref.CID)
			continue
		}
		metricPictures.WithLabelValues("fetched").Inc()
		pics = append(pics, img)
	}
	return pics, failed
}
