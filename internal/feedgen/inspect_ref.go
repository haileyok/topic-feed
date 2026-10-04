package feedgen

import (
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// postCollection is the collection every post is a record of.
const postCollection = "app.bsky.feed.post"

// maxPostRefLen bounds what is parsed: a real address is far shorter.
const maxPostRefLen = 600

// PostRef names a post by who wrote it and its record key. Actor is a DID, or a handle that
// still has to be resolved to one.
type PostRef struct {
	Actor string
	RKey  string
}

// IsDID reports whether the author is already a DID.
func (r PostRef) IsDID() bool { return strings.HasPrefix(r.Actor, "did:") }

// URI is the at:// address of the post, once the author's DID is known.
func (r PostRef) URI(did string) string { return "at://" + did + "/" + postCollection + "/" + r.RKey }

// RefError is a reason an address can't be used, worded for the person who typed it.
type RefError struct{ msg string }

func (e *RefError) Error() string { return e.msg }

func refErr(msg string) error { return &RefError{msg: msg} }

// bskyHosts are the web addresses of the Bluesky app whose post links are understood.
var bskyHosts = map[string]bool{"bsky.app": true, "www.bsky.app": true}

// postLinkTails are the pages of a post in the app, which share its address up to the key.
var postLinkTails = map[string]bool{"liked-by": true, "reposted-by": true, "quotes": true}

// ParsePostRef reads a post's address as it is copied from the Bluesky app
// (https://bsky.app/profile/alice.example/post/3kabc...), or as an at:// address
// (at://did:plc:.../app.bsky.feed.post/3kabc...), with a DID or a handle for the author.
func ParsePostRef(raw string) (PostRef, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return PostRef{}, refErr("Paste a link to a post, or its at:// address.")
	}
	if len(s) > maxPostRefLen {
		return PostRef{}, refErr("That's too long to be a post address.")
	}
	if len(s) >= 5 && strings.EqualFold(s[:5], "at://") {
		// The scheme is not case sensitive, and a slash left on the end is a slip, not a key.
		return parseATURI("at://" + strings.TrimRight(s[5:], "/"))
	}
	return parsePostLink(s)
}

func parseATURI(s string) (PostRef, error) {
	u, err := syntax.ParseATURI(s)
	if err != nil {
		return PostRef{}, refErr("That doesn't look like a valid at:// address.")
	}
	if u.Collection() == "" {
		return PostRef{}, refErr("That at:// address is an account, not a post.")
	}
	if u.Collection().String() != postCollection {
		return PostRef{}, refErr("That at:// address is for a " + u.Collection().String() + " record, not a post.")
	}
	if u.RecordKey() == "" {
		return PostRef{}, refErr("That at:// address has no post key at the end.")
	}
	return refOf(u.Authority().String(), u.RecordKey().String())
}

func parsePostLink(s string) (PostRef, error) {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return PostRef{}, refErr("That doesn't look like a link to a post on bsky.app.")
	}
	if !bskyHosts[strings.ToLower(u.Hostname())] {
		return PostRef{}, refErr("Only links to posts on bsky.app, and at:// addresses, are understood.")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "profile" || parts[1] == "" {
		return PostRef{}, refErr("That's not a link to a post. It should look like bsky.app/profile/…/post/…")
	}
	if len(parts) == 2 {
		return PostRef{}, refErr("That's a link to a profile, not a post.")
	}
	if parts[2] != "post" {
		return PostRef{}, refErr("That's a link to a " + sanitizeWord(parts[2]) + ", not a post.")
	}
	if len(parts) < 4 || parts[3] == "" {
		return PostRef{}, refErr("That link has no post key at the end.")
	}
	if len(parts) > 4 && !(len(parts) == 5 && postLinkTails[parts[4]]) {
		return PostRef{}, refErr("That doesn't look like a link to a post on bsky.app.")
	}
	return refOf(parts[1], parts[3])
}

// refOf checks the author and the key, and gives a handle in its normal (lower case) form.
func refOf(actor, rkey string) (PostRef, error) {
	id, err := syntax.ParseAtIdentifier(actor)
	if err != nil {
		return PostRef{}, refErr("The account in that address isn't a valid handle or DID.")
	}
	key, err := syntax.ParseRecordKey(rkey)
	if err != nil {
		return PostRef{}, refErr("The post key in that address isn't valid.")
	}
	if h, err := id.AsHandle(); err == nil {
		return PostRef{Actor: h.Normalize().String(), RKey: key.String()}, nil
	}
	return PostRef{Actor: id.String(), RKey: key.String()}, nil
}

// sanitizeWord is a part of an address made safe to repeat in a message: letters, digits, dashes.
func sanitizeWord(s string) string {
	var b strings.Builder
	for _, r := range s {
		if len(b.String()) >= 24 {
			break
		}
		if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "page"
	}
	return b.String()
}
