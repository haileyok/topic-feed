package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTexts is where previews read what posts say.
type fakeTexts struct {
	mu    sync.Mutex
	err   error
	asked [][]string
}

func (f *fakeTexts) PostTexts(_ context.Context, uris []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slices.Clone(uris))
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, u := range uris {
		out[u] = "  text of\n" + u + "  "
	}
	return out, nil
}

func (f *fakeTexts) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

// tuneRig is a signed-in page over a real personal feed service whose viewers have liked a
// lot of AI and some cats; baseball is the topic they haven't liked.
type tuneRig struct {
	*meRig
	feed  *fakeSource
	p     *Personal
	texts *fakeTexts
	sink  *servedSink
}

func newTuneRig(t *testing.T) *tuneRig {
	t.Helper()
	m := newMeRig(t)
	pool := threeTopicPool(bigPool)
	for _, posts := range pool { // what the model scored, which a preview shows
		for i := range posts {
			posts[i].Tone = map[string]float32{"informative": 0.7, "humorous": 0.3}
			posts[i].Signals = map[string]float32{"substance": 0.5, "news": 0.25}
			if i == 0 { // the others have none: a list that is empty is still a list
				posts[i].Labels = []string{"label-" + posts[i].DID}
			}
			posts[i].Reposts, posts[i].Replies, posts[i].Quotes = 3, 2, 1
		}
	}
	feed := &fakeSource{likes: aiLover(), pool: pool}
	p, _, sink := tunedPersonal(t, feed, m.tun, nil)
	texts := &fakeTexts{}
	m.s.Me.Interests = &InterestsBuilder{Src: m.src, Cfg: PersonalConfigDefaults(), Now: func() time.Time { return now },
		Names: map[string]TopicName{
			ai:                {Name: "AI", Broad: "Technology"},
			cats:              {Name: "Cats", Broad: "Animals and nature"},
			bball:             {Name: "Baseball", Broad: "Sports"},
			"food/baking":     {Name: "Baking", Broad: "Food"},
			"technology":      {Name: "Technology", Broad: "Technology"}, // a broad topic: not offered
			"adult_content/x": {Name: "Adult thing", Broad: "Adult content"},
		}}
	m.s.Me.Personal, m.s.Me.Feed, m.s.Me.Origin, m.s.Me.Texts, m.s.Me.Ranking = p, "for-you", signInOrigin, texts, DefaultRanking
	m.s.Me.Edits, m.s.Me.Previews = NewIPLimiter(time.Hour, 50), NewIPLimiter(time.Hour, 50)
	return &tuneRig{meRig: m, feed: feed, p: p, texts: texts, sink: sink}
}

var fromOurPage = map[string]string{"Content-Type": "application/json", "Origin": signInOrigin}

