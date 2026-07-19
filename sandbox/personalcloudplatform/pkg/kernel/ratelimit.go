// ratelimit.go — per-IP/per-username token buckets guarding the auth
// endpoints. Purely in-memory and per-replica by design: defense in
// depth against online guessing and floods — cloudferry's edge limiter
// (phase 7) is the real abuse surface in front of these.
package kernel

import (
	"sync"
	"time"
)

// Limits (per minute; burst = the same number unless noted).
const (
	loginIPPerMinute   = 10
	loginUserPerMinute = 10
	signupIPPerMinute  = 5
	// API bearer requests, per key (apiauth.go). Generous — real client
	// software syncs in bursts — but bounded.
	apiKeyPerMinute = 300
	apiKeyBurst     = 60
	// Upload REQUESTS per member per minute (not bytes — bodies are
	// bounded by the upload cap). The web client batches small files
	// per request, so a big drop rides well under this.
	uploadPerMinute = 120
	// rateLimiterMaxKeys bounds the bucket map so an attacker rotating
	// keys can't grow memory without limit.
	rateLimiterMaxKeys = 10000
)

// rateLimiter is a token-bucket limiter keyed by string (IP, username).
// Mutex-only: allow never blocks on anything and a nil limiter always
// allows, so rate limiting can only ever fail open.
type rateLimiter struct {
	mu      sync.Mutex
	perMin  float64
	burst   float64
	buckets map[string]*rlBucket
	now     func() time.Time // injectable for tests
}

type rlBucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter allows perMinute requests/min per key with an equal
// burst.
func newRateLimiter(perMinute int) *rateLimiter {
	return newRateLimiterBurst(perMinute, perMinute)
}

// newRateLimiterBurst allows perMinute requests/min per key with an
// explicit bucket size.
func newRateLimiterBurst(perMinute, burst int) *rateLimiter {
	return &rateLimiter{
		perMin:  float64(perMinute),
		burst:   float64(burst),
		buckets: make(map[string]*rlBucket),
		now:     time.Now,
	}
}

// allow spends one token for key, reporting whether it was available.
func (l *rateLimiter) allow(key string) bool {
	if l == nil {
		return true // unwired limiter: fail open, never lock anyone out
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= rateLimiterMaxKeys {
			l.sweep(now)
		}
		b = &rlBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Minutes()*l.perMin)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep (locked) drops buckets that have refilled to full — they carry
// no throttling state, so deleting them is behavior-neutral.
func (l *rateLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Minutes()*l.perMin >= l.burst {
			delete(l.buckets, k)
		}
	}
}
