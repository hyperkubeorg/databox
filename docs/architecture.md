# Architecture

You will understand what runs where: the storage cluster vs. the stateless
processing layers, the Raft group topology, and the separate data path blobs
travel on.

## Storage system vs. processing layers

```
                 ┌───────────────────────────────────────────────┐
   users/apps ──►│  Processing Layers (stateless, same binary)   │
                 │   databox gateway sql   (postgres wire)       │
                 │   databox gateway s3    (S3-compatible HTTP)  │
                 └───────────────┬───────────────────────────────┘
                                 │ HTTPS API (user auth)
                 ┌───────────────▼───────────────────────────────┐
   users/apps ──►│  Storage Cluster: databox server (N nodes)    │
   (direct API,  │   • Metadata Raft group (topology, users,     │
    GUI, CLI)    │     locks, tx coordination, policies)         │
                 │   • Data Raft groups (sharded KV ranges)      │
                 │   • Blob engine (chunk store, replication/EC) │
                 └───────────────────────────────────────────────┘
```

- **Storage cluster** — `databox server` nodes. The source of truth. Each
  node runs one HTTPS listener serving the public API (`/api/v1/*`), the web
  GUI (`/*`), health probes (`/health`, `/healthz` liveness; `/readyz` readiness — unauthenticated), and
  PSK-gated internal node RPC (`/internal/*`).
- **Layers** — `databox gateway sql` and `databox gateway s3` are stateless
  translators that connect to the cluster as authenticated clients. Scale
  them by running more processes; they hold no data. See
  [layers/](layers/README.md).

## Raft topology

Consensus is multi-group Raft (etcd-raft state machines; databox owns the
transport, PebbleDB-backed storage, snapshotting, and group lifecycle).

| Group | Contents | Key prefix | Placement |
|-------|----------|-----------|-----------|
| Metadata (group 1) | Cluster topology, shard map, users/grants/tokens, lock state + fencing counters, transaction records, policies, alerts, audit trail | system keys (no leading `/`) | **1, 3, or 5 voters** — 1 below 3 nodes, 3 from 3–7 nodes, 5 at 8+ nodes (lowest ordinals preferred). **Metadata exists on these nodes and nowhere else** |
| Data (groups 2..N) | User KV, sharded by key range | keys starting with `/` | `replicas` nodes (default 3) |

A first principle governs metadata placement: **databox never replicates
a piece of data to all nodes.** Fleet size is user behavior — someone
will run thousands of nodes — and neither raft membership *nor full
copies of any kind* may scale with it. Metadata is therefore replicated
to the 1/3/5 metadata members, period.

Every other node **routes** metadata lookups to the members
(`pkg/server/metaproxy.go`): one internal RPC per lookup, fronted by a
**bounded, expiring cache** (3 s TTL, hard entry cap) that holds only the
entries the node actually asked for — the shard-map page it routes with,
the token and grant records for callers it is currently serving. That is
a cache, not a replica: it fills on demand and dies in seconds. The
staleness bound is safe by construction — shard routing is epoch-guarded
(a stale route gets a retryable answer), and grant/token changes reach
non-member nodes within the TTL. Metadata *writes* from a non-member hop
once to a member, like any routed operation. Member discovery rides a
separate tiny channel (`/internal/metamembers`) so the proxy never
recurses into itself.

**Finding the members is automatic, forever.** Voter seats move, and every
machine in the fleet may eventually be replaced — so nothing pins
discovery to particular addresses and no member list is ever hand-edited.
Every node (member or not) periodically persists a **peer address book**:
current members first, then every other known node, capped at 128 entries.
Any cluster node answers `/internal/metamembers` from its own view, so a
restarting node walks its book until *one* surviving peer of any role
answers, then learns the current members from it — discovery survives
complete member turnover, and complete fleet turnover so long as the
generations overlap. The one unrecoverable case is a node offline across a
total, non-overlapping fleet replacement; it rejoins with a fresh join
token, like new hardware.

