# Consistency Guarantees

**This document is normative.** It states exactly what databox guarantees
and what it does not. Every guarantee maps to at least one e2e/chaos test by
name (in `e2e/`, run with `make e2e`); a guarantee without a passing test is
a release blocker. Do not infer stronger semantics than what is written
here.

## Guaranteed

### Linearizable single-key KV — `TestLinearizableKV`

`Get`, `Set`, and `Delete` on a single key are linearizable. Reads run a
ReadIndex barrier against the owning shard's leader by default (or are
proposed through the Raft log in the fallback `linearizable_reads:
proposal` mode — never served from a possibly stale copy in either mode),
so a read observes every write committed before it,
regardless of which node you ask. After a `Set` returns revision *r*, every
subsequent `Get` from any client through any node returns that value (or a
later one).

### Transactions: all-or-nothing with OCC — `TestTxAtomicity`

Transactions are optimistic: the client accumulates a read set
(key → revision observed) and a write set, then submits both in one commit.
Commit validates that every read revision is still current and applies all
writes atomically — or aborts with a retryable `Conflict` error and applies
nothing.

- Single-shard transactions commit as one Raft command (atomic by
  construction).
- Cross-shard transactions use two-phase commit: intents are prepared on
  every shard, then a transaction record in the metadata group flips to
  `committed` — **that flip is the atomic commit point**. If the
  coordinator dies mid-protocol, a janitor loop on the metadata leader
  finishes committed transactions and rolls back everything else. Intents
  staged by an aborted transaction are never visible.

Readers never observe a subset of a transaction's writes.

### Snapshot reads inside a transaction — `TestSnapshotReadStability`

Reads inside a `Tx` are MVCC snapshot reads, **per shard**. The
transaction's first read against a shard pins that shard's revision (the
read version, captured lazily; `tx/begin` hands out no versions). Every
later read against that shard — including first reads of other keys —
executes at the pinned revision: concurrent writers cannot change what the
transaction sees. Versioned `List` reconstructs a whole prefix as of the
pin, including values overwritten or deleted since.

Exactly what is and is not guaranteed:

- **Per shard**: each read is a consistent snapshot of one shard.
  Revisions are per-shard counters, so a transaction spanning shards holds
  one snapshot per shard, pinned at different moments — not one global
  snapshot. Commit-time OCC validation still checks every read key against
  its latest revision, so any transaction that would commit against
  changed data aborts with `Conflict` regardless of shard count
  (serializable-checked at commit).
- Snapshot reads never weaken OCC: the read set records the revision of
  the **version read**, so a concurrent writer conflicts even when the
  transaction only ever saw the snapshot value.
- `List` inside a transaction validates the keys it returned; keys that
  *appear* in the range later (phantoms) do not conflict.
- Shard splits during a transaction break that shard's pin (the new shard
  starts a new revision sequence); commit validation still rejects stale
  reads, so the failure mode is `Conflict`, not wrong data.

### Bounded history: `TxTooOld` — `TestTxTooOld`

Snapshot reads are served from bounded MVCC history: each shard retains
the last `MVCCHistoryRevisions` revisions (default 4096; horizon pruning
is itself deterministic and replicated — history a pinned read could still
need is never deleted out from under it). A transaction whose pin falls
behind the horizon gets HTTP 410 with code `TxTooOld` on its next read —
never silently wrong data. The fix is always: restart the transaction
(fresh `Tx`, fresh pins). `Client.RunTx` does this automatically with
backoff, for both `TxTooOld` and commit `Conflict`.

### Read-your-writes within a transaction — `TestTxReadYourWrites`

