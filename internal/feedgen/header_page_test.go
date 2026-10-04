package feedgen

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The pages that share the header, the script each one starts from, and the link the header marks
// as the page you're on.
var pagesWithTheHeader = []struct{ file, path, script, current string }{
	{"web/index.html", "/", "app.js", `href="/"`},
	{"web/feeds.html", "/feeds", "feeds-page.js", `href="/feeds"`},
	{"web/me.html", "/me", "me.js", `href="/me"`},
	{"web/inspect.html", "/inspect", "inspect.js", `href="/inspect"`},
}

var headerMarkup = regexp.MustCompile(`(?s)<header class="topbar">.*?</header>`)

// headerIDs are the elements header.js fills in: they belong to the header, not to any one page.
func headerIDs(t *testing.T) map[string]bool {
	t.Helper()
	src, err := webFS.ReadFile("web/static/header.js")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\("([a-z][a-z0-9-]*)"\)`).FindAllStringSubmatch(string(src), -1) {
		ids[m[1]] = true
	}
	return ids
}

// loadsFromAnotherSite reports whether a page has the browser load something from elsewhere: a script,
// a style, a picture, a frame, or a form that posts away. A link someone may follow loads nothing.
func loadsFromAnotherSite(html string) bool {
	return regexp.MustCompile(`(?is)<(?:script|link|img|iframe|frame|form|source|video|audio|embed|object|track)\b[^>]*\s(?:href|src|srcset|action|data|poster)\s*=\s*"(?:https?:)?//`).MatchString(html)
}

func TestLoadsFromAnotherSite(t *testing.T) {
	for html, want := range map[string]bool{
		`<script src="https://evil.test/x.js"></script>`:              true,
		`<link rel="stylesheet" href="//evil.test/x.css">`:            true,
		`<img alt="" src="http://evil.test/x.png">`:                   true,
		`<form action="https://evil.test/">`:                          true,
		`<a href="https://bsky.app/profile/hailey.at">by @hailey</a>`: false,
		`<script type="module" src="/static/v/1/app.js"></script>`:    false,
	} {
		if got := loadsFromAnotherSite(html); got != want {
			t.Errorf("%s: %v, want %v", html, got, want)
		}
	}
}

func TestEveryPageHasTheSameHeader(t *testing.T) {
	var first string
	for _, p := range pagesWithTheHeader {
		raw, err := webFS.ReadFile(p.file)
		if err != nil {
			t.Fatal(err)
		}
		header := headerMarkup.FindString(string(raw))
		if header == "" {
			t.Errorf("%s has no header", p.file)
			continue
		}
		// It marks one link as the page you're on: this page's.
		marked := regexp.MustCompile(`<a [^>]*aria-current="page"[^>]*>`).FindAllString(header, -1)
		if len(marked) != 1 || !strings.Contains(marked[0], p.current) {
			t.Errorf("%s marks %v as the current page, want the link with %s", p.file, marked, p.current)
		}
		// Otherwise every page has the very same header.
		same := strings.Replace(header, ` aria-current="page"`, "", 1)
		if first == "" {
			first = same
		} else if same != first {
			t.Errorf("%s's header differs from %s's:\n%s\n---\n%s", p.file, pagesWithTheHeader[0].file, same, first)
		}
		// Every element header.js fills in is there, and only the script fills it in.
		for id := range headerIDs(t) {
			if !strings.Contains(header, `id="`+id+`"`) {
				t.Errorf("%s: the header has no #%s, which header.js uses", p.file, id)
			}
		}
		for _, id := range []string{"account-signin", "account-who", "account-signout", "account-problem", "nav-inspect"} {
			if !regexp.MustCompile(`id="` + id + `"[^>]*\shidden`).MatchString(header) {
				t.Errorf("%s: #%s is not hidden until the script shows it", p.file, id)
			}
		}
		// The page's own script brings the header's along.
		js, err := webFS.ReadFile("web/static/" + p.script)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`(?m)^import "\./header\.js";`).Match(js) {
			t.Errorf("%s doesn't import header.js", p.script)
		}
		// And it is served with the page.
		w := serve(testServer(t), "GET", p.path, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `id="account"`) {
			t.Errorf("GET %s: %d, or the header isn't in it", p.path, w.Code)
		}
	}
	for _, link := range []string{`href="/"`, `href="/?view=browse"`, `href="/feeds"`, `href="/me"`, `href="/inspect"`} {
		if !strings.Contains(first, link) {
			t.Errorf("the header has no link with %s", link)
		}
	}
}

func TestTheHeaderScriptPutsTextOnThePageAsText(t *testing.T) {
	js, err := webFS.ReadFile("web/static/header.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("header.js uses %s", bad)
		}
	}
}

// TestHeaderScript runs header.js in a simulated browser (jsdom). It only runs when TOPICFEED_JSDOM
// names jsdom's directory:
//
//	TOPICFEED_JSDOM=/path/to/node_modules/jsdom go test ./internal/feedgen/ -run TestHeaderScript -v
func TestHeaderScript(t *testing.T) {
	jsdom := os.Getenv("TOPICFEED_JSDOM")
	if jsdom == "" {
		t.Skip("set TOPICFEED_JSDOM to the path of jsdom (node_modules/jsdom) to run the header's script tests")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	cmd := exec.Command(node, "--test", "webtest/header.test.mjs")
	cmd.Env = append(os.Environ(), "TOPICFEED_JSDOM="+jsdom)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the header's script tests failed: %v\n%s", err, out)
	}
}
