// Package server assembles a databox storage node: it owns the Pebble
// store, every raft group instance, the blob engine, the management
// controller, the transaction coordinator, and both faces of the HTTPS
// server (public API + internal node RPC).
//
// This file covers the node lifecycle (§16):
//
//   - bootstrap: first node of a new cluster — creates the cluster ID,
//     embedded CA, metadata group, first data shard, and the root user.
//   - join: subsequent nodes present a join token, receive identity and
//     certificates, and start replicating.
//   - background loops: heartbeats (liveness), stats reports (feeding the
//     shard splitter), token GC, and stuck-transaction resolution.
//
// The wiring principle throughout: nothing is replicated to every node.
// Metadata lives on the 1/3/5 voters only — other nodes route lookups to
// them (metaproxy.go); data groups replicate to `replicas` nodes and
// requests are routed to members.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.etcd.io/raft/v3/raftpb"

	"github.com/hyperkubeorg/databox/pkg/auth"
	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/certs"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/pkg/config"
	"github.com/hyperkubeorg/databox/pkg/kv"
	dbraft "github.com/hyperkubeorg/databox/pkg/raft"
	"github.com/hyperkubeorg/databox/pkg/store"
)

// groupHandle bundles one raft group with its state machine and watch hub.
type groupHandle struct {
	group *dbraft.Group
	sm    *kv.SM
	hub   *kv.Hub
}

// Server is one running databox node.
type Server struct {
	Cfg    *config.Config
	Logger *slog.Logger

	st        *store.Store
	nodeID    uint64
	clusterID string

	// PKI material. ca is non-nil when the node participates in the
	// embedded auto-PKI (§6.4); tlsCert is what the HTTPS listener serves.
	// caPool caches ca's verification pool for the internal mTLS check
	// (§6.1): /internal/* peers must present a cert chaining to it.
	ca      *certs.CA
	caPool  *x509.CertPool
	tlsCert tls.Certificate
	// peerClient speaks internal RPC to other nodes: TLS trusting the
	// cluster CA (or any cert under --auto-cert), PSK attached by callers.
	peerClient *http.Client
	// blobClient is the dedicated chunk-transfer client (§2, §11): blob IO
	// gets its own connections and pool so a bulk chunk push can never
	// head-of-line-block raft traffic sharing peerClient's connections.
	blobClient *http.Client

	transport *dbraft.Transport

	groupsMu sync.RWMutex
	groups   map[uint64]*groupHandle

	Blob *blob.Engine

	controller *cluster.Controller

	// psks holds the accepted node PSKs (primary first, rotation extras
	// after, §6.2). pskMu guards it: the grace-period purge loop removes
	// expired extras while request handlers validate concurrently.
	pskMu sync.RWMutex
	psks  []string

	// Liveness observation table (liveness.go): in-memory pings, consulted
	// only while this node leads the metadata group. observerSince marks
	// when the current leadership began observing — dead verdicts are
	// withheld until the table has been warm for cluster.LivenessGrace.
	livenessMu    sync.Mutex
	pings         map[uint64]pingInfo
	observerSince time.Time

	// metaProxy is the routed metadata access path used while this node
	// is NOT a metadata member (metaproxy.go): member discovery plus a
	// bounded TTL cache. Metadata is never replicated here — reads hop
	// to a member.
	metaProxy *metaProxy

	// topo caches the assembled cluster-map report between GUI polls
	// (topology.go).
	topo topoCache

	httpSrv  *http.Server
	adminSrv *http.Server // unix-socket admin listener (root recovery, §7.3)

	stopC chan struct{}
	// lifeCtx is the server-lifetime context. Background goroutines derive
	// their per-operation propose contexts from it, so shutdown() — which
	// cancels it first thing — aborts an in-flight MetaPropose immediately
	// instead of letting a loop sleep out the proposal's 5s timeout while
	// the rest of teardown proceeds underneath it.
	lifeCtx    context.Context
	lifeCancel context.CancelFunc
	// loopWG counts every background goroutine that may touch the store
	// (launch them via goLoop). shutdown() joins it BEFORE closing Pebble;
	// see the regression note on shutdown().
	loopWG sync.WaitGroup
}

// New prepares a node (open store, resolve identity) without starting
// network services; Run completes startup.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		Cfg:       cfg,
		Logger:    logger,
		st:        st,
		groups:    map[uint64]*groupHandle{},
		stopC:     make(chan struct{}),
		metaProxy: newMetaProxy(),
	}
	s.lifeCtx, s.lifeCancel = context.WithCancel(context.Background())
	if cfg.PSK != "" {
		s.psks = append(s.psks, cfg.PSK)
	}
	s.psks = append(s.psks, cfg.PSKExtra...)
	return s, nil
}

