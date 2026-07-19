# Getting Started

You will: run a single node with podman or docker, store keys and blobs,
and grow to a 3-node cluster with join tokens.

Databox deploys as a container. The only prerequisite is podman or docker
(`docker` substitutes for `podman` 1:1 in every example), or a Kubernetes
cluster ([admin/kubernetes.md](admin/kubernetes.md)). Images at
`ghcr.io/hyperkubeorg/databox` are multi-arch (amd64/arm64); `latest` is
fine for kicking tires, production pins a date-stamp tag
(`YYYYMMDD.HHMMSS`).

## Run a single node

```sh
podman run -d --name databox \
  -v databox:/var/lib/databox \
  -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest
```

That's a complete cluster. Zero configuration means:

- HTTPS listener on `:8443` (API, web GUI, and node RPC on one port)
- data in `/var/lib/databox` — the named volume above owns it, so the
  container is disposable and the data is not
- an embedded CA is created and the node issues its own certificate
- a `root` user with **no password** — set one immediately:

```sh
podman exec -it databox databox user passwd root
```

The image's entrypoint is the `databox` binary, so the full CLI is always
one `exec` away. The rest of this page calls it as plain `databox`; in a
container deployment that means either an exec —

```sh
alias databox='podman exec -it databox databox'
```

— or a one-shot client container pointed at the node
(`podman run --rm -it --network host ghcr.io/hyperkubeorg/databox:latest console`).

A single node is a complete cluster but **not a durable one**: there is
only one copy of every key and blob — no cross-node replication, no erasure
coding, no failover. A lost disk is lost data unless you have backups
([admin/backup-restore.md](admin/backup-restore.md)). Replication and
erasure coding switch on once you [grow to 3 nodes](#grow-to-3-nodes).

Every setting can be changed by flag, `DATABOX_*` environment variable, or
config file — precedence is flags > env > file > defaults. Inspect the
effective configuration and where each value came from:

```sh
databox config show
```

Common flags: `--listen`, `--advertise`, `--data-dir`, `--node-name`,
`--config /etc/databox/config.yaml`, `--auto-cert` (throwaway self-signed
cert instead of the embedded CA), `--tls-cert`/`--tls-key` (bring your own),
`--root-password-file`, `--replicas`, `--psk`, `--join`.

### All settings

YAML config-file names; the env var is the same name uppercased with a
`DATABOX_` prefix (`listen` → `DATABOX_LISTEN`).

| Setting | Default | Meaning |
|---------|---------|---------|
| `listen` | `:8443` | HTTPS listen address (API + GUI + internal RPC) |
| `advertise_addr` | listen + hostname | address peers and clients reach this node at |
| `data_dir` | `/var/lib/databox` (or `./databox-data`) | PebbleDB, blob chunks, node identity |
| `node_name` | hostname | stable unique node name |
| `join` | — | join token; empty = bootstrap if the data dir is empty |
| `psk` | generated | primary node pre-shared key |
| `psk_extra` | — | additional accepted PSKs during rotation (comma-separated in env) |
| `psk_extra_grace` | `720h` | extra PSKs stop authenticating this long after first seen |
| `internal_client_certs` | `require` | `/internal/*` peer mTLS: `require` or `off` (upgrade escape hatch) |
| `auto_cert` | `false` | throwaway self-signed cert instead of the embedded CA |
| `tls_cert_file`, `tls_key_file` | — | operator-provided static certificates (set both) |
| `root_password_file` | — | bootstrap root password, read once at cluster init |
| `replicas` | `3` | KV replication factor for data shards. The metadata group is placed separately: 1/3/5 voters by fleet size hold ALL metadata; every other node routes lookups to them (bounded 3s cache) |
| `max_value_bytes` | `4194304` (4 MiB) | hard cap on a single KV value |
| `chunk_bytes` | `8388608` (8 MiB) | blob chunk size |
| `shard_split_bytes` | `17179869184` (16 GiB) | shard size split threshold |
| `split_threshold_qps` | `0` (off) | sustained per-shard QPS split trigger; enable only for range-spread workloads — a single hot key cannot be split away |
| `repair_bytes_per_sec` | `67108864` (64 MiB/s) | blob repair/scrub IO cap; ≤ 0 uncaps |
| `token_ttl` | `12h` | session token lifetime |
| `tx_timeout` | `5s` | max transaction open time before commit fails |
| `mvcc_history_revs` | `4096` | readable MVCC history per shard (`TxTooOld` horizon) |
| `mvcc_gc_interval` | `512` | applied entries between deterministic history prunes |
| `linearizable_reads` | `readindex` | linearizable KV read path: `readindex` barrier or legacy `proposal` through the log |

`mvcc_history_revs` and `mvcc_gc_interval` must be identical on every node
(pruning is replicated through the raft log); change them only with a
coordinated whole-cluster restart.

## First keys and blobs

The console is an interactive REPL over the API. On first connect it shows
the server certificate's SHA-256 fingerprint and asks you to trust it (the
pin is stored in `~/.databox/known_certs/`).

```sh
databox console --endpoint localhost:8443 --user root
```

