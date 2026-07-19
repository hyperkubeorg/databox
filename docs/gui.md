# The Web Portal

You will find every cluster and data operation in the built-in GUI, which
serves on the same HTTPS port as the API. This page maps what is where.

Sign in at `/login` with any databox user (fresh clusters: `root`, empty
password — set one immediately). Sessions are HttpOnly cookies; every
page enforces the same grants (§7.2) as the API.

## Navigation

| Page | Who | What |
|---|---|---|
| Dashboard `/` | everyone | topology, shard map, alerts, per-node safe-to-remove |
| Cluster Map `/cluster` | everyone | explorable cluster map: nodes, raft roles, shard placement |
| KV `/kv` | per grants | browse/create/edit/delete keys |
| Blobs `/blobs` | per grants | browse/upload/download/delete blobs |
| Watch `/watch` | per grants | live change stream console |
| Query `/query` | per grants | interactive scratchpad: one KV op per submit |
| Users `/users` | admins | user directory → per-user management |
| Policies `/policies` | admins | replication/EC durability rules + resolver |
| Locks `/locks` | admins | active locks, holders, fencing; force-unlock |
| Audit `/audit` | admins | the audit trail, newest first, read-only |
| System (nav) | admins | the `.databox/` metadata view inside the KV explorer |
| *your name* `/account` | everyone | own password + API keys |

## Cluster Map

`/cluster` draws the fleet as a full-bleed explorable graph (the "Meet
the Cluster" visual language): drag nodes to arrange them (positions
stick per browser), drag the background to pan, wheel to zoom. Each node
wears its roles — ★n for raft groups it leads, ◆ for metadata voters
(gold = leader), and a `shards⛁ chunks▦` count line. Dashed edges are the
metadata voter mesh; solid edges are shard leader→follower links (hover
one to see which shards ride it; hovering a node highlights exactly its
drawn links). Clicking nodes opens floating info windows — as many as you
like, draggable by their headers, resizable, refreshed live — showing
health, metadata role, a per-shard table (role, range, keys, size, QPS,
state), and blob chunk totals. The map shows placement only — key and
blob names never appear; numbers come from the group leaders' ~10s stats
reports.

## KV explorer

- **Hierarchical by default**: child path segments render as directories;
  the scan skips whole subtrees, so a directory with a million keys under
  it costs one probe. Breadcrumbs walk back up.
- **Recursive** checkbox: flat listing of everything under the prefix.
- **Pagination is range-based**: pages continue from the last key shown,
  and *Start from* jumps into the middle of ordered keys (inclusive) —
  e.g. prefix `/logs/` start `/logs/2026-06-15` for date-ordered keys.
  Every mode is a bounded page; nothing lists exhaustively.
- **New key** creates inline; opening a key gives value view (text or hex
  preview), edit, delete — each control appears only when your grants
  allow the verb.
- **Watch this prefix / watch this key** buttons jump straight into a
  running watch stream.

### Admin extras in the explorer

- The root listing shows **`.databox/`** — the read-only metadata
  keyspace (§19): nodes, shards, groups, users, alerts, audit trail,
  backups, all browsable and pretty-printed.
- Every key's view shows a **Placement** panel: the shard covering the
  key, its raft group, and each replica node with health and leader.
- Blob keys add a **Storage layout** panel: durability mode and every
  chunk or Reed-Solomon shard with size, content hash, and the nodes
  holding it. Note the §12 policy: blobs erasure-code (`rs-4-2`) only
  when they span multiple chunks (> 8 MiB) *and* the cluster has 3+
  nodes; smaller blobs and small clusters use plain replication.

## Blob browser

Same navigation as the KV explorer (directories, recursion toggle, range
pagination); rows are blob-backed keys only, sized from their manifests,
with the durability mode shown. Upload posts a file to any key; download
streams with hash verification.

## Watch console

Enter a prefix, press Start. The status line always tells the truth —
`● watching /x` or `○ idle` — and only the applicable button is enabled.
Events append as NDJSON lines. Deep links (`/watch?prefix=/a/&go=1`)
start streaming on load; the KV/blob "watch" buttons use them.

## Query scratchpad

`/query` runs one KV operation per submit — get, set, delete, or list — as
you, with your grants, result inline under the form. A permission denial
renders as the result, so "can I do this?" is an answerable query. Get
shows text or a hex dump (blob keys show their manifest); list pages with
a Next button. The **watch preview** streams the first ~20 events on a
prefix and stops by itself — the full console stays `/watch`.

## Policies (admins)

`/policies` manages the §12 durability rules stored in metadata:

- **Resolver**: type any key path and see exactly what a blob written
  there gets — replica count, EC geometry — and which stored rule won
  (or "built-in default").
- **Replication rules** (`{"replicas":N}`) and **EC rules**
  (`{"data":D,"parity":P,"enabled":B}`), listed per governed path with
  add/delete. Forms are structured; JSON is built and validated
  server-side. Most specific path wins per family.
- The **built-in defaults** (rs-4-2 EC for large blobs, 2 replicas for
  small ones) are always displayed — they are rules too.

Policy changes apply to blobs written afterwards; existing blobs keep
their layout.

## Locks (admins)

`/locks` lists every active lock from the metadata group: one row per
(resource, holder) with mode, fencing token, and expiry — expired-but-
unpruned holders are flagged. **Force unlock** releases a stuck lock; the
reason you type is required and lands in the audit trail with your name.
Fencing tokens keep a forced release safe for well-behaved clients.

## Audit (admins)

`/audit` shows the newest audited operations first — user/grant changes,
key mints, impersonations, force unlocks, recovery resets — filterable by
actor and action prefix. Strictly read-only.

## Users (admins)

`/users` is a searchable, paginated directory — built for thousands of
users, so rows are just doorways. Everything about one user lives on
their detail page: grants (add with verb checkboxes, remove per row),
password reset, API keys (mint/revoke), deletion, and
**Impersonate**.

Impersonation adopts the user's session so you see exactly what their
grants allow — the honest way to debug permissions. A warning banner
stays visible until you return to your own session, and both identities
are recorded in the audit trail.

## My account

Every user manages their own credentials at `/account`: change password,
view their grants (read-only — answers "why can't I…?"), and mint or
revoke their own API keys (optionally scoped to prefixes). Secrets display exactly once, at mint
time.