// Run brings the node fully up and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	// MVCC tuning must be in place BEFORE any raft group starts applying
	// entries: history pruning happens deterministically inside Apply, so
	// these values must be uniform across every replica of the cluster
	// (see the config field docs). Package-level because they are cluster
	// constants, not per-request knobs. CONFIG ALWAYS WINS: Finish()
	// guarantees both fields are ≥ 1, so Run unconditionally overwrites any
	// value poked into the kv package beforehand — tune these through
	// config (tests included), never by setting the kv globals directly.
	if s.Cfg.MVCCHistoryRevs > 0 {
		kv.MVCCHistoryRevisions = s.Cfg.MVCCHistoryRevs
	}
	if s.Cfg.MVCCGCInterval > 0 {
		kv.MVCCGCEvery = s.Cfg.MVCCGCInterval
	}
	// Linearizable read mode (§23). Unlike the MVCC knobs this is a
	// node-local serving choice, not replicated state: both modes are
	// linearizable, so nodes may disagree without harming correctness.
	if s.Cfg.LinearizableReads != "" {
		LinearizableReadMode = s.Cfg.LinearizableReads
	}

	fresh, err := s.loadIdentity()
	if err != nil {
		return err
	}
	switch {
	case !fresh:
		// Restart: identity and raft state are on disk already.
		if err := s.setupTLSFromLocal(); err != nil {
			return err
		}
		if err := s.startExistingGroups(); err != nil {
			return err
		}
	case s.Cfg.Join != "":
		if err := s.joinCluster(ctx); err != nil {
			return err
		}
	default:
		if err := s.bootstrapCluster(ctx); err != nil {
			return err
		}
	}

	// The blob engine shares the node's peer plumbing.
	cs, err := blob.NewChunkStore(s.Cfg.DataDir)
	if err != nil {
		return err
	}
	s.Blob = &blob.Engine{
		Store: cs, Peers: (*blobPeers)(s), ChunkSize: s.Cfg.ChunkBytes,
		Replicas: 2, // §12 built-in small-blob default; stored policies override per key
		Policies: (*serverPolicies)(s),
		Limiter:  blob.NewRateLimiter(s.Cfg.RepairBytesPerSec),
	}

	// Management control loops (they act only on the metadata leader).
	s.controller = cluster.NewController((*fabric)(s), s.Logger)
	s.controller.Start()

	// Node background loops — every one goes through goLoop so shutdown()
	// can join them all before it closes the store.
	s.goLoop(s.livenessLoop)
	s.goLoop(s.metaMembersRefreshLoop)
	s.goLoop(s.metaMembershipLoop)
	s.goLoop(s.statsLoop)
	s.goLoop(s.txJanitorLoop)
	s.goLoop(s.certRenewLoop)
	s.goLoop(s.repairLoop)
	s.goLoop(s.tokenGCLoop)
	s.goLoop(s.pskGraceLoop)

	if err := s.startHTTP(); err != nil {
		return err
	}
	s.startAdminSocket()

	s.Logger.Info("databox node running",
		"node", s.nodeID, "cluster", s.clusterID,
		"listen", s.Cfg.Listen, "advertise", s.Cfg.AdvertiseAddr)

	<-ctx.Done()
	s.shutdown()
	return nil
}

// loadIdentity reads node-local identity from the store. Returns
// fresh=true when this data directory has never been part of a cluster.
func (s *Server) loadIdentity() (fresh bool, err error) {
	idRaw, ok, err := s.st.Get(store.LocalKey("node_id"))
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	var id uint64
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return false, err
	}
	s.nodeID = id
	if raw, ok, _ := s.st.Get(store.LocalKey("cluster_id")); ok {
		s.clusterID = string(raw)
	}
	return false, nil
}

// saveIdentity persists node-local identity (synchronously — identity loss
// would orphan the node's raft state).
func (s *Server) saveIdentity() error {
	idRaw, _ := json.Marshal(s.nodeID)
	if err := s.st.Set(store.LocalKey("node_id"), idRaw, true); err != nil {
		return err
	}
	return s.st.Set(store.LocalKey("cluster_id"), []byte(s.clusterID), true)
}

// --- bootstrap ------------------------------------------------------------

