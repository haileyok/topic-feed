package feedgen

import (
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestMePageIsServedWithTheSitesSecurityHeaders(t *testing.T) {
	s := testServer(t) // sign-in is off here: the page is static, and says so when it asks
	w := serve(s, "GET", "/me", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": contentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
		"Cache-Control":           "no-cache",
		"X-Robots-Tag":            "noindex",
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	body := w.Body.String()
	for _, want := range []string{`/me.js`, `/me.css`, `id="login"`, `name="handle"`, `id="account-signout"`, `noindex`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	// What the page loads is served: its scripts and styles by version, and its icon.
	assets := versionedAssets(body)
	if len(assets) != 3 {
		t.Errorf("the page loads %v, want app.css, me.css and me.js by version", assets)
	}
	for _, path := range append(assets, "/static/icon.svg") {
		if w := serve(s, "GET", path, nil); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w := serve(s, "POST", "/me", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /me: %d", w.Code)
	}
}

// The site's content security policy allows only the page's own files. Anything inline would be
// blocked by browsers, so the page would silently not work.
func TestMePageNeedsNothingTheSecurityPolicyForbids(t *testing.T) {
	for _, bad := range []string{"'unsafe-inline'", "'unsafe-eval'", "script-src *", "default-src *"} {
		if strings.Contains(contentSecurityPolicy, bad) {
			t.Fatalf("the policy allows %s", bad)
		}
	}
	raw, err := webFS.ReadFile("web/me.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	if m := regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>`).FindAllString(html, -1); len(m) != 1 || !strings.Contains(m[0], `src="/static/v/__V__/me.js"`) {
		t.Errorf("scripts: %v, want only the one that loads me.js", m)
	}
	if regexp.MustCompile(`(?is)<script[^>]*>[^<]+</script>`).MatchString(html) {
		t.Error("an inline script")
	}
	if regexp.MustCompile(`(?is)<style`).MatchString(html) || regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(html) {
		t.Error("inline style")
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(html) {
		t.Error("an inline event handler")
	}
	if loadsFromAnotherSite(html) {
		t.Error("the page loads something from another site")
	}
	js, err := webFS.ReadFile("web/static/me.js")
	if err != nil {
		t.Fatal(err)
	}
	// Text goes on the page as text, never as HTML.
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("me.js uses %s", bad)
		}
	}
}

// TestMePageScript runs the page's own script, in a simulated browser (jsdom), against the real
// page. It needs node and jsdom, so it only runs when TOPICFEED_JSDOM names jsdom's directory:
//
//	TOPICFEED_JSDOM=/path/to/node_modules/jsdom go test ./internal/feedgen/ -run TestMePageScript -v
func TestMePageScript(t *testing.T) {
	jsdom := os.Getenv("TOPICFEED_JSDOM")
	if jsdom == "" {
		t.Skip("set TOPICFEED_JSDOM to the path of jsdom (node_modules/jsdom) to run the page's script tests")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	cmd := exec.Command(node, "--test", "webtest/me.test.mjs", "webtest/tuning.test.mjs")
	cmd.Env = append(os.Environ(), "TOPICFEED_JSDOM="+jsdom)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the page's script tests failed: %v\n%s", err, out)
	}
}

func TestHomePageLinksToTheMePage(t *testing.T) {
	w := serve(testServer(t), "GET", "/", nil)
	if !strings.Contains(w.Body.String(), `href="/me"`) {
		t.Error("the home page has no way to sign in")
	}
}

// scriptsOfTheMePage are the scripts the page at /me is made of.
var scriptsOfTheMePage = []string{"me.js", "header.js", "tuning.js", "dom.js", "draft.js", "knobs.js", "scores.js", "stats.js", "posts.js"}

func TestEveryScriptOfTheMePageIsServedAndWhatItImportsExists(t *testing.T) {
	s := testServer(t)
	body := serve(s, "GET", "/me", nil).Body.String()
	var dir string
	for _, a := range versionedAssets(body) {
		if strings.HasSuffix(a, "/me.js") {
			dir = a[:strings.LastIndex(a, "/")+1]
		}
	}
	if dir == "" {
		t.Fatalf("the page doesn't load me.js by version: %v", versionedAssets(body))
	}
	imports := regexp.MustCompile(`(?m)^(?:import|export)[^;]*? from "\./([A-Za-z0-9._-]+)";`)
	for _, name := range scriptsOfTheMePage {
		src, err := webFS.ReadFile("web/static/" + name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Modules are loaded from the same version as the page, as the files they import.
		w := serve(s, "GET", dir+name, nil)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || w.Body.String() != string(src) {
			t.Errorf("%s%s: %d %v", dir, name, w.Code, w.Header())
		}
		for _, m := range imports.FindAllStringSubmatch(string(src), -1) {
			if w := serve(s, "GET", dir+m[1], nil); w.Code != 200 {
				t.Errorf("%s imports ./%s, which is served as %d", name, m[1], w.Code)
			}
			if !slicesContain(scriptsOfTheMePage, m[1]) {
				t.Errorf("%s imports ./%s, which this test doesn't know belongs to the page", name, m[1])
			}
		}
	}
}

func slicesContain(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestEveryElementTheMePageScriptsUseIsInThePage catches a script asking for an element that
// isn't there (a typo in an id), which only shows when that part of the page is used.
func TestEveryElementTheMePageScriptsUseIsInThePage(t *testing.T) {
	html, err := fs.ReadFile(webFS, "web/me.html")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(string(html), -1) {
		if have[m[1]] {
			t.Errorf("two elements have the id %q", m[1])
		}
		have[m[1]] = true
	}
	// $("id") in the scripts, and the ids derived from another ($("author-gap") has "author-gap-value").
	uses := regexp.MustCompile(`\$\("([a-z][a-z0-9-]*)"\)|setRange\("([a-z-]+)"`)
	for _, name := range scriptsOfTheMePage {
		src, err := webFS.ReadFile("web/static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range uses.FindAllStringSubmatch(string(src), -1) {
			id := m[1]
			if id == "" {
				id = m[2]
				if !have[id+"-value"] {
					t.Errorf("%s: the page has no #%s-value", name, id)
				}
			}
			if !have[id] {
				t.Errorf("%s uses #%s, which the page doesn't have", name, id)
			}
		}
	}
	// And the other way: a control in the page that no script knows about does nothing.
	var all strings.Builder
	for _, name := range scriptsOfTheMePage {
		src, _ := webFS.ReadFile("web/static/" + name)
		all.Write(src)
	}
	// (Some ids are for the page itself: a label points at the heading, and the container that
	// announces changes to screen readers holds the others.)
	byTheHTML := map[string]bool{"interests": true}
	for _, m := range regexp.MustCompile(`(?:for|aria-labelledby)="([^"]+)"`).FindAllStringSubmatch(string(html), -1) {
		byTheHTML[m[1]] = true
	}
	for id := range have {
		if byTheHTML[id] {
			continue
		}
		if !strings.Contains(all.String(), `"`+id+`"`) && !strings.Contains(all.String(), `"`+strings.TrimSuffix(id, "-value")+`"`) {
			t.Errorf("#%s is in the page, but no script uses it", id)
		}
	}
}
