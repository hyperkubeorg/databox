/**
 * Hello-world databox layer: a stateless HTTP greeting service built on
 * databoxjs the way the SQL and S3 gateways are built on pkg/client.
 *
 * - a lease (TTL'd lock, auto-refreshed) identifies the running instance,
 * - every visit increments a per-name counter in a transaction (safe under
 *   concurrent instances — OCC conflicts re-run automatically),
 * - custom greetings are plain KV writes,
 * - a background watch logs greeting changes as they commit.
 *
 *     bun examples/hello-layer.js --endpoint localhost:18443 --password devpass123
 *
 *     curl localhost:8092/world
 *     curl -X PUT localhost:8092/world -d 'Howdy'
 *     curl localhost:8092/world
 */

import { Databox } from "../src/index.js";

function arg(name, fallback = "") {
  const i = process.argv.indexOf("--" + name);
  return i >= 0 ? process.argv[i + 1] : fallback;
}

const PREFIX = "/hello-layer";
const text = (v) => new TextDecoder().decode(v);

const db = new Databox({ endpoint: arg("endpoint", "localhost:8443"), insecure: true });
await db.login(arg("user", "root"), arg("password"));

const lease = await db.lease("hello-layer/instance-js", {
  ttlMs: 10_000,
  onLost: (err) => console.error("lease lost:", err),
});
console.log(`holding lease hello-layer/instance-js, fencing token ${lease.fencing}`);

void (async () => {
  for await (const ev of db.watch(PREFIX + "/greetings/")) {
    const name = ev.key.split("/").pop();
    if (ev.type === "put") console.log(`watch: greeting for ${name} is now ${text(ev.value)}`);
    else console.log(`watch: greeting for ${name} removed`);
  }
})();

const port = Number(arg("listen", "8092"));
Bun.serve({
  port,
  async fetch(req) {
    const name = new URL(req.url).pathname.replace(/^\/+|\/+$/g, "") || "world";

    if (req.method === "PUT") {
      const greeting = (await req.text()).trim() || "Hello";
      const rev = await db.set(`${PREFIX}/greetings/${name}`, greeting);
      return new Response(`greeting for ${name} set at rev ${rev}\n`);
    }

    let visits = 0;
    await db.runTx(async (tx) => {
      const cur = await tx.get(`${PREFIX}/visits/${name}`);
      visits = Number(cur ? text(cur) : "0") + 1;
      tx.set(`${PREFIX}/visits/${name}`, String(visits));
    });
    const g = await db.get(`${PREFIX}/greetings/${name}`);
    const greeting = g ? text(g.value) : "Hello";
    return new Response(`${greeting}, ${name}! (visit #${visits})\n`);
  },
});
console.log(`hello layer listening on :${port}`);