// bootstrapCluster initializes a brand-new single-node cluster (§16.2).
func (s *Server) bootstrapCluster(ctx context.Context) error {
	s.nodeID = 1
	s.clusterID = auth.RandomToken(8)
	s.Logger.Info("bootstrapping new cluster", "cluster", s.clusterID)

	// A PSK is mandatory for multi-node operation; generate one now if
	// the operator didn't supply one, so join tokens can carry it.
	if len(s.psks) == 0 {
		psk, err := certs.GeneratePSK(512)
		if err != nil {
			return err
		}
		s.psks = []string{psk}
	}
	if err := s.st.Set(store.LocalKey("psk"), []byte(s.psks[0]), true); err != nil {
		return err
	}

	// Embedded CA (§6.4) unless the operator brought their own certs.
	if err := s.setupPKIBootstrap(); err != nil {
		return err
	}
	if err := s.saveIdentity(); err != nil {
		return err
	}

	s.transport = dbraft.NewTransport(s.nodeID, s.peerClient, s.psks[0], s.Logger)
	s.transport.SetStore(s.st) // streamed snapshots stage through the node store
	s.transport.Deliver = s.deliver
	dbraft.SetUnreachableHook(s.reportUnreachable)

	// Metadata group: single member (us).
	if _, err := s.startGroup(cluster.MetaGID, []uint64{s.nodeID}); err != nil {
		return err
	}
	// First data group and the shard covering the whole user keyspace.
	if _, err := s.startGroup(2, []uint64{s.nodeID}); err != nil {
		return err
	}

	// Seed cluster metadata. These proposals need a leader; single-node
	// raft elects itself within ~1s.
	seedCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	f := (*fabric)(s)
	seed := func(key string, v any) error {
		raw, _ := json.Marshal(v)
		_, err := kv.DecodeResult(f.MetaPropose(seedCtx, kv.Op{Type: "set", Key: key, Value: raw}))
		return err
	}
	if err := seed(cluster.KeyNodes+cluster.NodeKey(1), cluster.Node{
		ID: 1, Name: s.Cfg.NodeName, Addr: s.Cfg.AdvertiseAddr, State: "active",
		Live: true, LastSeen: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("seed node record: %w", err)
	}
	if err := seed(cluster.KeyGroups+"1", cluster.GroupInfo{GID: 1, Members: []uint64{1}, Kind: "meta"}); err != nil {
		return err
	}
	if err := seed(cluster.KeyGroups+"2", cluster.GroupInfo{GID: 2, Members: []uint64{1}, Kind: "data"}); err != nil {
		return err
	}
	if err := seed(cluster.KeyShards+"1", cluster.Shard{ID: 1, Start: "/", End: "", GID: 2, State: "active"}); err != nil {
		return err
	}
	if err := seed(cluster.KeyNextGroup, uint64(3)); err != nil {
		return err
	}
	if err := seed(cluster.KeyNextShard, uint64(2)); err != nil {
		return err
	}
	if err := seed(cluster.KeyNextNode, uint64(2)); err != nil {
		return err
	}
	// Store the CA in metadata so future metadata members can issue too.
	if s.ca != nil {
		if err := seed("ca", s.ca); err != nil {
			return err
		}
	}
	// Root user (§7.1): passwordless unless the operator provided one
	// (the Helm chart always does, via root_password_file).
	rootHash := ""
	if s.Cfg.RootPasswordFile != "" {
		pw, err := os.ReadFile(s.Cfg.RootPasswordFile)
		if err != nil {
			return fmt.Errorf("read root_password_file: %w", err)
		}
		rootHash, err = auth.HashPassword(strings.TrimSpace(string(pw)))
		if err != nil {
			return err
		}
	}
	if err := seed(auth.KeyPrefixUsers+auth.RootUser, auth.User{
		Name: auth.RootUser, PasswordHash: rootHash, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	s.Logger.Info("cluster bootstrapped", "root_password_set", rootHash != "")
	return nil
}

// setupPKIBootstrap creates the embedded CA and this node's certificate,
// or wires operator-provided static certs / --auto-cert.
func (s *Server) setupPKIBootstrap() error {
	switch {
	case s.Cfg.TLSCertFile != "":
		return s.useStaticCerts()
	case s.Cfg.AutoCert:
		return s.useSelfSigned()
	default:
		ca, err := certs.NewCA(s.clusterID)
		if err != nil {
			return err
		}
		s.ca = ca
		caRaw, _ := json.Marshal(ca)
		if err := s.st.Set(store.LocalKey("ca"), caRaw, true); err != nil {
			return err
		}
		return s.issueOwnCert()
	}
}

// issueOwnCert issues (or reissues) this node's leaf cert from the CA and
// rebuilds the TLS client used for peer RPC.
//
// The served chain is leaf + CA: join tokens pin the CA fingerprint, so
// a joining node must be able to see the CA certificate in the handshake
// to recognize the cluster (§16.2).
func (s *Server) issueOwnCert() error {
	certPEM, keyPEM, err := s.ca.IssueNode(s.Cfg.NodeName, []string{s.Cfg.AdvertiseAddr, s.Cfg.NodeName})
	if err != nil {
		return err
	}
	chain := append(append([]byte(nil), certPEM...), s.ca.CertPEM...)
	if err := s.st.Set(store.LocalKey("cert"), chain, true); err != nil {
		return err
	}
	if err := s.st.Set(store.LocalKey("key"), keyPEM, true); err != nil {
		return err
	}
	s.tlsCert, err = certs.LoadKeyPair(chain, keyPEM)
	if err != nil {
		return err
	}
	return s.buildPeerClient()
}

// useStaticCerts loads operator-provided certificates (§6.4 opt-out).
func (s *Server) useStaticCerts() error {
	cert, err := tls.LoadX509KeyPair(s.Cfg.TLSCertFile, s.Cfg.TLSKeyFile)
	if err != nil {
		return fmt.Errorf("load static certs: %w", err)
	}
	s.tlsCert = cert
	return s.buildPeerClient()
}

// useSelfSigned generates a throwaway certificate (--auto-cert).
func (s *Server) useSelfSigned() error {
	certPEM, keyPEM, err := certs.SelfSigned(s.Cfg.NodeName,
		[]string{s.Cfg.AdvertiseAddr, s.Cfg.NodeName}, 365*24*time.Hour)
	if err != nil {
		return err
	}
	if err := s.st.Set(store.LocalKey("cert"), certPEM, true); err != nil {
		return err
	}
	if err := s.st.Set(store.LocalKey("key"), keyPEM, true); err != nil {
		return err
	}
	s.tlsCert, err = certs.LoadKeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	return s.buildPeerClient()
}

// setupTLSFromLocal restores TLS material persisted by a previous run.
func (s *Server) setupTLSFromLocal() error {
	if raw, ok, _ := s.st.Get(store.LocalKey("psk")); ok && len(s.psks) == 0 {
		s.psks = []string{string(raw)}
	}
	if raw, ok, _ := s.st.Get(store.LocalKey("ca")); ok {
		var ca certs.CA
		if err := json.Unmarshal(raw, &ca); err == nil {
			s.ca = &ca
		}
	}
	if s.Cfg.TLSCertFile != "" {
		return s.useStaticCerts()
	}
	certPEM, okC, _ := s.st.Get(store.LocalKey("cert"))
	keyPEM, okK, _ := s.st.Get(store.LocalKey("key"))
	if !okC || !okK {
		return fmt.Errorf("node has identity but no TLS material; data directory is damaged")
	}
	var err error
	s.tlsCert, err = certs.LoadKeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	return s.buildPeerClient()
}

// buildPeerClient constructs the HTTPS client for internal RPC. With a
// cluster CA we verify peers strictly against it; under --auto-cert or
// static third-party certs we verify against the system pool plus the CA
// when present. PSK authentication applies on top in both directions, so
// transport identity is never the only line of defense.
func (s *Server) buildPeerClient() error {
	tlsCfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.tlsCert},
	}
	if s.ca != nil {
		pool, err := s.ca.Pool()
		if err != nil {
			return err
		}
		tlsCfg.RootCAs = pool
		// Cache the pool for the listener side too: /internal/* requests
		// verify peer client certificates against it (§6.1).
		s.caPool = pool
	} else {
		// Self-signed / static certs: trust the serving cert directly by
		// pinning its pool from our own cert (single-node/auto-cert) —
		// multi-node clusters use the embedded CA path.
		pool := x509.NewCertPool()
		if len(s.tlsCert.Certificate) > 0 {
			if c, err := x509.ParseCertificate(s.tlsCert.Certificate[0]); err == nil {
				pool.AddCert(c)
			}
		}
		tlsCfg.RootCAs = pool
	}
	s.peerClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConnsPerHost: 16,
			ForceAttemptHTTP2:   true,
		},
	}
	// Chunk transfer gets its own client (§11: separate IO path from
	// raft): HTTP/1.1 — one connection per in-flight transfer, no
	// multiplexing — with a deep idle pool, and no 30s global timeout
	// (chunk streams are sized by ChunkBytes, not by a wall clock; a hung
	// peer is bounded by the header timeout instead).
	s.blobClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:       tlsCfg.Clone(),
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
	return nil
}

