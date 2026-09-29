package feedgen

import (
	"embed"
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

func addWebRoutes(e *echo.Echo) {
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		panic(err)
	}
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		panic(err)
	}
	files := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	e.GET("/static/*", func(c echo.Context) error {
		c.Response().Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(c.Response(), c.Request())
		return nil
	})
	e.GET("/", func(c echo.Context) error {
		h := c.Response().Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Cache-Control", "no-cache")
		return c.HTMLBlob(http.StatusOK, index)
	})
}
