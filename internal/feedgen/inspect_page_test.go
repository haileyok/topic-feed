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

// scriptsOfTheInspectPage are the scripts the page at /inspect is made of.
var scriptsOfTheInspectPage = []string{"inspect.js", "header.js", "dom.js", "posts.js", "stats.js", "scores.js", "draft.js"}

func TestInspectPageIsServedWithTheSitesSecurityHeaders(t *testing.T) {
	s := testServer(t)
	w := serve(s, "GET", "/inspect", nil)
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
	for _, want := range []string{`/inspect.js`, `/inspect.css`, `id="inspect-form"`, `name="post"`, `noindex`} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	if strings.Contains(body, "__V__") || strings.Contains(body, "__ORIGIN__") {
		t.Error("the page still holds a placeholder")
	}
	// What the page loads is served: its scripts and styles by version, and its icon.
	assets := versionedAssets(body)
	if len(assets) != 4 {
		t.Errorf("the page loads %v, want app.css, me.css, inspect.css and inspect.js by version", assets)
	}
	for _, path := range append(assets, "/static/icon.svg") {
		if w := serve(s, "GET", path, nil); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w := serve(s, "POST", "/inspect", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /inspect: %d", w.Code)
	}
}

// The page holds nothing private (it asks /api/inspect, which answers only the owner), and what it
// is made of must be allowed by the site's content security policy: anything inline would be
// blocked by browsers, so the page would silently not work.
func TestInspectPageNeedsNothingTheSecurityPolicyForbids(t *testing.T) {
	raw, err := webFS.ReadFile("web/inspect.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	if m := regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>`).FindAllString(html, -1); len(m) != 1 || !strings.Contains(m[0], `src="/static/v/__V__/inspect.js"`) {
		t.Errorf("scripts: %v, want only the one that loads inspect.js", m)
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
	if !strings.Contains(html, `name="robots" content="noindex"`) {
		t.Error("the page doesn't ask to be left out of search results")
	}
	// Text goes on the page as text, never as HTML: what it shows includes what other people posted.
	for _, name := range scriptsOfTheInspectPage {
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
	css, err := webFS.ReadFile("web/static/inspect.css")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)url\(\s*["']?(?:https?:)?//|@import`).Match(css) {
		t.Error("inspect.css loads something from another site")
	}
}

func TestEveryScriptOfTheInspectPageIsServedAndWhatItImportsExists(t *testing.T) {
	s := testServer(t)
	body := serve(s, "GET", "/inspect", nil).Body.String()
	var dir string
	for _, a := range versionedAssets(body) {
		if strings.HasSuffix(a, "/inspect.js") {
			dir = a[:strings.LastIndex(a, "/")+1]
		}
	}
	if dir == "" {
		t.Fatalf("the page doesn't load inspect.js by version: %v", versionedAssets(body))
	}
	imports := regexp.MustCompile(`(?m)^(?:import|export)[^;]*? from "\./([A-Za-z0-9._-]+)";`)
	for _, name := range scriptsOfTheInspectPage {
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
			if !slicesContain(scriptsOfTheInspectPage, m[1]) {
				t.Errorf("%s imports ./%s, which this test doesn't know belongs to the page", name, m[1])
			}
		}
	}
}

// TestEveryElementTheInspectPageScriptsUseIsInThePage catches a script asking for an element that
// isn't there (a typo in an id), which only shows when that part of the page is used.
func TestEveryElementTheInspectPageScriptsUseIsInThePage(t *testing.T) {
	html, err := fs.ReadFile(webFS, "web/inspect.html")
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
	src, err := webFS.ReadFile("web/static/inspect.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`\$\("([a-z][a-z0-9-]*)"\)`).FindAllStringSubmatch(string(src), -1) {
		if !have[m[1]] {
			t.Errorf("inspect.js uses #%s, which the page doesn't have", m[1])
		}
	}
	// The states of the page are sections the script shows one at a time, by name.
	states := []string{"loading", "signed-out", "forbidden", "inspector"}
	for _, id := range states {
		if !have[id] {
			t.Errorf("the page has no #%s", id)
		}
		if !strings.Contains(string(src), `"`+id+`"`) {
			t.Errorf("inspect.js never shows #%s", id)
		}
	}
	// And the other way: a part of the page that no script knows about is never shown or filled.
	header := headerIDs(t) // the shared header's, filled in by header.js (TestEveryPageHasTheSameHeader)
	for id := range have {
		if !strings.Contains(string(src), `("`+id+`")`) && !slicesContain(states, id) && !header[id] {
			t.Errorf("the page has #%s, which inspect.js never uses", id)
		}
	}
	// Only the section that says it is loading is on show before the script has decided which state it is.
	for _, id := range states[1:] {
		if !regexp.MustCompile(`id="` + id + `"[^>]*\shidden`).Match(html) {
			t.Errorf("#%s is not hidden until the script shows it", id)
		}
	}
	if regexp.MustCompile(`id="loading"[^>]*\shidden`).Match(html) {
		t.Error("#loading is hidden before the script has run: the page would be blank")
	}
}

// The classes the script gives what it builds are the ones the stylesheet (or the stylesheets of the
// pages it shares) style: a typo shows as an unstyled piece of the page, and only for some answers.
func TestEveryInspectClassTheScriptUsesIsStyled(t *testing.T) {
	src, err := webFS.ReadFile("web/static/inspect.js")
	if err != nil {
		t.Fatal(err)
	}
	var styles strings.Builder
	for _, name := range []string{"app.css", "me.css", "inspect.css"} {
		css, err := webFS.ReadFile("web/static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		styles.Write(css)
	}
	for _, m := range regexp.MustCompile(`class: "([a-z][a-z0-9 -]*)"`).FindAllStringSubmatch(string(src), -1) {
		for _, class := range strings.Fields(m[1]) {
			if !strings.Contains(styles.String(), "."+class) {
				t.Errorf("inspect.js gives an element the class %q, which no stylesheet of the page mentions", class)
			}
		}
	}
}

// TestInspectPageScript runs the page's own script, in a simulated browser (jsdom), against the real
// page. It needs node and jsdom, so it only runs when TOPICFEED_JSDOM names jsdom's directory:
//
//	TOPICFEED_JSDOM=/path/to/node_modules/jsdom go test ./internal/feedgen/ -run TestInspectPageScript -v
func TestInspectPageScript(t *testing.T) {
	jsdom := os.Getenv("TOPICFEED_JSDOM")
	if jsdom == "" {
		t.Skip("set TOPICFEED_JSDOM to the path of jsdom (node_modules/jsdom) to run the page's script tests")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	cmd := exec.Command(node, "--test", "webtest/inspect.test.mjs")
	cmd.Env = append(os.Environ(), "TOPICFEED_JSDOM="+jsdom)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the page's script tests failed: %v\n%s", err, out)
	}
}
