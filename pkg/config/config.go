// Package config defines every tunable setting for a databox node and the
// rules for how settings are resolved.
//
// Resolution precedence (§16.1), strongest first:
//
//  1. CLI flags                 (--listen, --data-dir, ...)
//  2. Environment variables     (DATABOX_LISTEN, DATABOX_DATA_DIR, ...)
//  3. Config file               (--config /etc/databox/config.yaml)
//  4. Built-in defaults
//
// Every field records *where* its final value came from so that
// `databox config show` can print the effective configuration together with
// its source — an operator should never have to guess which knob won.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Source describes where a configuration value was resolved from.
// It is used purely for display in `databox config show`.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
)

// Config is the complete set of node settings. YAML tags define the config
// file schema; the same names uppercased with DATABOX_ and underscores form
// the environment variable names (listen → DATABOX_LISTEN).
type Config struct {
	// Listen is the address the HTTPS server (API + GUI + internal RPC)
	// binds to. One port serves everything, per the single-binary goal.
	Listen string `yaml:"listen"`

	// AdvertiseAddr is the address other nodes and clients should use to
	// reach this node. Defaults to Listen with the hostname filled in.
	// On Kubernetes this is the pod's stable StatefulSet DNS name.
	AdvertiseAddr string `yaml:"advertise_addr"`

	// DataDir is the directory holding everything this node persists:
	// the PebbleDB store, blob chunks, and node-local identity material.
	DataDir string `yaml:"data_dir"`

	// NodeName is a human-friendly stable identifier for this node.
	// Defaults to the OS hostname. Must be unique within the cluster.
	NodeName string `yaml:"node_name"`

	// Join is a join token produced by `databox cluster join-token`.
	// Empty means "bootstrap a new cluster if the data directory is empty".
	Join string `yaml:"join"`

	// PSK is the primary pre-shared key used for node-to-node
	// authentication. PSKExtra lists additional accepted keys so that
	// keys can be rotated with zero downtime (§6.2).
	PSK      string   `yaml:"psk"`
	PSKExtra []string `yaml:"psk_extra"`

	// PSKExtraGrace bounds how long an extra (rotation) PSK stays
	// accepted after this cluster first saw it in config (§6.2:
	// "deprecated keys are purged after a configurable grace period").
	// First-seen times persist in the metadata keyspace, so restarts do
	// not reset the clock. Default 720h (30 days).
	PSKExtraGrace time.Duration `yaml:"psk_extra_grace"`

	// InternalClientCerts controls whether /internal/* peers must present
	// a client certificate chaining to the embedded cluster CA in
	// addition to the PSK (§6.1 mTLS). "require" (default) enforces it;
	// "off" is the upgrade escape hatch for clusters whose peers predate
	// cluster-issued certificates. Only meaningful with the embedded CA.
	InternalClientCerts string `yaml:"internal_client_certs"`

	// AutoCert requests a throwaway self-signed certificate instead of the
	// cluster-managed PKI. Useful for quick experiments.
	AutoCert bool `yaml:"auto_cert"`

	// TLSCertFile / TLSKeyFile switch the node to operator-provided static
	// certificates instead of the embedded auto-PKI (§6.4). Both must be
	// set together.
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`

	// RootPasswordFile, when set, is read once at cluster bootstrap and
	// becomes the root user's initial password. The Helm chart points this
	// at the generated secret. Ignored after the cluster is initialized.
	RootPasswordFile string `yaml:"root_password_file"`

	// Replicas is the KV replication factor for both the metadata group
	// and new data shards. Effective replication is min(Replicas, nodes).
	Replicas int `yaml:"replicas"`

	// MaxValueBytes is the hard cap on a single KV value (§9.1).
	// The documented recommendation is 2 MB; the default cap is 4 MiB.
	MaxValueBytes int `yaml:"max_value_bytes"`

	// ChunkBytes is the blob engine's chunk size (§11). Default 8 MiB.
	ChunkBytes int `yaml:"chunk_bytes"`

	// ShardSplitBytes is the size threshold above which the shard splitter
	// divides a key range (§15). Default 16 GiB.
	ShardSplitBytes int64 `yaml:"shard_split_bytes"`

	// SplitThresholdQPS is the sustained per-shard operations-per-second
	// above which the splitter divides a key range (§15). Default 0 =
	// disabled, deliberately: splitting on QPS only helps when the load is
	// range-partitionable — traffic spread across the shard's keys ends up
	// halved between the two children. A single hot key can never be split
	// away (both halves route it to one group), so a QPS trigger would just
	// churn shards without relieving anything. Enable it when you know the
	// workload spreads across the keyspace.
	SplitThresholdQPS int64 `yaml:"split_threshold_qps"`

	// RepairBytesPerSec caps the blob repair loop's background IO —
	// re-replication, EC reconstruction, and at-rest scrubbing (§11:
	// "rate-limited to protect foreground traffic"). Default 64 MiB/s;
	// 0 or negative removes the cap.
	RepairBytesPerSec int64 `yaml:"repair_bytes_per_sec"`

	// TokenTTL is the lifetime of login session tokens (§7.1).
	TokenTTL time.Duration `yaml:"token_ttl"`

	// TxTimeout bounds how long a transaction may stay open before commit
	// attempts fail with TxTooOld (§10).
	TxTimeout time.Duration `yaml:"tx_timeout"`

	// MVCCHistoryRevs is how many shard revisions of value history stay
	// readable for versioned reads (§10). MVCCGCInterval is how often (in
	// applied raft entries) each group prunes history beyond that horizon.
	//
	// Both values MUST be identical on every node of a cluster: history
	// pruning is applied deterministically inside the raft log, so
	// replicas configured differently would diverge. Change them only
	// with a coordinated whole-cluster restart.
	MVCCHistoryRevs int `yaml:"mvcc_history_revs"`
	MVCCGCInterval  int `yaml:"mvcc_gc_interval"`

	// LinearizableReads selects how linearizable KV reads execute on shards
	// this node hosts (§23): "readindex" (default) uses a ReadIndex barrier
	// plus a direct local state-machine read; "proposal" is the legacy path
	// where every read rides the raft log. Both modes are linearizable —
	// this is a node-local serving choice, NOT replicated state, so nodes
	// may disagree without harming correctness. "proposal" exists as an
	// operational escape hatch if the ReadIndex path misbehaves.
	LinearizableReads string `yaml:"linearizable_reads"`

	// sources tracks, per YAML field name, where the value came from.
	sources map[string]Source
}

// Default returns a Config populated entirely with built-in defaults.
// Zero-configuration startup must produce a working single-node cluster,
// so every default here has to be safe and sensible on a laptop.
func Default() *Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "databox" // extremely defensive: Hostname() basically never fails
	}
	return &Config{
		Listen:              ":8443",
		AdvertiseAddr:       "", // filled from Listen+hostname in Finish()
		DataDir:             defaultDataDir(),
		NodeName:            host,
		Replicas:            3,
		MaxValueBytes:       4 << 20,  // 4 MiB hard cap (§9.1)
		ChunkBytes:          8 << 20,  // 8 MiB blob chunks (§11)
		ShardSplitBytes:     16 << 30, // 16 GiB split threshold (§15)
		RepairBytesPerSec:   64 << 20, // 64 MiB/s repair/scrub budget (§11)
		TokenTTL:            12 * time.Hour,
		TxTimeout:           5 * time.Second,
		PSKExtraGrace:       720 * time.Hour, // 30-day rotation grace (§6.2)
		InternalClientCerts: "require",       // peer client certs on by default (§6.1)
		MVCCHistoryRevs:     4096,            // kv.MVCCHistoryRevisions default
		MVCCGCInterval:      512,             // kv.MVCCGCEvery default
		LinearizableReads:   "readindex",     // §23 ReadIndex read path
		sources:             map[string]Source{},
	}
}

// defaultDataDir prefers the traditional system location and falls back to
// a directory beside the process when /var/lib is not writable (laptops,
// containers running as non-root, CI).
func defaultDataDir() string {
	const system = "/var/lib/databox"
	// Try to create the system directory; if that works we can use it.
	if err := os.MkdirAll(system, 0o750); err == nil {
		return system
	}
	return "./databox-data"
}

// LoadFile merges settings from a YAML config file into c. Values present
// in the file override defaults but lose to env vars and flags, which are
// applied later by the caller in precedence order.
func (c *Config) LoadFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	// Unmarshal into a fresh struct so we can tell which fields the file
	// actually set (yaml leaves absent fields at their zero value).
	var f Config
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	merge := func(name string, set bool, apply func()) {
		if set {
			apply()
			c.sources[name] = SourceFile
		}
	}
	merge("listen", f.Listen != "", func() { c.Listen = f.Listen })
	merge("advertise_addr", f.AdvertiseAddr != "", func() { c.AdvertiseAddr = f.AdvertiseAddr })
	merge("data_dir", f.DataDir != "", func() { c.DataDir = f.DataDir })
	merge("node_name", f.NodeName != "", func() { c.NodeName = f.NodeName })
	merge("join", f.Join != "", func() { c.Join = f.Join })
	merge("psk", f.PSK != "", func() { c.PSK = f.PSK })
	merge("psk_extra", len(f.PSKExtra) > 0, func() { c.PSKExtra = f.PSKExtra })
	merge("auto_cert", f.AutoCert, func() { c.AutoCert = true })
	merge("tls_cert_file", f.TLSCertFile != "", func() { c.TLSCertFile = f.TLSCertFile })
	merge("tls_key_file", f.TLSKeyFile != "", func() { c.TLSKeyFile = f.TLSKeyFile })
	merge("root_password_file", f.RootPasswordFile != "", func() { c.RootPasswordFile = f.RootPasswordFile })
	merge("replicas", f.Replicas != 0, func() { c.Replicas = f.Replicas })
	merge("max_value_bytes", f.MaxValueBytes != 0, func() { c.MaxValueBytes = f.MaxValueBytes })
	merge("chunk_bytes", f.ChunkBytes != 0, func() { c.ChunkBytes = f.ChunkBytes })
	merge("shard_split_bytes", f.ShardSplitBytes != 0, func() { c.ShardSplitBytes = f.ShardSplitBytes })
	merge("split_threshold_qps", f.SplitThresholdQPS != 0, func() { c.SplitThresholdQPS = f.SplitThresholdQPS })
	merge("repair_bytes_per_sec", f.RepairBytesPerSec != 0, func() { c.RepairBytesPerSec = f.RepairBytesPerSec })
	merge("token_ttl", f.TokenTTL != 0, func() { c.TokenTTL = f.TokenTTL })
	merge("tx_timeout", f.TxTimeout != 0, func() { c.TxTimeout = f.TxTimeout })
	merge("psk_extra_grace", f.PSKExtraGrace != 0, func() { c.PSKExtraGrace = f.PSKExtraGrace })
	merge("internal_client_certs", f.InternalClientCerts != "", func() { c.InternalClientCerts = f.InternalClientCerts })
	merge("mvcc_history_revs", f.MVCCHistoryRevs != 0, func() { c.MVCCHistoryRevs = f.MVCCHistoryRevs })
	merge("mvcc_gc_interval", f.MVCCGCInterval != 0, func() { c.MVCCGCInterval = f.MVCCGCInterval })
	merge("linearizable_reads", f.LinearizableReads != "", func() { c.LinearizableReads = f.LinearizableReads })
	return nil
}

// LoadEnv merges DATABOX_* environment variables. Env beats the file but
// loses to explicit CLI flags.
func (c *Config) LoadEnv() {
	str := func(name string, dst *string) {
		if v, ok := os.LookupEnv("DATABOX_" + strings.ToUpper(name)); ok {
			*dst = v
			c.sources[name] = SourceEnv
		}
	}
	str("listen", &c.Listen)
	str("advertise_addr", &c.AdvertiseAddr)
	str("data_dir", &c.DataDir)
	str("node_name", &c.NodeName)
	str("join", &c.Join)
	str("psk", &c.PSK)
	str("tls_cert_file", &c.TLSCertFile)
	str("tls_key_file", &c.TLSKeyFile)
	str("root_password_file", &c.RootPasswordFile)
	if v, ok := os.LookupEnv("DATABOX_PSK_EXTRA"); ok {
		c.PSKExtra = strings.Split(v, ",")
		c.sources["psk_extra"] = SourceEnv
	}
	if v, ok := os.LookupEnv("DATABOX_AUTO_CERT"); ok {
		c.AutoCert = v == "true" || v == "1"
		c.sources["auto_cert"] = SourceEnv
	}
	num := func(name string, apply func(int64)) {
		if v, ok := os.LookupEnv("DATABOX_" + strings.ToUpper(name)); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				apply(n)
				c.sources[name] = SourceEnv
			}
		}
	}
	num("replicas", func(n int64) { c.Replicas = int(n) })
	num("max_value_bytes", func(n int64) { c.MaxValueBytes = int(n) })
	num("chunk_bytes", func(n int64) { c.ChunkBytes = int(n) })
	num("shard_split_bytes", func(n int64) { c.ShardSplitBytes = n })
	num("split_threshold_qps", func(n int64) { c.SplitThresholdQPS = n })
	num("repair_bytes_per_sec", func(n int64) { c.RepairBytesPerSec = n })
	num("mvcc_history_revs", func(n int64) { c.MVCCHistoryRevs = int(n) })
	num("mvcc_gc_interval", func(n int64) { c.MVCCGCInterval = int(n) })
	str("internal_client_certs", &c.InternalClientCerts)
	str("linearizable_reads", &c.LinearizableReads)
	dur := func(name string, dst *time.Duration) {
		if v, ok := os.LookupEnv("DATABOX_" + strings.ToUpper(name)); ok {
			if d, err := time.ParseDuration(v); err == nil {
				*dst = d
				c.sources[name] = SourceEnv
			}
		}
	}
	dur("token_ttl", &c.TokenTTL)
	dur("tx_timeout", &c.TxTimeout)
	dur("psk_extra_grace", &c.PSKExtraGrace)
}

// SetFlag records a value that arrived via a CLI flag — the strongest
// source. Callers pass the YAML field name so `config show` stays coherent.
func (c *Config) SetFlag(name string, apply func(*Config)) {
	apply(c)
	c.sources[name] = SourceFlag
}

// Finish validates the configuration and fills derived values. It must be
// called after all sources are merged and before the config is used.
func (c *Config) Finish() error {
	if c.AdvertiseAddr == "" {
		// Derive an advertise address from the listen address: a bare
		// ":8443" listen becomes "<hostname>:8443" so peers can reach us.
		host := c.NodeName
		_, port, ok := strings.Cut(c.Listen, ":")
		if !ok || port == "" {
			port = "8443"
		}
		if h := strings.TrimSuffix(c.Listen, ":"+port); h != "" {
			host = h
		}
		c.AdvertiseAddr = host + ":" + port
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return fmt.Errorf("tls_cert_file and tls_key_file must be set together")
	}
	if c.Replicas < 1 {
		return fmt.Errorf("replicas must be >= 1 (got %d)", c.Replicas)
	}
	if c.MaxValueBytes < 1024 {
		return fmt.Errorf("max_value_bytes must be >= 1024 (got %d)", c.MaxValueBytes)
	}
	if c.InternalClientCerts != "require" && c.InternalClientCerts != "off" {
		return fmt.Errorf(`internal_client_certs must be "require" or "off" (got %q)`, c.InternalClientCerts)
	}
	if c.MVCCHistoryRevs < 1 {
		return fmt.Errorf("mvcc_history_revs must be >= 1 (got %d)", c.MVCCHistoryRevs)
	}
	if c.MVCCGCInterval < 1 {
		return fmt.Errorf("mvcc_gc_interval must be >= 1 (got %d)", c.MVCCGCInterval)
	}
	if c.LinearizableReads != "readindex" && c.LinearizableReads != "proposal" {
		return fmt.Errorf(`linearizable_reads must be "readindex" or "proposal" (got %q)`, c.LinearizableReads)
	}
	return nil
}

// Show renders the effective configuration together with each value's
// source, for `databox config show`. Secrets are redacted — the point is
// to explain precedence, not to leak credentials into terminals and logs.
func (c *Config) Show() string {
	src := func(name string) Source {
		if s, ok := c.sources[name]; ok {
			return s
		}
		return SourceDefault
	}
	redact := func(v string) string {
		if v == "" {
			return `""`
		}
		return "(redacted)"
	}
	var b strings.Builder
	line := func(name, value string) {
		fmt.Fprintf(&b, "%-20s %-24s (%s)\n", name, value, src(name))
	}
	line("listen", c.Listen)
	line("advertise_addr", c.AdvertiseAddr)
	line("data_dir", c.DataDir)
	line("node_name", c.NodeName)
	line("join", redact(c.Join))
	line("psk", redact(c.PSK))
	line("psk_extra", fmt.Sprintf("%d extra key(s)", len(c.PSKExtra)))
	line("auto_cert", strconv.FormatBool(c.AutoCert))
	line("tls_cert_file", c.TLSCertFile)
	line("tls_key_file", c.TLSKeyFile)
	line("root_password_file", c.RootPasswordFile)
	line("replicas", strconv.Itoa(c.Replicas))
	line("max_value_bytes", strconv.Itoa(c.MaxValueBytes))
	line("chunk_bytes", strconv.Itoa(c.ChunkBytes))
	line("shard_split_bytes", strconv.FormatInt(c.ShardSplitBytes, 10))
	line("split_threshold_qps", strconv.FormatInt(c.SplitThresholdQPS, 10))
	line("repair_bytes_per_sec", strconv.FormatInt(c.RepairBytesPerSec, 10))
	line("token_ttl", c.TokenTTL.String())
	line("tx_timeout", c.TxTimeout.String())
	line("psk_extra_grace", c.PSKExtraGrace.String())
	line("internal_client_certs", c.InternalClientCerts)
	line("mvcc_history_revs", strconv.Itoa(c.MVCCHistoryRevs))
	line("mvcc_gc_interval", strconv.Itoa(c.MVCCGCInterval))
	line("linearizable_reads", c.LinearizableReads)
	return b.String()
}