Inside one `Tx` (the Go client's `NewTx`), reads see the transaction's own
staged writes and deletes before commit. This is implemented client-side
(a transaction-local cache), so it holds regardless of server state.
Different transactions and non-transactional readers do **not** see staged
writes.

### Per-shard-ordered watch delivery — `TestWatchOrdering`

A watch stream delivers each shard's events in apply order, tagged with
that shard's monotonically increasing revision. A dropped single-shard
stream resumes with `from_revision` without missing events, as long as the
revision is still in the server's resume buffer; older resume points get
`RevisionCompacted` (HTTP 410) and the client must re-list and
re-subscribe. Slow consumers may have events dropped from their stream —
detectable as a revision gap — and recover the same way.

### Blob visibility = manifest commit — `TestBlobVisibility`

A blob is visible if and only if its manifest committed to KV. The manifest
is written only after every chunk is durably stored, so a visible blob is
always fully readable (hash-verified on read). Failed or interrupted
uploads leave no visible state — partial blobs do not exist. Blob manifest
reads inherit KV linearizability.

### Backup is a per-shard point-in-time capture — `TestBackupPointInTime`, `TestBackupShardSnapshot`

A backup that reports completed captures **each shard at a single shard
revision**. At job start the coordinator pins every shard's current
revision — the pins are taken together, one immediately after another —
and streams each shard's KV (blob manifests included) at its pin via MVCC
reads. Referenced chunk bytes are copied afterward, safe because chunks
are content-addressed and immutable. Within one shard the capture is a
true snapshot: a torn view — one key from before a concurrent write, a
later-scanned key from after it — cannot occur. `TestBackupShardSnapshot`
races an ordered writer against a backup and asserts the restored state is
a consistent cut; `TestBackupPointInTime` verifies committed keys and
blob content survive backup + restore byte-exactly.

Stated precisely, because there are two qualifications:

- **Cross-shard transactions may straddle pins.** There is no global read
  version (see "Not guaranteed"); pins are per-shard revisions taken at
  job start, not one cluster-wide instant. A transaction that committed
  across two shards in the window between their pins can appear in one
  shard's capture and not the other's.
- **A re-pinned shard moves its capture point.** MVCC history is bounded
  (`TxTooOld`, above): if sustained writes on a shard outrun the horizon
  before its unit finishes, that unit re-pins at a fresh revision and
  restarts. The shard is still captured at a single revision — just a
  later one than the other shards. Re-pins are logged and reported in the
  job status (`repins`; also per unit in the destination `manifest.json`),
  so an operator can always tell whether every pin is from job start.

A restore reports done only after a verify pass has re-read every restored
key and re-hashed every restored blob against the backup manifest
(asserted in `TestBackupPointInTime` via the job's `verified` flag). See
[admin/backup-restore.md](admin/backup-restore.md).

## Not guaranteed

- **Cross-shard watch ordering.** A watch over a prefix spanning multiple
  shards interleaves the shards' streams arbitrarily. Revisions are
  per-shard counters; comparing revisions from different shards is
  meaningless. `from_revision` resume is therefore only accepted for
  prefixes contained in a single shard.
- **Watch resume across shard splits.** When a shard splits, its revision
  sequence does not carry over to the new shard. A watcher spanning the
  split must re-list and re-subscribe; events emitted during the migration
  window may only be discoverable by re-listing.
- **Snapshot-point `List` under concurrent writes.** A plain `List` page
  is a consistent snapshot *per shard* at the moment that shard is
  scanned, but a multi-shard (or multi-page) listing is not a single
  global snapshot: writes that race the scan may appear in one shard's
  portion and not another's. Listing inside a transaction pins each
  shard's revision, which makes multi-page scans stable per shard — but
  still not one cross-shard snapshot (see `TestSnapshotReadStability`).
- **Global read versions.** There is no cluster-wide read timestamp;
  transaction snapshots are per-shard pins acquired lazily. Cross-shard
  coherence comes from commit-time OCC validation, not from the reads.
- **Debug metadata freshness.** Shard/latency/routing debug info in
  response headers is eventually consistent and for profiling only.

## Client retry convention

Verified behavior of the reference client (`pkg/client`), which the CLI,
console, and layers use:

- Retry on network errors, HTTP 409 (`Conflict`, `LockHeld`) and 503
  (`ShardSplitting`, leadership churn): exponential backoff starting at
  100 ms, doubling to a 3 s cap, plus jitter; **5 attempts** by default.
- Transaction commits are the exception: a `Conflict` on commit is an
  answer, not a fault — re-run the transaction body against fresh reads
  and commit again.
- HTTP 410 `TxTooOld` is never retried at the request level (the same
  read can never succeed); it restarts the whole transaction. Use
  `Client.RunTx`, or check `errors.Is(err, client.ErrTxTooOld)`.
