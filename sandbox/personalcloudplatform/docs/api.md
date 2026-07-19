# PCP API v1

Versioned REST API at `/api/v1/`. Everything a member can do to their files, mail,
and calendars in the web UI is (or will be) available here. Response shapes documented
below are test-covered.

## Authentication

Mint keys in **Settings → API keys** (name, scope set, optional expiry). The token —

```
pcp_<keyID>_<secret>
```

— is displayed exactly once at creation; the server stores only its SHA-256 digest.
Send it on every request:

```
Authorization: Bearer pcp_...
```

Bearer auth is a parallel path to browser sessions: no cookies, no CSRF, and a key
never grants the web UI. Revoking a key (Settings, or an admin) takes effect on the
next request. Key creation and revocation are audited.

Failures: `401` (missing/invalid/expired token, or the owning account is banned or
deleted; carries `WWW-Authenticate: Bearer`), `403` (valid key, missing scope),
`429` (per-key rate limit, ~300 req/min; carries `Retry-After`).

## Scopes

A key holds only the scopes picked at mint time, and is always additionally capped by
its owner's own access.

| Scope | Grants |
|---|---|
| `profile:read` | Read the owner's profile: username, display name, quota/usage |
| `drive:read` | List drives/folders, stat, download |
| `drive:write` | Mkdir, rename, move, delete, upload, share links |
| `mail:read` | Folders, labels, threads, messages, attachments (read) |
| `mail:write` | Flags, stars, moves, labels, drafts |
| `mail:send` | Send mail (its own scope — sending is the dangerous one) |
| `calendar:read` | Calendars and events (read) |
| `calendar:write` | Event CRUD, RSVP |
| `contacts:read` | Contacts (read) |
| `contacts:write` | Contacts (write) |
| `media:read` | Media catalogs, thumbnails, ranged streaming, progress/watchlist/playlists (read) |
| `media:write` | Progress heartbeats, watched marks, watchlist/favorites, playlists |
| `messenger:read` | Servers, channels, DMs, messages, attachments, typing, unread, presence, search (read) |
| `messenger:write` | Send/edit/delete messages, typing, open DMs, mark read, set status, redeem invites |
| `git:read` | Git repos/orgs/issues/MRs (read) + `git fetch`/`clone` over the wire protocol |
| `git:write` | Git mutations (repos, grants, orgs, teams, issues, MRs, merges) + `git push` |

## Conventions

- JSON bodies both ways; times are RFC 3339 UTC.
- Errors are always `{"code": "...", "error": "..."}` — including 404s inside
  `/api/v1/`. Codes: `unauthorized`, `forbidden`, `rate_limited`, `not_found`,
  `bad_request`, `internal`.
