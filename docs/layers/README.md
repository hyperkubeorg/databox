# Processing Layers

Layers are stateless protocol services on top of the kv/blob system. They
hold no data: they connect to the cluster as authenticated clients,
translate their protocol onto the KV/blob API, and scale by running more
processes behind any TCP load balancer. Restart them freely. Build custom
gateways the same way on `pkg/client` (the FoundationDB layers idea) or in
Python/TypeScript/JavaScript via the [language clients](../../clients/).

- **[SQL layer](sql.md)** — PostgreSQL-wire gateway plus a full
  walkthrough of the dialect (its own SQL, not pg or MySQL).
- **[S3 gateway](s3.md)** — S3-compatible object storage over blobs.

`databox service` remains as an alias for `databox gateway`.

## Common flags

Both gateways share the same flags (`databox gateway sql --help`):

| Flag | Default | Meaning |
|------|---------|---------|
| `--listen` | `:5432` (sql) / `:9000` (s3) | this layer's own listen address |
| `--cluster` | `localhost:8443` | databox cluster endpoint (host:port) |
| `--ca-fingerprint` | — | pin the cluster certificate by SHA-256 fingerprint |
| `--tls-cert`, `--tls-key` | — | TLS material for this layer's listener (TLS 1.3 floor) |
| `--allow-cleartext` | off | accept non-TLS connections — trusted networks only; credentials travel in the clear |

Without a certificate, connections are refused unless `--allow-cleartext`
explicitly opts in: password and SigV4 auth are only safe over TLS.

## Client utilities

The binary ships clients for both layers, defaulting to the in-cluster
service addresses a chart deployment creates:

- `databox utils sql` — SQL REPL embedding the SQL engine in-process
  (default endpoint `databox:8443`; `-e 'SQL'` for scripts).
- `databox utils s3` — minimal SigV4 S3 client: `ls`/`mb`/`rb`/`cp`/`rm`/
  `stat` (default endpoint `http://databox-gateway-s3:9000`).
