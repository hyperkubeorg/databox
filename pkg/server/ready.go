// ready.go — the node-level readiness gate (§4).
//
// /healthz answers "is the process alive" and nothing more; it must stay
// dumb so a node that is recovering is not killed while it catches up.
// /readyz answers the question load balancers actually ask: "can this
// node usefully serve a client request RIGHT NOW?" A node that just
// joined (or is mid-bootstrap) accepts TCP long before its metadata
// replica can serve a login — without this gate a Service routes
// clients into a black hole and a bootstrapping cluster looks broken
// from the outside, even though every pod is "running".
package server

import "fmt"

// readyMaxLag bounds how far a replica's applied index may trail the
// group's commit index and still count as serviceable. Proposals made
// through a node wait for LOCAL apply, so a deeply-behind replica
// (e.g. mid-snapshot after joining) stalls clients even when the group
// itself is healthy.
const readyMaxLag = 1024

// readiness is the pure decision for a METADATA MEMBER, split out for
// testing: a leader must be known and this replica must be close enough
// behind the commit index to serve without stalling.
func readiness(started bool, lead, commit, applied uint64) (bool, string) {
	if !started {
		return false, "metadata group not started"
	}
	if lead == 0 {
		return false, "no metadata leader known"
	}
	if commit > applied+readyMaxLag {
		return false, fmt.Sprintf("catching up (applied %d of %d)", applied, commit)
	}
	return true, ""
}

// Ready reports whether this node can serve client traffic, with a
// human-readable reason when it cannot. The two node modes have two
// different bars: a metadata member serves from its own replica (leader
// known, apply lag bounded); a non-member serves by ROUTING metadata
// lookups (pkg/server/metaproxy.go), so for it "can serve" means member
// discovery has found the metadata group.
func (s *Server) Ready() (bool, string) {
	if h := s.meta(); h != nil {
		st := h.group.Status()
		return readiness(true, st.Lead, st.Commit, st.Applied)
	}
	s.metaProxy.mu.Lock()
	known := len(s.metaProxy.members) > 0
	s.metaProxy.mu.Unlock()
	if !known {
		return false, "metadata members not discovered yet"
	}
	return true, ""
}