- List endpoints use cursor pagination: `?cursor=` (opaque, from the previous
  response's `nextCursor`) and `?limit=` (default 50, max 200).
- Mutations return the resource with its new revision. `Idempotency-Key` is honored
  on send and upload.
- No CORS headers, deliberately — the API is for native clients, not cross-origin
  browser JS.

## Endpoints

### GET /api/v1/profile — `profile:read`

The calling key's owner.

```json
{"username": "ada", "displayName": "Ada Morgan", "admin": false,
 "quotaBytes": 10737418240, "usedBytes": 1073741824}
```

`quotaBytes` 0 means unlimited.

### GET /api/v1/scopes — any valid key

The calling key's own metadata, for client self-inspection.

```json
{"keyId": "aB3xY9...", "name": "phone mail", "scopes": ["mail:read", "mail:send"],
 "expiresAt": "2027-01-02T03:04:05Z"}
```

`expiresAt` is absent for keys that never expire.

## Drive

Access is always additionally capped by the key owner's own access: drive membership
first, then per-user grants on the node or an ancestor. A node the owner can't reach is
`not_found` — never `forbidden` — so a key can't map the keyspace. Node/drive/upload
ids and version revs are opaque strings.

The node resource (listings, stat, and what every mutation returns):

```json
{"id": "aB3xY9k2LmNp", "name": "Report.pdf", "dir": false, "size": 48211,
 "contentType": "application/pdf", "rev": 3,
 "createdAt": "2026-07-01T10:00:00Z", "modifiedAt": "2026-07-06T09:30:00Z",
 "modifiedBy": "ada"}
```

`rev` is the content revision (bumped on every content write). Folders omit
`size`/`contentType`/`rev`. Every drive has an implicit root folder with node id
`root`.

### GET /api/v1/drive/drives — `drive:read`

```json
{"drives": [{"id": "…", "name": "My Drive", "type": "personal", "role": "owner",
             "owner": "ada", "createdAt": "2026-07-01T10:00:00Z"}]}
```

### GET /api/v1/drive/list/{drive}/{node} — `drive:read`

One folder's children, cursor-paginated (`?cursor=&limit=`), in case-insensitive name
order with folders and files interleaved.

```json
{"nodes": [/* node resources */], "nextCursor": ""}
```

### GET /api/v1/drive/stat/{drive}/{node} — `drive:read`

The node resource.

### POST /api/v1/drive/mkdir — `drive:write`

Body `{"driveId", "parentId", "name"}` → the new folder's node resource. Name
collisions answer `409 {"code": "conflict"}`.

### POST /api/v1/drive/rename — `drive:write`

Body `{"driveId", "nodeId", "name"}` → the node resource.

### POST /api/v1/drive/move — `drive:write`

Body `{"driveId", "nodeId", "parentId"}` (parentId = destination folder) → the node
resource. Moving a folder into its own subtree is refused.

### DELETE /api/v1/drive/nodes/{drive}/{node} — `drive:write`

PERMANENT (there is no trash): a folder's whole subtree, every version blob,
thumbnail, share link, and grant dies with it; charged quota is refunded to whoever
was charged.

```json
{"deleted": true, "freedBytes": 48211}
```

### PUT /api/v1/drive/upload/{drive}/{parent}?name=File.bin — `drive:write`

Single-shot upload: the raw request body becomes the file's content. Content type is
sniffed server-side (the declared type is never trusted); an existing file of the
same name gains a new version, never a duplicate — which is also why repeating an
interrupted PUT is safe (`Idempotency-Key` is accepted but redundant here). Body size
is capped by the site's upload limit; the owner's quota is enforced
(`507 {"code": "quota_exceeded"}`). Returns `201` + the node resource.

### Chunked-resumable upload — `drive:write`

For big files. Offsets are byte positions in the assembled file; chunks must be
contiguous:

- `POST /api/v1/drive/uploads` body `{"driveId", "parentId", "name"}` →
  `201 {"uploadId": "…"}`
- `PUT /api/v1/drive/uploads/{id}?offset=N` (raw chunk body, ≤64 MiB) →
  `{"committed": M}`. A wrong offset answers
  `409 {"code": "offset_mismatch", "committed": M}` — resume from `M`.
- `GET /api/v1/drive/uploads/{id}` → `{"committed": M}` (the resume point)
- `POST /api/v1/drive/uploads/{id}/finish` → `201` + the node resource (server-side
  splice; quota charged here)

Abandoned sessions are swept after 24h.

### GET /api/v1/drive/download/{drive}/{node} — `drive:read`

The file's bytes. HTTP `Range` is honored (single range; `206` with `Content-Range`),
`?rev=` serves an old version, `ETag` is the immutable blob id (`Cache-Control:
immutable` is exact, not heuristic).

### GET /api/v1/drive/versions/{drive}/{node} — `drive:read`

Newest first:

```json
{"versions": [{"rev": "…", "n": 3, "size": 48211,
               "contentType": "application/pdf", "by": "ada",
               "at": "2026-07-06T09:30:00Z"}]}
```

### POST /api/v1/drive/restore — `drive:write`

Body `{"driveId", "nodeId", "rev"}` → the node resource. The restore is itself a NEW
version — history only moves forward.

### Share links

- `GET /api/v1/drive/shares/{drive}/{node}` — `drive:read` → `{"shares": [share…]}`
- `POST /api/v1/drive/shares` — `drive:write`, body `{"driveId", "nodeId", "perms",
  "password", "expiresIn"}` (`perms`: `view`|`download`; `password` optional;
  `expiresIn` a Go duration like `"168h"`, `""` = never) → `201` + the share resource
- `DELETE /api/v1/drive/shares/{token}` — `drive:write` (the link's creator, or an
  editor on the node) → `{"revoked": true}`

The share resource:

```json
{"token": "…", "url": "/s/…", "driveId": "…", "nodeId": "…", "perms": "download",
 "password": false, "expiresAt": "2026-07-14T00:00:00Z", "by": "ada",
 "createdAt": "2026-07-07T00:00:00Z"}
```

`expiresAt` is absent for links that never expire. The `url` is the public page —
no bearer token needed there; that's the point of a link.

## Mail

A mailbox id names one message store the key's owner holds; anyone else's mailbox id
answers `not_found`, never `forbidden`. Thread, message, label, folder, and draft ids
are opaque strings. Message HTML bodies come back **sanitized** (whitelist tags, no
scripts/frames/forms, `href` limited to http/https/mailto, remote images rewritten to
`data-mail-src` for client click-to-load) — the same renderer the web app uses.

The thread resource (listings, get, and what every thread mutation returns):

```json
{"id": "b06712cd80a1e1a2", "boxId": "…", "subject": "Trip plans", "folder": "inbox",
 "participants": ["Bob <bob@remote.example>"], "msgCount": 3, "unreadCount": 2,
 "starred": true, "labels": ["labelId…"], "snippet": "see you there",
 "attachCount": 1, "lastActivity": "2026-07-06T09:30:00Z"}
```

`folder` is `inbox`, `archive`, `spam`, `trash`, or a custom folder id. Sent is a
facet, not a folder. Zero-valued flags (`unreadCount`, `starred`, …) are omitted.

### GET /api/v1/mail/mailboxes — `mail:read`

```json
{"mailboxes": [{"id": "…", "addr": "ada@example.test", "signature": "— Ada",
                "createdAt": "2026-07-01T10:00:00Z"}]}
```

### GET /api/v1/mail/folders/{box} — `mail:read`

The rail: system folders (inbox carries its unread-thread count, capped at 999),
custom folders (`"custom": true`), and the owner's labels.

```json
{"folders": [{"id": "inbox", "name": "Inbox", "unread": 2}, {"id": "archive", "name": "Archive"},
             {"id": "spam", "name": "Spam"}, {"id": "trash", "name": "Trash"},
             {"id": "folderId…", "name": "Receipts", "custom": true}],
 "labels": [{"id": "…", "name": "Work", "color": "#67C99A", "order": 0}]}
```

### GET /api/v1/mail/threads/{box} — `mail:read`

One view's threads, newest-activity first, cursor-paginated. Pick the view with ONE
of: `?folder=` (system name or custom id; also the facets `starred` and `sent`;
default `inbox`), `?label=` (label id), or `?q=` (search — free terms AND-match, plus
the operators `from:` `to:` `label:` `in:` `has:file`; search is capped at `limit`
with no cursor).

```json
{"threads": [/* thread resources */], "nextCursor": ""}
```

### GET /api/v1/mail/threads/{box}/{thread} — `mail:read`

The thread resource plus its message headers, oldest first:

```json
{"thread": {/* thread resource */},
 "messages": [{"id": "…", "from": "Bob <bob@remote.example>", "to": ["ada@example.test"],
               "subject": "Trip plans", "date": "2026-07-06T09:30:00Z", "size": 4821,
               "seen": true, "outbound": false, "snippet": "…", "hasAttach": true,
               "messageId": "<t1@remote.example>"}]}
```

### GET /api/v1/mail/messages/{msg} — `mail:read`

One message parsed for display: the header resource, the sanitized HTML body, the
plain-text body, and attachment metadata (`n` indexes the download endpoint).

```json
{"message": {/* message header resource */},
 "html": "<p>sanitized…</p>", "text": "plain body",
 "attachments": [{"n": 0, "name": "doc.pdf", "size": 48211, "contentType": "application/pdf"}]}
```

### GET /api/v1/mail/messages/{msg}/raw — `mail:read`

The raw RFC 822 source (`Content-Type: message/rfc822`).

### GET /api/v1/mail/messages/{msg}/attachments/{n} — `mail:read`

Attachment bytes with `Content-Disposition: attachment`. Active content types
(HTML/SVG/XML/JS) override to `application/octet-stream` — hostile mail never
executes in the platform's origin.

### Thread mutations — `mail:write`

Each returns the mutated thread resource.

- `POST /api/v1/mail/threads/{box}/{thread}/read` body `{"read": true|false}`
- `POST /api/v1/mail/threads/{box}/{thread}/star` body `{"starred": true|false}`
- `POST /api/v1/mail/threads/{box}/{thread}/move` body `{"folder": "archive"}`
  (archive/trash/spam/inbox or a custom folder id; moving to trash starts the
  retention clock)
- `POST /api/v1/mail/threads/{box}/{thread}/labels` body `{"labelId": "…", "on": true|false}`

### Folders + labels — `mail:write`

- `POST /api/v1/mail/folders/{box}` body `{"name": "Receipts"}` → `201 {"id", "name",
  "custom": true}`
- `DELETE /api/v1/mail/folders/{box}/{id}` → `{"deleted": true}` (its threads move to
  Archive)
- `POST /api/v1/mail/labels` body `{"name", "color"}` (`#RRGGBB`) → `201` + the label
- `PATCH /api/v1/mail/labels/{id}` body `{"name", "color", "order"}` → the label
- `DELETE /api/v1/mail/labels/{id}` → `{"deleted": true}` (threads shed it)

### Drafts — `mail:write`

The draft resource:

```json
{"id": "…", "boxId": "…", "to": ["x@remote.example"], "subject": "…",
 "text": "", "html": "<p>…</p>", "inReplyTo": "<t1@remote.example>",
 "references": ["<t1@remote.example>"], "threadId": "…",
 "attachments": [{"id": "…", "name": "doc.pdf", "size": 48211,
                  "contentType": "application/pdf"}],
 "updatedAt": "2026-07-06T09:30:00Z"}
```

Draft HTML is sanitized on save. Attachments are server-owned — the JSON body can't
set them; uploads add them.

- `GET /api/v1/mail/drafts/{box}` → `{"drafts": [draft…]}` (newest-updated first)
- `GET /api/v1/mail/drafts/{box}/{id}` → the draft
- `POST /api/v1/mail/drafts` body = the draft fields (+ `"boxId"`; empty `"id"`
  creates → `201`, an existing id updates → `200`)
- `DELETE /api/v1/mail/drafts/{box}/{id}` → `{"deleted": true}` (staged attachment
  blobs die with it)
- `POST /api/v1/mail/drafts/{box}/{id}/attachments?name=doc.pdf` — raw request body =
  the attachment bytes (declared `Content-Type` recorded; size capped by the site's
  message limit) → `201` + the updated draft

### POST /api/v1/mail/send — `mail:send`

Body: `{"boxId", …}` plus EITHER `"draftId"` (sends the saved draft, attachments
included, and consumes it) OR inline fields (`to`/`cc`/`bcc`, `subject`, `text`,
`html`, `inReplyTo`, `references`). HTML is sanitized before the message builds; the
mailbox signature appends automatically; send rate limits apply.

`Idempotency-Key` is honored for 24h: a replayed POST with the same key answers the
recorded result (with `"replayed": true`) instead of sending twice.

Returns `202`:

```json
{"outId": "…", "holdUntil": "2026-07-06T09:30:10Z"}
```

`holdUntil` is the undo-send release (the owner's setting: off/10s/30s). Until then
the message exists nowhere but the queue.

### POST /api/v1/mail/send/cancel — `mail:send`

Body `{"outId": "…"}`. Before `holdUntil`, withdraws the send and restores the
compose as a draft (attachments intact):

```json
{"cancelled": true, "draft": {/* draft resource */}}
```

After the hold releases: `400 {"code": "bad_request"}` — it's on its way.

---

## Calendar

Calendars are `.pccal` collaborative documents in drives; an event is addressed by
its `(driveId, nodeId, id)` triple. Event mutations run through the same CRDT
substrate as the web app (server-minted ops), so notifications and external ICS
invite mail fire identically.

### GET /api/v1/calendar/calendars — `calendar:read`

The owner's whole calendar world, subscription state layered in:

```json
{"calendars": [{"driveId": "…", "nodeId": "…", "name": "Team", "driveName": "Ops",
  "personal": false, "color": "#5B8CFF", "subscribed": true, "hidden": false,
  "canEdit": true}]}
```

### GET /api/v1/calendar/events?from=&to= — `calendar:read`

`from`/`to` are RFC 3339, ordered, spanning at most 100 days. Aggregates across the
owner's subscribed calendars; `&drive=&node=` narrows to one calendar (any calendar
the owner can read, subscribed or not). The event resource:

```json
{"events": [{"id": "…", "driveId": "…", "nodeId": "…", "title": "Standup",
  "start": "2026-07-07T16:00:00Z", "end": "2026-07-07T16:15:00Z", "allDay": false,
  "location": "", "notes": "", "invites": {"bob": "yes", "x@remote.example": "invited"},
  "tags": ["carol"], "by": "ada"}]}
```

Invite keys are member usernames or external email addresses; externals get ICS
mail (METHOD:REQUEST, SEQUENCE-bumped updates, METHOD:CANCEL) instead of
notifications.

### Event CRUD — `calendar:write`

- `POST /api/v1/calendar/events` body = the event fields (`title`, `start`, `end`,
  `allDay`, `location`, `notes`, `invites`, `tags`, plus `driveId`/`nodeId`;
  omitting the pair targets the owner's primary personal calendar, created lazily)
  → `201` + the event
- `PATCH /api/v1/calendar/events/{drive}/{node}/{id}` — partial body, absent fields
  keep their values → the event
- `DELETE /api/v1/calendar/events/{drive}/{node}/{id}` → `{"deleted": true}`
  (externals on the invite list get METHOD:CANCEL)

### POST /api/v1/calendar/events/{drive}/{node}/{id}/rsvp — `calendar:write`

Body `{"status": "yes"|"no"|"maybe"}`. Being ON THE INVITE LIST is the entire
authorization — no drive role is required, and strangers get `404`. Returns the
updated event; the creator is notified (answer changes re-notify).

## Contacts

Contact cards are `.pccard` files in drives; a contact is addressed by its
`(driveId, nodeId)` pair. The resource:

```json
{"driveId": "…", "nodeId": "…", "name": "Grace Hopper",
 "emails": ["grace@remote.example"], "phones": ["+1 555 0100"],
 "org": "Navy", "title": "Rear Admiral", "notes": "",
 "fields": [{"label": "Door code", "type": "secret", "value": "4242"}],
 "canEdit": true}
```

`fields` holds any number of custom entries. `type` is one of `text`,
`secret`, `note`, `url`, `date` (unknown types store as `text`); labels
cap at 60 chars, values at 500 (`secret`/`note`: 4000 — over-long values
refuse rather than truncate), 50 fields per card.

- `GET /api/v1/contacts` — `contacts:read` → `{"contacts": [contact…]}`, aggregated
  across every drive the owner can reach (shared drives are shared address books)
- `GET /api/v1/contacts/{drive}/{node}` — `contacts:read` → the contact
- `POST /api/v1/contacts` — `contacts:write`; body = the fields (+ optional
  `driveId`; default = the personal drive's Contacts folder, created lazily) →
  `201` + the contact
- `PUT /api/v1/contacts/{drive}/{node}` — `contacts:write`; body replaces the
  card's fields → the contact
- `DELETE /api/v1/contacts/{drive}/{node}` — `contacts:write` → `{"deleted": true}`

## Media

Catalogs derive from drive-level registered folders (join a shared drive and its
registered content appears in the union; leave and it's gone). Playable bytes and
drive-file art stream through `GET /api/v1/drive/download/{drive}/{node}` (Range
honored) with `drive:read` — catalog entries carry the `driveId`/`nodeId` to feed
it. Catalog and per-user state reads need `media:read`; the per-user playback
state a native player writes — progress heartbeats, watched marks,
watchlist/favorites, playlists — needs `media:write`. None of those rows grant
access: every read and stream re-checks drive membership, so a stale reference
simply drops out of view.

The catalog entry resource (children/detail rows share it):

```json
{"kind": "albums", "slug": "artist-one-album-alpha-1a2b", "title": "Album Alpha",
 "artist": "Artist One", "year": 2001, "items": 10,
 "driveId": "…", "nodeId": "…", "artNode": "…",
 "artUrl": "/api/v1/media/art/…", "addedAt": "2026-07-01T12:00:00Z"}
```

`kind` is one of `albums|artists|tracks|movies|series|episodes`. Tracks/episodes
carry `nodeId` (the playable file), `track` or `season`/`episode`; series carry
`seasons`/`parts`; artists carry `refs` (their album slugs). Art: `artUrl` serves
embedded ID3 cover art under this scope; `artNode` is an ordinary drive file
(poster.jpg / cover.jpg / frame captures) for `drive:read` download.

- `GET /api/v1/media/folders` — the caller's registered-folder union →
  `{"folders": [{"driveId", "folderId", "kind": "video|music", "name",
  "driveName", "hidden", "items", "scannedAt"}]}` (`hidden` is the caller's own
  per-user override). Registration kinds are a set: a folder registered as both
  video AND music yields one entry per kind
- `GET /api/v1/media/catalog/{drive}/{folder}` — one folder's catalog →
  `{"kind": "video|music", "kinds": ["video|music"…], "entries": [entry…]}`
  (`kinds` is the folder's registration set; `kind` is its first entry, kept
  for older clients); default = the top-level kinds of every registered kind
  (albums+artists and/or movies+series); `?kind=` picks any one catalog kind
- `GET /api/v1/media/entry/{drive}/{folder}/{kind}/{slug}` — one top-level entry
  (`albums|artists|movies|series`) → `{"entry": entry, "children": [entry…]}`
  (album → its tracks, series → its episodes, artist → its albums, movie → `[]`)
- `GET /api/v1/media/progress` — the caller's playback state, newest first →
  `{"progress": [{"driveId", "nodeId", "kind": "video|music", "title", "pos",
  "dur", "watched", "at"}]}`; `?kind=` filters; rows whose drive access is gone
  are omitted
- `PUT /api/v1/media/progress` — `media:write`; one playback heartbeat, body
  `{"driveId", "nodeId", "kind": "video|music", "title", "pos", "dur"}` → the
  stored progress row (a manual watched mark survives later heartbeats); the
  played file's drive access is checked up front (404 without it)
- `POST /api/v1/media/watched` — `media:write`; body `{"driveId", "nodeId",
  "watched": true|false}` → `{"watched"}`; off clears the position too
  ("watch it fresh")
- `GET /api/v1/media/lists/{list}` — `media:read`; `list` ∈
  `watchlist|favorites` → `{"items": [{"driveId", "folderId", "kind":
  "movies|series|albums|artists", "slug", "title", "at"}]}`, newest first;
  rows whose folder access is gone are omitted
- `PUT /api/v1/media/lists/{list}` — `media:write`; body `{"driveId",
  "folderId", "kind", "slug", "title", "on": true|false}` → `{"on"}`
- `GET /api/v1/media/playlists` — `media:read` → `{"playlists": [playlist…]}`,
  name-sorted. The playlist resource: `{"id", "name", "tracks": [{"driveId",
  "nodeId", "title", "artist"}], "at"}` — tracks reference playable files
  directly, so playlists span folders and survive catalog rescans; playback
  re-checks access via `/drive/download`
- `POST /api/v1/media/playlists` — `media:write`; body `{"name"}` → `201` the
  empty playlist
- `GET /api/v1/media/playlists/{id}` — `media:read` → the playlist
- `PATCH /api/v1/media/playlists/{id}` — `media:write`; body `{"name"?,
  "tracks"?}` — either or both; `tracks` REPLACES the list (fetch, reorder
  locally, PATCH back; capped at 500) → the updated playlist
- `DELETE /api/v1/media/playlists/{id}` — `media:write` → `{"deleted": true}`
- `GET /api/v1/media/art/{drive}/{folder}/{slug}` — harvested ID3 cover art
  bytes (`image/*`; 404 = none — fall back to `artNode` or an initial)

## Messenger

A conversation id (`cid`) is a server channel's id, a DM's `dm_<lo>_<hi>`, or a
group DM's `g<id>`. Only conversations the key's owner may see are ever returned.

- `GET /api/v1/messenger/servers` — `messenger:read` →
  `{"servers": [{"id", "name", "visibility": "open|invite", "owner"}]}`
- `GET /api/v1/messenger/servers/{server}/channels` — `messenger:read`; viewable
  channels only → `{"channels": [{"id", "name", "topic", "category"}]}`
- `GET /api/v1/messenger/servers/{server}/members` — `messenger:read` →
  `{"members": [{"username", "status": "online|away|dnd|offline", "roleIds"}]}`
- `GET /api/v1/messenger/channels/{cid}/messages` — `messenger:read`; newest-first,
  `?cursor=&limit=` → `{"messages": [{"id", "cid", "author", "body", "html", "ts",
  "edited", "deleted", "attachments": [{"name", "size", "contentType", "url"}],
  "inviteCode"}], "nextCursor"}`
- `POST /api/v1/messenger/channels/{cid}/messages` — `messenger:write`; body
  `{"body": "…"}` → `201` the created message resource (mentions `@user`/`@here`/
  `@channel` are parsed server-side; markdown is rendered safely)
- `POST /api/v1/messenger/channels/{cid}/read` — `messenger:write`; advance the
  read marker → `{"ok": true}`
- `GET /api/v1/messenger/channels/{cid}/typing` — `messenger:read`; who is
  typing right now, excluding the caller → `{"typing": ["user"…]}`
- `POST /api/v1/messenger/channels/{cid}/typing` — `messenger:write`; record a
  short-lived typing signal for the caller → `{"ok": true}`
- `PATCH /api/v1/messenger/messages/{msg}` — `messenger:write`; body
  `{"body": "…"}`; author-only (moderators delete, they don't edit) → the
  edited message resource
- `DELETE /api/v1/messenger/messages/{msg}` — `messenger:write`; the author
  always may; in a channel a moderator (manage-messages) may remove anyone's
  (audited) → `{"deleted": true}`. Deletes tombstone — scrollback stays
  contiguous
- `GET /api/v1/messenger/dms` — `messenger:read` → `{"conversations": [{"cid",
  "kind": "dm|group", "other", "name", "unread", "mention", "lastMsgTs"}]}`
- `POST /api/v1/messenger/dms` — `messenger:write`; body `{"user": "bob"}` →
  `{"cid"}` (idempotent; opens or returns the existing DM)
- `GET /api/v1/messenger/unread` — `messenger:read` → `{"unread": [{"cid", "count",
  "mention", "serverId", "kind", "lastTs"}]}`
- `GET /api/v1/messenger/search?scope=&server=&channel=&q=` — `messenger:read`;
  `scope` ∈ `all|dms|server|channel`; `q` supports `from:user has:file
  before:/after:YYYY-MM-DD` → `{"hits": [{"message", "serverId", "where"}]}`
- `GET /api/v1/messenger/presence` — `messenger:read` → `{"status", "message"}`
- `PUT /api/v1/messenger/presence` — `messenger:write`; body `{"status":
  "online|away|dnd|invisible|offline", "message": "…"}`
- `POST /api/v1/messenger/join/{code}` — `messenger:write`; redeem an invite →
  `{"server": {…}}`

- `GET /api/v1/messenger/att/{cid}/{blob}` — `messenger:read`; one attachment's
  bytes (message resources carry this URL in `attachments[].url`); whole-blob
  stream, no Range

Live updates for API clients are polling-based in v1: `GET /messenger/unread`
and the per-conversation typing endpoint are cheap to poll. (The SSE stream at
`/messenger/events` is session-authed and serves the web app only.)

## Git

Scopes `git:read` / `git:write` cover both the JSON endpoints under
`/api/v1/git/…` (repos, grants, orgs, members, teams, issues, merge
requests, the git profile) and git's own smart-HTTP wire protocol — one
credential works as a bearer token *and* as the Basic-auth password for
`git clone`/`push`. After the scope check every repo route resolves the
caller's repo role; private-no-access answers 404, never 403. Raw git
data rides the wire protocol, not JSON. The endpoint list and the
credential walkthrough live in [gitservices.md](gitservices.md).
