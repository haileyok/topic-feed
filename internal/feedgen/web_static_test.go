package feedgen

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

var versionedURL = regexp.MustCompile(`/static/v/[0-9a-f]{12}/[A-Za-z0-9._-]+`)

// versionedAssets are the scripts and styles a page names by version.
func versionedAssets(html string) []string { return versionedURL.FindAllString(html, -1) }

func TestStaticVersionChangesExactlyWhenAFileDoes(t *testing.T) {
	files := func() fstest.MapFS {
		return fstest.MapFS{
			"app.js":       {Data: []byte("console.log(1)")},
			"app.css":      {Data: []byte("body{}")},
			"sub/posts.js": {Data: []byte("export {}")},
		}
	}
	base, err := staticVersion(files())
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(base) {
		t.Fatalf("%q, %v", base, err)
	}
	if again, _ := staticVersion(files()); again != base {
		t.Errorf("the same files gave %q and then %q", base, again)
	}
	changed := map[string]func(fstest.MapFS){
		"a file's content changes":        func(m fstest.MapFS) { m["app.js"] = &fstest.MapFile{Data: []byte("console.log(2)")} },
		"a file's content changes a bit":  func(m fstest.MapFS) { m["app.css"] = &fstest.MapFile{Data: []byte("body{ }")} },
		"a file is added":                 func(m fstest.MapFS) { m["new.js"] = &fstest.MapFile{Data: []byte("x")} },
		"a file is removed":               func(m fstest.MapFS) { delete(m, "app.css") },
		"a file is renamed":               func(m fstest.MapFS) { m["renamed.js"] = m["app.js"]; delete(m, "app.js") },
		"a file in a folder changes":      func(m fstest.MapFS) { m["sub/posts.js"] = &fstest.MapFile{Data: []byte("export {a}")} },
		"content moves between two files": func(m fstest.MapFS) { m["app.js"], m["app.css"] = m["app.css"], m["app.js"] },
	}
	for name, change := range changed {
		m := files()
		change(m)
		if got, _ := staticVersion(m); got == base {
			t.Errorf("%s, but the version stayed %s", name, base)
		}
	}
	// A file's name and content can't be confused with another's: "a"+"bc" is not "ab"+"c".
	one, _ := staticVersion(fstest.MapFS{"a": {Data: []byte("bc")}})
	two, _ := staticVersion(fstest.MapFS{"ab": {Data: []byte("c")}})
	if one == two {
		t.Error("two different files gave the same version")
	}
	// Nor can one file's content be made to read as another file's name and content: here a
	// single file ends in what looks like "b", a separator, and "X".
	merged, _ := staticVersion(fstest.MapFS{"a": {Data: []byte("1b\x00X")}})
	split, _ := staticVersion(fstest.MapFS{"a": {Data: []byte("1")}, "b": {Data: []byte("X")}})
	if merged == split {
		t.Error("one file and two files that read alike gave the same version")
	}
}

func TestPagesNameTheirScriptsAndStylesByVersion(t *testing.T) {
	s := testServer(t)
	embedded, err := fs.Sub(webFS, "web/static")
	if err != nil {
		t.Fatal(err)
	}
	version, err := staticVersion(embedded)
	if err != nil {
		t.Fatal(err)
	}
	for page, want := range map[string]int{"/": 2, "/me": 3} {
		body := serve(s, "GET", page, nil).Body.String()
		if strings.Contains(body, "__V__") || strings.Contains(body, "__ORIGIN__") {
			t.Errorf("%s still has a placeholder", page)
		}
		assets := versionedAssets(body)
		if len(assets) != want {
			t.Errorf("%s names %v by version, want %d files", page, assets, want)
		}
		for _, a := range assets {
			if !strings.HasPrefix(a, "/static/v/"+version+"/") {
				t.Errorf("%s: %s is not of this version (%s)", page, a, version)
			}
		}
		// And none of its scripts or styles is named the old way, which caches could keep for hours.
		for _, old := range regexp.MustCompile(`(?:href|src)="(/static/[A-Za-z0-9._-]+\.(?:js|css))"`).FindAllStringSubmatch(body, -1) {
			t.Errorf("%s still loads %s without a version", page, old[1])
		}
	}
}

func TestVersionedFilesAreServedForeverAndOldVersionsAreGone(t *testing.T) {
	s := testServer(t)
	body := serve(s, "GET", "/", nil).Body.String()
	js := ""
	for _, a := range versionedAssets(body) {
		if strings.HasSuffix(a, "/app.js") {
			js = a
		}
	}
	if js == "" {
		t.Fatalf("the home page doesn't load app.js by version: %s", versionedAssets(body))
	}
	w := serve(s, "GET", js, nil)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	want, _ := webFS.ReadFile("web/static/app.js")
	if w.Body.String() != string(want) {
		t.Error("the versioned file is not the embedded one")
	}
	// A module's relative import lands in the same version, so a page's files always match.
	dir := js[:strings.LastIndex(js, "/")+1]
	if w := serve(s, "GET", dir+"posts.js", nil); w.Code != 200 {
		t.Errorf("app.js imports ./posts.js: %d", w.Code)
	}
	// A page from before a deploy asks for a version that is gone: not found, and not cached.
	old := "/static/v/000000000000/app.js"
	w = serve(s, "GET", old, nil)
	if w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%s: %d %v", old, w.Code, w.Header())
	}
	if w := serve(s, "GET", "/static/v/", nil); w.Code != http.StatusNotFound {
		t.Errorf("/static/v/: %d", w.Code)
	}
	// What is linked from elsewhere still works at its plain address.
	if w := serve(s, "GET", "/static/og.png", nil); w.Code != 200 {
		t.Errorf("/static/og.png: %d", w.Code)
	}
	// The pages themselves are never cached: they say which version to load.
	for _, page := range []string{"/", "/me"} {
		if cc := serve(s, "GET", page, nil).Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q", page, cc)
		}
	}
}
