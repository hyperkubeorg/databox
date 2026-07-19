// limits.go — the edge limits from the config push: a per-IP
// per-minute token bucket and a concurrent-connection gate. Both read
// their tuning atomically so a config push retunes them live. (The
// kernel has its own per-replica limiters; these are the real edge in
// front of them — cloudferry can't import the kernel, so the bucket is
// reimplemented at gateway size.)
package cloudferry

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ipLimiterMaxKeys bounds the bucket map so an attacker rotating
// source addresses can't grow memory without limit.
const ipLimiterMaxKeys = 100000

// ipLimiter is a token-bucket limiter keyed by client IP. The rate is
// atomic — retune() adjusts it on every config push.
type ipLimiter struct {
	perMin  atomic.Int64
	mu      sync.Mutex
	buckets map[string]*ipBucket
	nowFn   func() time.Time // injectable for tests
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

// setRate installs requests/minute (also the burst size).
func (l *ipLimiter) setRate(perMinute int) { l.perMin.Store(int64(perMinute)) }

// now resolves the clock (tests inject one).
func (l *ipLimiter) now() time.Time {
	if l.nowFn != nil {
		return l.nowFn()
	}
	return time.Now()
}

// allow spends one token for ip, reporting whether it was available.
func (l *ipLimiter) allow(ip string) bool {
	rate := float64(l.perMin.Load())
	if rate <= 0 {
		rate = defaultPerIPPerMin
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.buckets == nil {
		l.buckets = map[string]*ipBucket{}
	}
	b := l.buckets[ip]
	if b == nil {
		if len(l.buckets) >= ipLimiterMaxKeys {
			l.sweep(now, rate)
		}
		b = &ipBucket{tokens: rate, last: now}
		l.buckets[ip] = b
	}
	b.tokens = min(rate, b.tokens+now.Sub(b.last).Minutes()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep (locked) drops buckets that have refilled to full — they carry
// no throttling state, so deleting them is behavior-neutral.
func (l *ipLimiter) sweep(now time.Time, rate float64) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*rate >= rate {
			delete(l.buckets, k)
		}
	}
}

// connGate caps concurrent public connections. Accept-side: when full,
// new connections are closed immediately (fail fast beats a hung
// browser tab; the pool is per config push).
type connGate struct {
	max  atomic.Int64
	open atomic.Int64
}

// setMax installs the cap.
func (g *connGate) setMax(n int) { g.max.Store(int64(n)) }

// take reserves one slot, false when at cap.
func (g *connGate) take() bool {
	limit := g.max.Load()
	if limit <= 0 {
		limit = defaultMaxConns
	}
	if g.open.Add(1) > limit {
		g.open.Add(-1)
		return false
	}
	return true
}

// release frees one slot.
func (g *connGate) release() { g.open.Add(-1) }

// gatedListener wraps a public listener with the connection cap.
type gatedListener struct {
	net.Listener
	gate *connGate
}

// Accept enforces the cap: over-limit connections are closed on the
// spot, and Accept keeps looping for the next one.
func (l gatedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !l.gate.take() {
			_ = c.Close()
			continue
		}
		return &gatedConn{Conn: c, gate: l.gate}, nil
	}
}

// gatedConn releases its slot exactly once on close.
type gatedConn struct {
	net.Conn
	gate *connGate
	once sync.Once
}

func (c *gatedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.gate.release() })
	return err
}
