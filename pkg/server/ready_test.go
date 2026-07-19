// ready_test.go — the readiness decision (§4 /readyz).
package server

import "testing"

func TestReadiness(t *testing.T) {
	cases := []struct {
		name    string
		started bool
		lead    uint64
		commit  uint64
		applied uint64
		want    bool
	}{
		{"not started", false, 0, 0, 0, false},
		{"no leader", true, 0, 10, 10, false},
		{"caught up", true, 1, 10, 10, true},
		{"small lag ok", true, 1, 100, 90, true},
		{"at the lag bound", true, 1, readyMaxLag + 5, 5, true},
		{"too far behind", true, 1, readyMaxLag + 6, 5, false},
		{"fresh single node", true, 1, 0, 0, true},
	}
	for _, c := range cases {
		got, reason := readiness(c.started, c.lead, c.commit, c.applied)
		if got != c.want {
			t.Errorf("%s: readiness = %v (%s), want %v", c.name, got, reason, c.want)
		}
		if !got && reason == "" {
			t.Errorf("%s: unready without a reason", c.name)
		}
	}
}

// TestReadyNonMember — a node that hosts no metadata group serves by
// ROUTING, so its readiness bar is member discovery, not a local replica.
// (Before the metaproxy era this path returned "metadata group not
// started" forever, wedging every non-member behind load balancers.)
func TestReadyNonMember(t *testing.T) {
	s := &Server{metaProxy: newMetaProxy()}
	ok, reason := s.Ready()
	if ok || reason == "" {
		t.Fatalf("undiscovered non-member: Ready = %v (%q), want unready with reason", ok, reason)
	}
	s.metaProxy.setMembers([]metaMemberInfo{{ID: 2, Addr: "m:1"}}, 2)
	if ok, reason := s.Ready(); !ok {
		t.Fatalf("discovered non-member: Ready = false (%q), want ready", reason)
	}
}
