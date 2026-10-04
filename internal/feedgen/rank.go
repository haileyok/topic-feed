package feedgen

import (
	"math"
	"slices"
	"time"
)

// ScoreParts is how a post's ranking score is made: (Prior + Engagement) / Decay, where Decay is
// (age in hours + 2) to the power of the feed's gravity.
type ScoreParts struct {
	// Prior is what a post is worth before anyone has reacted: 1, plus the model's substance and
	// general-interest scores, less the feed's penalty for promotional posts, plus the feed's tone
	// and signal nudges; never under 0.1.
	Prior float64 `json:"prior"`
	// Engagement is its likes, reposts, replies and quotes, each at the feed's weight, raised to
	// the feed's engagement power.
	Engagement float64 `json:"engagement"`
	AgeHours   float64 `json:"ageHours"`
	Decay      float64 `json:"decay"`
	Score      float64 `json:"score"`
}

// ScoreBreakdown is a post's ranking score in the feed, with what it is made of. Score uses it, so
// what is explained is what is ranked.
func ScoreBreakdown(p Post, f Feed, now time.Time) ScoreParts {
	r := f.Ranking
	s := p.Signals
	prior := max(0.1, 1+float64(s["substance"])+float64(s["general_interest"])-r.PromoPenalty*float64(s["promo"])+
		f.Tone.Nudge(p.Tone)+f.Signals.Nudge(s))
	eng := p.Engagement(r.Weights)
	if pw := r.power(); pw != 1 {
		eng = math.Pow(eng, pw)
	}
	age := max(0, now.Sub(p.IndexedAt).Hours())
	decay := math.Pow(age+2, r.Gravity)
	return ScoreParts{Prior: prior, Engagement: eng, AgeHours: age, Decay: decay, Score: (prior + eng) / decay}
}

// Score sets each post's ranking score (see Ranking), including the feed's tone and
// signal nudges.
func Score(posts []Post, f Feed, now time.Time) {
	for i := range posts {
		posts[i].Score = ScoreBreakdown(posts[i], f, now).Score
	}
}

// Rank scores the posts and returns them in feed order: highest score first, with every
// FreshEvery-th slot given to the newest post not yet placed, and an author's posts at
// least AuthorGap slots apart where the candidates allow it. Every post appears once.
func Rank(posts []Post, f Feed, now time.Time) []Post {
	r := f.Ranking
	Score(posts, f, now)
	byScore := slices.Clone(posts)
	slices.SortStableFunc(byScore, func(a, b Post) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return newerFirst(a, b)
	})
	byTime := slices.Clone(posts)
	slices.SortStableFunc(byTime, newerFirst)

	placed := make(map[string]bool, len(posts))
	lastSlot := map[string]int{} // author -> slot of their latest placed post
	heads := [2]int{}            // per list: everything before this index is placed
	out := make([]Post, 0, len(posts))

	pick := func(list []Post, head *int) int {
		for *head < len(list) && placed[list[*head].URI] {
			*head++
		}
		for i := *head; i < len(list); i++ {
			if placed[list[i].URI] {
				continue
			}
			last, seen := lastSlot[list[i].DID]
			if r.AuthorGap == 0 || !seen || len(out)-last >= r.AuthorGap {
				return i
			}
		}
		// Only posts by recently placed authors remain: take the next one anyway.
		if *head < len(list) {
			return *head
		}
		return -1
	}

	for len(out) < len(posts) {
		slot := len(out) + 1
		list, head := byScore, &heads[0]
		if r.FreshEvery > 0 && slot%r.FreshEvery == 0 {
			list, head = byTime, &heads[1]
		}
		i := pick(list, head)
		if i < 0 {
			break
		}
		p := list[i]
		placed[p.URI] = true
		lastSlot[p.DID] = len(out)
		out = append(out, p)
	}
	return out
}

func newerFirst(a, b Post) int {
	if c := b.IndexedAt.Compare(a.IndexedAt); c != 0 {
		return c
	}
	if a.URI > b.URI {
		return -1
	}
	if a.URI < b.URI {
		return 1
	}
	return 0
}
