package feedgen

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// ErrNoSuchHandle is what a handle resolver answers for a handle that names no account.
var ErrNoSuchHandle = errors.New("no such handle")

// InspectSource reads what is stored about a post. *Store implements it.
type InspectSource interface {
	PostFacts(ctx context.Context, uri, did string) (PostFacts, error)
}

// RemotePost is what Bluesky says about a post, asked when we hold nothing to explain it ourselves.
type RemotePost struct {
	Found     bool
	IsReply   bool
	Langs     []string
	CreatedAt time.Time
}

// RemoteSource asks Bluesky about a post.
type RemoteSource interface {
	Lookup(ctx context.Context, uri string) (RemotePost, error)
}

const (
	inspectTimeout = 15 * time.Second
	resolveTimeout = 5 * time.Second
	remoteTimeout  = 5 * time.Second
)

// InspectAPI serves the post inspector (/api/inspect): for one post, what we stored about it, how the
// model scored it, and which feeds would take it. It is for the owner of the feeds only, because it
// lays bare how posts are judged and reads the database for whoever asks.
type InspectAPI struct {
	// Viewer says who is signed in on a request (signin.Handler.Viewer); Owner is the one account
	// allowed to use the inspector.
	Viewer func(r *http.Request) (did string, ok bool)
	Owner  string
	Source InspectSource
	// Feeds are the live feeds: their rules, and what each holds right now.
	Feeds  *Feeds
	Policy *labelpolicy.Policy
	// Names say how topics are called for people; nil: their paths are shown.
	Names map[string]TopicName
	// Resolve finds the DID of a handle (ErrNoSuchHandle if there is none); Handle finds an
	// account's handle ("" if none); Remote asks Bluesky about a post we don't hold. Any may be nil.
	Resolve func(ctx context.Context, handle string) (string, error)
	Handle  func(ctx context.Context, did string) string
	Remote  RemoteSource
	// Interests reads what an account's likes say it is into, with the tuning it saved (Tunings), for
	// the account inspector; nil: that answers 404.
	Interests *InterestsBuilder
	Tunings   TuningStore
	// Limit bounds how often one account can ask: each answer reads the database.
	Limit *IPLimiter
	Log   *slog.Logger
	// Now is the time, for tests.
	Now func() time.Time
}

func (a *InspectAPI) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// ScoredTopic is one topic the model scored a post on (the shape of a topic in the /me preview, with its broad topic).
type ScoredTopic struct {
	Path  string  `json:"path"`
	Name  string  `json:"name"`
	Broad string  `json:"broad"`
	P     float32 `json:"p"`
}

// InspectedPost is the post as the page shows it. Its first fields have the shape of a post in the
// feed preview on /me, which the page's score panels read.
type InspectedPost struct {
	Text      string             `json:"text"`
	Topic     string             `json:"topic"`
	TopicPath string             `json:"topicPath"`
	Broad     string             `json:"broad"`
	Top       []ScoredTopic      `json:"top"` // the model's most likely subtopics, best first
	Tone      map[string]float32 `json:"tone"`
	Signals   map[string]float32 `json:"signals"`
	Labels    []string           `json:"labels"`
	IndexedAt time.Time          `json:"indexedAt"`
	Likes     uint64             `json:"likes"`
	Reposts   uint64             `json:"reposts"`
	Replies   uint64             `json:"replies"`
	Quotes    uint64             `json:"quotes"`

	// What is stored and what happened to it.
	Stored        *StoredPost   `json:"stored,omitempty"`
	Pipeline      *PipelineRow  `json:"pipeline,omitempty"`
	Subtopics     []ScoredTopic `json:"subtopics,omitempty"` // every subtopic the model scored, best first
	Broads        []ScoredTopic `json:"broads,omitempty"`    // every broad topic it scored, best first
	Deleted       bool          `json:"deleted"`
	Inactive      bool          `json:"authorInactive"`
	CurrentLabels []string      `json:"currentLabels"`
	Retry         *RetryInfo    `json:"retry,omitempty"`
}

