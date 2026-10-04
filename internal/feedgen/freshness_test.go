package feedgen

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A post that went viral hours ago stays on top of a feed that asks for the newest posts unless
// big numbers count for less: at engagement power 0.5 the fresh posts win.
func TestEngagementPowerSinksOldViralPosts(t *testing.T) {
	viral := Post{URI: "viral", DID: "a", IndexedAt: now.Add(-6 * time.Hour), Likes: 4000}
	fresh := Post{URI: "fresh", DID: "b", IndexedAt: now.Add(-15 * time.Minute), Likes: 40}
	r := DefaultRanking
	r.Gravity = 3
	order := func(r Ranking) string { return Rank([]Post{viral, fresh}, Feed{Ranking: r}, now)[0].URI }
	if got := order(r); got != "viral" {
		t.Fatalf("in full, 4,000 likes six hours ago should still win (today's behavior): got %s first", got)
	}
	r.EngagementPower = 0.5
	if got := order(r); got != "fresh" {
		t.Errorf("at power 0.5 the fresh post should win: got %s first", got)
	}
	// 0 is the default: in full, as before.
	full, zero := DefaultRanking, DefaultRanking
	full.EngagementPower, zero.EngagementPower = 1, 0
	if a, b := ScoreBreakdown(viral, Feed{Ranking: full}, now), ScoreBreakdown(viral, Feed{Ranking: zero}, now); a != b {
		t.Errorf("power 0 should be power 1: %+v vs %+v", a, b)
	}
	// The breakdown shows the engagement as counted.
	r.EngagementPower = 0.5
	if e := ScoreBreakdown(Post{Likes: 400}, Feed{Ranking: r}, now).Engagement; e != 20 {
		t.Errorf("400 likes at power 0.5 count 20, got %v", e)
	}
}

func TestFeedMaxAge(t *testing.T) {
	day := 24 * time.Hour
	f := Feed{}
	if f.Window(day) != day || !f.Since(now, day).Equal(now.Add(-day)) {
		t.Error("no max age: the service's window")
	}
	f.MaxAgeMinutes = 90
	if f.Window(day) != 90*time.Minute || !f.Since(now, day).Equal(now.Add(-90*time.Minute)) {
		t.Errorf("a 90-minute max age: %v", f.Window(day))
	}
	if f.Window(time.Hour) != time.Hour {
		t.Error("a max age can't reach past the service's window")
	}
}