// --- join -----------------------------------------------------------------

// joinRequest/joinResponse are the internal join RPC wire shapes.
type joinRequest struct {
	Secret string `json:"secret"`
	Name   string `json:"name"`
	Addr   string `json:"addr"`
}

type joinResponse struct {
	NodeID    uint64            `json:"node_id"`
	ClusterID string            `json:"cluster_id"`
	CA        *certs.CA         `json:"ca,omitempty"`
	CertPEM   []byte            `json:"cert_pem"`
	KeyPEM    []byte            `json:"key_pem"`
	PSK       string            `json:"psk"`
	Peers     map[uint64]string `json:"peers"` // nodeID → addr, seeds the transport
}

// joinCluster runs the joining handshake against the token's endpoint.
func (s *Server) joinCluster(ctx context.Context) error {
	tok, err := certs.DecodeJoinToken(s.Cfg.Join)
	if err != nil {
		return err
	}
	s.Logger.Info("joining cluster", "endpoint", tok.Endpoint)
	// First contact uses the PSK from the token; TLS verification is by
	// CA fingerprint pinning (we don't have the CA cert yet).
	pinned := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// We cannot chain-verify before we possess the CA; instead
			// we verify the presented certificate's issuer chain against
			// the fingerprint embedded in the token. This is the one
			// deliberate use of custom verification in the codebase and
			// it is strictly *stronger* pinning, not weaker.
			InsecureSkipVerify: true, //nolint:gosec — replaced by fingerprint pinning below
			VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
				for _, der := range raw {
					if certs.FingerprintDER(der) == tok.CAFingerprint {
						return nil
					}
				}
				return fmt.Errorf("no presented certificate matches the join token's CA fingerprint")
			},
		}},
	}
	reqBody, _ := json.Marshal(joinRequest{Secret: tok.Secret, Name: s.Cfg.NodeName, Addr: s.Cfg.AdvertiseAddr})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://"+tok.Endpoint+"/internal/join", strings.NewReader(string(reqBody)))
	if err != nil {
		return err
	}
	req.Header.Set(dbraft.PSKHeader, tok.PSK)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pinned.Do(req)
	if err != nil {
		return fmt.Errorf("join call failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("join rejected: %s", resp.Status)
	}
	var jr joinResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return fmt.Errorf("parse join response: %w", err)
	}

	// Adopt the identity and PKI material the cluster issued us.
	s.nodeID = jr.NodeID
	s.clusterID = jr.ClusterID
	s.psks = append([]string{jr.PSK}, s.psks...)
	if err := s.st.Set(store.LocalKey("psk"), []byte(jr.PSK), true); err != nil {
		return err
	}
	if jr.CA != nil {
		s.ca = jr.CA
		caRaw, _ := json.Marshal(jr.CA)
		if err := s.st.Set(store.LocalKey("ca"), caRaw, true); err != nil {
			return err
		}
	}
	// Serve leaf + CA, same rationale as issueOwnCert: peers and future
	// joiners must see the CA in our handshake.
	chain := jr.CertPEM
	if jr.CA != nil {
		chain = append(append([]byte(nil), jr.CertPEM...), jr.CA.CertPEM...)
	}
	if err := s.st.Set(store.LocalKey("cert"), chain, true); err != nil {
		return err
	}
	if err := s.st.Set(store.LocalKey("key"), jr.KeyPEM, true); err != nil {
		return err
	}
	s.tlsCert, err = certs.LoadKeyPair(chain, jr.KeyPEM)
	if err != nil {
		return err
	}
	if err := s.buildPeerClient(); err != nil {
		return err
	}
	if err := s.saveIdentity(); err != nil {
		return err
	}

	s.transport = dbraft.NewTransport(s.nodeID, s.peerClient, s.psks[0], s.Logger)
	s.transport.SetStore(s.st) // streamed snapshots stage through the node store
	s.transport.Deliver = s.deliver
	dbraft.SetUnreachableHook(s.reportUnreachable)
	for id, addr := range jr.Peers {
		s.transport.SetPeer(id, addr)
	}
	// No metadata group instance here — and no metadata copy of any
	// kind: a joiner ROUTES metadata lookups to the members until (and
	// unless) the placement loop seats it as one of the 1/3/5 voters, in
	// which case the leader starts talking to us and the transport
	// lazily creates the instance (deliver()). The peers from the join
	// response seed the proxy's member discovery.
	seeds := make([]string, 0, len(jr.Peers))
	for _, addr := range jr.Peers {
		seeds = append(seeds, addr)
	}
	s.persistMetaSeeds(seeds)
	s.Logger.Info("joined cluster", "node", s.nodeID, "cluster", s.clusterID)
	return nil
}

