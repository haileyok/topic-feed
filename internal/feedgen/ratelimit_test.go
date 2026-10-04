package feedgen

import (
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type limiterClock struct{ t atomic.Int64 }

func (c *limiterClock) Now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *limiterClock) Advance(d time.Duration) { c.t.Add(int64(d)) }

func newTestLimiter(every time.Duration, burst int) (*IPLimiter, *limiterClock) {
	l := NewIPLimiter(every, burst)
	c := &limiterClock{}
	c.t.Store(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixNano())
	l.Now = c.Now
	return l, c
}

func TestIPLimiterAllowsABurstThenOnePerInterval(t *testing.T) {
	l, clk := newTestLimiter(10*time.Second, 3)
	for i := range 3 {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("use %d of the burst was refused", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Fatal("a fourth use at once was allowed")
	}
	clk.Advance(9 * time.Second)
	if l.Allow("1.2.3.4") {
		t.Error("allowed before the interval passed")
	}
	clk.Advance(time.Second)
	if !l.Allow("1.2.3.4") {
		t.Error("refused after the interval")
	}
	if l.Allow("1.2.3.4") {
		t.Error("only one more should come back per interval")
	}
	clk.Advance(time.Hour)
	for i := range 3 {
		if !l.Allow("1.2.3.4") {
			t.Errorf("after a long wait, use %d of a full burst was refused", i+1)
		}
	}
	if l.Allow("1.2.3.4") {
		t.Error("the burst is the most that builds up")
	}
}

func TestIPLimiterTreatsAddressesSeparately(t *testing.T) {
	l, _ := newTestLimiter(time.Minute, 1)
	if !l.Allow("1.1.1.1") || !l.Allow("2.2.2.2") || !l.Allow("2001:db8::1") {
		t.Error("each address has a burst of its own")
	}
	if l.Allow("1.1.1.1") || l.Allow("2.2.2.2") || l.Allow("2001:db8::1") {
		t.Error("and uses it up on its own")
	}
}

func TestIPLimiterForgetsAddressesThatHaveGone(t *testing.T) {
	l, clk := newTestLimiter(time.Second, 2)
	for i := range 100 {
		l.Allow(fmt.Sprintf("10.0.0.%d", i))
	}
	if n := l.tracked(); n != 100 {
		t.Fatalf("%d tracked", n)
	}
	clk.Advance(limiterIdleAfter + limiterSweepEvery)
	l.Allow("10.9.9.9")
	if n := l.tracked(); n != 1 {
		t.Errorf("%d tracked after everyone was idle for a long time, want only the one just seen", n)
	}
	// Someone who comes back after being forgotten has their full burst, which is what they
	// would have anyway by then.
	if !l.Allow("10.0.0.5") || !l.Allow("10.0.0.5") || l.Allow("10.0.0.5") {
		t.Error("a returning address should get exactly its burst")
	}
}

func TestIPLimiterRefusesNewAddressesWhenFlooded(t *testing.T) {
	l, clk := newTestLimiter(time.Second, 1)
	for i := range maxLimiterVisitors {
		if !l.Allow(fmt.Sprint("flood-", i)) {
			t.Fatalf("address %d was refused before the table was full", i)
		}
	}
	if l.Allow("one-more") {
		t.Error("a new address was allowed with the table full")
	}
	if n := l.tracked(); n != maxLimiterVisitors {
		t.Errorf("%d tracked, the table grew past its limit", n)
	}
	// Addresses already known are still counted properly.
	if l.Allow("flood-0") {
		t.Error("a known address got a second use at once")
	}
	// Once the flood is idle, there is room again.
	clk.Advance(limiterIdleAfter + time.Second)
	if !l.Allow("one-more") {
		t.Error("still refused after the flood went quiet")
	}
}

func TestIPLimiterUnderConcurrentUse(t *testing.T) {
	l := NewIPLimiter(time.Hour, 5)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if l.Allow("same") {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if n := allowed.Load(); n != 5 {
		t.Errorf("%d allowed at once from one address, want exactly the burst of 5", n)
	}
}

func TestRequestIP(t *testing.T) {
	for name, tc := range map[string]struct {
		remote, cf, want string
	}{
		"a direct connection":     {"203.0.113.9:5555", "", "203.0.113.9"},
		"through the tunnel":      {"127.0.0.1:41234", "198.51.100.7", "198.51.100.7"},
		"IPv6 through the tunnel": {"127.0.0.1:41234", "2001:db8::7", "2001:db8::7"},
		"IPv6 direct":             {"[2001:db8::1]:5555", "", "2001:db8::1"},
		"an address with no port": {"203.0.113.9", "", "203.0.113.9"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		if tc.cf != "" {
			r.Header.Set("CF-Connecting-IP", tc.cf)
		}
		if got := requestIP(r); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
	l, _ := newTestLimiter(time.Hour, 1)
	r := httptest.NewRequest("POST", "/oauth/login", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("CF-Connecting-IP", "198.51.100.7")
	if !l.AllowRequest(r) || l.AllowRequest(r) {
		t.Error("AllowRequest should count the visitor behind the tunnel")
	}
}
