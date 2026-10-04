package feedgen

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// maxLimiterVisitors bounds the addresses an IPLimiter remembers. Past it, addresses it
	// has not seen are refused until old ones are swept: the things it guards (calling out to
	// other servers, reading from the database) are worse to flood than to refuse.
	maxLimiterVisitors = 50_000
	limiterSweepEvery  = time.Minute
	limiterIdleAfter   = 10 * time.Minute
)

// IPLimiter limits how often each visitor, by address, may do something: a burst, then one
// more every `every`.
type IPLimiter struct {
	every time.Duration
	burst int
	// Now is the clock; nil: time.Now.
	Now func() time.Time

	mu       sync.Mutex
	visitors map[string]*limitedVisitor
	swept    time.Time
}

type limitedVisitor struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewIPLimiter allows each address `burst` uses at once and one more every `every`.
func NewIPLimiter(every time.Duration, burst int) *IPLimiter {
	return &IPLimiter{every: every, burst: burst, visitors: map[string]*limitedVisitor{}}
}

func (l *IPLimiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// Allow reports whether the visitor at ip may go ahead, and counts it if so.
func (l *IPLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	v, ok := l.visitors[ip]
	if !ok {
		if len(l.visitors) >= maxLimiterVisitors {
			return false
		}
		v = &limitedVisitor{lim: rate.NewLimiter(rate.Every(l.every), l.burst)}
		l.visitors[ip] = v
	}
	v.seen = now
	return v.lim.AllowN(now, 1)
}

// AllowRequest is Allow for the address a request came from.
func (l *IPLimiter) AllowRequest(r *http.Request) bool { return l.Allow(requestIP(r)) }

// sweep forgets visitors who haven't been seen for a while (they have their full burst
// back by then anyway), at most once a minute, or at once if the table is full.
func (l *IPLimiter) sweep(now time.Time) {
	if now.Sub(l.swept) < limiterSweepEvery && len(l.visitors) < maxLimiterVisitors {
		return
	}
	l.swept = now
	idle := max(limiterIdleAfter, l.every*time.Duration(l.burst)*2)
	for ip, v := range l.visitors {
		if now.Sub(v.seen) > idle {
			delete(l.visitors, ip)
		}
	}
}

func (l *IPLimiter) tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.visitors)
}

// requestIP is the visitor's address: Cloudflare's header when the request came through the
// tunnel (the server only listens on localhost), else the connection's.
func requestIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