func (m *meRig) send(method, target, body string, hdr map[string]string, cookies ...*http.Cookie) (int, http.Header, []byte) {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := serveRec(m.s, r)
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

func (m *meRig) save(did, body string) (int, []byte) {
	code, _, out := m.send("PUT", "/api/me/tuning", body, fromOurPage, m.cookie(did))
	return code, out
}

func (m *meRig) previewOf(t *testing.T, did, body string) (PreviewResponse, int) {
	t.Helper()
	code, _, out := m.send("POST", "/api/me/preview", body, fromOurPage, m.cookie(did))
	var r PreviewResponse
	if code == 200 {
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	return r, code
}

func errorCode(body []byte) string {
	var e struct{ Error string }
	_ = json.Unmarshal(body, &e)
	return e.Error
}

func previewTopics(r PreviewResponse) map[string]int {
	out := map[string]int{}
	for _, p := range r.Posts {
		out[p.Topic]++
	}
	return out
}

func TestMeTuningNeedsSignIn(t *testing.T) {
	m := newTuneRig(t)
	for name, cookies := range map[string][]*http.Cookie{
		"no cookie":       nil,
		"a made-up value": {{Name: "__Host-feeds_session", Value: "v1.AAAA.9999999999.AAAA"}},
		"the DID itself":  {{Name: "__Host-feeds_session", Value: meDID}},
	} {
		for _, rq := range []struct{ method, path string }{{"GET", "/api/me/tuning"}, {"PUT", "/api/me/tuning"}, {"POST", "/api/me/preview"}} {
			code, hdr, body := m.send(rq.method, rq.path, `{"freshness":"fresh"}`, fromOurPage, cookies...)
			if code != http.StatusUnauthorized || hdr.Get("Cache-Control") != "no-store" || errorCode(body) != "not signed in" {
				t.Errorf("%s, %s %s: %d %s", name, rq.method, rq.path, code, body)
			}
		}
	}
	if _, saves := m.tun.counts(); saves != 0 {
		t.Error("someone who isn't signed in saved a tuning")
	}
	if m.texts.calls() != 0 || len(m.feed.halfLives()) != 0 {
		t.Error("someone who isn't signed in caused reads")
	}
}

func TestMeTuningIsTheViewersOwn(t *testing.T) {
	m := newTuneRig(t)
	if err := m.tun.SaveTuning(context.Background(), meDID, Tuning{Topics: map[string]float64{cats: 0, bball: 2}, Freshness: FreshnessFresh}); err != nil {
		t.Fatal(err)
	}
	code, hdr, body := m.get("/api/me/tuning?did="+otherDID, map[string]string{"X-Viewer": otherDID}, m.cookie(meDID))
	if code != 200 || hdr.Get("Cache-Control") != "no-store" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %v %s", code, hdr, body)
	}
	var r TuningResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.Tuning.Topics[cats] != 0 || r.Tuning.Topics[bball] != 2 || len(r.Tuning.Topics) != 2 || r.Tuning.Freshness != FreshnessFresh {
		t.Errorf("tuning %+v", r.Tuning)
	}
	cfg := PersonalConfigDefaults()
	d := r.Defaults
	if d.Freshness != FreshnessBalanced || d.AuthorGap != *cfg.AuthorGap || d.HalfLifeDays != cfg.HalfLifeDays || r.MaxWeight != MaxTopicWeight {
		t.Errorf("defaults %+v, max weight %v", d, r.MaxWeight)
	}
	if d.LookbackDays != cfg.LookbackDays || d.MinLikes != cfg.MinLikes || d.Interests != cfg.Topics || d.WindowHours != cfg.WindowHours ||
		d.MinTopicProb != float64(cfg.MinTopicProb) || d.MaxServes != cfg.MaxServes || d.ListSize != cfg.ListSize ||
		d.MinEngagement != *cfg.MinEngagement || d.MinEngagement != DefaultMinEngagement {
		t.Errorf("the feed's own settings: %+v, want those of %+v", d, cfg)
	}
	// The ranking numbers each freshness setting starts from.
	want := func(r Ranking) RankingValues {
		return RankingValues{Gravity: r.Gravity, FreshEvery: r.FreshEvery, PromoPenalty: r.PromoPenalty,
			Like: r.Weights.Like, Repost: r.Weights.Repost, Reply: r.Weights.Reply, Quote: r.Weights.Quote, EngagementPower: 1}
	}
	if len(d.Ranking) != 3 || d.Ranking[FreshnessBalanced] != want(DefaultRanking) {
		t.Errorf("ranking %+v", d.Ranking)
	}
	if g := d.Ranking[FreshnessPopular]; g.Gravity != 1.2 || g.FreshEvery != 0 || g.Like != DefaultRanking.Weights.Like {
		t.Errorf("popular %+v", g)
	}
	if g := d.Ranking[FreshnessFresh]; g.Gravity != 3 || g.FreshEvery != 3 || g.Quote != DefaultRanking.Weights.Quote {
		t.Errorf("fresh %+v", g)
	}
	// The furthest each can go: the feed's own where it can only be made stricter.
	l := r.Limits
	if l.Weight != MaxTopicWeight || l.Boost != MaxBoost || l.Gravity != MaxGravity || l.FreshEvery != MaxFreshEvery || l.PromoPenalty != MaxPromoPenalty ||
		l.Engagement != MaxEngagement || l.AuthorGap != MaxAuthorGap || l.LookbackDays != MaxLookbackDays || l.MinLikes != MaxMinLikes ||
		l.Interests != MaxInterests || l.MaxServes != MaxServesSetting || l.MinEngagement != MaxMinEngagement ||
		l.MinWindowHours != MinWindowHours || l.MinEngagementPower != MinEngagementPower {
		t.Errorf("limits %+v", l)
	}
	if l.WindowHours != cfg.WindowHours || l.ListSize != cfg.ListSize || l.MinTopicProb != float64(cfg.MinTopicProb) {
		t.Errorf("the limits that are the feed's own: %+v", l)
	}
	if !slices.Equal(r.Tones, Tones) || !slices.Equal(r.Signals, Signals) {
		t.Errorf("score names %v %v", r.Tones, r.Signals)
	}
	// Topics that can be added: subtopics only, none adult, in the order a person reads them.
	var got []string
	for _, c := range r.Topics {
		got = append(got, c.Path)
	}
	if want := []string{cats, "food/baking", bball, ai}; !slices.Equal(got, want) {
		t.Errorf("topics %v, want %v", got, want)
	}
	if r.Topics[0].Name != "Cats" || r.Topics[0].Broad != "Animals and nature" {
		t.Errorf("%+v", r.Topics[0])
	}
	// Someone else sees their own, which is nothing.
	code, _, body = m.get("/api/me/tuning", nil, m.cookie(otherDID))
	var other TuningResponse
	if err := json.Unmarshal(body, &other); err != nil || code != 200 || !other.Tuning.IsZero() {
		t.Errorf("%d %s", code, body)
	}
}

func TestMeTuningReadIgnoresASavedTuningThatIsNotValid(t *testing.T) {
	m := newTuneRig(t)
	if err := m.tun.SaveTuning(context.Background(), meDID, Tuning{Topics: map[string]float64{cats: 9}}); err != nil {
		t.Fatal(err)
	}
	code, _, body := m.get("/api/me/tuning", nil, m.cookie(meDID))
	var r TuningResponse
	if err := json.Unmarshal(body, &r); err != nil || code != 200 || !r.Tuning.IsZero() {
		t.Errorf("a tuning the feed ignores was shown: %d %s", code, body)
	}
}

func TestMeTuningReadFailureSaysNothingAboutWhy(t *testing.T) {
	m := newTuneRig(t)
	m.tun.failReads(errors.New("clickhouse: password for user x is wrong"))
	code, _, body := m.get("/api/me/tuning", nil, m.cookie(meDID))
	if code != http.StatusServiceUnavailable || errorCode(body) != "unavailable" || strings.Contains(string(body), "clickhouse") {
		t.Errorf("%d %s", code, body)
	}
}

func TestSavingATuningStoresItAndAppliesItToTheFeed(t *testing.T) {
	m := newTuneRig(t)
	// Their feed as it is: AI and cats.
	before := waitPersonal(t, m.p, meDID, 40)
	if mix := mixOf(m.feed.pool, before); mix[cats] == 0 || mix[ai] == 0 || mix[bball] != 0 {
		t.Fatalf("before: %v", mix)
	}
	code, body := m.save(meDID, `{"topics":{"animals_nature/cats":0,"sports/baseball":3},"freshness":"fresh","authorGap":4,"halfLifeDays":10,"hidePromo":true}`)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var echoed struct{ Tuning Tuning }
	if err := json.Unmarshal(body, &echoed); err != nil || echoed.Tuning.Topics[bball] != 3 || echoed.Tuning.Freshness != FreshnessFresh {
		t.Errorf("answer %s", body)
	}
	gap := 4
	wantSaved(t, m.tun.get(meDID), Tuning{Topics: map[string]float64{cats: 0, bball: 3}, Freshness: FreshnessFresh, AuthorGap: &gap, HalfLifeDays: 10, HidePromo: true})
	if !m.tun.get(otherDID).IsZero() {
		t.Error("another viewer's tuning changed")
	}
	// Their next request assembles a feed from the start with it: no cats, and the topic they
	// turned on.
	eventually(t, "the feed to follow the saved tuning", func() bool {
		mix := mixOf(m.feed.pool, page(t, m.p, meDID, "", 40))
		return mix[cats] == 0 && mix[bball] > 0 && mix[ai] > 0
	})
	// And the page now shows it back.
	_, _, body = m.get("/api/me/tuning", nil, m.cookie(meDID))
	var r TuningResponse
	if err := json.Unmarshal(body, &r); err != nil || r.Tuning.Topics[cats] != 0 || *r.Tuning.AuthorGap != 4 {
		t.Errorf("read back %s", body)
	}
}

func wantSaved(t *testing.T, got, want Tuning) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("saved %s, want %s", g, w)
	}
}

