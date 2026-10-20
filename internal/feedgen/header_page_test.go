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
	{"web/filtered.html", "/filtered", "filtered.js", `href="/filtered"`},
	{"web/left-out.html", "/filtered/left-out", "left-out.js", `href="/filtered"`},
	{"web/inspect-account.html", "/inspect/account", "inspect-account.js", `href="/inspect"`},
}

var headerMarkup = regexp.MustCompile(`(?s)<header class="topbar">.*?</header>`)

// idsUsedBy are the elements the scripts ask for by id ($("...")).
func idsUsedBy(t *testing.T, scripts ...string) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	for _, name := range scripts {
		src, err := webFS.ReadFile("web/static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range regexp.MustCompile(`\$\("([a-z][a-z0-9-]*)"\)`).FindAllStringSubmatch(string(src), -1) {
			ids[m[1]] = true
		}
	}
	return ids
}

// headerIDs are the elements header.js fills in: they belong to the header, not to any one page.
func headerIDs(t *testing.T) map[string]bool { return idsUsedBy(t, "header.js") }

// sharedIDs are the elements of what several pages share: the header and the sign-in form.
func sharedIDs(t *testing.T) map[string]bool { return idsUsedBy(t, "header.js", "signin-form.js") }

// The pages that need someone signed in, and the script each starts from. (The builder needs it only
// to save a feed, and shows the form in a dialog then.)
var pagesWithTheSignInForm = []struct{ file, script string }{
	{"web/me.html", "me.js"},
	{"web/feeds.html", "feeds.js"},
	{"web/inspect.html", "inspect.js"},
	{"web/inspect-account.html", "inspect-account.js"},
	{"web/index.html", "app.js"},
}

var signedOutSection = regexp.MustCompile(`(?s)<section class="me-card" id="signed-out"(?: hidden)?>.*?</section>`)

func TestEveryPageThatNeedsSignInHasTheSameForm(t *testing.T) {
	// Only the heading and the line under it, which say what the page is for, differ.
	pagesOwn := regexp.MustCompile(`(?s)<h1[^>]*>.*?</h1>\s*<p class="me-lead">.*?</p>`)
	var first string
	for _, p := range pagesWithTheSignInForm {
		raw, err := webFS.ReadFile(p.file)
		if err != nil {
			t.Fatal(err)
		}
		section := signedOutSection.FindString(string(raw))
		if section == "" || !pagesOwn.MatchString(section) {
			t.Errorf("%s has no signed-out section with a heading and a line under it", p.file)
			continue
		}
		// (The builder's card is in a dialog, which is what hides it until it's wanted.)
		same := strings.Replace(pagesOwn.ReplaceAllString(section, ""), `id="signed-out" hidden>`, `id="signed-out">`, 1)
		if first == "" {
			first = same
		} else if same != first {
			t.Errorf("%s's sign-in differs from %s's:\n%s\n---\n%s", p.file, pagesWithTheSignInForm[0].file, same, first)
		}
		for id := range idsUsedBy(t, "signin-form.js") {
			if !strings.Contains(section, `id="`+id+`"`) {
				t.Errorf("%s: the sign-in has no #%s, which signin-form.js uses", p.file, id)
			}
		}
		js, err := webFS.ReadFile("web/static/" + p.script)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(js), `from "./signin-form.js";`) || !strings.Contains(string(js), "setupSignInForm(") {
			t.Errorf("%s doesn't set up the sign-in form", p.script)
		}
	}
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
	for _, link := range []string{`href="/"`, `href="/?view=browse"`, `href="/feeds"`, `href="/me"`, `href="/filtered"`, `href="/inspect"`} {
		if !strings.Contains(first, link) {
			t.Errorf("the header has no link with %s", link)
		}
	}
}

func TestTheHeaderScriptPutsTextOnThePageAsText(t *testing.T) {
	for _, name := range []string{"header.js", "filtered.js", "left-out.js", "inspect-account.js"} {
		js, err := webFS.ReadFile("web/static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
			if strings.Contains(string(js), bad) {
				t.Errorf("%s uses %s", name, bad)
			}
		}
	}
}

// TestFilteredPageScript runs the page at /filtered's script, in a simulated browser (jsdom), against
// the real page. It only runs when TOPICFEED_JSDOM names jsdom's directory.
func TestFilteredPageScript(t *testing.T) {
	runNodeTests(t, true, "webtest/filtered.test.mjs", "webtest/left-out.test.mjs")
}

// TestAccountInspectorPageScript runs the account inspector's script against the real page, in jsdom.
func TestAccountInspectorPageScript(t *testing.T) {
	runNodeTests(t, true, "webtest/inspect-account.test.mjs")
}

// The page at /filtered has the sign-in form (its own words around it: that sign-in is a different
// one), with every element signin-form.js and its own script use.
func TestTheFilteredPageHasWhatItsScriptsUse(t *testing.T) {
	raw, err := webFS.ReadFile("web/filtered.html")
	if err != nil {
		t.Fatal(err)
	}
	for id := range idsUsedBy(t, "signin-form.js", "filtered.js") {
		if !strings.Contains(string(raw), `id="`+id+`"`) {
			t.Errorf("filtered.html has no #%s", id)
		}
	}
	js, _ := webFS.ReadFile("web/static/filtered.js")
	if !strings.Contains(string(js), `setupSignInForm({ endpoint: "/oauth/connect" })`) {
		t.Error("the page's sign-in isn't the filtered feeds' (/oauth/connect)")
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
