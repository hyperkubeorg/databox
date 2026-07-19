# Backup & Restore

You will run consistent, resumable cluster backups to S3-compatible storage
or SFTP, monitor and cancel them, and restore into a fresh cluster.

## What a backup is (§17)

A backup is a **cluster-executed job**. At job start the coordinator pins
every shard's current revision (the pins are taken together), then streams
each shard's KV — blob manifests included — at its pin via MVCC reads, and
finally copies the chunk bytes the captured manifests reference
(content-addressed and immutable, so copying after the pin is safe).

The result: **each shard is captured at a single revision**. Within a shard
a torn view is impossible. Cross-shard transactions may straddle pins —
there is no global read version. If write load on a shard outruns the MVCC
history horizon (4096 revisions) before its unit finishes, that unit
re-pins and restarts; the shard is still a single-revision capture, just a
later one, and every re-pin is logged and counted in `backup status`
(`repins`) and in the destination `manifest.json`. Exact guarantees and the
named tests backing them: [../consistency.md](../consistency.md).

**Scope: data, not cluster identity.** A backup captures the user keyspace
(`/` prefix) — inline KV and blobs. Users, grants, access keys, policies,
and other system state are the *cluster's* identity and are deliberately
not captured or restored: a restored cluster keeps its own root password,
users, and PSK. Recreate grants on the new cluster as needed.

## Create a backup

```sh
# To S3-compatible storage:
databox backup create --to s3://my-bucket/databox/2026-07-01

# Or to SFTP:
databox backup create --to sftp://user@backup-host/srv/databox/2026-07-01
```

The command returns a backup ID. Credentials (`--access-key`/`--secret-key`
or `AWS_*` env vars for S3, `--sftp-password` for SFTP) are supplied at
issue time and held **AES-256-GCM-encrypted in the system keyspace** for
the job's lifetime — the key is derived from the cluster's PSK, so the
ciphertext under `.databox/backups/` is useless without cluster membership.
They are purged when the job completes or is cancelled, and never logged.

## Monitor

```sh
databox backup status           # all jobs
databox backup status <id>      # one job: state, progress, bytes, ETA, repins
```

The ETA extrapolates the observed rate and appears once the job is >5%
done. Jobs are also visible under `.databox/backups/`, in the GUI, and as
Prometheus metrics.

## Cancel and resume

```sh
databox backup cancel <id>
databox backup create --id <id>     # resume — no URL or credentials needed
```

Cancelling stops the job at its next checkpoint and purges the stored
credentials; the destination has no `manifest.json`, marking it incomplete
(resume it or garbage-collect the files). Because per-unit checkpoints —
including the shard pins — and the encrypted credentials live under
`backups/<id>` in the replicated system keyspace, a coordinator crash loses
nothing: run `backup create --id <id>` against **any** node and the job
continues from its last checkpoint at the same pins, skipping files already
on the destination. A resume only needs re-supplied credentials after
completion/cancellation purged them.

## Restore

Restore populates an **empty** cluster (v1 does not restore in place over
live data):

```sh
databox restore --from s3://my-bucket/databox/2026-07-01
```

It applies KV pages in manifest order, re-uploads blobs (chunk placement is
rebuilt for the new cluster's topology), then runs a **verify pass**:
every restored key is re-read and byte-compared with the backup, every
restored blob is re-read (chunks are hash-checked by the blob engine) and
its whole-blob SHA-256 compared with the backup manifest, and the total key
count is checked. The job reports `done` (with `verified: true`) only after
verification — and **the cluster refuses user writes until then** (HTTP 503
`RestoreInProgress`, retryable), a gate that survives coordinator failover.
Resume with
`databox restore --id <id>`; monitor with `databox backup status`-style
records under `.databox/restores/`.

## Recommended cadence

- Schedule regular backups to at least two destinations (e.g. S3 for
  routine, SFTP off-site).
- Periodically test-restore into a scratch cluster and run
  `databox cluster status` plus spot `get`/`getblob` checks — an untested
  backup is not a backup.
- Watch `repins` in backup status: repeated re-pins mean the backup is
  racing heavy writes; schedule it in a quieter window for pins that all
  date from job start.
- Backups are databox's disaster-recovery story; cross-region replication
  is out of scope for v1 (§25).