func TestSavingStoresEverySetting(t *testing.T) {
	m := newTuneRig(t)
	body := `{"topics":{"food/baking":1.5},"freshness":"popular","authorGap":0,"halfLifeDays":2.5,"hidePromo":true,"showSeen":true,
		"lookbackDays":14,"minLikes":3,"interests":12,"windowHours":12,"minTopicProb":0.7,"maxServes":1,"listSize":80,"minEngagement":0,
		"ranking":{"gravity":0,"freshEvery":0,"promoPenalty":2,"like":1.5,"repost":0,"reply":3,"quote":4},
		"tone":{"max":{"outraged":0.4},"min":{"informative":0.1},"weights":{"supportive":1.5}},
		"signals":{"min":{"substance":0.3},"max":{"spam":0.1},"weights":{"news":-1}}}`
	if code, out := m.save(meDID, body); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	zero := 0
	wantSaved(t, m.tun.get(meDID), Tuning{Topics: map[string]float64{"food/baking": 1.5}, Freshness: FreshnessPopular, AuthorGap: &zero,
		HalfLifeDays: 2.5, HidePromo: true, ShowSeen: true, LookbackDays: 14, MinLikes: 3, Interests: 12, WindowHours: 12, MinTopicProb: 0.7, MaxServes: 1, ListSize: 80, MinEngagement: ptr(0.0),
		Ranking: &RankingTuning{Gravity: ptr(0.0), FreshEvery: ptr(0), PromoPenalty: ptr(2.0), Like: ptr(1.5), Repost: ptr(0.0), Reply: ptr(3.0), Quote: ptr(4.0)},
		Tone:    Rules{Max: map[string]float32{"outraged": 0.4}, Min: map[string]float32{"informative": 0.1}, Weights: map[string]float64{"supportive": 1.5}},
		Signals: Rules{Min: map[string]float32{"substance": 0.3}, Max: map[string]float32{"spam": 0.1}, Weights: map[string]float64{"news": -1}}})
}

