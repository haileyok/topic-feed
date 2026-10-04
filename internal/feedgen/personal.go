package feedgen

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// unclearTopic is the taxonomy's catch-all for posts the model couldn't place; liking one
// says nothing about what the viewer is interested in.
const unclearTopic = "unclear"

// minGenericPool is how many recent posts a subtopic needs to be part of the mix shown to
// a viewer who hasn't liked enough yet.
const minGenericPool = 20

// TopicShare is one interest: a subtopic path and its share of the viewer's interests.
type TopicShare struct {
	Path  string
	Share float64
}

// Profile is a viewer's interests, strongest first. The shares add up to 1.
type Profile struct {
	Topics  []TopicShare
	Likes   int       // liked, classified posts it rests on (0 for a generic mix)
	BuiltAt time.Time // when it was read from the viewer's likes
}

// NewProfile turns how much of each subtopic a viewer liked (mass) into shares of the
// strongest `topics` interests. "unclear" and empty subtopics are left out; ties are
// broken by path so the same likes always give the same profile.
func NewProfile(mass map[string]float64, likes, topics int, now time.Time) Profile {
	type m struct {
		path string
		mass float64
	}
	ms := make([]m, 0, len(mass))
	for path, v := range mass {
		if path == unclearTopic || !(v > 0) || math.IsInf(v, 0) {
			continue
		}
		ms = append(ms, m{path, v})
	}
	slices.SortFunc(ms, func(a, b m) int {
		if c := cmp.Compare(b.mass, a.mass); c != 0 {
			return c
		}
		return cmp.Compare(a.path, b.path)
	})
	if len(ms) > topics {
		ms = ms[:topics]
	}
	total := 0.0
	for _, x := range ms {
		total += x.mass
	}
	p := Profile{Likes: likes, BuiltAt: now, Topics: make([]TopicShare, len(ms))}
	for i, x := range ms {
		p.Topics[i] = TopicShare{Path: x.path, Share: x.mass / total}
	}
	return p
}

// Personalized reports whether the profile rests on enough likes to be used.
func (p Profile) Personalized(minLikes int) bool { return p.Likes >= minLikes && len(p.Topics) > 0 }

// GenericProfile is an even mix of the `topics` busiest subtopics, for a viewer who hasn't
// liked enough to have interests of their own yet.
func GenericProfile(pool map[string][]Post, topics int, now time.Time) Profile {
	type c struct {
		path string
		n    int
	}
	var cs []c
	for path, ps := range pool {
		if path != unclearTopic && len(ps) >= minGenericPool {
			cs = append(cs, c{path, len(ps)})
		}
	}
	slices.SortFunc(cs, func(a, b c) int {
		if r := cmp.Compare(b.n, a.n); r != 0 {
			return r
		}
		return cmp.Compare(a.path, b.path)
	})
	if len(cs) > topics {
		cs = cs[:topics]
	}
	p := Profile{BuiltAt: now, Topics: make([]TopicShare, len(cs))}
	for i, x := range cs {
		p.Topics[i] = TopicShare{Path: x.path, Share: 1 / float64(len(cs))}
	}
	return p
}

// Feed states: what kind of feed a viewer is getting.
const (
	// StatePersonal is a feed built from the viewer's interests: their likes, or topics they
	// chose.
	StatePersonal = "personal"
	// StateGeneric is a mix of every topic, for a viewer with no interests to go on.
	StateGeneric = "generic"
)

// ProfileFor is the interests a viewer's feed is built from: what they liked (mass, from
// `likes` classified posts) with their tuning applied, and which kind of feed that makes.
//
//   - Enough likes: those interests, adjusted by the tuning.
//   - Too few likes, but topics turned on in the tuning: just those topics, since the viewer
//     chose them.
//   - Otherwise a mix of the busiest subtopics in pool (pool may be nil, which gives no
//     topics), still leaving out any the viewer muted.
//
// If tuning leaves a liker with no interests (everything muted), they get the mix, and
// the mix leaves their muted topics out too: muting is always honoured, even if it empties
// the feed.
func ProfileFor(cfg PersonalConfig, t Tuning, mass map[string]float64, likes int, pool map[string][]Post, now time.Time) (Profile, string) {
	if NewProfile(mass, likes, cfg.Topics, now).Personalized(cfg.MinLikes) {
		if prof := NewProfile(t.ApplyToMass(mass, cfg.Topics), likes, cfg.Topics, now); len(prof.Topics) > 0 {
			return prof, StatePersonal
		}
	} else if t.hasTopics() {
		return NewProfile(t.ApplyToMass(nil, cfg.Topics), likes, cfg.Topics, now), StatePersonal
	}
	g := GenericProfile(pool, cfg.Topics, now)
	even := make(map[string]float64, len(g.Topics))
	for _, x := range g.Topics {
		even[x.Path] = 1
	}
	return NewProfile(t.ApplyToMass(even, cfg.Topics), 0, cfg.Topics, now), StateGeneric
}

