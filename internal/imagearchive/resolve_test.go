package imagearchive

import (
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

var testPolicy = &labelpolicy.Policy{
	Labelers:  []string{"did:plc:labeler"},
	AdultOnly: []string{"porn", "sexual"},
	Drop:      []string{"spam", "gore"},
}

func viewWith(labels []Label, embed map[string]any) *PostView {
	v := &PostView{URI: "at://did:plc:a/app.bsky.feed.post/1", Embed: embed}
	v.Author.DID = "did:plc:a"
	v.Labels = labels
	return v
}

func imagesEmbed() map[string]any {
	return map[string]any{
		"$type": "app.bsky.embed.images#view",
		"images": []any{
			map[string]any{"fullsize": "https://cdn.bsky.app/img/feed_fullsize/plain/did:plc:a/bafy1"},
		},
	}
}

func TestPlanGone(t *testing.T) {
	now := time.Now()
	r, imgs := Plan("at://did:plc:a/app.bsky.feed.post/1", "did:plc:a", nil, testPolicy, now)
	if r.Outcome != OutcomeGone || imgs != nil {
		t.Fatalf("row = %+v, imgs = %+v", r, imgs)
	}
	if r.NImages != 0 {
		t.Fatalf("NImages = %d, want 0", r.NImages)
	}
}

func TestPlanNoImages(t *testing.T) {
	now := time.Now()
	r, imgs := Plan("at://u/1", "did:plc:a", viewWith(nil, nil), testPolicy, now)
	if r.Outcome != OutcomeNoImages || imgs != nil {
		t.Fatalf("row = %+v, imgs = %+v", r, imgs)
	}
}

func TestPlanOK(t *testing.T) {
	now := time.Now()
	r, imgs := Plan("at://u/1", "did:plc:a", viewWith(nil, imagesEmbed()), testPolicy, now)
	if r.Outcome != OutcomeFound || r.Policy != labelpolicy.OK || r.NImages != 1 {
		t.Fatalf("row = %+v", r)
	}
	if len(imgs) != 1 || imgs[0].Status != StatusPending || imgs[0].URL == "" || imgs[0].Idx != 0 {
		t.Fatalf("imgs = %+v", imgs)
	}
	if imgs[0].Policy != labelpolicy.OK {
		t.Fatalf("img policy = %q", imgs[0].Policy)
	}
}

func TestPlanAdultOnly(t *testing.T) {
	now := time.Now()
	r, imgs := Plan("at://u/1", "did:plc:a", viewWith([]Label{{Val: "porn"}}, imagesEmbed()), testPolicy, now)
	if r.Policy != labelpolicy.AdultOnly || r.Outcome != OutcomeFound {
		t.Fatalf("row = %+v", r)
	}
	if len(imgs) != 1 || imgs[0].Status != StatusPending {
		t.Fatalf("adult_only posts' pictures are fetched: imgs = %+v", imgs)
	}
}

func TestPlanDrop(t *testing.T) {
	now := time.Now()
	r, imgs := Plan("at://u/1", "did:plc:a", viewWith([]Label{{Val: "gore"}}, imagesEmbed()), testPolicy, now)
	if r.Policy != labelpolicy.Drop || r.Outcome != OutcomeFound {
		t.Fatalf("row = %+v", r)
	}
	if len(imgs) != 1 || imgs[0].Status != StatusSkippedPolicy {
		t.Fatalf("drop posts' pictures are never fetched: imgs = %+v", imgs)
	}
}

func TestPlanAuthorLabels(t *testing.T) {
	now := time.Now()
	v := viewWith(nil, imagesEmbed())
	v.Author.Labels = []Label{{Val: "spam"}}
	r, imgs := Plan("at://u/1", "did:plc:a", v, testPolicy, now)
	if r.Policy != labelpolicy.Drop {
		t.Fatalf("author label must count: row = %+v", r)
	}
	if len(imgs) != 1 || imgs[0].Status != StatusSkippedPolicy {
		t.Fatalf("imgs = %+v", imgs)
	}
}