func TestSavingAnEmptyTuningResetsIt(t *testing.T) {
	m := newTuneRig(t)
	if err := m.tun.SaveTuning(context.Background(), meDID, Tuning{Topics: map[string]float64{cats: 0}}); err != nil {
		t.Fatal(err)
	}
	waitPersonal(t, m.p, meDID, 10)
	if code, body := m.save(meDID, `{}`); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if !m.tun.get(meDID).IsZero() {
		t.Errorf("saved %+v", m.tun.get(meDID))
	}
	eventually(t, "cats to come back", func() bool {
		return mixOf(m.feed.pool, page(t, m.p, meDID, "", 40))[cats] > 0
	})
}

// refusals is what neither saving nor previewing accepts.
func refusals() []struct {
	name string
	hdr  map[string]string
	body string
	code int
	err  string
} {
	type c = struct {
		name string
		hdr  map[string]string
		body string
		code int
		err  string
	}
	json := func(origin string) map[string]string {
		h := map[string]string{"Content-Type": "application/json"}
		if origin != "" {
			h["Origin"] = origin
		}
		return h
	}
	return []c{
		{"another site's page", json("https://evil.example"), `{"freshness":"fresh"}`, http.StatusForbidden, "forbidden"},
		{"the same host over http", json("http://feeds.example.com"), `{"freshness":"fresh"}`, http.StatusForbidden, "forbidden"},
		{"a cross-site fetch without an origin", map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, `{"freshness":"fresh"}`, http.StatusForbidden, "forbidden"},
		{"a form post", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": signInOrigin}, `freshness=fresh`, http.StatusUnsupportedMediaType, "unsupported"},
		{"plain text", map[string]string{"Content-Type": "text/plain", "Origin": signInOrigin}, `{"freshness":"fresh"}`, http.StatusUnsupportedMediaType, "unsupported"},
		{"no content type", map[string]string{"Origin": signInOrigin}, `{"freshness":"fresh"}`, http.StatusUnsupportedMediaType, "unsupported"},
		{"an empty body", fromOurPage, ``, http.StatusBadRequest, "invalid"},
		{"not JSON", fromOurPage, `freshness`, http.StatusBadRequest, "invalid"},
		{"a list", fromOurPage, `[{"freshness":"fresh"}]`, http.StatusBadRequest, "invalid"},
		{"null", fromOurPage, `null`, http.StatusOK, ""}, // decodes to nothing: the empty tuning
		{"a setting that doesn't exist", fromOurPage, `{"freshness":"fresh","hideEverything":true}`, http.StatusBadRequest, "invalid"},
		{"two tunings", fromOurPage, `{"freshness":"fresh"}{"freshness":"popular"}`, http.StatusBadRequest, "invalid"},
		{"something after the tuning", fromOurPage, `{"freshness":"fresh"} 7`, http.StatusBadRequest, "invalid"},
		{"a weight that is a string", fromOurPage, `{"topics":{"animals_nature/cats":"0"}}`, http.StatusBadRequest, "invalid"},
		{"a weight past the limit", fromOurPage, `{"topics":{"animals_nature/cats":5.5}}`, http.StatusBadRequest, "invalid"},
		{"a negative weight", fromOurPage, `{"topics":{"animals_nature/cats":-1}}`, http.StatusBadRequest, "invalid"},
		{"a weight too big for a number", fromOurPage, `{"topics":{"animals_nature/cats":1e999}}`, http.StatusBadRequest, "invalid"},
		{"a topic that doesn't exist", fromOurPage, `{"topics":{"nope/nothing":2}}`, http.StatusBadRequest, "invalid"},
		{"a broad topic", fromOurPage, `{"topics":{"technology":2}}`, http.StatusBadRequest, "invalid"},
		{"an adult topic", fromOurPage, `{"topics":{"adult_content/x":2}}`, http.StatusBadRequest, "invalid"},
		{"a freshness that doesn't exist", fromOurPage, `{"freshness":"yesterday"}`, http.StatusBadRequest, "invalid"},
		{"an author gap past the limit", fromOurPage, `{"authorGap":51}`, http.StatusBadRequest, "invalid"},
		{"a half-life past the limit", fromOurPage, `{"halfLifeDays":31}`, http.StatusBadRequest, "invalid"},
		{"a tone cutoff past 1", fromOurPage, `{"tone":{"max":{"outraged":1.5}}}`, http.StatusBadRequest, "invalid"},
		{"a signal cutoff past 1", fromOurPage, `{"signals":{"min":{"substance":1.5}}}`, http.StatusBadRequest, "invalid"},
		{"a tone that doesn't exist", fromOurPage, `{"tone":{"max":{"sarcastic":0.5}}}`, http.StatusBadRequest, "invalid"},
		{"a signal that doesn't exist", fromOurPage, `{"signals":{"weights":{"vibes":1}}}`, http.StatusBadRequest, "invalid"},
		{"a minimum above the maximum", fromOurPage, `{"tone":{"min":{"outraged":0.6},"max":{"outraged":0.5}}}`, http.StatusBadRequest, "invalid"},
		{"a boost past the limit", fromOurPage, `{"signals":{"weights":{"substance":11}}}`, http.StatusBadRequest, "invalid"},
		{"a setting of the rules that doesn't exist", fromOurPage, `{"tone":{"loud":{"outraged":1}}}`, http.StatusBadRequest, "invalid"},
		{"a gravity past the limit", fromOurPage, `{"ranking":{"gravity":11}}`, http.StatusBadRequest, "invalid"},
		{"fresh slots that aren't a whole number", fromOurPage, `{"ranking":{"freshEvery":1.5}}`, http.StatusBadRequest, "invalid"},
		{"a ranking setting that doesn't exist", fromOurPage, `{"ranking":{"speed":1}}`, http.StatusBadRequest, "invalid"},
		{"a lookback past 30 days", fromOurPage, `{"lookbackDays":31}`, http.StatusBadRequest, "invalid"},
		{"a window past a month", fromOurPage, `{"windowHours":1000}`, http.StatusBadRequest, "invalid"},
		{"a topic probability of 1", fromOurPage, `{"minTopicProb":1}`, http.StatusBadRequest, "invalid"},
		{"a list size past 1000", fromOurPage, `{"listSize":2000}`, http.StatusBadRequest, "invalid"},
		{"a count that isn't a whole number", fromOurPage, `{"interests":2.5}`, http.StatusBadRequest, "invalid"},
		{"a body past the size limit", fromOurPage, `{"freshness":"fresh"}` + strings.Repeat(" ", maxTuningBody), http.StatusRequestEntityTooLarge, "too_large"},
	}
}

func TestSavingRefusesWhatItShouldNotAccept(t *testing.T) {
	m := newTuneRig(t)
	for _, c := range refusals() {
		_, savesBefore := m.tun.counts()
		code, _, body := m.send("PUT", "/api/me/tuning", c.body, c.hdr, m.cookie(meDID))
		if code != c.code || errorCode(body) != c.err {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, body, c.code, c.err)
		}
		if _, saves := m.tun.counts(); c.code != 200 && saves != savesBefore {
			t.Fatalf("%s: a tuning was saved", c.name)
		}
	}
}