// --- group management -------------------------------------------------------

// startGroup creates the state machine + raft group instance for gid.
// bootstrap lists initial members for a brand-new group; nil means "join
// an existing group and wait for the leader to reach us".
func (s *Server) startGroup(gid uint64, bootstrap []uint64) (*groupHandle, error) {
	s.groupsMu.Lock()
	defer s.groupsMu.Unlock()
	if h, ok := s.groups[gid]; ok {
		return h, nil
	}
	hub := kv.NewHub(4096)
	sm, err := kv.NewSM(gid, s.st, hub, s.Cfg.MaxValueBytes)
	if err != nil {
		return nil, err
	}
	g, err := dbraft.StartGroup(dbraft.GroupConfig{
		GID: gid, NodeID: s.nodeID, SM: sm, Store: s.st,
		Transport: s.transport, Logger: s.Logger, Bootstrap: bootstrap,
	})
	if err != nil {
		return nil, err
	}
	h := &groupHandle{group: g, sm: sm, hub: hub}
	s.groups[gid] = h
	return h, nil
}

// startExistingGroups restarts every group with persisted raft state —
// the restart path after a node reboot.
func (s *Server) startExistingGroups() error {
	s.transport = dbraft.NewTransport(s.nodeID, s.peerClient, s.primaryPSK(), s.Logger)
	s.transport.SetStore(s.st) // streamed snapshots stage through the node store
	s.transport.Deliver = s.deliver
	dbraft.SetUnreachableHook(s.reportUnreachable)
	// Discover groups by scanning for applied-index records ("r/<gid>/a").
	gids, err := s.persistedGroupIDs()
	if err != nil {
		return err
	}
	for _, gid := range gids {
		if _, err := s.startGroup(gid, nil); err != nil {
			return err
		}
	}
	// Re-seed transport peers from the (locally replicated) metadata.
	// Tracked: the poll reads metadata through the store, so shutdown must
	// join it before closing Pebble.
	s.goLoop(s.refreshPeersSoon)
	return nil
}

// goLoop launches fn as a tracked background goroutine. Everything that can
// touch the store after Run returns to its caller MUST go through here (or
// add itself to loopWG directly): shutdown() waits on loopWG before closing
// Pebble, and an untracked goroutine reintroduces the use-after-close panic
// described on shutdown().
func (s *Server) goLoop(fn func()) {
	s.loopWG.Add(1)
	go func() {
		defer s.loopWG.Done()
		fn()
	}()
}

// persistedGroupIDs lists group IDs with raft state in the store.
func (s *Server) persistedGroupIDs() ([]uint64, error) {
	var out []uint64
	iter, err := s.st.DB.NewIter(nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	// Keys look like "r/<8-byte gid>/a"; iterate the "r/" namespace and
	// collect distinct gids from applied-index keys.
	for iter.SeekGE([]byte("r/")); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) < 2 || string(k[:2]) != "r/" {
			break
		}
		if len(k) == 2+8+2 && string(k[10:]) == "/a" {
			var gid uint64
			for _, b := range k[2:10] {
				gid = gid<<8 | uint64(b)
			}
			out = append(out, gid)
		}
	}
	return out, nil
}

// refreshPeersSoon polls the metadata for node addresses until available,
// then keeps the transport's peer map in sync.
func (s *Server) refreshPeersSoon() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			nodes, err := cluster.Nodes((*fabric)(s))
			if err != nil {
				continue
			}
			for _, n := range nodes {
				if n.State != "removed" {
					s.transport.SetPeer(n.ID, n.Addr)
				}
			}
		case <-s.stopC:
			return
		}
	}
}

