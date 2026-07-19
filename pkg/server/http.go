// http.go runs the node's HTTPS listener and the internal RPC surface.
//
// One port serves everything (§4):
//
//	/internal/*   node-to-node RPC — PSK-gated, never for clients
//	/api/v1/*     public JSON API   — bearer-token auth (pkg/routes/v1api)
//	/health(z)    unauthenticated probes
//	/*            web GUI            (pkg/routes/frontend)
//
// The internal endpoints implemented here:
//
//	POST /internal/raft?gid=N     raft message delivery (pkg/raft/transport)
//	POST /internal/propose        {gid, op}   → kv.Result
//	POST /internal/list           {gid, prefix, cursor, limit} → entries
//	POST /internal/groups         {gid, members} start a group instance
//	POST /internal/confchange     {gid, node_id, add} membership change
//	GET  /internal/metamembers    current metadata members, from any node's view
//	GET  /internal/nodestats      local blob chunk totals (cluster map, topology.go)
//	POST /internal/transferlead   {gid, from, to} leadership transfer
//	POST /internal/liveness       {node_id, addr} in-memory liveness ping to the meta leader
//	PUT/GET/HEAD /internal/chunk/<hash>   blob chunk exchange
//	POST /internal/join           join handshake (§16.2)
//	GET  /internal/watch          NDJSON event stream proxy for remote shards
package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/certs"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/kv"
	dbraft "github.com/hyperkubeorg/databox/pkg/raft"
	"github.com/hyperkubeorg/databox/pkg/telemetry"
)

// RouteMounter lets the routes packages attach themselves without an
// import cycle: pkg/routes/* import pkg/server for the Server type; the
// command wiring passes their mount functions in.
type RouteMounter func(r *mux.Router, s *Server)

// Mounters are attached by cmd/databox before Run (API + GUI routers).
var Mounters []RouteMounter

// traceMiddleware wraps every matched route in an OpenTelemetry server
// span (§19). No-op (one atomic load) unless the process was started with
// an OTLP endpoint — see pkg/telemetry. Policy:
//
//   - /internal/raft and /internal/raftsnap are NEVER traced: they carry
//     every heartbeat and snapshot byte, and per-message spans would cost
//     more than they could ever explain.
//   - other /internal/* RPCs only JOIN traces (a span is created only when
//     the caller propagated a sampled traceparent) — cross-node hops show
//     up inside client traces without generating standalone volume.
//   - public routes are named by their mux route template, never the raw
//     path, so span-name cardinality stays bounded.
func traceMiddleware() mux.MiddlewareFunc {
	return mux.MiddlewareFunc(telemetry.Middleware(telemetry.MiddlewareConfig{
		RouteName: func(r *http.Request) string {
			if route := mux.CurrentRoute(r); route != nil {
				if tpl, err := route.GetPathTemplate(); err == nil {
					return tpl
				}
			}
			return ""
		},
		Skip: func(r *http.Request) bool {
			return r.URL.Path == "/internal/raft" || r.URL.Path == "/internal/raftsnap"
		},
		JoinOnly: func(r *http.Request) bool {
			return strings.HasPrefix(r.URL.Path, "/internal/")
		},
	}))
}