func TestSavingExplainsAnInvalidTuning(t *testing.T) {
	m := newTuneRig(t)
	code, body := m.save(meDID, `{"topics":{"animals_nature/cats":7}}`)
	var e struct{ Error, Message string }
	_ = json.Unmarshal(body, &e)
	if code != 400 || e.Error != "invalid" || !strings.Contains(e.Message, "animals_nature/cats") || !strings.Contains(e.Message, "between 0 and 5") {
		t.Errorf("%d %s", code, body)
	}
}

func TestSavingSaysSoWhenItFails(t *testing.T) {
	m := newTuneRig(t)
	waitPersonal(t, m.p, meDID, 10)
	m.tun.failSaves(errors.New("clickhouse: connection refused to 10.1.2.3"))
	code, _, body := m.send("PUT", "/api/me/tuning", `{"topics":{"animals_nature/cats":0}}`, fromOurPage, m.cookie(meDID))
	if code != http.StatusServiceUnavailable || errorCode(body) != "unavailable" || strings.Contains(string(body), "10.1.2.3") {
		t.Errorf("%d %s", code, body)
	}
	// The feed carries on as it was.
	if mix := mixOf(m.feed.pool, page(t, m.p, meDID, "", 40)); mix[cats] == 0 {
		t.Errorf("a tuning that wasn't saved changed the feed: %v", mix)
	}
}