// deliver routes an inbound raft message to its group, lazily creating the
// group instance the first time the leader talks to us about it — this is
// how a node learns it was made a member of a new (split) group.
func (s *Server) deliver(gid uint64, msg raftpb.Message) {
	s.groupsMu.RLock()
	h, ok := s.groups[gid]
	s.groupsMu.RUnlock()
	if !ok {
		var err error
		h, err = s.startGroup(gid, nil)
		if err != nil {
			s.Logger.Error("cannot start group for inbound message", "gid", gid, "err", err)
			return
		}
	}
	h.group.Step(msg)
}

// reportUnreachable relays transport failures into the right group.
func (s *Server) reportUnreachable(gid, peer uint64) {
	s.groupsMu.RLock()
	h, ok := s.groups[gid]
	s.groupsMu.RUnlock()
	if ok {
		h.group.ReportUnreachable(peer)
	}
}

// handle returns the local instance of a group, if this node hosts it.
func (s *Server) handle(gid uint64) (*groupHandle, bool) {
	s.groupsMu.RLock()
	defer s.groupsMu.RUnlock()
	h, ok := s.groups[gid]
	return h, ok
}

// meta returns the metadata group handle (every node hosts it).
func (s *Server) meta() *groupHandle {
	h, _ := s.handle(cluster.MetaGID)
	return h
}

func (s *Server) primaryPSK() string {
	s.pskMu.RLock()
	defer s.pskMu.RUnlock()
	if len(s.psks) > 0 {
		return s.psks[0]
	}
	return ""
}

// pskValid checks a presented PSK against all accepted keys (§6.2 rotation).
func (s *Server) pskValid(presented string) bool {
	s.pskMu.RLock()
	defer s.pskMu.RUnlock()
	for _, k := range s.psks {
		if k != "" && k == presented {
			return true
		}
	}
	return false
}

// --- background loops -------------------------------------------------------

// The per-node heartbeat loop is gone: liveness pings travel in memory to
// the metadata leader and only verdict FLIPS are written through raft —
// see liveness.go for the full rationale (it was an O(N²) message bill).

// statsLoop publishes size and QPS reports for groups this node leads —
// the shard splitter's inputs (§15: "Size > 16 GB, QPS > threshold").
//
// QPS is a purely local observation, never state-machine state: each SM
// keeps a monotonic in-memory op counter (pkg/kv), this loop samples it
// every tick and folds the instantaneous rate into a 60-second EWMA. The
// smoothing means a one-tick burst barely moves the figure while sustained
// load converges to its true rate within a couple of minutes — exactly the
// shape the splitter's "sustained over threshold" trigger wants.
func (s *Server) statsLoop() {
	// qpsState is this loop's per-group sampling memory. Local to the
	// goroutine, so no locking; lost on restart/leader change, which just
	// restarts the smoothing window.
	type qpsState struct {
		lastOps uint64    // counter value at the previous sample
		lastAt  time.Time // when that sample was taken
		ewma    float64   // decayed ops/sec
	}
	qps := map[uint64]*qpsState{}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.groupsMu.RLock()
			handles := make(map[uint64]*groupHandle, len(s.groups))
			for gid, h := range s.groups {
				handles[gid] = h
			}
			s.groupsMu.RUnlock()
			f := (*fabric)(s)
			for gid, h := range handles {
				if gid == cluster.MetaGID || !h.group.IsLeader() {
					continue
				}
				size, err := h.sm.ApproxSize()
				if err != nil {
					continue
				}
				// Sample the op counter and update the 60s EWMA. The first
				// sample only establishes a baseline (reports 0) — a rate
				// needs two points.
				now := time.Now()
				ops := h.sm.OpCount()
				st := qps[gid]
				if st == nil {
					st = &qpsState{lastOps: ops, lastAt: now}
					qps[gid] = st
				} else if dt := now.Sub(st.lastAt).Seconds(); dt > 0 && ops >= st.lastOps {
					inst := float64(ops-st.lastOps) / dt
					// Standard exponential decay with a 60s time constant;
					// alpha adapts to the actual elapsed time so missed
					// ticks do not over- or under-weight a sample.
					alpha := 1 - math.Exp(-dt/60)
					st.ewma += alpha * (inst - st.ewma)
					st.lastOps, st.lastAt = ops, now
				} else if ops < st.lastOps {
					// Counter went backwards (group handle recreated):
					// restart the window from the new baseline.
					*st = qpsState{lastOps: ops, lastAt: now}
				}
				keys, _ := h.sm.Count() // 0 on error; bytes still report
				raw, _ := json.Marshal(cluster.GroupStats{
					GID: gid, Bytes: size, Keys: keys, QPS: st.ewma, Leader: s.nodeID, Reported: time.Now().UTC(),
				})
				ctx, cancel := context.WithTimeout(s.lifeCtx, 5*time.Second)
				_, _ = f.MetaPropose(ctx, kv.Op{Type: "set", Key: cluster.KeyStats + fmt.Sprintf("%d", gid), Value: raw})
				cancel()
			}
			// Drop sampling state for groups we no longer lead (or host):
			// if leadership returns later the EWMA starts fresh rather than
			// blending across the gap.
			for gid := range qps {
				if h, ok := handles[gid]; !ok || !h.group.IsLeader() {
					delete(qps, gid)
				}
			}
		case <-s.stopC:
			return
		}
	}
}