// startHTTP builds the router and starts the TLS listener.
func (s *Server) startHTTP() error {
	r := mux.NewRouter()
	r.Use(traceMiddleware())

	// --- health probes: top-level, unauthenticated (§4) ---
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","node":%d}`, s.nodeID)
	}
	r.HandleFunc("/health", health).Methods(http.MethodGet)
	r.HandleFunc("/healthz", health).Methods(http.MethodGet)
	// /readyz is the READINESS gate (§4): 503 until this node can
	// actually serve — metadata group up, leader known, apply lag
	// bounded. Load balancers key on this so a bootstrapping or
	// freshly-joined node receives no client traffic it would stall.
	r.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if ok, reason := s.Ready(); !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"status":"unready","reason":%q,"node":%d}`, reason, s.nodeID)
			return
		}
		fmt.Fprintf(w, `{"status":"ready","node":%d}`, s.nodeID)
	}).Methods(http.MethodGet)

	// Prometheus metrics — token-authenticated (§19, see metrics.go).
	r.HandleFunc("/metrics", s.handleMetrics).Methods(http.MethodGet)

	// --- internal RPC, PSK-gated ---
	in := r.PathPrefix("/internal").Subrouter()
	in.Use(s.pskMiddleware)
	in.HandleFunc("/raft", s.transport.Handler()).Methods(http.MethodPost)
	// Streamed (v2) snapshot transfer rides its own endpoint so bulk state
	// never queues behind raft heartbeats (§8, snapshot.go for the format).
	in.HandleFunc("/raftsnap", s.transport.SnapshotHandler()).Methods(http.MethodPost)
	in.HandleFunc("/propose", s.handleInternalPropose).Methods(http.MethodPost)
	in.HandleFunc("/list", s.handleInternalList).Methods(http.MethodPost)
	in.HandleFunc("/groups", s.handleInternalGroups).Methods(http.MethodPost)
	in.HandleFunc("/confchange", s.handleInternalConfChange).Methods(http.MethodPost)
	in.HandleFunc("/transferlead", s.handleInternalTransferLead).Methods(http.MethodPost)
	in.HandleFunc("/liveness", s.handleInternalLiveness).Methods(http.MethodPost)
	in.HandleFunc("/metamembers", s.handleInternalMetaMembers).Methods(http.MethodGet)
	in.HandleFunc("/nodestats", s.handleInternalNodeStats).Methods(http.MethodGet)
	in.HandleFunc("/metaget", s.handleInternalMetaGet).Methods(http.MethodPost)
	in.HandleFunc("/chunk/{hash}", s.handleInternalChunk)
	in.HandleFunc("/join", s.handleJoin).Methods(http.MethodPost)
	in.HandleFunc("/watch", s.handleInternalWatch).Methods(http.MethodGet)

	// --- public surfaces (API, GUI) mounted by cmd wiring ---
	for _, mount := range Mounters {
		mount(r, s)
	}

	// Structured 404 for anything unmatched (§4): JSON for API-ish
	// clients, HTML for browsers, plain text as the last resort.
	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		accept := req.Header.Get("Accept")
		switch {
		case strings.Contains(accept, "json") || strings.HasPrefix(req.URL.Path, "/api/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "NotFound", "path": req.URL.Path})
		case strings.Contains(accept, "text/html"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "<!doctype html><html><head><title>404 — databox</title></head>"+
				"<body><h1>404 Not Found</h1><p>No page at <code>%s</code>.</p>"+
				`<p><a href="/">databox home</a></p></body></html>`, html.EscapeString(req.URL.Path))
		default:
			http.Error(w, "404 not found", http.StatusNotFound)
		}
	})

	s.httpSrv = &http.Server{
		Addr:    s.Cfg.Listen,
		Handler: r,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// GetCertificate (not a static cert) so auto-rotation (§6.4)
			// takes effect without restarting the listener.
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &s.tlsCert, nil
			},
			// Request (never require) a client certificate: the one
			// listener serves browsers, API clients, AND /internal/* peers.
			// Peers present their cluster-issued cert; everyone else sends
			// nothing and is unaffected. The /internal/* middleware then
			// verifies the presented chain against the cluster CA (§6.1
			// mTLS) — enforcement is per-route, not per-connection.
			ClientAuth: tls.RequestClientCert,
		},
		// ReadHeaderTimeout bounds the header-read phase (the classic
		// slowloris vector); IdleTimeout reaps idle keep-alive connections
		// so a client can't park goroutines by holding sockets open between
		// requests. Read/WriteTimeout are deliberately left UNSET: blob PUT/
		// GET stream arbitrarily large bodies and /api/v1/watch is a
		// long-lived NDJSON stream, so any fixed whole-request deadline
		// would sever legitimate transfers. Body SIZE is bounded per-handler
		// (readJSON's MaxBytesReader) instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errC := make(chan error, 1)
	go func() { errC <- s.httpSrv.ListenAndServeTLS("", "") }()
	// Give the listener a beat to fail fast on bind errors.
	select {
	case err := <-errC:
		return fmt.Errorf("https listener: %w", err)
	case <-time.After(300 * time.Millisecond):
		return nil
	}
}

// pskMiddleware authenticates internal RPC requests (§6.1, §6.2): a valid
// node PSK is always required, and — when the cluster runs the embedded CA
// and internal_client_certs is "require" (the default) — the caller must
// ALSO have presented a client certificate chaining to the cluster CA.
// Two factors: the PSK is a bearer secret, the certificate is proof of
// possession of a cluster-issued identity.
//
// The join handshake is exempt from the certificate requirement: a joining
// node has no cluster certificate yet — that is precisely what /internal/
// join issues it. Join remains gated by the join secret + PSK carried in
// the join token (§16.2).
func (s *Server) pskMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.pskValid(r.Header.Get(dbraft.PSKHeader)) {
			http.Error(w, "invalid node credentials", http.StatusUnauthorized)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/internal/join") {
			if err := s.verifyPeerCert(r); err != nil {
				http.Error(w, "peer certificate required: "+err.Error(), http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// verifyPeerCert checks that the request's TLS client certificate chains
// to the embedded cluster CA. Verification is manual because the listener
// uses tls.RequestClientCert (certificates are optional for the public
// surfaces sharing the port), so the TLS layer never enforces anything.
//
// The check is skipped when there is nothing sound to verify against:
// internal_client_certs is "off" (the upgrade escape hatch for clusters
// whose peers predate cluster-issued certs), or the node runs without the
// embedded CA (static certs / --auto-cert have no per-node identities).
func (s *Server) verifyPeerCert(r *http.Request) error {
	if s.Cfg.InternalClientCerts == "off" || s.caPool == nil {
		return nil
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return fmt.Errorf("no client certificate presented")
	}
	leaf := r.TLS.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range r.TLS.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         s.caPool,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return fmt.Errorf("certificate does not chain to the cluster CA: %w", err)
	}
	return nil
}

// handleInternalPropose executes a routed proposal on this node.
func (s *Server) handleInternalPropose(w http.ResponseWriter, r *http.Request) {
	var req proposeRPC
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h, ok := s.handle(req.GID)
	if !ok {
		http.Error(w, fmt.Sprintf("group %d not hosted here", req.GID), http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	res, err := h.group.Propose(ctx, req.Op)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// handleInternalList executes a routed scan on this node.
func (s *Server) handleInternalList(w http.ResponseWriter, r *http.Request) {
	var req listRPC
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h, ok := s.handle(req.GID)
	if !ok {
		http.Error(w, fmt.Sprintf("group %d not hosted here", req.GID), http.StatusNotFound)
		return
	}
	entries, err := h.sm.List(req.Prefix, req.Cursor, req.Limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// handleInternalGroups starts a raft group instance on this node (shard
// splits use this to spin the new group up on every member).
func (s *Server) handleInternalGroups(w http.ResponseWriter, r *http.Request) {
	var req createGroupRPC
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.startGroup(req.GID, req.Members); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleInternalConfChange runs a membership change on a locally hosted group.
func (s *Server) handleInternalConfChange(w http.ResponseWriter, r *http.Request) {
	var req confChangeRPC
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	f := (*fabric)(s)
	var err error
	if req.Add {
		err = f.AddMember(ctx, req.GID, req.NodeID)
	} else {
		err = f.RemoveMember(ctx, req.GID, req.NodeID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleInternalTransferLead runs a leadership transfer on a locally
// hosted group (the forwarding half of fabric.TransferGroupLeadership).
func (s *Server) handleInternalTransferLead(w http.ResponseWriter, r *http.Request) {
	var req transferLeadRPC
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	h, ok := s.handle(req.GID)
	if !ok {
		http.Error(w, "group not hosted here", http.StatusNotFound)
		return
	}
	if h.group.LeaderID() == req.From {
		h.group.TransferLeadership(req.To)
	}
	w.WriteHeader(http.StatusOK)
}

// handleInternalChunk delegates to the blob engine's chunk endpoints.
func (s *Server) handleInternalChunk(w http.ResponseWriter, r *http.Request) {
	s.Blob.ServePeerChunk(w, r, mux.Vars(r)["hash"])
}

// handleInternalWatch streams a local group's events as NDJSON — the
// server-to-server side of cross-node watch proxying.
func (s *Server) handleInternalWatch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var gid, from uint64
	fmt.Sscanf(q.Get("gid"), "%d", &gid)
	fmt.Sscanf(q.Get("from"), "%d", &from)
	prefix := q.Get("prefix")
	h, ok := s.handle(gid)
	if !ok {
		http.Error(w, "group not hosted here", http.StatusNotFound)
		return
	}
	ch, unsub, err := h.hub.Subscribe(prefix, from)
	if err != nil {
		http.Error(w, err.Error(), http.StatusGone) // RevisionCompacted
		return
	}
	defer unsub()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	for {
		select {
		case ev := <-ch:
			if err := enc.Encode(ev); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

// proxyWatch is the client side of /internal/watch: it forwards a remote
// shard's events into the local fan-in channel.
func (s *Server) proxyWatch(ctx context.Context, gid uint64, prefix string, from uint64, events chan<- kv.Event) error {
	addr, err := s.memberAddr(gid)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://%s/internal/watch?gid=%d&prefix=%s&from=%d", addr, gid, prefix, from)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
	// Watches are long-lived: use a client without the default timeout.
	client := &http.Client{Transport: s.peerClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote watch: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		var ev kv.Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		select {
		case events <- ev:
		case <-ctx.Done():
			return nil
		}
	}
	return sc.Err()
}

// probeRemoteWatch asks a remote shard whether a watch resume from `from`
// is still possible — the pre-flight behind the API's "410 before the
// stream starts" contract (§9.2). It opens the remote stream just long
// enough to read the status line: 410 means the revision was compacted.
func (s *Server) probeRemoteWatch(ctx context.Context, gid uint64, prefix string, from uint64) error {
	addr, err := s.memberAddr(gid)
	if err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := fmt.Sprintf("https://%s/internal/watch?gid=%d&prefix=%s&from=%d", addr, gid, prefix, from)
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(dbraft.PSKHeader, s.primaryPSK())
	resp, err := s.peerClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close() // status is the answer; discard the stream
	if resp.StatusCode == http.StatusGone {
		return kv.ErrCompacted
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote watch preflight: %s", resp.Status)
	}
	return nil
}

// --- join handshake (§16.2) --------------------------------------------------

// joinSecretPrefix stores active join secrets in the metadata keyspace.
const joinSecretPrefix = "jointokens/"

// joinSecret is the stored form of one join secret.
type joinSecret struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// MintJoinToken creates a join secret valid for ttl and encodes the
// one-line token (§16.2). Reusable until expiry, so one token can bring up
// several nodes.
func (s *Server) MintJoinToken(ctx context.Context, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	secret := auth.RandomToken(24)
	raw, _ := json.Marshal(joinSecret{ExpiresAt: time.Now().UTC().Add(ttl)})
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: joinSecretPrefix + secret, Value: raw})
	if err := firstErr(err, res); err != nil {
		return "", err
	}
	fp := ""
	if s.ca != nil {
		fp = s.ca.Fingerprint()
	}
	return certs.JoinToken{
		Endpoint:      s.Cfg.AdvertiseAddr,
		CAFingerprint: fp,
		Secret:        secret,
		PSK:           s.primaryPSK(),
	}.Encode(), nil
}

// handleJoin admits a new node: validates the join secret, allocates a
// node ID, issues a certificate from the embedded CA, and registers the
// node in the metadata. The joiner holds NO metadata — it routes
// metadata lookups to the members (metaproxy.go); the placement loop
// seats it as a voter only if the 1/3/5 target (cluster.MetaVoterTarget)
// has an open seat, and conf-changes it into any data groups needing
// replicas.
func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	var req joinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f := (*fabric)(s)
	rec, ok, err := f.MetaGet(joinSecretPrefix + req.Secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var js joinSecret
	if !ok || json.Unmarshal(rec.Value, &js) != nil || time.Now().After(js.ExpiresAt) {
		http.Error(w, "join token invalid or expired", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	nodeID, err := cluster.NextID(ctx, f, cluster.KeyNextNode, 2)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	resp := joinResponse{
		NodeID:    nodeID,
		ClusterID: s.clusterID,
		PSK:       s.primaryPSK(),
		Peers:     map[uint64]string{},
	}
	// Issue the joiner's certificate from the cluster CA (§6.4).
	if s.ca != nil {
		certPEM, keyPEM, err := s.ca.IssueNode(req.Name, []string{req.Addr, req.Name})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.CA, resp.CertPEM, resp.KeyPEM = s.ca, certPEM, keyPEM
	}
	// Register the node so liveness/placement see it immediately.
	nodeRaw, _ := json.Marshal(cluster.Node{
		ID: nodeID, Name: req.Name, Addr: req.Addr, State: "active",
		// Born live: the joiner pings within one interval, and the grace
		// window protects it if its first pings race the record.
		Live: true, LastSeen: time.Now().UTC(),
	})
	if res, err := f.MetaPropose(ctx, kv.Op{Type: "set", Key: cluster.KeyNodes + cluster.NodeKey(nodeID), Value: nodeRaw}); firstErr(err, res) != nil {
		http.Error(w, "register node failed", http.StatusServiceUnavailable)
		return
	}
	// Tell the joiner where everyone is.
	if nodes, err := cluster.Nodes(f); err == nil {
		for _, n := range nodes {
			if n.State != "removed" {
				resp.Peers[n.ID] = n.Addr
			}
		}
	}
	// Start replicating to the joiner right away.
	s.transport.SetPeer(nodeID, req.Addr)
	s.Logger.Info("node joined", "node", nodeID, "name", req.Name, "addr", req.Addr)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Decommission removes a node (§16.3). The normal path marks the node
// draining and lets the controller migrate its replicas off one guided
// step at a time. force is for dead hardware: it skips the drain entirely,
// conf-changing the node out of every group it is in RIGHT NOW and marking
// it removed — the repair and placement loops re-replicate the lost copies
// afterward.
func (s *Server) Decommission(ctx context.Context, nodeID uint64, actor string, force bool) error {
	f := (*fabric)(s)
	rec, ok, err := f.MetaGet(cluster.KeyNodes + cluster.NodeKey(nodeID))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("node %d not found", nodeID)
	}
	var n cluster.Node
	if err := json.Unmarshal(rec.Value, &n); err != nil {
		return err
	}
	if nodeID == s.nodeID && force {
		return fmt.Errorf("refusing to force-remove the node handling the request")
	}
	if force {
		err := s.forceRemove(ctx, f, n)
		s.audit(ctx, actor, "decommission", fmt.Sprintf("node=%d force=true", nodeID))
		return err
	}
	n.State = "draining"
	raw, _ := json.Marshal(n)
	if res, err := f.MetaPropose(ctx, kv.Op{Type: "set", Key: cluster.KeyNodes + cluster.NodeKey(nodeID), Value: raw}); firstErr(err, res) != nil {
		return fmt.Errorf("mark draining: %w", err)
	}
	idRaw, _ := json.Marshal(nodeID)
	if res, err := f.MetaPropose(ctx, kv.Op{Type: "set", Key: cluster.KeyDecomm + cluster.NodeKey(nodeID), Value: idRaw}); firstErr(err, res) != nil {
		return err
	}
	s.audit(ctx, actor, "decommission", fmt.Sprintf("node=%d force=false", nodeID))
	return nil
}

// forceRemove is the dead-hardware path: no drain, no safety gates — the
// operator asserted the node is gone. Remove it from every group and mark
// it removed. Per-group failures (e.g. a group that lost quorum with this
// node's death) are collected, not fatal: the rest of the cleanup still
// proceeds and the reported error tells the operator which groups need
// attention.
func (s *Server) forceRemove(ctx context.Context, f *fabric, n cluster.Node) error {
	groups, err := cluster.Groups(f)
	if err != nil {
		return err
	}
	var failures []string
	for _, g := range groups {
		member := false
		for _, m := range g.Members {
			if m == n.ID {
				member = true
				break
			}
		}
		if !member {
			continue
		}
		if err := f.RemoveMember(ctx, g.GID, n.ID); err != nil {
			failures = append(failures, fmt.Sprintf("group %d: %v", g.GID, err))
			continue
		}
		kept := make([]uint64, 0, len(g.Members))
		for _, m := range g.Members {
			if m != n.ID {
				kept = append(kept, m)
			}
		}
		g.Members = kept
		raw, _ := json.Marshal(g)
		if res, err := f.MetaPropose(ctx, kv.Op{Type: "set", Key: cluster.KeyGroups + fmt.Sprintf("%d", g.GID), Value: raw}); firstErr(err, res) != nil {
			failures = append(failures, fmt.Sprintf("group %d record: %v", g.GID, err))
		}
		s.Logger.Info("force-remove: node conf-changed out of group", "node", n.ID, "gid", g.GID)
	}
	// Mark the node removed and clear any pending drain marker so the
	// decommission reconciler stops considering it.
	n.State = "removed"
	raw, _ := json.Marshal(n)
	if res, err := f.MetaPropose(ctx, kv.Op{Type: "set", Key: cluster.KeyNodes + cluster.NodeKey(n.ID), Value: raw}); firstErr(err, res) != nil {
		return fmt.Errorf("mark removed: %w", err)
	}
	if res, err := f.MetaPropose(ctx, kv.Op{Type: "delete", Key: cluster.KeyDecomm + cluster.NodeKey(n.ID)}); firstErr(err, res) != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("node %d marked removed, but some groups could not drop it (repair may be needed): %s",
			n.ID, strings.Join(failures, "; "))
	}
	return nil
}

// StatusReport is the assembled cluster-status view for the CLI and GUI
// (§16.3): topology, shards, alerts, and the per-node safety verdicts.
type StatusReport struct {
	ClusterID string              `json:"cluster_id"`
	Nodes     []NodeStatus        `json:"nodes"`
	Shards    []cluster.Shard     `json:"shards"`
	Groups    []cluster.GroupInfo `json:"groups"`
	Alerts    []cluster.Alert     `json:"alerts"`
	// SafeToProceed is the headline answer to "may I take another node
	// down?": true only when no alert is critical.
	SafeToProceed bool `json:"safe_to_proceed"`
	// Paused reports the §16.4 admin pause flags per subsystem
	// (rebalance/split/repair) so status always shows suspended automation.
	Paused map[string]bool `json:"paused"`
}

// NodeStatus decorates a node record with liveness and removal safety.
type NodeStatus struct {
	cluster.Node
	Healthy bool `json:"healthy"`
	// SafeToRemove: removing this node right now would leave every one
	// of its groups with a healthy majority.
	SafeToRemove bool `json:"safe_to_remove"`
}

// --- admin pause/resume (§16.4) ----------------------------------------------

// AdminPause sets or clears one automation pause flag (rebalance | split |
// repair). The flag is a metadata key, so every node — and specifically the
// controller on the metadata leader and the repair loop — observes it
// within one replication round-trip. Audited: pausing automation is an
// operational act someone must be able to answer for.
func (s *Server) AdminPause(ctx context.Context, actor, target string, paused bool) error {
	valid := false
	for _, t := range cluster.PauseTargets {
		if t == target {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("unknown pause target %q (valid: %s)", target, strings.Join(cluster.PauseTargets, ", "))
	}
	raw, _ := json.Marshal(cluster.PauseFlag{Paused: paused, Actor: actor, Since: time.Now().UTC()})
	res, err := (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: cluster.KeyAdminPause + target, Value: raw})
	if err := firstErr(err, res); err != nil {
		return err
	}
	action := "admin-pause"
	if !paused {
		action = "admin-resume"
	}
	s.audit(ctx, actor, action, "target="+target)
	return nil
}

// AdminPauseFlags reports the current pause state of every §16.4 target.
func (s *Server) AdminPauseFlags() map[string]bool {
	out := map[string]bool{}
	for _, t := range cluster.PauseTargets {
		out[t] = cluster.Paused((*fabric)(s), t)
	}
	return out
}

// RepairPaused exposes the blob-repair pause flag to the repair loop
// (pkg/server/repair.go checks it at the top of each pass).
func (s *Server) RepairPaused() bool { return cluster.Paused((*fabric)(s), "repair") }

// Status assembles the report from metadata state.
func (s *Server) Status() (*StatusReport, error) {
	f := (*fabric)(s)
	nodes, err := cluster.Nodes(f)
	if err != nil {
		return nil, err
	}
	groups, err := cluster.Groups(f)
	if err != nil {
		return nil, err
	}
	shards, err := cluster.Shards(f)
	if err != nil {
		return nil, err
	}
	var alerts []cluster.Alert
	if entries, err := f.MetaList(cluster.KeyAlerts, 1000); err == nil {
		for _, e := range entries {
			var a cluster.Alert
			if json.Unmarshal(e.Record.Value, &a) == nil {
				alerts = append(alerts, a)
			}
		}
	}
	healthy := map[uint64]bool{}
	for _, n := range nodes {
		healthy[n.ID] = n.State == "active" && n.Live
	}
	report := &StatusReport{ClusterID: s.clusterID, Shards: shards, Groups: groups, Alerts: alerts,
		SafeToProceed: true, Paused: s.AdminPauseFlags()}
	for _, a := range alerts {
		if a.Severity == "critical" {
			report.SafeToProceed = false
		}
	}
	for _, n := range nodes {
		ns := NodeStatus{Node: n, Healthy: healthy[n.ID], SafeToRemove: true}
		for _, g := range groups {
			member := false
			alive := 0
			for _, m := range g.Members {
				if m == n.ID {
					member = true
				}
				if healthy[m] {
					alive++
				}
			}
			if !member {
				continue
			}
			// Would the group keep a healthy majority without this node?
			remainingAlive := alive
			if healthy[n.ID] {
				remainingAlive--
			}
			if remainingAlive < len(g.Members)/2+1 {
				ns.SafeToRemove = false
			}
		}
		report.Nodes = append(report.Nodes, ns)
	}
	return report, nil
}