func TestSavingIsLimitedPerViewer(t *testing.T) {
	m := newTuneRig(t)
	m.s.Me.Edits = NewIPLimiter(time.Hour, 2)
	for i := range 2 {
		if code, body := m.save(meDID, `{}`); code != 200 {
			t.Fatalf("save %d: %d %s", i+1, code, body)
		}
	}
	code, hdr, body := m.send("PUT", "/api/me/tuning", `{"freshness":"fresh"}`, fromOurPage, m.cookie(meDID))
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" || errorCode(body) != "limited" {
		t.Errorf("%d %v %s", code, hdr, body)
	}
	if !m.tun.get(meDID).IsZero() {
		t.Error("a refused save was stored")
	}
	if code, body := m.save(otherDID, `{}`); code != 200 {
		t.Errorf("someone else: %d %s", code, body)
	}
}

func TestReadingTheTuningIsLimitedPerViewer(t *testing.T) {
	m := newTuneRig(t)
	m.s.Me.Edits = NewIPLimiter(time.Hour, 2)
	for i := range 2 {
		if code, _, body := m.get("/api/me/tuning", nil, m.cookie(meDID)); code != 200 {
			t.Fatalf("read %d: %d %s", i+1, code, body)
		}
	}
	code, hdr, body := m.get("/api/me/tuning", nil, m.cookie(meDID))
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" || errorCode(body) != "limited" {
		t.Errorf("%d %v %s", code, hdr, body)
	}
	if reads, _ := m.tun.counts(); reads != 2 {
		t.Errorf("%d reads of the store: a refused request must not read it", reads)
	}
	if code, _, _ := m.get("/api/me/tuning", nil, m.cookie(otherDID)); code != 200 {
		t.Errorf("someone else: %d", code)
	}
}

func TestOnlyTheRightMethodsAreServed(t *testing.T) {
	m := newTuneRig(t)
	for _, rq := range []struct{ method, path string }{
		{"POST", "/api/me/tuning"}, {"PATCH", "/api/me/tuning"}, {"DELETE", "/api/me/tuning"},
		{"GET", "/api/me/preview"}, {"PUT", "/api/me/preview"},
	} {
		code, _, _ := m.send(rq.method, rq.path, `{}`, fromOurPage, m.cookie(meDID))
		if code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
			t.Errorf("%s %s: %d", rq.method, rq.path, code)
		}
	}
	if _, saves := m.tun.counts(); saves != 0 {
		t.Error("a tuning was saved")
	}
}

func TestTuningRoutesAnswer404WhenTheyAreOff(t *testing.T) {
	m := newTuneRig(t)
	m.s.Me.Personal = nil
	for _, rq := range []struct{ method, path string }{{"PUT", "/api/me/tuning"}, {"POST", "/api/me/preview"}} {
		if code, _, _ := m.send(rq.method, rq.path, `{}`, fromOurPage, m.cookie(meDID)); code != http.StatusNotFound {
			t.Errorf("%s %s: %d", rq.method, rq.path, code)
		}
	}
	m.s.Me = nil
	for _, rq := range []struct{ method, path string }{{"GET", "/api/me/tuning"}, {"PUT", "/api/me/tuning"}, {"POST", "/api/me/preview"}} {
		if code, _, _ := m.send(rq.method, rq.path, `{}`, fromOurPage, m.cookie(meDID)); code != http.StatusNotFound {
			t.Errorf("%s %s: %d", rq.method, rq.path, code)
		}
	}
}

func TestPreviewShowsWhatADraftWouldPick(t *testing.T) {
	m := newTuneRig(t)
	plain, code := m.previewOf(t, meDID, `{}`)
	if code != 200 || plain.State != StatePersonal || len(plain.Posts) != previewPosts {
		t.Fatalf("%d %+v", code, plain)
	}
	if mix := previewTopics(plain); mix["AI"] == 0 || mix["Cats"] == 0 || mix["Baseball"] != 0 {
		t.Errorf("untuned: %v", mix)
	}
	// A draft that mutes cats and turns on baseball.
	draft, code := m.previewOf(t, meDID, `{"topics":{"animals_nature/cats":0,"sports/baseball":5}}`)
	if code != 200 || len(draft.Posts) != previewPosts {
		t.Fatalf("%d %+v", code, draft)
	}
	if mix := previewTopics(draft); mix["Cats"] != 0 || mix["Baseball"] == 0 || mix["AI"] == 0 {
		t.Errorf("tuned: %v", mix)
	}
	p := draft.Posts[0]
	if !strings.HasPrefix(p.URL, "https://bsky.app/profile/did:plc:") || !strings.Contains(p.URL, "/post/1") ||
		p.Broad == "" || p.IndexedAt.IsZero() || p.Likes == 0 {
		t.Errorf("%+v", p)
	}
	// Text is on one line, from the posts' own text.
	if !strings.HasPrefix(p.Text, "text of at://did:plc:") || strings.ContainsAny(p.Text, "\n") || strings.HasPrefix(p.Text, " ") {
		t.Errorf("text %q", p.Text)
	}
	// It asked for the texts of exactly the posts it showed.
	m.texts.mu.Lock()
	asked := m.texts.asked[len(m.texts.asked)-1]
	m.texts.mu.Unlock()
	if len(asked) != previewPosts {
		t.Errorf("asked for %d texts", len(asked))
	}
	for i, u := range asked {
		if postURL(u) != draft.Posts[i].URL {
			t.Errorf("text %d was asked for %s, but the post shown is %s", i, u, draft.Posts[i].URL)
		}
	}
}

