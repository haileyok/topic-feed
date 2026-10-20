package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	// defaultWelcomeText is what a viewer sees at the top of a personal feed the first time,
	// while their feed is read from their likes (a second or two).
	defaultWelcomeText = "Hey, welcome! We're building your feed from the posts you've liked. Check back in a few seconds, or pull down to refresh."

	maxPostText = 300 // graphemes; runes are a close upper bound

	// defaultDaysAgo dates the welcome post about three months back. Bluesky sorts a post by the
	// earlier of its createdAt and the time it first saw it, so a backdated post lands that far
	// down followers' timelines and the author's profile instead of at the top.
	defaultDaysAgo = 90
	maxDaysAgo     = 3650
)

// validateDaysAgo checks how far back to date the post.
func validateDaysAgo(n int) error {
	if n < 0 || n > maxDaysAgo {
		return fmt.Errorf("-days-ago must be 0-%d, got %d", maxDaysAgo, n)
	}
	return nil
}

// welcomeRecord is the post record: the text, dated daysAgo days before now. Web addresses in the
// text are made links (a facet each): Bluesky doesn't link a bare address in a post's text.
func welcomeRecord(text string, now time.Time, daysAgo int) map[string]any {
	rec := map[string]any{
		"$type": "app.bsky.feed.post", "text": text, "langs": []string{"en"},
		"createdAt": now.UTC().AddDate(0, 0, -daysAgo).Format(time.RFC3339),
	}
	if facets := linkFacets(text); len(facets) > 0 {
		rec["facets"] = facets
	}
	return rec
}

// linkFacets are app.bsky.richtext.facet links for every https:// address in text, by byte offsets.
func linkFacets(text string) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(text); {
		j := strings.Index(text[i:], "https://")
		if j < 0 {
			break
		}
		start := i + j
		end := start
		for end < len(text) && !strings.ContainsRune(" \n\t", rune(text[end])) {
			end++
		}
		for end > start && strings.ContainsRune(".,;:!?)", rune(text[end-1])) { // trailing punctuation isn't the address
			end--
		}
		out = append(out, map[string]any{
			"index":    map[string]int{"byteStart": start, "byteEnd": end},
			"features": []map[string]string{{"$type": "app.bsky.richtext.facet#link", "uri": text[start:end]}},
		})
		i = end + 1
	}
	return out
}

// welcomeKind is which feeds a post is for: the personal feed's welcome post, or the post the
// filtered feeds show viewers who haven't signed in for them.
type welcomeKind struct {
	envVar      string
	defaultText func(host string) string
}

var welcomeKinds = map[string]welcomeKind{
	"personal": {"FEEDGEN_WELCOME_POST", func(string) string { return defaultWelcomeText }},
	"filtered": {"FEEDGEN_FILTER_SIGNIN_POST", func(host string) string {
		return "This feed needs you to sign in first. Go to https://" + host + "/filtered and sign in with Bluesky: " +
			"that lets it fetch Discover or For You as you, and choose what it leaves out. Then pull down to refresh."
	}},
}

// welcome posts the welcome message as the feeds' owner, dated daysAgo days back. The post is
// real and public, so at a terminal it shows the text and asks first.
func welcome(ctx context.Context, dry bool, code, text string, daysAgo int, kind string) error {
	s, err := load()
	if err != nil {
		return err
	}
	k, ok := welcomeKinds[kind]
	if !ok {
		return fmt.Errorf("-for must be personal or filtered, got %q", kind)
	}
	if text == "" {
		text = k.defaultText(s.hostname)
	}
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n == 0 || n > maxPostText {
		return fmt.Errorf("the post's text must be 1-%d characters, got %d", maxPostText, n)
	}
	if err := validateDaysAgo(daysAgo); err != nil {
		return err
	}
	id, err := syntax.ParseAtIdentifier(env("FEEDGEN_HANDLE", s.owner.String()))
	if err != nil {
		return fmt.Errorf("FEEDGEN_HANDLE: %w", err)
	}
	record := welcomeRecord(text, time.Now(), daysAgo)
	fmt.Printf("Post as %s, dated %s (%d days ago):\n\n  %s\n\n", id.String(), record["createdAt"], daysAgo, text)
	if dry {
		return nil
	}
	password := os.Getenv("FEEDGEN_APP_PASSWORD")
	if password == "" {
		return errors.New("FEEDGEN_APP_PASSWORD must be set (or use -dry-run)")
	}

	in := bufio.NewReader(os.Stdin)
	tty := isTerminal(os.Stdin)
	if tty && !strings.EqualFold(prompt(in, "Publish this post? It will be public. [y/N] "), "y") {
		return errors.New("not posted")
	}
	login, err := ownerLogin(ctx, identity.DefaultDirectory(), id, password, code, in, tty, "make feeds-welcome")
	if err != nil {
		return err
	}
	if login.AccountDID == nil || *login.AccountDID != s.owner {
		return fmt.Errorf("logged in as %v, but the feeds belong to %s", login.AccountDID, s.owner)
	}
	var out struct {
		URI string `json:"uri"`
	}
	err = login.Post(ctx, syntax.NSID("com.atproto.repo.createRecord"), map[string]any{
		"repo": s.owner.String(), "collection": "app.bsky.feed.post",
		"record": welcomeRecord(text, time.Now(), daysAgo),
	}, &out)
	if err != nil {
		return fmt.Errorf("create post: %w", err)
	}
	if err := checkWelcomePost(out.URI); err != nil {
		return fmt.Errorf("Bluesky answered with an unexpected post URI: %w", err)
	}
	fmt.Printf("Posted %s\n\nTo use it, add this line to ~/.config/topic-feed/env and run `make feeds`:\n\n  %s=%s\n", out.URI, k.envVar, out.URI)
	return nil
}
