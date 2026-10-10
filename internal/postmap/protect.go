package postmap

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits protect the service when it is reachable from the internet: how often one client may ask for things, and how
// many text searches (each scans every post) run at once. A nil *Limits means no limits.
type Limits struct {
	// Per client: requests per second kept up (and the burst allowed) for posts and segments, and for searches.
	PostsPerSec, PostsBurst   float64
	SearchPerSec, SearchBurst float64
	// Text searches running at the same time, over all clients; the rest are told to wait.
	SearchesAtOnce int
	// The longest search text, in characters.
	MaxQueryChars int
	// TrustProxyHeader makes the CF-Connecting-IP header count whoever the peer is. Set it only when the port can be
	// reached by nothing but the tunnel (in a container the tunnel's connections do not arrive from the loopback address).
	TrustProxyHeader bool

	now func() time.Time // for tests

	mu       sync.Mutex
	buckets  map[string]*bucket
	lastGC   time.Time
	searches chan struct{}
	once     sync.Once
}

// DefaultLimits are meant for the public internet: enough for a person using the page (it asks for a post's text when
// the pointer rests on a point, and for matches as they type), not enough to copy the map through the API.
func DefaultLimits() *Limits {
	return &Limits{PostsPerSec: 12, PostsBurst: 60, SearchPerSec: 2, SearchBurst: 10, SearchesAtOnce: 4, MaxQueryChars: 120}
}

type bucket struct {
	tokens float64
	at     time.Time
}

func (l *Limits) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// allow takes one token from the client's bucket of this kind, or says how long to wait.
func (l *Limits) allow(client, kind string, perSec, burst float64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	if now.Sub(l.lastGC) > 5*time.Minute { // forget clients that have been quiet long enough to be full again
		for k, b := range l.buckets {
			if now.Sub(b.at) > time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	key := kind + "|" + client
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.at).Seconds()*perSec)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / perSec * float64(time.Second))
}

// clientIP is who is asking. Behind the Cloudflare tunnel every request arrives from the loopback address and says who
// it is for in CF-Connecting-IP; that header is believed only then (or when told to believe it always, for a port only
// the tunnel can reach), so a client reaching the port directly cannot pass itself off as someone else.
func clientIP(r *http.Request, trustHeader bool) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); trustHeader || (ip != nil && ip.IsLoopback()) {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); net.ParseIP(cf) != nil {
			return cf
		}
	}
	return host
}

// limit wraps a handler so that a client asking too often gets 429 and how long to wait.
func (l *Limits) limit(kind string, perSec, burst float64, h http.HandlerFunc) http.HandlerFunc {
	if l == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.allow(clientIP(r, l.TrustProxyHeader), kind, perSec, burst); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many requests: wait a moment"})
			return
		}
		h(w, r)
	}
}

// startSearch reserves one of the places for a text search running at the same time; the returned func gives it back.
// It is nil when they are all taken.
func (l *Limits) startSearch() func() {
	if l == nil {
		return func() {}
	}
	l.once.Do(func() { l.searches = make(chan struct{}, max(1, l.SearchesAtOnce)) })
	select {
	case l.searches <- struct{}{}:
		return func() { <-l.searches }
	default:
		return nil
	}
}

// protect puts headers on everything the service sends: the page may load only its own files and talk only to this
// service, may not be framed, and the browser may not guess types.
func protect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.ServeHTTP(w, r)
	})
}