func TestPreviewShowsWhatTheFeedKnowsAboutEachPost(t *testing.T) {
	m := newTuneRig(t)
	r, code := m.previewOf(t, meDID, `{}`)
	if code != 200 || len(r.Posts) != previewPosts {
		t.Fatalf("%d %+v", code, r)
	}
	p := r.Posts[0]
	if !strings.HasPrefix(p.URI, "at://did:plc:") || p.DID == "" || !strings.HasPrefix(p.URI, "at://"+p.DID+"/") || p.URL != postURL(p.URI) {
		t.Errorf("who and where: %+v", p)
	}
	if p.TopicPath == "" || p.Topic == "" || p.Broad == "" || len(p.Top) != 1 || p.Top[0].Path != p.TopicPath || p.Top[0].P != 0.9 || p.Top[0].Name != p.Topic {
		t.Errorf("topics: %+v", p)
	}
	if p.Tone["informative"] != 0.7 || p.Tone["humorous"] != 0.3 || p.Signals["substance"] != 0.5 || p.Signals["news"] != 0.25 {
		t.Errorf("scores: %+v %+v", p.Tone, p.Signals)
	}
	if len(p.Labels) != 1 || p.Labels[0] != "label-"+p.DID {
		t.Errorf("labels %v", p.Labels)
	}
	if p.Score <= 0 || p.Likes == 0 || p.Reposts != 3 || p.Replies != 2 || p.Quotes != 1 || p.IndexedAt.IsZero() {
		t.Errorf("what it ranked by: %+v", p)
	}
	for i := 1; i < len(r.Posts); i++ { // every post has them, as lists and not as nulls
		q := r.Posts[i]
		if q.Labels == nil || q.Tone == nil || q.Signals == nil || q.Top == nil {
			t.Fatalf("post %d: %+v", i, q)
		}
	}
	// The topics the feed is built from with this draft, strongest first, with their shares.
	if len(r.Interests) != 2 || r.Interests[0].Name != "AI" || r.Interests[0].Broad != "Technology" || r.Interests[1].Name != "Cats" ||
		math.Abs(r.Interests[0].Share-0.75) > 1e-6 || math.Abs(r.Interests[1].Share-0.25) > 1e-6 {
		t.Errorf("interests %+v", r.Interests)
	}
	tuned, _ := m.previewOf(t, meDID, `{"topics":{"animals_nature/cats":0,"sports/baseball":1}}`)
	if len(tuned.Interests) != 2 || tuned.Interests[0].Name == "Cats" || tuned.Interests[0].Share+tuned.Interests[1].Share < 0.999 {
		t.Errorf("a muted topic is not among the interests: %+v", tuned.Interests)
	}
	for _, in := range tuned.Interests {
		if in.Name == "Cats" {
			t.Errorf("cats are muted: %+v", tuned.Interests)
		}
	}
	// A ranking of their own changes the scores shown.
	cheap, _ := m.previewOf(t, meDID, `{"ranking":{"like":0,"repost":0,"reply":0,"quote":0}}`)
	if cheap.Posts[0].Score >= p.Score {
		t.Errorf("score %v with engagement counting for nothing, %v with it", cheap.Posts[0].Score, p.Score)
	}
}

func TestPreviewSavesAndRecordsNothing(t *testing.T) {
	m := newTuneRig(t)
	waitPersonal(t, m.p, meDID, 5)
	served := func() int { m.sink.mu.Lock(); defer m.sink.mu.Unlock(); return len(m.sink.rows) }
	before := served()
	for range 3 {
		if _, code := m.previewOf(t, meDID, `{"topics":{"animals_nature/cats":0},"freshness":"fresh","halfLifeDays":3}`); code != 200 {
			t.Fatalf("%d", code)
		}
	}
	if _, saves := m.tun.counts(); saves != 0 || !m.tun.get(meDID).IsZero() {
		t.Error("a preview saved a tuning")
	}
	if served() != before {
		t.Error("a preview marked posts as sent to the viewer")
	}
	// Their real feed still has cats.
	if mix := mixOf(m.feed.pool, page(t, m.p, meDID, "", 40)); mix[cats] == 0 {
		t.Errorf("a preview changed the feed: %v", mix)
	}
	// And it is the same preview each time: asking doesn't use posts up.
	a, _ := m.previewOf(t, meDID, `{}`)
	b, _ := m.previewOf(t, meDID, `{}`)
	for i := range a.Posts {
		if a.Posts[i].URL != b.Posts[i].URL {
			t.Fatalf("preview changed between asks at %d", i)
		}
	}
}