```text
databox> set /app/config {"debug":true}
databox> get /app/config
databox> list /app
databox> watch /app          # streams changes until Ctrl-C
databox> putblob /files/data.tar.gz ./data.tar.gz
databox> getblob /files/data.tar.gz /tmp/restored.tar.gz
databox> del /app/config
databox> exit
```

`putblob`/`getblob` file arguments resolve where the console process runs —
inside the container under the `exec` alias. Bind-mount a host directory
(`podman exec` sees the container's mounts) or use the HTTP API below for
blob transfers from the host.

Non-interactive scripting with `-e` (commands separated by `;` or newlines)
and structured output with `-o json|yaml`:

```sh
databox console -e 'set /a 1; get /a' -o json
```

The same operations over plain HTTPS (values are base64 in JSON):

```sh
# Log in, keep the bearer token
TOKEN=$(curl -sk https://localhost:8443/api/v1/auth/login \
  -d '{"username":"root","password":"YOUR_PASSWORD"}' | jq -r .token)

# Set, get, list
curl -sk -H "Authorization: Bearer $TOKEN" -X PUT \
  https://localhost:8443/api/v1/kv/app/config \
  -d "{\"value\":\"$(echo -n '{"debug":true}' | base64)\"}"
curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8443/api/v1/kv/app/config
curl -sk -H "Authorization: Bearer $TOKEN" 'https://localhost:8443/api/v1/list?prefix=/app'

# Blobs stream raw bytes
curl -sk -H "Authorization: Bearer $TOKEN" -X PUT \
  --data-binary @data.tar.gz https://localhost:8443/api/v1/blobs/files/data.tar.gz
curl -sk -H "Authorization: Bearer $TOKEN" \
  https://localhost:8443/api/v1/blobs/files/data.tar.gz -o /tmp/restored.tar.gz
```

`-k` skips curl's CA check because the cluster issues its own certificates;
pin the fingerprint or install your own certs for production
(see [security.md](security.md)).

Values above the configured cap (4 MiB default, 2 MB recommended) are
rejected with `ValueTooLarge` — use the blob API for anything big.

## Retry safety: the client contract

There are no idempotency keys because the API is idempotent by
construction. Retrying any operation after an ambiguous failure (timeout,
dropped connection) is always safe:

- **`Set` / `Delete` / `DeleteRange`** — absolute state changes; applying
  one twice yields the same state.
- **`PutBlob`** — the manifest commit atomically replaces the key; a retry
  overwrites with identical content, never a partial or duplicated blob.
- **`AppendBlob`** — CAS-guarded: the new manifest commits only if the
  manifest is still at the revision the append read. A retry after an
  ambiguous failure either succeeds or fails with a clean `Conflict`
  (meaning the first attempt landed, or another writer won) — appends never
  duplicate or interleave.
- **Transaction commit** — OCC-validated: a retry against changed data
  aborts with `Conflict` and applies nothing. Re-run the transaction body
  (`RunTx` in the Go client automates this).

The reference client (`pkg/client`, used by the CLI, console, and gateways)
retries network errors, HTTP 409 (`Conflict`, `LockHeld`) and 503
(`ShardSplitting`, leadership churn, `RestoreInProgress`) with exponential
backoff — 100 ms doubling to a 3 s cap, plus jitter, 5 attempts. HTTP 410
(`TxTooOld`) is never retried at the request level; it restarts the whole
transaction. Copy this convention in your own clients; the normative
version lives in [consistency.md](consistency.md).

## Grow to 3 nodes

Each node is one container on its own host, publishing `8443` and
advertising an address the other nodes can reach — so the single-node
command grows one flag. (Re-create node 1 with
`server --advertise node1:8443` too if it started without one.)

On the running node, mint a join token (admin credentials required; the
token embeds the endpoint, the CA fingerprint, and a short-lived secret —
default validity 1 hour, reusable for several nodes until it expires):

```sh
databox cluster join-token --endpoint node1:8443 --user root --ttl 1h
```

Start the other two nodes with it:

```sh
# on node2
podman run -d --name databox \
  -v databox:/var/lib/databox -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest \
  server --advertise node2:8443 --join 'PASTE_TOKEN'

# on node3 — same, with node3:8443
```

Each joiner authenticates with the token, receives its identity, node
certificate, and PSK from the cluster, and the placement controller starts
replicating shards to it. `--advertise` must be an address the other nodes
can reach.

Verify:

```sh
databox cluster status --endpoint node1:8443 --user root
```

You should see three nodes `active`/healthy and `safe_to_proceed: true`.
The metadata group seats 3 voters at this size (5 at 8+ nodes) — other
nodes route metadata lookups to them; data shards replicate 3 ways
by default (`--replicas`); and blobs larger than one chunk written from now
on are erasure-coded `rs-4-2` (see
[architecture.md](architecture.md#blob-engine-the-second-data-path)).
Existing single-node data re-replicates in the background as the placement
and repair loops run — durability rises over time, not instantly.

Next steps:

- [architecture.md](architecture.md) — what those groups and shards are
- [security.md](security.md) — users, grants, TLS, PSK rotation
- [admin/bare-metal.md](admin/bare-metal.md) — systemd units, decommission
- [admin/kubernetes.md](admin/kubernetes.md) — the Helm chart does all of
  this automatically
