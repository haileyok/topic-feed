package feedgen

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/labstack/echo/v4"
)

// The feed builder page (web/index.html and web/static/*), served at "/".
//
//go:embed web
var webFS embed.FS

// contentSecurityPolicy lets the page load only its own files, posts from Bluesky's
// public API, and images from Bluesky's CDNs.
const contentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; style-src 'self'; font-src 'self'; " +
	"img-src 'self' data: https://cdn.bsky.app https://video.bsky.app https://video.cdn.bsky.app; " +
	"connect-src 'self' https://public.api.bsky.app; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// feedsContentSecurityPolicy is the policy of the page at /feeds alone, which publishes a person's feeds
// from their browser: it signs in to, and writes to, their own Bluesky account, whose server (and the
// server that signs them in) can be anywhere, so it may connect to any https address. That is a real
// loosening, and it is why every other page keeps the policy above. What makes it acceptable: scripts
// are still only the site's own (no inline script, no eval), nothing from another site is rendered
// (all text is put on the page as text), and the page can't be put in a frame or send a form anywhere.
// Pictures of accounts live on whatever server each account is on, so images may come from any https
// address too, and from blob: and data: for local previews.
const feedsContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; style-src 'self'; font-src 'self'; " +
	"img-src 'self' data: blob: https:; " +
	"connect-src 'self' https:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// staticVersion names this version of the static files: a hash of their names and contents, which
// changes exactly when one of them does.
func staticVersion(fsys fs.FS) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", path, len(b))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12], err
}

// addWebRoutes serves the page. origin (https://<hostname>) replaces __ORIGIN__ in it,
// for the absolute URLs link previews need.
func addWebRoutes(e *echo.Echo, origin string) {
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err)
	}
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		panic(err)
	}
	version, err := staticVersion(static)
	if err != nil {
		panic(err)
	}
	// Pages name their scripts and styles by version, so a deploy that changes one is a new
	// address that no browser or cache (Cloudflare keeps these for hours whatever we say) has
	// seen. A page and its files can then never come from different deploys.
	fill := func(page []byte) []byte {
		return bytes.ReplaceAll(bytes.ReplaceAll(page, []byte("__ORIGIN__"), []byte(origin)), []byte("__V__"), []byte(version))
	}
	index := fill(raw)
	files := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	e.GET("/static/*", func(c echo.Context) error { // for what is linked from elsewhere (the preview image)
		c.Response().Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(c.Response(), c.Request())
		return nil
	})
	versionPrefix := "/static/v/" + version + "/"
	versioned := http.StripPrefix(versionPrefix, http.FileServer(http.FS(static)))
	e.GET(versionPrefix+"*", func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		versioned.ServeHTTP(c.Response(), c.Request())
		return nil
	})
	// A page from before a deploy asking for its own files: they are gone, and that must not stick.
	e.GET("/static/v/*", func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		return echo.ErrNotFound
	})
	pageWith := func(c echo.Context, body []byte, policy string) error {
		h := c.Response().Header()
		h.Set("Content-Security-Policy", policy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cache-Control", "no-cache")
		return c.HTMLBlob(http.StatusOK, body)
	}
	page := func(c echo.Context, body []byte) error { return pageWith(c, body, contentSecurityPolicy) }
	e.GET("/", func(c echo.Context) error { return page(c, index) })

	// The page where a viewer signs in and tunes their feed. It is for one person at a time, so
	// it is kept out of search results; what it shows comes from /api/me and friends.
	meRaw, err := webFS.ReadFile("web/me.html")
	if err != nil {
		panic(err)
	}
	me := fill(meRaw)
	e.GET("/me", func(c echo.Context) error {
		c.Response().Header().Set("X-Robots-Tag", "noindex")
		return page(c, me)
	})

	// The post inspector: the owner's page for how one post was scored and which feeds take it. It
	// asks /api/inspect, which answers only the owner; the page itself holds nothing private.
	inspectRaw, err := webFS.ReadFile("web/inspect.html")
	if err != nil {
		panic(err)
	}
	inspect := fill(inspectRaw)
	e.GET("/inspect", func(c echo.Context) error {
		c.Response().Header().Set("X-Robots-Tag", "noindex")
		return page(c, inspect)
	})

	// A signed-in person's own feeds, and publishing them to Bluesky from their browser. It has a
	// policy of its own (see feedsContentSecurityPolicy).
	feedsRaw, err := webFS.ReadFile("web/feeds.html")
	if err != nil {
		panic(err)
	}
	feedsPage := fill(feedsRaw)
	e.GET("/feeds", func(c echo.Context) error {
		c.Response().Header().Set("X-Robots-Tag", "noindex")
		return pageWith(c, feedsPage, feedsContentSecurityPolicy)
	})
}