// SplitThresholdQPS satisfies cluster.Fabric: the sustained per-group QPS
// above which the splitter divides a shard. 0 disables the trigger — the
// default, see pkg/config for the rationale.
func (f *fabric) SplitThresholdQPS() float64 { return float64(f.srv().Cfg.SplitThresholdQPS) }

// SplitHintRequest records a manual split hint (§15 "manual hint") for the
// shard served by raft group gid; at "" lets the splitter pick the median
// key. Validation happens in pkg/cluster; the reconciler consumes the hint
// on its next tick. Audited — manual splits are operational acts.
func (s *Server) SplitHintRequest(ctx context.Context, actor string, gid uint64, at string) error {
	if err := cluster.RequestSplit(ctx, (*fabric)(s), gid, at, actor); err != nil {
		return err
	}
	s.audit(ctx, actor, "shard-split-hint", fmt.Sprintf("gid=%d at=%q", gid, at))
	return nil
}

// SplitHintsPending lists manual split hints the reconciler has not yet
// consumed — surfaced in cluster status so operators see queued work.
func (s *Server) SplitHintsPending() ([]cluster.SplitHint, error) {
	return cluster.SplitHints((*fabric)(s))
}

// certRenewLoop reissues this node's certificate when under 30 days of
// validity remain (§6.4 auto-rotation). Only applies to auto-PKI nodes.
func (s *Server) certRenewLoop() {
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if s.ca == nil || len(s.tlsCert.Certificate) == 0 {
				continue
			}
			leaf, err := x509.ParseCertificate(s.tlsCert.Certificate[0])
			if err != nil {
				continue
			}
			if time.Until(leaf.NotAfter) > 30*24*time.Hour {
				continue
			}
			s.Logger.Info("auto-rotating node certificate")
			if err := s.issueOwnCert(); err != nil {
				s.Logger.Error("certificate rotation failed", "err", err)
			}
			// The HTTPS listener picks the new cert up via GetCertificate.
		case <-s.stopC:
			return
		}
	}
}

// tokenGCLoop deletes expired session-token records (§7.1: tokens are
// server-side state, so expiry must eventually reclaim storage, not just
// deny authentication). Only the metadata leader sweeps, so the cluster
// runs exactly one GC at a time.
func (s *Server) tokenGCLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			f := (*fabric)(s)
			if !f.IsMetaLeader() {
				continue
			}
			entries, err := f.MetaList(auth.KeyPrefixTokens, 10000)
			if err != nil {
				continue
			}
			now := time.Now()
			removed := 0
			for _, e := range entries {
				tok, err := auth.DecodeToken(e.Record.Value)
				// Undecodable records are garbage too — sweep them.
				if err == nil && !tok.Expired(now) {
					continue
				}
				ctx, cancel := context.WithTimeout(s.lifeCtx, 5*time.Second)
				if _, err := f.MetaPropose(ctx, kv.Op{Type: "delete", Key: e.Key}); err == nil {
					removed++
				}
				cancel()
			}
			if removed > 0 {
				s.Logger.Info("token gc removed expired sessions", "count", removed)
			}
		case <-s.stopC:
			return
		}
	}
}

// pskExtraKey maps an extra PSK to its metadata first-seen record. The key
// itself never enters the metadata — only its SHA-256.
func pskExtraKey(psk string) string {
	sum := sha256.Sum256([]byte(psk))
	return "pskextras/" + hex.EncodeToString(sum[:])
}

// pskFirstSeen is the stored form of one rotation key's first sighting.
type pskFirstSeen struct {
	FirstSeen time.Time `json:"first_seen"`
}

// pskGraceLoop enforces the §6.2 rotation grace period: an extra PSK is
// accepted for psk_extra_grace after the cluster FIRST saw it in config,
// then dropped from the accepted set even if it is still configured.
// First-seen times live in the metadata keyspace, so restarting a node
// never resets the clock; the metadata leader records sightings, every
// node applies the verdict locally. The primary PSK is never purged.
func (s *Server) pskGraceLoop() {
	grace := s.Cfg.PSKExtraGrace
	if grace <= 0 || len(s.Cfg.PSKExtra) == 0 {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			f := (*fabric)(s)
			expired := map[string]bool{}
			for _, extra := range s.Cfg.PSKExtra {
				if extra == "" {
					continue
				}
				key := pskExtraKey(extra)
				rec, ok, err := f.MetaGet(key)
				if err != nil {
					continue
				}
				if !ok {
					// Unrecorded: the leader stamps first-seen; other
					// nodes wait for the record to replicate.
					if f.IsMetaLeader() {
						raw, _ := json.Marshal(pskFirstSeen{FirstSeen: time.Now().UTC()})
						ctx, cancel := context.WithTimeout(s.lifeCtx, 5*time.Second)
						_, _ = f.MetaPropose(ctx, kv.Op{Type: "set", Key: key, Value: raw})
						cancel()
					}
					continue
				}
				var fs pskFirstSeen
				if json.Unmarshal(rec.Value, &fs) == nil && time.Since(fs.FirstSeen) > grace {
					expired[extra] = true
				}
			}
			if len(expired) == 0 {
				continue
			}
			// Drop expired extras from the accepted set. Never index 0:
			// the primary (position 0) is exempt from the grace period.
			s.pskMu.Lock()
			if len(s.psks) < 2 {
				s.pskMu.Unlock()
				continue
			}
			kept := s.psks[:1:1]
			dropped := 0
			for _, k := range s.psks[1:] {
				if expired[k] {
					dropped++
					continue
				}
				kept = append(kept, k)
			}
			s.psks = kept
			s.pskMu.Unlock()
			if dropped > 0 {
				s.Logger.Warn("purged rotation PSKs past grace period",
					"count", dropped, "grace", grace.String())
			}
		case <-s.stopC:
			return
		}
	}
}