// RetryInfo is the state of a post's pictures in the retry queue.
type RetryInfo struct {
	Status   string `json:"status"`
	Attempts uint8  `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

// Inspected states.
const (
	inspectScored      = "scored"      // the model scored it
	inspectUnscored    = "unscored"    // processed, but not scored (the label policy dropped it)
	inspectUnprocessed = "unprocessed" // stored, not processed
	inspectNotStored   = "not_stored"  // we didn't keep it
)

// InspectSummary counts what the feeds make of the post.
type InspectSummary struct {
	Feeds    int `json:"feeds"`    // topic feeds asked
	Matching int `json:"matching"` // whose rules the post meets
	InNow    int `json:"inNow"`    // that hold it right now
}

// InspectResponse is the answer to one post.
type InspectResponse struct {
	URI     string         `json:"uri"`
	URL     string         `json:"url"`
	DID     string         `json:"did"`
	Handle  string         `json:"handle,omitempty"`
	State   string         `json:"state"`
	Why     []string       `json:"why,omitempty"` // for a post that isn't scored: what happened to it
	Post    *InspectedPost `json:"post,omitempty"`
	Feeds   []FeedVerdict  `json:"feeds"`
	Summary InspectSummary `json:"summary"`
	Now     time.Time      `json:"now"`
	// WindowHours is how far back the feeds reach.
	WindowHours float64 `json:"windowHours"`
}

func inspectError(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": code}
	if message != "" {
		body["message"] = message
	}
	meJSON(w, status, body)
}

// ServeInspect answers GET /api/inspect?post=<a link to a post, or its at:// address>.
func (a *InspectAPI) ServeInspect(w http.ResponseWriter, r *http.Request) {
	meHeaders(w)
	did, ok := a.Viewer(r)
	if !ok {
		inspectError(w, http.StatusUnauthorized, "not signed in", "")
		return
	}
	if did != a.Owner {
		inspectError(w, http.StatusForbidden, "forbidden", "")
		return
	}
	if a.Limit != nil && !a.Limit.Allow(did) {
		w.Header().Set("Retry-After", "2")
		inspectError(w, http.StatusTooManyRequests, "limited", "")
		return
	}
	ref, err := ParsePostRef(r.URL.Query().Get("post"))
	if err != nil {
		var re *RefError
		if errors.As(err, &re) {
			inspectError(w, http.StatusBadRequest, "invalid", re.Error())
			return
		}
		inspectError(w, http.StatusBadRequest, "invalid", "That isn't a post address.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), inspectTimeout)
	defer cancel()

	authorDID := ref.Actor
	if !ref.IsDID() {
		if a.Resolve == nil {
			inspectError(w, http.StatusBadRequest, "invalid", "Use the author's DID in the address: handles can't be looked up here.")
			return
		}
		rctx, rcancel := context.WithTimeout(ctx, resolveTimeout)
		resolved, err := a.Resolve(rctx, ref.Actor)
		rcancel()
		switch {
		case errors.Is(err, ErrNoSuchHandle):
			inspectError(w, http.StatusNotFound, "no_such_account", "Couldn't find an account with the handle "+strconv.Quote(ref.Actor)+". Check the spelling, or use the post's at:// address.")
			return
		case err != nil || resolved == "":
			a.logf("inspect: resolving a handle failed", "handle", ref.Actor, "err", err)
			inspectError(w, http.StatusServiceUnavailable, "unavailable", "Couldn't look up that handle just now. Try again, or use the post's at:// address.")
			return
		}
		authorDID = resolved
	}
	uri := ref.URI(authorDID)

	facts, err := a.Source.PostFacts(ctx, uri, authorDID)
	if err != nil {
		a.logf("inspect: reading a post failed", "uri", uri, "err", err)
		inspectError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	meJSON(w, http.StatusOK, a.respond(ctx, facts))
}

func (a *InspectAPI) logf(msg string, args ...any) {
	if a.Log != nil {
		a.Log.Error(msg, args...)
	}
}

// respond judges the post against every feed and says what became of it.
func (a *InspectAPI) respond(ctx context.Context, facts PostFacts) InspectResponse {
	now := a.now()
	resp := InspectResponse{URI: facts.URI, DID: facts.DID, Now: now, Feeds: []FeedVerdict{}}
	resp.URL = "https://bsky.app/profile/" + facts.DID + "/post/" + facts.URI[strings.LastIndex(facts.URI, "/")+1:]
	if a.Handle != nil {
		hctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		resp.Handle = a.Handle(hctx, facts.DID)
		cancel()
	}
	if a.Feeds != nil {
		resp.WindowHours = a.Feeds.Window.Hours()
	}

	post := a.describe(facts)
	if facts.Stored != nil || facts.Pipeline != nil {
		resp.Post = post
	}
	switch {
	case facts.Pipeline != nil && facts.Pipeline.Model != "":
		resp.State = inspectScored
	case facts.Pipeline != nil:
		resp.State = inspectUnscored
	case facts.Stored != nil:
		resp.State = inspectUnprocessed
	default:
		resp.State = inspectNotStored
	}
	if resp.State != inspectScored {
		resp.Why = a.explain(ctx, facts, resp.State)
	}
	if facts.Pipeline == nil || a.Feeds == nil {
		return resp
	}

	in := facts.EvalInput(a.Policy)
	for _, f := range a.Feeds.List() {
		if f.Filtered != nil { // its posts are another feed's, and its filters each viewer's own
			continue
		}
		if f.Personal != nil {
			resp.Feeds = append(resp.Feeds, EvaluatePersonal(f, in, now))
			continue
		}
		v := EvaluateFeed(f, in, now, f.Window(a.Feeds.Window))
		limit := a.Feeds.MaxPosts
		if f.MaxPosts > 0 {
			limit = f.MaxPosts
		}
		posts, builtAt, ok := a.Feeds.Posts(f.Rkey)
		if ok {
			v.Live = LiveStatusIn(posts, builtAt, facts.URI, v, facts.Post.IndexedAt, limit, f.Window(a.Feeds.Window))
		}
		resp.Summary.Feeds++
		if v.Matches {
			resp.Summary.Matching++
		}
		if v.Live != nil && v.Live.InBuild {
			resp.Summary.InNow++
		}
		resp.Feeds = append(resp.Feeds, v)
	}
	// Feeds that take the post first, and within them the ones that hold it now; config order otherwise.
	sort.SliceStable(resp.Feeds, func(i, j int) bool {
		x, y := resp.Feeds[i], resp.Feeds[j]
		if x.Personal != y.Personal {
			return y.Personal // the personal feed last: it depends on the viewer
		}
		if x.Matches != y.Matches {
			return x.Matches
		}
		xl, yl := x.Live != nil && x.Live.InBuild, y.Live != nil && y.Live.InBuild
		return xl && !yl
	})
	return resp
}

// name is a topic as it is shown.
func (a *InspectAPI) name(path string) (name, broad string) {
	if t, ok := a.Names[path]; ok {
		return t.Name, t.Broad
	}
	return path, ""
}

// ranked is a map of probabilities as a list, most likely first.
func (a *InspectAPI) ranked(m map[string]float32) []ScoredTopic {
	out := make([]ScoredTopic, 0, len(m))
	for path, p := range m {
		n, b := a.name(path)
		out = append(out, ScoredTopic{Path: path, Name: n, Broad: b, P: p})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].P != out[j].P {
			return out[i].P > out[j].P
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func (a *InspectAPI) describe(f PostFacts) *InspectedPost {
	p := &InspectedPost{Stored: f.Stored, Pipeline: f.Pipeline, Deleted: f.Deleted, Inactive: f.AuthorInactive, CurrentLabels: nonNil(f.CurrentLabels),
		Tone: map[string]float32{}, Signals: map[string]float32{}, Labels: []string{}, Top: []ScoredTopic{},
		Likes: f.Post.Likes, Reposts: f.Post.Reposts, Replies: f.Post.Replies, Quotes: f.Post.Quotes}
	if f.Stored != nil {
		p.Text, p.IndexedAt = f.Stored.Text, f.Stored.IndexedAt
	}
	if f.Pipeline != nil {
		pl := f.Pipeline
		p.IndexedAt = pl.IndexedAt
		p.Tone, p.Signals, p.Labels = nonNilMap(pl.Tone), nonNilMap(pl.Signals), nonNil(pl.Labels)
		p.Subtopics, p.Broads = a.ranked(pl.PathProbs), a.ranked(pl.BroadProbs)
		p.Top = p.Subtopics[:min(3, len(p.Subtopics))]
		p.TopicPath = pl.TopPath
		p.Topic, p.Broad = a.name(pl.TopPath)
	}
	if f.RetryStatus != "" {
		p.Retry = &RetryInfo{Status: f.RetryStatus, Attempts: f.RetryAttempts, Error: f.RetryError}
	}
	return p
}

func nonNilMap(m map[string]float32) map[string]float32 {
	if m == nil {
		return map[string]float32{}
	}
	return m
}

// explain says, in words, why a post has no scores.
func (a *InspectAPI) explain(ctx context.Context, f PostFacts, state string) []string {
	switch state {
	case inspectUnscored:
		out := []string{"The pipeline processed this post but the topic model never scored it, so no feed can take it."}
		if f.Pipeline.FeedPolicy == labelpolicy.Drop {
			out = append(out, "The label policy marks it \"drop\""+labelsSuffix(slices.Concat(f.Pipeline.Labels, f.CurrentLabels))+": posts like that are never classified or shown.")
		} else {
			out = append(out, "Its feed policy is "+strconv.Quote(f.Pipeline.FeedPolicy)+", so it wasn't dropped: it may have had nothing to classify (no text, picture or link of its own).")
		}
		return out
	case inspectUnprocessed:
		return []string{
			"We stored this post, but the pipeline has no record of it.",
			"Either it was posted before the classifier pipeline began running, or it is brand new: new posts are scored about a minute after they arrive.",
		}
	}
	// Not stored: ask Bluesky, if we can, what kind of post it is.
	var out []string
	remote, err := RemotePost{}, error(nil)
	if a.Remote != nil {
		rctx, cancel := context.WithTimeout(ctx, remoteTimeout)
		remote, err = a.Remote.Lookup(rctx, f.URI)
		cancel()
	}
	switch {
	case a.Remote == nil || err != nil:
		out = append(out, "We don't hold this post, so there's nothing to score.")
		if f.SeenText {
			out = append(out, "We did see it (we keep the text of every post for a week), so it was left out when it arrived: replies and posts not tagged as English aren't stored.")
		} else {
			out = append(out, "We have no record of seeing it: it may be older than our data, a reply from more than a week ago, or deleted.")
		}
	case !remote.Found:
		out = append(out, "We don't hold this post, and Bluesky doesn't return it either: it was probably deleted, or its author hides it.")
	case remote.IsReply:
		out = append(out, "This is a reply. Replies aren't stored or classified: the topic model is built for standalone posts, so no feed can take one.")
	case !hasEnglish(remote.Langs):
		if len(remote.Langs) == 0 {
			out = append(out, "This post has no language tag, and only posts tagged as English are kept.")
		} else {
			out = append(out, "This post is tagged "+strings.Join(remote.Langs, ", ")+", not English, and only posts tagged as English are kept.")
		}
	default:
		out = append(out, "This is a top-level post tagged as English, so it should have been kept, but we have no record of it.")
		out = append(out, "It may have arrived while ingest was stopped, or been posted before our data begins.")
	}
	return out
}

func hasEnglish(langs []string) bool {
	for _, l := range langs {
		l = strings.ToLower(l)
		if l == "en" || strings.HasPrefix(l, "en-") {
			return true
		}
	}
	return false
}

func labelsSuffix(labels []string) string {
	labels = slices.Compact(slices.Sorted(slices.Values(labels)))
	if len(labels) == 0 {
		return ""
	}
	return " (labels: " + strings.Join(labels, ", ") + ")"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