Voter seats prefer the lowest-ordinal active nodes for predictability,
but a seated voter keeps its seat until it is decommissioned or removed —
no membership churn from the preference alone. When a seat frees up (or
the fleet crosses a threshold), the placement loop seats the
lowest-ordinal eligible node — the only moment metadata moves to a new
node; a node conf-changed out stops its instance and routes like everyone
else. Failure math: metadata tolerates `⌊voters/2⌋` *simultaneous voter*
failures; non-member failures never affect metadata availability. Like
data groups, a dead voter is not auto-replaced — decommission it (or
`cluster remove --force` dead hardware) and the seat is refilled.

Nothing high-frequency may ride the metadata log — its writes reach every
member and invalidate caches fleet-wide. Node liveness is the case that
matters: it flows as in-memory pings to the metadata leader, and only
verdict *transitions* are written through Raft (see the Liveness loop
below) — steady-state liveness costs zero log writes at any fleet size.

Each node persists everything in one PebbleDB instance under
`<data-dir>/pebble`, namespaced by group: Raft log entries, hard state,
membership, applied index, and the committed KV state (plus in-flight
transaction intents). Node-local identity (node ID, certificates, PSK)
lives beside it.

Raft snapshots stream page by page (v2 format): a follower catching up on
a large shard installs the snapshot with O(page) memory on both ends —
never a full in-memory copy of the shard.

## Request routing (KV data path)

1. A client sends `GET/PUT /api/v1/kv/<key>` to any node.
2. The node authenticates the bearer token and checks grants — both from
   metadata: read locally on a metadata member, otherwise through the
   routed 3s-TTL cache (metaproxy).
3. It resolves which shard owns the key (shard map, same path) and routes
   the operation to that shard's Raft group — locally if this node is a
   member, otherwise via internal RPC to one that is.
4. Writes are **proposed through the Raft log**; reads run a **ReadIndex
   barrier** by default (the leader quorum-confirms the commit index,
   the serving replica waits until it has applied that far) — or are
   proposed through the log too in the fallback `proposal` mode. Either
   way a read observes every write committed before it (linearizable —
   see [consistency.md](consistency.md)).

`List` and `Watch` hide shard boundaries: the node fans out to every shard
covering the prefix and merges (list, globally key-ordered pages) or
interleaves (watch, per-shard ordering only) the results.

KV and list responses carry debug headers — `X-Databox-Shards` (raft groups
contacted) and `X-Databox-Shard-Latency` (per-group elapsed time) — for
profiling which shards a request touched. They are informational only.

## Automatic management

A controller loop runs on every node but acts only on the metadata leader:

| Loop | Job | Trigger |
|------|-----|---------|
| Shard splitter | Split ranges that exceed the size threshold (16 GiB default) or a sustained QPS threshold (`split_threshold_qps`, off by default) | size/QPS reports from shard leaders; manual hints via `databox admin shard split <gid> [--at <key>]` |
| Placement | Assign groups/replicas to nodes; conf-change joiners in (metadata: seat/unseat voters to the 1/3/5 target — metadata lives only on members) | node join, membership drift |
| Decommission | Drain groups off `draining` nodes, replica by replica | `databox cluster decommission` |
| Leadership balancer | Spread raft leaderships across the fleet: one transfer per 30 s from the busiest node to its least-loaded fellow voter, ≥ 2-lead hysteresis; silent while any group is degraded, a drain is running, or rebalance is paused | leader counts from the ~10 s stat reports |
| Alerts | Publish warning/critical alerts for under-replicated or quorum-at-risk groups, plus a leadership-skew warning when one node leads far more than its fair share | continuous reconcile |
| Tx janitor | Finish or abort transactions whose coordinator died | records older than 2× tx timeout |
| Blob repair | Re-replicate lost chunks, reconstruct EC shards from survivors (leader); rehash local chunks at rest and delete corrupt copies; GC orphan chunks (every node) | continuous, IO capped by `repair_bytes_per_sec` (64 MiB/s default) |
| Token GC | Delete expired session-token records | 10 min sweep on the metadata leader |
| Liveness | Nodes ping the metadata leader in memory (5 s); the leader proposes a node's record only when its live/dead **verdict flips** or its address changes (CAS-guarded). Liveness telemetry never rides the replicated log — per-node heartbeat records through the log would cost O(N) writes/interval through one leader and defeat non-member caching | continuous; dead after 15 s unheard, with a 15 s cold-table grace after leader failover |
| Cert renewal | Reissue node certificates < 30 days from expiry | 12 h check |