func TestMaxAgeAndEngagementPowerValidation(t *testing.T) {
	paths := map[string]bool{"art": true}
	ok := func(mod func(*Feed)) error {
		f := Feed{Rkey: "f", DisplayName: "F", Paths: []string{"art"}, MinProb: 0.5, Ranking: DefaultRanking}
		mod(&f)
		return (&Config{Feeds: []Feed{f}}).Validate(paths)
	}
	for name, mod := range map[string]func(*Feed){
		"no max age":         func(f *Feed) {},
		"half an hour":       func(f *Feed) { f.MaxAgeMinutes = 30 },
		"a day":              func(f *Feed) { f.MaxAgeMinutes = 24 * 60 },
		"power 1":            func(f *Feed) { f.Ranking.EngagementPower = 1 },
		"the smallest power": func(f *Feed) { f.Ranking.EngagementPower = MinEngagementPower },
	} {
		if err := ok(mod); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, mod := range map[string]func(*Feed){
		"under half an hour": func(f *Feed) { f.MaxAgeMinutes = 29 },
		"over a day":         func(f *Feed) { f.MaxAgeMinutes = 24*60 + 1 },
		"negative max age":   func(f *Feed) { f.MaxAgeMinutes = -5 },
		"power above 1":      func(f *Feed) { f.Ranking.EngagementPower = 1.5 },
		"power too small":    func(f *Feed) { f.Ranking.EngagementPower = 0.1 },
		"a negative power":   func(f *Feed) { f.Ranking.EngagementPower = -0.5 },
	} {
		if err := ok(mod); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	personal := Feed{Rkey: "p", DisplayName: "P", Personal: &PersonalConfig{}, Ranking: DefaultRanking, MaxAgeMinutes: 60}
	personal.Personal.applyDefaults()
	if err := (&Config{Feeds: []Feed{personal}}).Validate(paths); err == nil || !strings.Contains(err.Error(), "window_hours") {
		t.Errorf("a personal feed's max age is its window_hours: %v", err)
	}
}

func TestFeedSpecKeepsMaxAgeAndEngagementPower(t *testing.T) {
	var s FeedSpec
	if err := json.Unmarshal([]byte(`{"display_name":"x","paths":["art"],"min_prob":0.5,"max_age_minutes":45,"ranking":{"engagement_power":0.5}}`), &s); err != nil {
		t.Fatal(err)
	}
	f := s.Feed("did:plc:x", "x")
	if f.MaxAgeMinutes != 45 || f.Ranking.EngagementPower != 0.5 || f.Ranking.Gravity != DefaultRanking.Gravity {
		t.Errorf("feed %+v", f)
	}
	back, err := SpecOf(f)
	if err != nil || back.MaxAgeMinutes != 45 || back.Ranking.EngagementPower != 0.5 {
		t.Errorf("spec of the feed %+v %v", back, err)
	}
	// A spec saved before these settings existed ranks as before.
	var old FeedSpec
	if err := json.Unmarshal([]byte(`{"display_name":"x","paths":["art"],"min_prob":0.5}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.MaxAgeMinutes != 0 || old.Ranking.power() != 1 {
		t.Errorf("an older spec: %+v", old)
	}
	if _, err := DecodeFeedSpec(strings.NewReader(`{"display_name":"x","paths":["art"],"min_prob":0.5,"max_age_minutes":60,"ranking":{"engagement_power":0.4}}`)); err != nil {
		t.Errorf("a spec sent with both: %v", err)
	}
}

func TestPreviewMaxAge(t *testing.T) {
	s, rb := adultTestServer(t)
	body := func(extra string) string {
		return `{"paths":["technology/ai"],"min_prob":0.5,"tone":{},"signals":{}` + extra + `}`
	}
	since := func() time.Duration {
		rb.mu.Lock()
		defer rb.mu.Unlock()
		return time.Since(rb.sinces[len(rb.sinces)-1]).Round(time.Minute)
	}
	if rec := do(s, http.MethodPost, "/api/preview", body(""), ""); rec.Code != http.StatusOK {
		t.Fatalf("plain preview: %d %s", rec.Code, rec.Body)
	}
	if got := since(); got != 24*time.Hour {
		t.Errorf("no max age reaches back a day, got %v", got)
	}
	if rec := do(s, http.MethodPost, "/api/preview", body(`,"max_age_minutes":45`), ""); rec.Code != http.StatusOK {
		t.Fatalf("45-minute preview: %d %s", rec.Code, rec.Body)
	}
	if got := since(); got != 45*time.Minute {
		t.Errorf("a 45-minute max age, got %v", got)
	}
	if f := rb.last(); f.MaxAgeMinutes != 45 {
		t.Errorf("the built feed: %+v", f)
	}
	if rec := do(s, http.MethodPost, "/api/preview", body(`,"max_age_minutes":5`), ""); rec.Code != http.StatusBadRequest {
		t.Errorf("a 5-minute max age: %d, want 400", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/api/preview", body(`,"ranking":{"weights":{"like":1},"gravity":3,"engagement_power":0.5}`), ""); rec.Code != http.StatusOK {
		t.Errorf("a preview at power 0.5: %d %s", rec.Code, rec.Body)
	} else if f := rb.last(); f.Ranking.EngagementPower != 0.5 {
		t.Errorf("the built feed's power: %+v", f.Ranking)
	}
}

func TestTuningShortWindowAndEngagementPower(t *testing.T) {
	cfg := PersonalConfigDefaults()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	post := func(age time.Duration) Post { return Post{IndexedAt: at.Add(-age), TopPathP: 0.9} }
	half := Tuning{WindowHours: 0.5}
	if err := half.check(); err != nil {
		t.Fatalf("half an hour: %v", err)
	}
	if c := half.Config(cfg); half.Excludes(post(29*time.Minute), c, at) || !half.Excludes(post(31*time.Minute), c, at) {
		t.Error("a window of half an hour")
	}
	if c := half.Config(cfg); c.WindowHours != 1 {
		t.Errorf("the feed's whole-hour window rounds up: %d", c.WindowHours)
	}
	// Saved tunings in whole hours read as before.
	var saved Tuning
	if err := json.Unmarshal([]byte(`{"windowHours":6}`), &saved); err != nil || saved.WindowHours != 6 {
		t.Errorf("a saved window of 6 hours: %+v %v", saved, err)
	}
	for _, bad := range []Tuning{{WindowHours: 0.4}, {WindowHours: -1}, {WindowHours: MaxWindowHours + 1},
		{Ranking: &RankingTuning{EngagementPower: ptr(0.1)}}, {Ranking: &RankingTuning{EngagementPower: ptr(1.1)}}} {
		if err := bad.check(); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
	tun := Tuning{Freshness: FreshnessFresh, Ranking: &RankingTuning{EngagementPower: ptr(0.5)}}
	if err := tun.check(); err != nil {
		t.Fatal(err)
	}
	if r := tun.RankingFor(DefaultRanking); r.EngagementPower != 0.5 || r.Gravity != 3 {
		t.Errorf("ranking %+v", r)
	}
	if !tun.customRanking() {
		t.Error("a popularity setting ranks the viewer's own way")
	}
}
