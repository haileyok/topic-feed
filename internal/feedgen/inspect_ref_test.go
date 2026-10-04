package feedgen

import (
	"errors"
	"strings"
	"testing"
)

func TestParsePostRef(t *testing.T) {
	const did = "did:plc:oisofpd7lj26yvgiivf3lxsi"
	const key = "3mwxzusywpk2f"
	good := map[string]PostRef{
		"at://" + did + "/app.bsky.feed.post/" + key:                           {did, key},
		"  at://" + did + "/app.bsky.feed.post/" + key + "  \n":                {did, key},
		"AT://" + did + "/app.bsky.feed.post/" + key:                           {did, key},
		"at://Alice.Example.COM/app.bsky.feed.post/" + key:                     {"alice.example.com", key},
		"https://bsky.app/profile/" + did + "/post/" + key:                     {did, key},
		"https://bsky.app/profile/alice.example.com/post/" + key:               {"alice.example.com", key},
		"https://bsky.app/profile/Alice.Example.com/post/" + key + "/":         {"alice.example.com", key},
		"https://www.bsky.app/profile/alice.example.com/post/" + key:           {"alice.example.com", key},
		"http://bsky.app/profile/alice.example.com/post/" + key:                {"alice.example.com", key},
		"bsky.app/profile/alice.example.com/post/" + key:                       {"alice.example.com", key},
		"https://bsky.app/profile/alice.example.com/post/" + key + "?x=1":      {"alice.example.com", key},
		"https://bsky.app/profile/alice.example.com/post/" + key + "#top":      {"alice.example.com", key},
		"https://bsky.app/profile/alice.example.com/post/" + key + "/liked-by": {"alice.example.com", key},
		"https://bsky.app/profile/alice.example.com/post/" + key + "/quotes":   {"alice.example.com", key},
		"https://bsky.app/profile/did:web:example.com/post/" + key:             {"did:web:example.com", key},
	}
	for in, want := range good {
		got, err := ParsePostRef(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}

	bad := map[string]string{
		"":            "Paste a link",
		"   ":         "Paste a link",
		"hello":       "Only links to posts on bsky.app",
		"at://":       "valid at:// address",
		"at://" + did: "account, not a post",
		"at://" + did + "/app.bsky.feed.like/" + key:                           "app.bsky.feed.like record, not a post",
		"at://" + did + "/app.bsky.feed.post":                                  "no post key",
		"at://" + did + "/app.bsky.feed.post/":                                 "no post key",
		"at://not a did/app.bsky.feed.post/" + key:                             "valid at:// address",
		"https://example.com/profile/alice.example.com/post/" + key:            "Only links to posts on bsky.app",
		"https://evil.test/https://bsky.app/profile/a.example.com/post/" + key: "Only links to posts on bsky.app",
		"https://bsky.app.evil.test/profile/alice.example.com/post/" + key:     "Only links to posts on bsky.app",
		"https://user:pw@bsky.app/profile/alice.example.com/post/" + key:       "doesn't look like a link",
		"ftp://bsky.app/profile/alice.example.com/post/" + key:                 "doesn't look like a link to a post",
		"https://bsky.app/":                                                    "not a link to a post",
		"https://bsky.app/profile/alice.example.com":                           "profile, not a post",
		"https://bsky.app/profile/alice.example.com/feed/abc":                  "link to a feed, not a post",
		"https://bsky.app/profile/alice.example.com/lists/abc":                 "link to a lists, not a post",
		"https://bsky.app/profile/alice.example.com/post/":                     "no post key",
		"https://bsky.app/profile/alice.example.com/post/" + key + "/extra/x":  "doesn't look like a link",
		"https://bsky.app/profile/alice.example.com/post/" + key + "/nonsense": "doesn't look like a link",
		"https://bsky.app/profile/not_a_handle/post/" + key:                    "isn't a valid handle or DID",
		"https://bsky.app/profile/alice.example.com/post/..":                   "post key",
		"https://bsky.app/profile/alice.example.com/post/a b":                  "post key",
		strings.Repeat("a", maxPostRefLen+1):                                   "too long",
	}
	for in, want := range bad {
		_, err := ParsePostRef(in)
		var re *RefError
		if err == nil {
			t.Errorf("%q was accepted", in)
			continue
		}
		if !errors.As(err, &re) {
			t.Errorf("%q: %T, want a RefError so the message can be shown", in, err)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %q, want it to say %q", in, err.Error(), want)
		}
	}
}

func TestPostRefURI(t *testing.T) {
	r := PostRef{Actor: "alice.example.com", RKey: "3kabc"}
	if r.IsDID() {
		t.Error("a handle is not a DID")
	}
	if got := r.URI("did:plc:xyz"); got != "at://did:plc:xyz/app.bsky.feed.post/3kabc" {
		t.Errorf("URI: %s", got)
	}
	if !(PostRef{Actor: "did:plc:xyz"}).IsDID() {
		t.Error("a DID is a DID")
	}
}

// What is repeated back in a message is made harmless: addresses come from anyone who can type.
func TestRefMessagesDoNotRepeatHostileInput(t *testing.T) {
	_, err := ParsePostRef("https://bsky.app/profile/alice.example.com/<script>alert(1)</script>/x")
	if err == nil {
		t.Fatal("accepted")
	}
	if strings.ContainsAny(err.Error(), "<>()") {
		t.Errorf("the message repeats markup: %q", err.Error())
	}
}
