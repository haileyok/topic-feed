package feedgen

import (
	"math"
	"slices"
	"time"
)

// Score sets each post's ranking score (see Ranking).
func Score(posts []Post, r Ranking, now time.Time) {
	w := r.Weights
	for i := range posts {
		p := &posts[i]
		prior := max(0.1, 1+float64(p.Substance)+float64(p.GeneralInterest)-r.PromoPenalty*float64(p.Promo))
		eng := w.Like*float64(p.Likes) + w.Repost*float64(p.Reposts) + w.Reply*float64(p.Replies) + w.Quote*float64(p.Quotes)
		age := max(0, now.Sub(p.IndexedAt).Hours())
		p.Score = (prior + eng) / math.Pow(age+2, r.Gravity)
	}
}

// Rank scores the posts and returns them in feed order: highest score first, with every
// FreshEvery-th slot given to the newest post not yet placed, and an author's posts at
// least AuthorGap slots apart where the candidates allow it. Every post appears once.
func Rank(posts []Post, r Ranking, now time.Time) []Post {
	Score(posts, r, now)
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