Rebalancing, shard splitting, and blob repair can each be suspended during
maintenance: `databox admin rebalance|split|repair pause|resume`
(`POST /api/v1/admin/<target>/<action>`). Paused automation is flagged
loudly in `databox cluster status`.

Everything is observable through `databox cluster status`, the GUI, and the
read-only `.databox/` system view — admin-gated, browsable both through the
normal `GET /api/v1/kv/.databox/…` / `/api/v1/list?prefix=.databox/` paths
and the dedicated `/api/v1/system/*` endpoints.

## Blob engine: the second data path

Blob bytes deliberately do **not** travel through Raft. Raft carries only
the small blob *manifest* (path, size, SHA-256, chunk map, mode); chunk
bytes stream node-to-node over dedicated mTLS connections so a large upload
can never stall KV consensus.

- **Namespace** — blobs live in the same key tree as KV. A key holds either
  an inline value or a blob manifest; grants apply identically to both.
- **Chunks** — fixed-size (8 MiB default), content-addressed by SHA-256,
  stored on node-local disk at `<data-dir>/chunks/<aa>/<sha256>`. Identical
  data deduplicates itself; any replica of a chunk is as good as any other.
- **Placement** — small blobs (a single chunk) and small clusters (< 3
  nodes) use plain replication (default 2 copies, capped at
  `min(replicas, active nodes)` — so a **single node keeps just one copy:
  no cross-node redundancy and no erasure coding until you reach 3 nodes**).
  Larger blobs on ≥ 3 nodes use Reed-Solomon `rs-4-2` erasure coding:
  stripes of 4 data chunks gain 2 parity chunks; any 4 of the 6 reconstruct
  the stripe. That tolerates 2 failures at 1.5× storage overhead.
- **Policies** — the defaults above are overridable per key subtree. Rules
  live in metadata under `policies/replication/<path>` (`{"replicas":N}`)
  and `policies/ec/<path>` (`{"data":D,"parity":P,"enabled":B}`); the
  longest governing path wins per family. Manage them via
  `/api/v1/policies/{replication|ec}/<path>` or the GUI's `/policies` page,
  which includes a resolver showing exactly which rule a key gets. Policy
  changes apply to blobs written afterwards.
- **Write path** — client streams to any node → the node chunks and hashes
  → chunks fan out to placement targets → only then is the manifest
  committed through Raft. **The manifest commit is the visibility point**:
  a failed upload leaves orphan chunks (garbage-collected later), never a
  partial blob. Placement enforces a **write quorum**: a chunk that cannot
  reach its required distinct-node count fails the upload with a retryable
  `QuorumError` — a write never silently under-replicates.
- **Read path** — resolve the manifest via KV, stream chunks from any valid
  replica, verify hashes, reconstruct from parity if chunks are missing.
- **Repair** — a background loop re-replicates lost chunks, rebuilds EC
  shards from stripe survivors, and scrubs local chunks at rest (corrupt
  copies are deleted and re-created from good replicas). Repair tops copy
  counts up to policy (so blobs written on a small cluster gain copies as
  nodes join) but never re-encodes: a blob keeps the replication-or-EC
  layout it was written with. All repair IO is rate-limited
  (`repair_bytes_per_sec`) so maintenance never starves foreground traffic.

## Observability

Every node emits structured JSON logs, Prometheus metrics at `/metrics`
(token-authenticated), and OpenTelemetry traces. Tracing is off (zero
overhead, no network) until you point the process at an OTLP/HTTP
collector via the standard OTel env vars:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 \
OTEL_TRACES_SAMPLER=parentbased_traceidratio OTEL_TRACES_SAMPLER_ARG=0.1 \
databox server
```

`OTEL_SERVICE_NAME` overrides the default service name (`databox` /
`databox-gateway`). Spans are named by route template and carry method,
route, and status. `/internal/raft` and `/internal/raftsnap` are never
traced; other `/internal/*` RPCs only join traces propagated by the
caller, never start their own.

## Where things live on disk

```
<data-dir>/
├── pebble/        # PebbleDB: Raft logs + KV state, all groups
├── chunks/        # blob chunk store, content-addressed
└── admin.sock     # node-local recovery socket (root password reset)
```