func TestPreviewOfAFeedLeftAMix(t *testing.T) {
	m := newTuneRig(t)
	// Muting everything they like leaves the mix of other topics: baseball is all there is.
	r, code := m.previewOf(t, meDID, `{"topics":{"technology/ai":0,"animals_nature/cats":0}}`)
	if code != 200 || r.State != StateGeneric {
		t.Fatalf("%d %+v", code, r)
	}
	if mix := previewTopics(r); len(mix) != 1 || mix["Baseball"] == 0 {
		t.Errorf("%v", mix)
	}
}

func TestPreviewStillWorksWhenTextsCannotBeRead(t *testing.T) {
	m := newTuneRig(t)
	m.texts.err = errors.New("clickhouse is down")
	r, code := m.previewOf(t, meDID, `{}`)
	if code != 200 || len(r.Posts) != previewPosts {
		t.Fatalf("%d %+v", code, r)
	}
	for _, p := range r.Posts {
		if p.Text != "" || p.URL == "" {
			t.Fatalf("%+v", p)
		}
	}
}

func TestPreviewWhenNoPostsAreLoadedYet(t *testing.T) {
	m := newTuneRig(t)
	m.p.feeds["for-you"].mu.Lock()
	m.p.feeds["for-you"].pools = nil
	m.p.feeds["for-you"].mu.Unlock()
	code, _, body := m.send("POST", "/api/me/preview", `{}`, fromOurPage, m.cookie(meDID))
	if code != http.StatusServiceUnavailable || errorCode(body) != "loading" {
		t.Errorf("%d %s", code, body)
	}
}

func TestPreviewRefusesWhatItShouldNotAccept(t *testing.T) {
	m := newTuneRig(t)
	for _, c := range refusals() {
		code, _, body := m.send("POST", "/api/me/preview", c.body, c.hdr, m.cookie(meDID))
		if code != c.code || errorCode(body) != c.err {
			t.Errorf("%s: %d %s, want %d %q", c.name, code, body, c.code, c.err)
		}
	}
	// Whatever was refused did no work.
	if got := m.texts.calls(); got != 1 { // only the one that was fine ("null")
		t.Errorf("%d reads of post texts", got)
	}
}

func TestPreviewIsLimitedPerViewer(t *testing.T) {
	m := newTuneRig(t)
	m.s.Me.Previews = NewIPLimiter(time.Hour, 2)
	for i := range 2 {
		if _, code := m.previewOf(t, meDID, `{}`); code != 200 {
			t.Fatalf("preview %d: %d", i+1, code)
		}
	}
	code, hdr, body := m.send("POST", "/api/me/preview", `{}`, fromOurPage, m.cookie(meDID))
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" || errorCode(body) != "limited" {
		t.Errorf("%d %v %s", code, hdr, body)
	}
	if got := m.texts.calls(); got != 2 {
		t.Errorf("%d reads of post texts: a refused preview must not do the work", got)
	}
	if _, code := m.previewOf(t, otherDID, `{}`); code != 200 {
		t.Errorf("someone else: %d", code)
	}
	// Saving isn't held up by previews.
	if code, body := m.save(meDID, `{}`); code != 200 {
		t.Errorf("save: %d %s", code, body)
	}
}

func TestPreviewAndSaveAreForTheSignedInViewerAlone(t *testing.T) {
	m := newTuneRig(t)
	// Whatever else the request says about whose feed it is, it is the signed-in viewer's.
	hdr := map[string]string{"Content-Type": "application/json", "Origin": signInOrigin, "X-Viewer": otherDID, "Authorization": "Bearer " + otherDID}
	target := "?did=" + otherDID + "&viewer=" + otherDID
	if code, _, body := m.send("POST", "/api/me/preview"+target, `{}`, hdr, m.cookie(meDID)); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, body := m.send("PUT", "/api/me/tuning"+target, `{"topics":{"animals_nature/cats":0}}`, hdr, m.cookie(meDID)); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if viewerOf(m.p, meDID) == nil {
		t.Error("the signed-in viewer's feed was not read")
	}
	if viewerOf(m.p, otherDID) != nil {
		t.Error("another viewer's feed was read because the request named them")
	}
	if got := m.tun.get(meDID); got.Topics[cats] != 0 || len(got.Topics) != 1 {
		t.Errorf("saved %+v", got)
	}
	if !m.tun.get(otherDID).IsZero() {
		t.Error("another viewer's tuning was saved because the request named them")
	}
}
