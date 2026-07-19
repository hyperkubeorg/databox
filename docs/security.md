# Security

You will understand databox's transport security, node authentication, the
zero-touch PKI, the certificate trust workflow, and the users-and-grants
model — and end with a hardening checklist.

## Transport (§6.1)

- **Client-facing APIs are HTTPS only, TLS 1.3+.** Plaintext connections are
  rejected at the transport layer. Every language speaks HTTP; that is the
  consumability guarantee.
- **Inner-node traffic** (Raft, chunk transfer, request forwarding) runs over
  an internal RPC protocol on HTTP/2 + mutual TLS, authenticated with a
  Pre-Shared Key. This is invisible to clients.
- **mTLS enforcement**: `/internal/*` requests must carry a valid PSK **and**
  a client certificate chaining to the embedded cluster CA (peers present
  theirs automatically; certificates are issued at join). The join handshake
  itself is exempt — a joining node has no certificate yet — and stays gated
  by the join secret + PSK. Escape hatch for upgrades from clusters whose
  peers predate certificates: `internal_client_certs: off` (default
  `require`). Clusters on static certs or `--auto-cert` have no per-node CA
  identities, so the certificate factor doesn't apply there; the PSK still
  does.
- **No `InsecureSkipVerify` anywhere.** The Go client verifies servers by CA
  chain, by pinned fingerprint, or by the interactive trust prompt — never by
  skipping verification.

## Node authentication: PSK (§6.2)

Nodes authenticate to each other with a random Pre-Shared Key, default
512-bit. Generate one:

```sh
databox psk generate --bit 512   # raw key to stdout
```

Provide it via environment (`DATABOX_PSK`) or a config file, never hardcoded.

**Rotation is zero-downtime**: multiple PSKs may be active at once, and old
keys age out automatically. The flow:

1. Generate a new key and set it as `psk` on every node; move the old key
   into `psk_extra` (or `DATABOX_PSK_EXTRA`, comma-separated).
2. Restart nodes one at a time. During the roll, both keys authenticate.
3. Done — no third step. The cluster records when each extra key was first
   seen and stops accepting it after `psk_extra_grace` (default `720h`),
   even if it is still in config. First-seen times persist in the metadata
   keyspace, so restarts never reset the clock. Remove retired keys from
   config at leisure.

The primary `psk` is never purged; only `psk_extra` entries age out.

## Zero-touch PKI (§6.4)

- Cluster bootstrap creates an **embedded CA** stored in the Metadata group.
- Node certificates are auto-issued at join time and auto-rotated before
  expiry — no operator action for the life of the cluster.
- Operators who want their own PKI supply static certificates instead:

  ```sh
  databox certificates generate --cn db.example.com --san db1.example.com,db2.example.com
  databox server --tls-cert server.crt --tls-key server.key
  ```

  Auto-PKI is the default, never a requirement.

## Certificate trust on first use (§6.3)

When `databox console` meets an unknown server certificate:

1. The connection pauses (it is not auto-rejected).
2. The terminal prints the certificate's SHA-256 fingerprint, issuer, and
   validity window.
3. You accept or deny.
4. On acceptance the fingerprint is stored under `~/.databox/known_certs/`.
5. Later connections validate against that store; a changed certificate
   re-triggers the prompt.

To skip the prompt in automation, pin the fingerprint explicitly:

```sh
databox console --endpoint db1:8443 --ca-fingerprint AA:BB:...:FF
```

## Users and grants (§7)

- **Users** live in the Metadata group. Passwords are hashed with
  **argon2id producing a 512-bit derived key** and a per-user 32-byte salt —
  never stored recoverably.
- **Root** exists from bootstrap with no password and bypasses all grants.
  Set a password immediately:

  ```sh
  databox user passwd root
  ```

- **Grants** are `{prefix, effect: allow|deny, verbs}` with verbs from
  `{list, read, write, delete, watch, lock, admin}`. Resolution is
  **most-specific-prefix-wins**, default deny, deny breaks ties. Example:

  ```sh
  databox user create sam
  databox grant add sam deny  /            list,read,write,delete,watch,lock,admin
  databox grant add sam allow /home/sam    list,read,write
  ```

  `sam` can now read/write anything under `/home/sam` and nothing else.

- **One model everywhere.** The same grant tree governs the HTTP API, the
  GUI, SQL tables (`/sql/<db>/<table>/`), and S3 buckets (`/s3/<bucket>/`).
- **Authentication per surface**: bearer tokens for API/GUI/CLI, pg password
  auth for SQL, SigV4 access keys for S3.

  ```sh
  databox user access-keys sam    # mint an S3 key pair; secret shown once
  ```

- **Sessions are revocable**: tokens live server-side in the metadata
  keyspace. `POST /api/v1/auth/logout` revokes the presented bearer
  immediately, cluster-wide; the GUI's Logout does the same before clearing
  its cookie. Expired token records are garbage-collected by a background
  sweep on the metadata leader (every 10 minutes).

## Recovery (§7.3)

Lost the root password? Reset it **on a node host** (never over the network):

```sh
databox recover root-password
```

It uses the node's local admin socket, requires filesystem access to a
running node, and is loudly audited. Possession of a node host is the
ultimate authority.

## Auditing

Force operations and identity mutations (force-unlock, root reset,
user/grant changes, access-key creation) are written to the audit trail
under `audit/` in the system keyspace, visible through the `.databox/` view
and the GUI system browser.

## Impersonation (admins)

An admin can adopt another user's session from that user's page in the
GUI (`/users/view` → Impersonate). The server mints a real token for the
target and records `actor` and `target` in the audit trail; the GUI shows
a persistent warning banner until the admin returns to their own session.
Use it to verify what a grant change actually allows — the impersonated
view enforces the §7.2 rules exactly.

## Self-service credentials

Every user manages their own password and API keys at `/account`
in the GUI (or `POST/GET/DELETE /api/v1/users/<name>/access-keys` for
their own name). Access-key secrets are returned exactly once, at mint
time; revocation is immediate. All mutations are audited.

## Hardening checklist

- [ ] Set a root password (`databox user passwd root`) before exposing the
      cluster.
- [ ] Generate and inject a 512-bit PSK; do not rely on defaults in config
      files committed to source control.
- [ ] Keep auto-PKI, or supply static certs from your own CA — never disable
      verification.
- [ ] Pin CA fingerprints in automation (`--ca-fingerprint`) so unattended
      clients cannot be MITM'd on first use.
- [ ] Give every human and application a named user with least-privilege
      grants; reserve root for break-glass.
- [ ] Run processing layers (`gateway sql`/`gateway s3`) with TLS on their
      own listeners (`--tls-cert`/`--tls-key`) so password/SigV4 auth rides
      encrypted transport.
- [ ] Point Prometheus at `/metrics` with a bearer token — it requires a
      valid session, like the rest of the API. (The `.databox/` system view
      is admin-gated already; nothing to do there.)
- [ ] Review the audit trail (`.databox/audit/`) periodically.
