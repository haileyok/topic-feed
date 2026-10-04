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

// scriptsOfTheFeedsPage are the scripts the page at /feeds is made of (those it imports; the vendored OAuth
// client is loaded by feeds-page.js with import()).
var scriptsOfTheFeedsPage = []string{"feeds-page.js", "feeds.js", "publish.js", "mine.js", "posts.js", "dom.js"}

const vendoredClient = "vendor/atproto-oauth-client-browser.js"

func TestFeedsPageIsServedWithItsOwnSecurityPolicy(t *testing.T) {
	s := testServer(t)
	w := serve(s, "GET", "/feeds", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": feedsContentSecurityPolicy,
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
	if strings.Contains(body, "__V__") || strings.Contains(body, "__ORIGIN__") {
		t.Error("the page still holds a placeholder")
	}
	assets := versionedAssets(body)
	if len(assets) != 4 {
		t.Errorf("the page loads %v, want app.css, me.css, feeds.css and feeds-page.js by version", assets)
	}
	for _, path := range append(assets, "/static/icon.svg") {
		if w := serve(s, "GET", path, nil); w.Code != 200 || w.Body.Len() == 0 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if w := serve(s, "POST", "/feeds", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /feeds: %d", w.Code)
	}
}

// The wider policy is for this page alone: connecting to any https address is what the page needs to reach
// a person's own server, and nothing else should have it.
func TestOnlyThePublishingPageMayConnectAnywhere(t *testing.T) {
	s := testServer(t)
	for _, page := range []string{"/", "/me", "/inspect"} {
		w := serve(s, "GET", page, nil)
		if got := w.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("%s has the policy %q", page, got)
		}
	}
	if regexp.MustCompile(`connect-src[^;]*\shttps:(\s|;|$)`).MatchString(contentSecurityPolicy) {
		t.Errorf("the site's policy lets pages connect to any address: %s", contentSecurityPolicy)
	}
	if !regexp.MustCompile(`connect-src 'self' https:;`).MatchString(feedsContentSecurityPolicy) {
		t.Errorf("the publishing page's policy must let it reach people's servers: %s", feedsContentSecurityPolicy)
	}
	// What keeps that loosening small.
	for _, bad := range []string{"'unsafe-inline'", "'unsafe-eval'", "script-src *", "script-src https:", "default-src *", "form-action *", "frame-ancestors *"} {
		if strings.Contains(feedsContentSecurityPolicy, bad) {
			t.Errorf("the publishing page's policy allows %s", bad)
		}
	}
	for _, want := range []string{"script-src 'self';", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'"} {
		if !strings.Contains(feedsContentSecurityPolicy, want) {
			t.Errorf("the publishing page's policy lacks %s", want)
		}
	}
}

func TestFeedsPageNeedsNothingTheSecurityPolicyForbids(t *testing.T) {
	raw, err := webFS.ReadFile("web/feeds.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	if m := regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>`).FindAllString(html, -1); len(m) != 1 || !strings.Contains(m[0], `src="/static/v/__V__/feeds-page.js"`) {
		t.Errorf("scripts: %v, want only the one that loads feeds-page.js", m)
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
	if regexp.MustCompile(`(?i)(href|src|action)\s*=\s*"(?:https?:)?//`).MatchString(html) {
		t.Error("the page loads something from another site")
	}
	if !strings.Contains(html, `name="robots" content="noindex"`) {
		t.Error("the page doesn't ask to be left out of search results")
	}
	// What the page shows includes other people's words, and it can write to their accounts: all of it goes on
	// the page as text.
	for _, name := range scriptsOfTheFeedsPage {
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

func TestEveryScriptOfTheFeedsPageIsServedAndWhatItImportsExists(t *testing.T) {
	s := testServer(t)
	body := serve(s, "GET", "/feeds", nil).Body.String()
	var dir string
	for _, a := range versionedAssets(body) {
		if strings.HasSuffix(a, "/feeds-page.js") {
			dir = a[:strings.LastIndex(a, "/")+1]
		}
	}
	if dir == "" {
		t.Fatalf("the page doesn't load feeds-page.js by version: %v", versionedAssets(body))
	}
	imports := regexp.MustCompile(`(?m)^(?:import|export)[^;]*? from "\./([A-Za-z0-9._-]+)";`)
	for _, name := range scriptsOfTheFeedsPage {
		src, err := webFS.ReadFile("web/static/" + name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		w := serve(s, "GET", dir+name, nil)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || w.Body.String() != string(src) {
			t.Errorf("%s%s: %d %v", dir, name, w.Code, w.Header())
		}
		for _, m := range imports.FindAllStringSubmatch(string(src), -1) {
			if w := serve(s, "GET", dir+m[1], nil); w.Code != 200 {
				t.Errorf("%s imports ./%s, which is served as %d", name, m[1], w.Code)
			}
			if !slicesContain(scriptsOfTheFeedsPage, m[1]) {
				t.Errorf("%s imports ./%s, which this test doesn't know belongs to the page", name, m[1])
			}
		}
	}
}

// The OAuth client is loaded with import() from the version's own folder, which the page's policy allows
// only if it is a script of the site's own with nothing in it the policy forbids.
func TestTheVendoredOAuthClientIsServedAndNeedsNoEval(t *testing.T) {
	s := testServer(t)
	body := serve(s, "GET", "/feeds", nil).Body.String()
	var dir string
	for _, a := range versionedAssets(body) {
		if strings.HasSuffix(a, "/feeds-page.js") {
			dir = a[:strings.LastIndex(a, "/")+1]
		}
	}
	page, err := webFS.ReadFile("web/static/feeds-page.js")
	if err != nil || !strings.Contains(string(page), `import("./`+vendoredClient+`")`) {
		t.Fatalf("feeds-page.js doesn't load %s: %v", vendoredClient, err)
	}
	w := serve(s, "GET", dir+vendoredClient, nil)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || w.Body.Len() < 100_000 {
		t.Fatalf("%d %s %d bytes", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
	src := w.Body.String()
	for _, bad := range []string{"eval(", "new Function"} {
		if strings.Contains(src, bad) {
			t.Errorf("the vendored client uses %s, which the page's policy forbids", bad)
		}
	}
	if !strings.Contains(src, "BrowserOAuthClient") {
		t.Error("the vendored script doesn't export the OAuth client")
	}
	// The licences of what is in it are served beside it.
	lic := serve(s, "GET", dir+"vendor/atproto-oauth-client-browser.LICENSES.txt", nil)
	if lic.Code != 200 || !strings.Contains(lic.Body.String(), "@atproto/oauth-client-browser@") || !strings.Contains(lic.Body.String(), "MIT") {
		t.Errorf("licences: %d", lic.Code)
	}
}

func TestEveryElementTheFeedsPageScriptsUseIsInThePage(t *testing.T) {
	html, err := fs.ReadFile(webFS, "web/feeds.html")
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
	src, err := webFS.ReadFile("web/static/feeds.js")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\("([a-z][a-z0-9-]*)"\)`).FindAllStringSubmatch(string(src), -1) {
		used[m[1]] = true
		if !have[m[1]] {
			t.Errorf("feeds.js uses #%s, which the page doesn't have", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`(?:aria-labelledby|aria-describedby|for)="([^"]+)"`).FindAllStringSubmatch(string(html), -1) {
		used[m[1]] = true // an id the page's own markup points at, for a screen reader or a label
	}
	for _, id := range []string{"loading", "signed-out", "feeds-main"} { // shown by name, in show()
		used[id] = true
		if !strings.Contains(string(src), `"`+id+`"`) {
			t.Errorf("feeds.js never shows #%s", id)
		}
	}
	for id := range have {
		if !used[id] {
			t.Errorf("the page has #%s, which feeds.js never uses", id)
		}
	}
	for _, id := range []string{"signed-out", "feeds-main", "connect-form", "disconnect-button", "notice", "empty"} {
		if !regexp.MustCompile(`id="` + id + `"[^>]*\shidden`).Match(html) {
			t.Errorf("#%s is not hidden until the script shows it", id)
		}
	}
	if regexp.MustCompile(`id="loading"[^>]*\shidden`).Match(html) {
		t.Error("#loading is hidden before the script has run: the page would be blank")
	}
}

func TestEveryClassTheFeedsPageScriptUsesIsStyled(t *testing.T) {
	src, err := webFS.ReadFile("web/static/feeds.js")
	if err != nil {
		t.Fatal(err)
	}
	var styles strings.Builder
	for _, name := range []string{"app.css", "me.css", "feeds.css"} {
		css, err := webFS.ReadFile("web/static/" + name)
		if err != nil {
			t.Fatal(err)
		}
		styles.Write(css)
	}
	for _, m := range regexp.MustCompile(`class: "([a-z][a-z0-9 -]*)"`).FindAllStringSubmatch(string(src), -1) {
		for _, class := range strings.Fields(m[1]) {
			if !strings.Contains(styles.String(), "."+class) {
				t.Errorf("feeds.js gives an element the class %q, which no stylesheet of the page mentions", class)
			}
		}
	}
	css, _ := webFS.ReadFile("web/static/feeds.css")
	if regexp.MustCompile(`(?i)url\(\s*["']?(?:https?:)?//|@import`).Match(css) {
		t.Error("feeds.css loads something from another site")
	}
}

// TestFeedsPageScript runs the page's own script, in a simulated browser (jsdom), against the real page. It
// needs node and jsdom, so it only runs when TOPICFEED_JSDOM names jsdom's directory.
func TestFeedsPageScript(t *testing.T) {
	runNodeTests(t, true, "webtest/feeds.test.mjs")
}

// TestPublishPageScript runs publish.js (publishing feeds from the browser) against a stand-in for the OAuth
// client and for the person's own server. It needs node only.
func TestPublishPageScript(t *testing.T) {
	runNodeTests(t, false, "webtest/publish.test.mjs")
}

func runNodeTests(t *testing.T, needsJSDOM bool, files ...string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	jsdom := os.Getenv("TOPICFEED_JSDOM")
	if needsJSDOM && jsdom == "" {
		t.Skip("set TOPICFEED_JSDOM to the path of jsdom (node_modules/jsdom) to run the page's script tests")
	}
	cmd := exec.Command(node, append([]string{"--test"}, files...)...)
	cmd.Env = append(os.Environ(), "TOPICFEED_JSDOM="+jsdom)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script's tests failed: %v\n%s", err, out)
	}
}
