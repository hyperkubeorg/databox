# databoxjs

JavaScript (ESM) client for databox (Bun runtime), with JSDoc types. Full
API surface: KV, transactions, watches, locks/leases, blobs, and a raw
passthrough for admin endpoints — enough to build complete layers, the way
the SQL and S3 gateways are built on the Go `pkg/client`.

Same API as `databoxts` (see its README for the full walkthrough); this
package is plain JavaScript for projects that don't want TypeScript.

```sh
bun add databoxjs   # from this repo: bun add file:clients/databoxjs
```

```js
import { Databox } from "databoxjs";

const db = new Databox({ endpoint: "localhost:8443", insecure: true }); // dev
await db.login("root", "password");

await db.set("/app/config", '{"debug":true}');
const entry = await db.get("/app/config");           // { key, value, rev, blob } | null

await db.runTx(async (tx) => {                       // OCC, auto re-run on conflict
  const cur = await tx.get("/app/counter");
  tx.set("/app/counter", String(Number(new TextDecoder().decode(cur ?? new Uint8Array())) + 1));
});

for await (const ev of db.watch("/app/")) { }        // "put" | "delete" events

const lease = await db.lease("jobs/leader", { ttlMs: 10_000 });  // TTL'd lock + keepalive
doWork(lease.fencing);
await lease.release();

await db.putBlob("/files/data.bin", bytes);          // blobs: put/get/range/stat/append/splice
```

TLS trust: `ca` (PEM, production) or `insecure: true` (dev only). Retries:
network errors, 409, and 503 with exponential backoff (100 ms → 3 s, jitter,
5 attempts); 410 never retries. Lock resources are plain names (no leading
slash), unlike keyspace keys.

## Demo layer

`examples/hello-layer.js` — HTTP greeting service holding a lease, counting
visits transactionally, watching for greeting changes:

```sh
bun examples/hello-layer.js --endpoint localhost:8443 --password ...
curl localhost:8092/world
```

## Tests

```sh
bun tests/smoke.js --endpoint localhost:8443 --password ...   # live smoke, every subsystem
```