// lane is one subtopic's ranked posts, being handed out in order.
type lane struct {
	share  float64
	posts  []Post
	used   []bool // handed out, or left out for good
	head   int    // everything before this index is used
	placed int
}

// peek returns the index of the lane's best post that isn't left out and, if gapOK is
// non-nil, passes it. Posts that are left out are dropped for good.
func (l *lane) peek(skip func(Post) bool, gapOK func(Post) bool) (int, bool) {
	for l.head < len(l.posts) && l.used[l.head] {
		l.head++
	}
	for i := l.head; i < len(l.posts); i++ {
		if l.used[i] {
			continue
		}
		if skip(l.posts[i]) {
			l.used[i] = true
			continue
		}
		if gapOK == nil || gapOK(l.posts[i]) {
			return i, true
		}
	}
	return 0, false
}

// Assemble builds a viewer's feed. Each slot goes to the interest furthest behind its share
// of the slots so far (so a viewer who likes AI posts twice as much as cat posts gets about
// twice as many), and takes that subtopic's best-ranked post that skip doesn't leave out.
// An author's posts are kept authorGap slots apart where the posts allow it. A subtopic
// with nothing left to show gives its slots to the others. Every post appears once, and
// the same inputs always give the same feed.
//
// pool maps each subtopic to its posts, best first (see RankPool).
func Assemble(prof Profile, pool map[string][]Post, skip func(Post) bool, size, authorGap int) []Post {
	type named struct {
		path string
		*lane
	}
	var lanes []named
	total := 0.0
	for _, t := range prof.Topics {
		if ps := pool[t.Path]; len(ps) > 0 {
			total += t.Share
			lanes = append(lanes, named{t.Path, &lane{share: t.Share, posts: ps, used: make([]bool, len(ps))}})
		}
	}
	for _, l := range lanes {
		l.share /= total // topics with nothing to show don't take their share with them
	}

	out := make([]Post, 0, size)
	lastSlot := map[string]int{} // author -> slot of their latest post
	gapOK := func(p Post) bool {
		last, seen := lastSlot[p.DID]
		return authorGap == 0 || !seen || len(out)-last >= authorGap
	}
	order := make([]int, len(lanes))
	for len(out) < size {
		slot := float64(len(out) + 1)
		for i := range order {
			order[i] = i
		}
		// Furthest behind its share first; ties go to the bigger interest, then by path.
		slices.SortFunc(order, func(a, b int) int {
			la, lb := lanes[a], lanes[b]
			if c := cmp.Compare(lb.share*slot-float64(lb.placed), la.share*slot-float64(la.placed)); c != 0 {
				return c
			}
			if c := cmp.Compare(lb.share, la.share); c != 0 {
				return c
			}
			return cmp.Compare(la.path, lb.path)
		})
		picked := false
		// First look for a post whose author hasn't just had one; only when no subtopic has
		// one, take the best post anyway.
		for _, g := range []func(Post) bool{gapOK, nil} {
			for _, i := range order {
				if idx, ok := lanes[i].peek(skip, g); ok {
					p := lanes[i].posts[idx]
					lanes[i].used[idx] = true
					lanes[i].placed++
					lastSlot[p.DID] = len(out)
					out = append(out, p)
					picked = true
					break
				}
			}
			if picked {
				break
			}
		}
		if !picked {
			break
		}
	}
	return out
}

// RankPool orders every subtopic's posts best first, ready for Assemble. The author gap is
// left to Assemble, which keeps authors apart across the whole feed rather than within one
// subtopic.
func RankPool(pool map[string][]Post, r Ranking, now time.Time) map[string][]Post {
	return RankPoolWith(pool, r, Rules{}, Rules{}, now)
}

// RankPoolWith is RankPool with boosts on the model's tone and signal scores (see Rules.Nudge).
func RankPoolWith(pool map[string][]Post, r Ranking, tone, signals Rules, now time.Time) map[string][]Post {
	r.AuthorGap = 0
	f := Feed{Ranking: r, Tone: tone, Signals: signals}
	out := make(map[string][]Post, len(pool))
	for path, ps := range pool {
		out[path] = Rank(slices.Clone(ps), f, now)
	}
	return out
}
