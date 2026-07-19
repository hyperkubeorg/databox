// ratelimit.go is the byte budget for background repair traffic (§11:
// the repair loop is "rate-limited to protect foreground traffic"). A
// plain token bucket is enough: repair and scrub call Wait before moving
// bytes, foreground reads/writes never do.
package blob

import (
	"sync"
	"time"
)

// RateLimiter is a token-bucket byte limiter. The zero of usefulness is a
// nil *RateLimiter: all methods are nil-safe no-ops, so "unlimited" needs
// no special-casing at call sites.
type RateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes replenished per second
	burst  float64 // bucket capacity (one second of rate)
	tokens float64 // may go negative: overdraft is repaid by sleeping
	last   time.Time
}

// NewRateLimiter builds a limiter allowing bytesPerSec of sustained
// throughput with a one-second burst. bytesPerSec ≤ 0 returns nil
// (unlimited).
func NewRateLimiter(bytesPerSec int64) *RateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	r := float64(bytesPerSec)
	return &RateLimiter{rate: r, burst: r, tokens: r, last: time.Now()}
}

// Wait blocks until n bytes of budget are available. Requests larger than
// the bucket are charged one full bucket — they proceed (a single chunk
// must never deadlock the limiter) and the overdraft delays what follows.
func (l *RateLimiter) Wait(n int64) {
	if l == nil || n <= 0 {
		return
	}
	l.mu.Lock()
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.last = now
	l.tokens -= min(float64(n), l.burst)
	var wait time.Duration
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	l.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}
}
