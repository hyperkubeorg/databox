package cloudferry

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
)

func TestIPLimiter(t *testing.T) {
	now := time.Now()
	l := &ipLimiter{nowFn: func() time.Time { return now }}
	l.setRate(3)

	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d refused inside the budget", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Error("4th request allowed past a 3/min budget")
	}
	// A different IP has its own bucket.
	if !l.allow("5.6.7.8") {
		t.Error("second IP throttled by the first's bucket")
	}
	// A minute later the bucket has refilled.
	now = now.Add(time.Minute)
	if !l.allow("1.2.3.4") {
		t.Error("bucket never refilled")
	}
	// Retuning applies live (config push).
	l.setRate(1)
	l.allow("9.9.9.9")
	if l.allow("9.9.9.9") {
		t.Error("retuned rate not applied")
	}
}

func TestConnGate(t *testing.T) {
	g := &connGate{}
	g.setMax(2)
	if !g.take() || !g.take() {
		t.Fatal("gate refused inside the cap")
	}
	if g.take() {
		t.Error("gate allowed past the cap")
	}
	g.release()
	if !g.take() {
		t.Error("released slot not reusable")
	}
}

// TestGitBodyLimitDispatch covers §6.4 (Git Draft 002): git wire POSTs
// ride maxGitBodyBytes; everything else keeps the general cap; both
// fall back to their defaults when the push left them zero.
func TestGitBodyLimitDispatch(t *testing.T) {
	req := func(method, path string) *http.Request {
		return httptest.NewRequest(method, "https://pcp.example.com"+path, nil)
	}
	ac := &appliedConfig{ConfigPush: ferryproto.ConfigPush{
		Limits: ferryproto.EdgeLimits{MaxBodyBytes: 10 << 20, MaxGitBodyBytes: 2 << 30},
	}}
	cases := []struct {
		method, path string
		want         int64
	}{
		{"POST", "/git/ada/hello/git-receive-pack", 2 << 30},
		{"POST", "/git/ada/hello.git/git-upload-pack", 2 << 30},
		{"GET", "/git/ada/hello/info/refs", 10 << 20}, // GET: no data body
		{"POST", "/drive/upload", 10 << 20},
		{"POST", "/gitlab/git-receive-pack", 10 << 20}, // not under /git/
	}
	for _, c := range cases {
		if got := ac.bodyLimitFor(req(c.method, c.path)); got != c.want {
			t.Errorf("bodyLimitFor(%s %s) = %d, want %d", c.method, c.path, got, c.want)
		}
	}
	// Zero fields resolve to the defaults — a never-pushed gateway still
	// bounds git bodies.
	empty := &appliedConfig{}
	if got := empty.bodyLimitFor(req("POST", "/git/a/b/git-receive-pack")); got != defaultMaxGitBodyBytes {
		t.Errorf("default git cap = %d, want %d", got, int64(defaultMaxGitBodyBytes))
	}
	if got := empty.bodyLimitFor(req("POST", "/x")); got != defaultMaxBodyBytes {
		t.Errorf("default general cap = %d, want %d", got, int64(defaultMaxBodyBytes))
	}
}
