/**
 * Live smoke test: exercises every client subsystem against a running
 * databox node.
 *
 *     bun tests/smoke.ts --endpoint localhost:18443 --password devpass123
 */

import { Databox, ConflictError } from "../src/index.ts";

function arg(name: string, fallback = ""): string {
  const i = process.argv.indexOf("--" + name);
  return i >= 0 ? process.argv[i + 1]! : fallback;
}

function assert(cond: unknown, msg = "assertion failed"): asserts cond {
  if (!cond) throw new Error(msg);
}

const text = (v: Uint8Array) => new TextDecoder().decode(v);

const PREFIX = "/smoketest-ts";
const LOCKNS = "smoketest-ts"; // lock resources are plain names, no leading slash

const db = new Databox({
  endpoint: arg("endpoint", "localhost:8443"),
  insecure: true,
});
await db.login(arg("user", "root"), arg("password"));
await db.deleteRange(PREFIX + "/", PREFIX + "0"); // '0' sorts just after '/'

// --- kv ---
const rev1 = await db.set(PREFIX + "/kv/a", "alpha");
const rev2 = await db.set(PREFIX + "/kv/b", "beta");
assert(rev2 > rev1);
const e = await db.get(PREFIX + "/kv/a");
assert(e && text(e.value) === "alpha" && e.rev === rev1);
assert((await db.get(PREFIX + "/kv/missing")) === null);
await db.set(PREFIX + "/kv/space key/x", "escaped ok");
const esc = await db.get(PREFIX + "/kv/space key/x");
assert(esc && text(esc.value) === "escaped ok");
const page = await db.list(PREFIX + "/kv/", { limit: 2 });
assert(page.entries.length === 2 && page.nextCursor);
const allKeys: string[] = [];
for await (const entry of db.iter(PREFIX + "/kv/", 2)) allKeys.push(entry.key);
assert(allKeys.length === 3, JSON.stringify(allKeys));
await db.delete(PREFIX + "/kv/b");
assert((await db.get(PREFIX + "/kv/b")) === null);
console.log("kv: ok");

// --- transactions: increment race ---
await db.set(PREFIX + "/counter", "0");
await Promise.all(
  Array.from({ length: 5 }, () =>
    db.runTx(async (tx) => {
      const cur = await tx.get(PREFIX + "/counter");
      tx.set(PREFIX + "/counter", String(Number(cur ? text(cur) : "0") + 1));
    }),
  ),
);
const counter = await db.get(PREFIX + "/counter");
assert(counter && text(counter.value) === "5", counter ? text(counter.value) : "missing");

// explicit conflict: two txs read the same rev, second commit must 409
const t1 = db.tx();
const t2 = db.tx();
await t1.get(PREFIX + "/counter");
await t2.get(PREFIX + "/counter");
t1.set(PREFIX + "/counter", "100");
t2.set(PREFIX + "/counter", "200");
await t1.commit();
let conflicted = false;
try {
  await t2.commit();
} catch (err) {
  conflicted = err instanceof ConflictError;
}
assert(conflicted, "expected ConflictError");

// create-if-absent (reads rev 0)
const t3 = db.tx();
assert((await t3.get(PREFIX + "/fresh")) === null);
t3.set(PREFIX + "/fresh", "first");
await t3.commit();

// tx list joins the read set + read-your-writes via get
const t4 = db.tx();
const listed = await t4.list(PREFIX + "/kv/");
assert(listed.entries.length === 2);
t4.set(PREFIX + "/kv/a", "staged");
assert(text((await t4.get(PREFIX + "/kv/a"))!) === "staged");
t4.delete(PREFIX + "/kv/a");
assert((await t4.get(PREFIX + "/kv/a")) === null);
await t4.commit();
assert((await db.get(PREFIX + "/kv/a")) === null);
console.log("tx: ok");

// --- locks + lease ---
const grant = await db.lockAcquire(LOCKNS + "/lock1", { ttlMs: 5000 });
assert(grant.fencing > 0);
assert((await db.lockStatus(LOCKNS + "/lock1")).locked === true);
await db.lockRelease(LOCKNS + "/lock1");
assert((await db.lockStatus(LOCKNS + "/lock1")).locked === false);

const lease = await db.lease(LOCKNS + "/leader", { ttlMs: 1200 });
assert(lease.fencing > 0 && lease.alive);
const firstFencing = lease.fencing;
await new Promise((r) => setTimeout(r, 1000)); // at least one ttl/3 refresh
assert(lease.alive && lease.fencing >= firstFencing);
await lease.release();
assert((await db.lockStatus(LOCKNS + "/leader")).locked === false);
console.log("locks/lease: ok");

// --- watch ---
const events: Array<{ type: string; value: Uint8Array }> = [];
const watchDone = (async () => {
  for await (const ev of db.watch(PREFIX + "/watched/")) {
    events.push(ev);
    if (events.length >= 2) break;
  }
})();
await new Promise((r) => setTimeout(r, 500)); // let the stream attach
await db.set(PREFIX + "/watched/x", "1");
await db.delete(PREFIX + "/watched/x");
await watchDone;
assert(events.length === 2);
assert(events[0]!.type === "put" && text(events[0]!.value) === "1");
assert(events[1]!.type === "delete");
console.log("watch: ok");

// --- blobs ---
const payload = new Uint8Array(16384).map((_, i) => i % 256);
const res = await db.putBlob(PREFIX + "/blob/data", payload, "application/octet-stream");
assert(res.size === payload.length);
assert(Buffer.compare(await db.getBlob(PREFIX + "/blob/data"), payload) === 0);
assert(
  Buffer.compare(await db.getBlobRange(PREFIX + "/blob/data", 256, 256), payload.slice(256, 512)) === 0,
);
assert(
  Buffer.compare(await db.getBlobRange(PREFIX + "/blob/data", payload.length - 16), payload.slice(-16)) === 0,
);
const st = await db.statBlob(PREFIX + "/blob/data");
assert(st && st.size === payload.length);
assert((await db.statBlob(PREFIX + "/blob/none")) === null);
await db.appendBlob(PREFIX + "/blob/data", "tail");
const appended = await db.getBlob(PREFIX + "/blob/data");
assert(appended.length === payload.length + 4 && text(appended.slice(-4)) === "tail");
const stream = await db.streamBlob(PREFIX + "/blob/data");
let streamedLen = 0;
for await (const chunk of stream) streamedLen += (chunk as Uint8Array).length;
assert(streamedLen === payload.length + 4);
await db.putBlob(PREFIX + "/blob/p1", "hello ");
await db.putBlob(PREFIX + "/blob/p2", "world");
const sp = await db.spliceBlobs(PREFIX + "/blob/joined", [PREFIX + "/blob/p1", PREFIX + "/blob/p2"]);
assert(sp.size === 11 && sp.composite);
assert(text(await db.getBlob(PREFIX + "/blob/joined")) === "hello world");
await db.deleteBlob(PREFIX + "/blob/joined");
assert((await db.statBlob(PREFIX + "/blob/joined")) === null);
console.log("blobs: ok");

// --- raw admin passthrough ---
const status = await db.raw("GET", "/api/v1/cluster/status");
assert(status.cluster_id && status.nodes.length > 0);
console.log("raw/admin: ok");

await db.deleteRange(PREFIX + "/", PREFIX + "0");
console.log("ALL TYPESCRIPT SMOKE TESTS PASSED");
process.exit(0);
