# databoxts

TypeScript client for databox (Bun runtime). Full API surface: KV,
transactions, watches, locks/leases, blobs, and a raw passthrough for admin
endpoints — enough to build complete layers, the way the SQL and S3
gateways are built on the Go `pkg/client`.

```sh
bun add databoxts   # from this repo: bun add file:clients/databoxts
```

## Connect

```ts
import { Databox } from "databoxts";

const db = new Databox({ endpoint: "db-node1:8443", ca: caPem });
// or: { endpoint: "localhost:8443", insecure: true }  — dev only
await db.login("sam", "password");
```

Retries follow the documented convention: network errors, 409, and 503
retry with exponential backoff (100 ms → 3 s, jitter, 5 attempts); 410 never
retries at the request level. Values are `Uint8Array` in, `Uint8Array` out;
strings are accepted and UTF-8 encoded.

## KV

```ts
const rev = await db.set("/app/config", '{"debug":true}');
const entry = await db.get("/app/config");   // KVEntry | null
await db.delete("/app/config");
await db.deleteRange("/app/", "/app0");      // [start, end)
const { entries, nextCursor } = await db.list("/app/", { limit: 100 });
for await (const e of db.iter("/app/")) { }  // pages transparently
```

## Transactions

Client-side OCC with per-shard snapshot reads. `runTx` re-runs the body on
commit conflict or `TxTooOldError`:

```ts
await db.runTx(async (tx) => {
  const a = Number(new TextDecoder().decode((await tx.get("/acct/a")) ?? new Uint8Array()));
  tx.set("/acct/a", String(a - 10));
});
```

Manual control: `const tx = db.tx()`, then `tx.get`/`tx.list`/`tx.set`/
`tx.delete`/`await tx.commit()` — `ConflictError` from commit means re-run
the body. Reads of missing keys join the read set at rev 0
(create-if-absent validates).

## Watches

NDJSON stream as an async generator; `type` is `"put"` or `"delete"`:

```ts
for await (const ev of db.watch("/app/", { signal: controller.signal })) {
  // ev.rev, ev.type, ev.key, ev.value
}
```

`RevisionCompactedError` (resume too old, or compaction raced the stream)
means re-list from current state and re-subscribe. No cross-shard ordering.

## Locks and leases

Lock resources are plain names (no leading slash), unlike keyspace keys.
Databox has no separate lease subsystem — a lease is a TTL'd lock plus
keepalive, guarded by the fencing token:

```ts
const grant = await db.lockAcquire("jobs/reindex", { ttlMs: 30_000 });
await db.lockRelease("jobs/reindex");

const lease = await db.lease("jobs/leader", { ttlMs: 10_000, onLost: console.error });
doWork(lease.fencing);   // reject smaller tokens downstream
await lease.release();
```

## Blobs

Raw byte streams, no KV size cap:

```ts
await db.putBlob("/files/data.bin", payload, "application/octet-stream");
await db.getBlob("/files/data.bin");
await db.getBlobRange("/files/data.bin", 1024, 512);
await db.statBlob("/files/data.bin");        // size/content-type/sha256, no data
await db.appendBlob("/files/data.bin", "more");  // ConflictError → retry
await db.spliceBlobs("/files/all", ["/files/p1", "/files/p2"]);  // server-side concat
const stream = await db.streamBlob("/files/data.bin");
```

## Admin

```ts
await db.raw("GET", "/api/v1/cluster/status");
await db.raw("POST", "/api/v1/users", { name: "svc", password: "..." });
```

## Demo layer

`examples/hello-layer.ts` is a complete hello-world layer: an HTTP greeting
service holding a lease, counting visits transactionally, and watching for
greeting changes.

```sh
bun examples/hello-layer.ts --endpoint localhost:8443 --password ...
curl localhost:8091/world
```

## Tests

```sh
bun tests/smoke.ts --endpoint localhost:8443 --password ...   # live smoke, every subsystem
bun run typecheck
```
