package kernel

import (
	"net/http"
	"strings"
	"testing"
)

func nop() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
}

// A duplicate route pattern must fail Router (main fatals on it) —
// never a silent last-mount-wins.
func TestRouterRejectsDuplicatePatterns(t *testing.T) {
	a := &App{}
	_, err := a.Router(
		Mount{App: "alpha", Routes: []Route{{Pattern: "GET /x", Handler: nop()}}},
		Mount{App: "beta", Routes: []Route{{Pattern: "GET /x", Handler: nop()}}},
	)
	if err == nil {
		t.Fatal("Router accepted a duplicate pattern")
	}
	for _, want := range []string{"GET /x", "alpha", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("duplicate error %q should name %q", err, want)
		}
	}
}

// An app colliding with a kernel-owned route must fail the same way.
func TestRouterRejectsKernelCollision(t *testing.T) {
	a := &App{}
	_, err := a.Router(Mount{App: "rogue", Routes: []Route{{Pattern: "GET /login", Handler: nop()}}})
	if err == nil {
		t.Fatal("Router accepted a collision with a kernel route")
	}
	if !strings.Contains(err.Error(), "kernel") {
		t.Errorf("collision error %q should name the kernel", err)
	}
}

func TestRouterAcceptsDistinctMounts(t *testing.T) {
	a := &App{}
	h, err := a.Router(
		Mount{App: "alpha", Routes: []Route{{Pattern: "GET /a", Handler: nop()}}},
		Mount{App: "beta", Routes: []Route{{Pattern: "GET /b", Handler: nop()}}},
	)
	if err != nil {
		t.Fatalf("Router: %v", err)
	}
	if h == nil {
		t.Fatal("Router returned a nil handler")
	}
}
