# Language clients

First-class clients for building databox layers outside Go. Each mirrors
the reference Go client (`pkg/client`): same retry convention, client-side
OCC transactions with per-shard snapshot pins, NDJSON watches, TTL'd locks
with fencing tokens (leases), and blobs.

| Client | Language / runtime | Tooling |
|--------|--------------------|---------|
| [databoxpy](databoxpy/) | Python ≥ 3.12 | uv |
| [databoxts](databoxts/) | TypeScript on Bun | bun |
| [databoxjs](databoxjs/) | JavaScript (ESM) on Bun | bun |

Each ships a live smoke test (`tests/`) covering every subsystem and a
hello-world demo layer (`examples/`) — an HTTP greeting service built the
way the SQL/S3 gateways are built on `pkg/client`.
