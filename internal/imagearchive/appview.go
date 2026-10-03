package imagearchive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

// MaxURIsPerCall is how many posts app.bsky.feed.getPosts answers for at once.
const MaxURIsPerCall = 25

// StatusError is an HTTP answer the archive does not retry.
type StatusError struct {
	Code       int
	Body       string
	RetryAfter int // seconds the server asked for (Retry-After); -1 when it said nothing
}

func (e *StatusError) Error() string { return fmt.Sprintf("http %d: %s", e.Code, e.Body) }

// AppView asks a Bluesky AppView about posts.
type AppView struct {
	BaseURL   string        // e.g. https://public.api.bsky.app
	Client    *http.Client  // default: 30 second timeout
	Limiter   *rate.Limiter // requests per second, shared by every caller; nil for no limit
	UserAgent string
	Backoff   time.Duration // first wait before a retry, doubling each time; default 1s
}

// GetPosts returns the views the AppView has for uris, keyed by URI. A post it has no view of
// (deleted, taken down, hidden from the public) is absent. It retries rate limits and server
// errors; any other failure is returned, as a *StatusError when the server said why.
func (a *AppView) GetPosts(ctx context.Context, uris []string) (map[string]PostView, error) {
	out := map[string]PostView{}
	if len(uris) == 0 {
		return out, nil
	}
	if len(uris) > MaxURIsPerCall {
		return nil, fmt.Errorf("getPosts takes at most %d posts, got %d", MaxURIsPerCall, len(uris))
	}
	q := url.Values{}
	for _, u := range uris {
		q.Add("uris", u)
	}
	endpoint := strings.TrimRight(a.BaseURL, "/") + "/xrpc/app.bsky.feed.getPosts?" + q.Encode()
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := a.Backoff
	if base <= 0 {
		base = time.Second
	}

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if a.Limiter != nil {
			if err := a.Limiter.Wait(ctx); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", a.UserAgent)
		resp, err := client.Do(req)
		wait := min(base<<attempt, 30*time.Second)
		if err != nil {
			lastErr = err
		} else {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK && rerr == nil:
				var parsed struct {
					Posts []PostView `json:"posts"`
				}
				if err := json.Unmarshal(body, &parsed); err != nil {
					return nil, fmt.Errorf("decode getPosts: %w", err)
				}
				for _, p := range parsed.Posts {
					out[p.URI] = p
				}
				return out, nil
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || rerr != nil:
				lastErr = &StatusError{Code: resp.StatusCode, Body: snippet(body)}
				if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
					wait = time.Duration(s) * time.Second
				}
			default:
				return nil, &StatusError{Code: resp.StatusCode, Body: snippet(body)}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("getPosts: giving up: %w", lastErr)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
