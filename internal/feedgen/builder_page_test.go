package feedgen

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// scriptsOfTheBuilderPage are the scripts the page at / is made of.
var scriptsOfTheBuilderPage = []string{"app.js", "header.js", "posts.js", "mine.js"}

func TestEveryScriptOfTheBuilderPageIsServedAndWhatItImportsExists(t *testing.T) {
	s := testServer(t)
	body := serve(s, "GET", "/", nil).Body.String()
	var dir string
	for _, a := range versionedAssets(body) {
		if strings.HasSuffix(a, "/app.js") {
			dir = a[:strings.LastIndex(a, "/")+1]
		}
	}
	if dir == "" {
		t.Fatalf("the page doesn't load app.js by version: %v", versionedAssets(body))
	}
	imports := regexp.MustCompile(`(?m)^(?:import|export)[^;]*? from "\./([A-Za-z0-9._-]+)";`)
	for _, name := range scriptsOfTheBuilderPage {
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
			if !slicesContain(scriptsOfTheBuilderPage, m[1]) {
				t.Errorf("%s imports ./%s, which this test doesn't know belongs to the page", name, m[1])
			}
		}
	}
}

// What a person's feed is saved from shows other people's words (the feed's name and description, the
// handle): all of it goes on the page as text.
func TestTheScriptThatSavesFeedsPutsTextOnThePageAsText(t *testing.T) {
	js, err := webFS.ReadFile("web/static/mine.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(string(js), bad) {
			t.Errorf("mine.js uses %s", bad)
		}
	}
}

// TestMinePageScript runs mine.js, which saves the feed being built as the signed-in person's own, in a
// simulated browser (jsdom). It only runs when TOPICFEED_JSDOM names jsdom's directory:
//
//	TOPICFEED_JSDOM=/path/to/node_modules/jsdom go test ./internal/feedgen/ -run TestMinePageScript -v
func TestMinePageScript(t *testing.T) {
	jsdom := os.Getenv("TOPICFEED_JSDOM")
	if jsdom == "" {
		t.Skip("set TOPICFEED_JSDOM to the path of jsdom (node_modules/jsdom) to run the page's script tests")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	cmd := exec.Command(node, "--test", "webtest/mine.test.mjs")
	cmd.Env = append(os.Environ(), "TOPICFEED_JSDOM="+jsdom)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the script's tests failed: %v\n%s", err, out)
	}
}