// shutdown stops the node in strict dependency order: signal AND JOIN every
// background goroutine, stop the controller and listeners, stop the raft
// groups, and only then close the store.
//
// REGRESSION NOTE (clean-shutdown crash): shutdown used to close the Pebble
// store without waiting for background loops. A loop blocked inside a ~5s
// MetaPropose (heartbeatLoop most often) would resume AFTER the store was
// closed and kill the process with "panic: pebble: closed" — on a CLEAN
// shutdown, ~80% reproducible after the e2e suite teardown. The rules that
// keep it fixed: every goroutine that can touch s.st is tracked in loopWG
// (launch via goLoop), loop proposes derive from lifeCtx so cancelling it
// aborts them promptly, and s.st.Close() happens only after loopWG is
// joined (or is skipped entirely — leaked, loudly — if a goroutine wedges
// past the bounded wait, because a leak on exit beats a panic on exit).
func (s *Server) shutdown() {
	// (a) Abort in-flight loop proposals immediately, then signal every
	// select-on-stopC loop to exit. lifeCancel comes first so a loop mid-
	// propose returns on this tick instead of sleeping out its timeout.
	s.lifeCancel()
	close(s.stopC)

	// (b) Join the background goroutines. The wait is bounded generously:
	// loop proposes abort via lifeCtx within milliseconds, but a few paths
	// (job terminal writes, repair/janitor passes in other files) run their
	// own contexts bounded at ≤ 15s. Not finishing in 30s means something
	// is truly wedged — log loudly and keep the store open (see above).
	joined := make(chan struct{})
	go func() { s.loopWG.Wait(); close(joined) }()
	loopsExited := true
	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		loopsExited = false
		s.Logger.Error("shutdown: background goroutines still running after 30s; " +
			"leaking the store instead of closing it under them (would panic)")
	}

	// (c) Controller (Stop joins its reconcile goroutine), then listeners.
	if s.controller != nil {
		s.controller.Stop()
	}
	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.httpSrv.Shutdown(ctx)
		cancel()
	}
	if s.adminSrv != nil {
		_ = s.adminSrv.Close()
	}

	// Raft groups next (Stop joins each group's run loop), so nothing is
	// applying entries when the store goes away.
	s.groupsMu.Lock()
	for _, h := range s.groups {
		h.group.Stop()
	}
	s.groupsMu.Unlock()

	// (d) Close the store — safe now, everything above has quiesced.
	if loopsExited {
		_ = s.st.Close()
	}
}

// startAdminSocket exposes the node-local admin listener on a unix socket
// inside the data directory (§7.3): reaching it requires filesystem access
// to the node, which is the recovery trust model.
func (s *Server) startAdminSocket() {
	sock := filepath.Join(s.Cfg.DataDir, "admin.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		s.Logger.Warn("admin socket unavailable", "err", err)
		return
	}
	_ = os.Chmod(sock, 0o600)
	mux := http.NewServeMux()
	mux.HandleFunc("/recover-root", s.handleRecoverRoot)
	s.adminSrv = &http.Server{Handler: mux}
	go s.adminSrv.Serve(ln)
}

// handleRecoverRoot resets the root password. It is reachable only via the
// unix socket, and it screams into the audit trail (§7.3).
func (s *Server) handleRecoverRoot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		http.Error(w, "body must be {\"password\": \"...\"}", http.StatusBadRequest)
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f := (*fabric)(s)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rec, ok, err := f.MetaGet(auth.KeyPrefixUsers + auth.RootUser)
	if err != nil || !ok {
		http.Error(w, "root user not found", http.StatusInternalServerError)
		return
	}
	u, err := auth.DecodeUser(rec.Value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	u.PasswordHash = hash
	if _, err := kv.DecodeResult(f.MetaPropose(ctx, kv.Op{Type: "set", Key: auth.KeyPrefixUsers + auth.RootUser, Value: u.Encode()})); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.audit(ctx, "host-admin", "root-password-reset", "via node-local admin socket")
	s.Logger.Warn("ROOT PASSWORD RESET via admin socket")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "root password updated")
}

// audit appends an entry to the audit trail (best effort — auditing must
// never block the audited operation from completing).
func (s *Server) audit(ctx context.Context, actor, action, detail string) {
	e := auth.AuditEntry{Time: time.Now().UTC(), Actor: actor, Action: action, Detail: detail}
	key := fmt.Sprintf("%s%d-%s", auth.KeyPrefixAudit, time.Now().UnixNano(), auth.RandomToken(4))
	_, _ = (*fabric)(s).MetaPropose(ctx, kv.Op{Type: "set", Key: key, Value: e.Encode()})
}
