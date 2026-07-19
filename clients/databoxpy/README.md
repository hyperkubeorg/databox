# databoxpy

Python client for databox. Full API surface: KV, transactions, watches,
locks/leases, blobs, and a raw passthrough for admin endpoints — enough to
build complete layers, the way the SQL and S3 gateways are built on the Go
`pkg/client`.

```sh
uv add databoxpy   # from this repo: uv add --editable clients/databoxpy
```

## Connect

```python
from databoxpy import Databox

db = Databox("db-node1:8443", ca_file="cluster-ca.pem")
# or: Databox("host:8443", fingerprints=["AB:CD:..."])  — pinned cert
# or: Databox("localhost:8443", insecure=True)          — dev only
db.login("sam", "password")
```

Retries follow the documented convention: network errors, 409, and 503
retry with exponential backoff (100 ms → 3 s, jitter, 5 attempts); 410 never
retries at the request level.

## KV

```python
rev = db.set("/app/config", b'{"debug":true}')
entry = db.get("/app/config")          # KVEntry(key, value, rev, blob) | None
db.delete("/app/config")
db.delete_range("/app/", "/app0")      # [start, end)
entries, cursor = db.list("/app/", limit=100)
for e in db.iter("/app/"):             # pages transparently
    ...
```

## Transactions

Client-side OCC with per-shard snapshot reads. `run_tx` re-runs the body on
commit conflict or `TxTooOldError`:

```python
def transfer(tx):
    a = int(tx.get("/acct/a") or b"0")
    tx.set("/acct/a", str(a - 10).encode())

db.run_tx(transfer)
```

Manual control: `tx = db.tx()`, then `tx.get`/`tx.list`/`tx.set`/`tx.delete`/
`tx.commit()` — `ConflictError` from commit means re-run the body. Reads of
missing keys join the read set at rev 0 (create-if-absent validates).

## Watches

NDJSON stream, one `Event(rev, type, key, value)` per change; `type` is
`"put"` or `"delete"`:

```python
for ev in db.watch("/app/"):
    ...
```

`RevisionCompactedError` (resume too old, or compaction raced the stream)
means re-list from current state and re-subscribe. No cross-shard ordering.

## Locks and leases

Lock resources are plain names (no leading slash), unlike keyspace keys.
Databox has no separate lease subsystem — a lease is a TTL'd lock plus
keepalive, guarded by the fencing token:

```python
grant = db.lock_acquire("jobs/reindex", ttl_ms=30_000)   # → fencing token
db.lock_release("jobs/reindex")

with db.lease("jobs/leader", ttl_ms=10_000) as lease:    # auto-refresh at ttl/3
    do_work(fencing=lease.fencing)   # reject smaller tokens downstream
```

## Blobs

Raw byte streams, no KV size cap:

```python
db.put_blob("/files/data.bin", payload, "application/octet-stream")
db.get_blob("/files/data.bin")
db.get_blob_range("/files/data.bin", offset=1024, length=512)
db.stat_blob("/files/data.bin")            # size/content-type/sha256, no data
db.append_blob("/files/data.bin", b"more") # ConflictError → retry
db.splice_blobs("/files/all", ["/files/p1", "/files/p2"])  # server-side concat
for chunk in db.stream_blob("/files/data.bin"):
    ...
```

## Admin

```python
db.raw("GET", "/api/v1/cluster/status")
db.raw("POST", "/api/v1/users", {"name": "svc", "password": "..."})
```

## Demo layer

`examples/hello_layer.py` is a complete hello-world layer: an HTTP greeting
service holding a lease, counting visits transactionally, and watching for
greeting changes.

```sh
uv run python examples/hello_layer.py --endpoint localhost:8443 --password ...
curl localhost:8090/world
```

## Tests

Live smoke test against a running node (covers every subsystem):

```sh
uv run python tests/smoke.py --endpoint localhost:8443 --password ...
```
