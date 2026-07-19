# personalcloudplatform

Personal Cloud Platform (PCP): a self-hosted consumer ecosystem on
databox, and the flagship example of building a real application on it.
Login lands on a launcher of six apps; every user, session, file byte,
mail message, calendar event, and chat message is a record or blob under
`/pcp/`, so the app is stateless and replicas are interchangeable.

## Documentation

| Page | For | Contents |
|------|-----|----------|
| [docs/deployment.md](docs/deployment.md) | operators | single-host quickstart (no replication/EC), migrating to a 3+ node cluster, production checklist, Kubernetes |
| [docs/usage.md](docs/usage.md) | everyone | end-user guide to every app — Drive, Email, Calendar, Contacts, Video, Music, API keys |
| [docs/admin.md](docs/admin.md) | admins | the admin console: people, storage/tiers, mail, gateways, security, health & observability, backups |
| [docs/messenger.md](docs/messenger.md) | everyone | the Messenger app — servers, channels, DMs, presence, mentions, search, invites, API |
| [docs/gitservices.md](docs/gitservices.md) | everyone | Git Services — repos, orgs/teams/roles, clone/push, issues & MRs, public repos over the ferry, quotas, GC, API |
| [docs/smarthome.md](docs/smarthome.md) | everyone | Smart Home — cameras & doorbells, the pcp-camd agent, recording, timeline review, clips, retention, deletion tools |
| [docs/api.md](docs/api.md) | developers | REST API v1 — scoped bearer keys, every endpoint, response shapes |
| [docs/postoffice.md](docs/postoffice.md) | operators | the mail gateway — protocol, trust model, pairing, ops |
| [docs/cloudferry.md](docs/cloudferry.md) | operators | the web gateway — tunnel, ACME, offline page, pairing, ops |

New here? [docs/deployment.md](docs/deployment.md) stands it up,
[docs/usage.md](docs/usage.md) covers using it, [docs/admin.md](docs/admin.md)
covers running it.

## What it is

- **Drive** — files, folders, shared drives (owner/editor/viewer),
  public links + per-user grants, versions with preview-before-restore,
  chunked resumable uploads, zip downloads, content search, and five
  collaborative editors (spreadsheet, document, markdown, diagram,
  kanban) on one LWW-CRDT substrate. Folders register as Video/Music
  content for the media apps.
- **Email** — full webmail on the postoffice gateway architecture:
  threaded conversations, labels, custom folders, starring, docked
  rich compose with attachments both directions (upload / attach from
  Drive / save to Drive), undo send, drafts, search operators
  (`from:` `to:` `label:` `in:` `has:file`), live updates over SSE.
- **Calendar** — calendars are collaborative files; aggregated
  month/week/day views, invites with RSVP, ICS mail round-trip with
  external addresses. **Contacts** shared with mail's typeahead.
- **Video / Music** — streaming-service views over registered Drive
  folders: series/movies detection + ID3 album/artist catalogs
  (rebuildable caches; the files stay the only truth), resume,
  watchlists, playlists, a persistent mini-player.
- **Messenger** — a group chat app (`docs/messenger.md`):
  servers with channels and per-server roles, direct + group messages,
  presence (Online/Away/DND/Invisible/Offline), `@here`/`@channel`/
  `@user` mentions, safe markdown, attachments from PC or Drive, live
  delivery + unread badges over SSE, a maintained inverted-index search,
  server invites, and shared-membership profiles.
- **Git Services** — git hosting (`docs/gitservices.md`, off by
  default): user + organization namespaces, teams and per-repo grants,
  stock `git clone`/`push` over smart HTTP with API-key credentials,
  issues and merge requests (file-level merge), forks that share
  objects, per-namespace quotas, repo GC, and public repos + opt-in
  profiles served anonymously through cloudferry.
- **Smart Home** — surveillance camera and doorbell storage
  (`docs/smarthome.md`, off by default): spaces of devices fed by the
  standalone `pcp-camd` agent (RTSP → segments, pushed out over HTTPS,
  disk-spooled through outages), live view with a coverage-honest
  timeline/scrubber, motion/ring events with notifications and typed
  search, retention with clip pinning, and first-class footage
  deletion tools.
- **API v1** — everything above over REST with scoped bearer keys
  (`docs/api.md`); response shapes are test-covered.
- **Admin console** — task pages per category, guided wizards (mail
  domain DNS, gateway pairing), plain-language health with a problems
  model, worker observability with history sparklines, and a view-only
  databox cluster panel. See `docs/admin.md`.

Two blind gateways carry the public traffic — PCP dials out, nothing
dials in, and neither gateway can read what it carries at rest:

- **postoffice** — SMTP in/out ([docs/postoffice.md](docs/postoffice.md))
- **cloudferry** — web visitors over a multiplexed tunnel, with ACME
  certs issued by PCP ([docs/cloudferry.md](docs/cloudferry.md))

## Quickstart

This is the from-source development loop. To **deploy**, run the
published containers (`ghcr.io/hyperkubeorg/{pcp,postoffice,cloudferry,pcp-runner}`)
with podman/docker or the Helm charts — [docs/deployment.md](docs/deployment.md).

A databox to talk to (single node is fine):

```sh
go run ./cmd/databox server --data-dir /tmp/dbx   # repo root; first run bootstraps
databox user create pcp
databox grant add pcp allow /pcp list,read,write,delete
databox grant add pcp allow /.databox list,read               # admin Databox panel
```

Then PCP:

```sh
DATABOX_ENDPOINT=localhost:8443 DATABOX_USER=pcp DATABOX_PASSWORD=… \
INSECURE_COOKIES=1 go run ./cmd/pcp
```

Open http://localhost:8080 and sign up — the **first account becomes
the admin** (`PCP_ADMIN=<name>` promotes a named account instead).
Everything else is configured from **Admin**: mail domains, gateway
pairing, signup mode, tiers, branding.

Gateways pair from the console, never by editing config on the box:
create the gateway in Admin (Mail → Post offices, or Web access →
Gateways), run `postoffice setup` / `cloudferry setup` on the cloud
host, paste the codes each way. Full runbooks: `docs/postoffice.md`,
`docs/cloudferry.md`.

Container/Kubernetes: `Dockerfile` builds four images (targets `pcp`,
`postoffice`, `cloudferry`, `pcp-runner`) — published multi-arch to
`ghcr.io/hyperkubeorg` by the release workflow; `charts/pcp/` (published
as `oci://ghcr.io/hyperkubeorg/charts/pcp`) deploys the app against a
databox chart release.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN` | `:8080` | listen address |
| `DATABOX_ENDPOINT` | `localhost:8443` | cluster host:port |
| `DATABOX_USER` | `pcp` | databox user (root works, warns loudly) |
| `DATABOX_PASSWORD` | — | that user's password |
| `DATABOX_CA_FINGERPRINT` | — | pin the cluster cert (recommended) |
| `DATABOX_REQUIRE_FINGERPRINT` | — | `1` = refuse to start unpinned |
| `TRUST_PROXY_HEADERS` | — | `1` = rate-limit by `X-Forwarded-For` (only behind a proxy that overwrites it; tunnel-served requests are always trusted) |
| `INSECURE_COOKIES` | — | `1` = non-Secure cookies (plain-HTTP dev) |
| `PCP_ADMIN` | — | username to promote to admin once it exists |
| `PCP_SIGNUP_MODE` | `open` | fresh-deploy signup mode (open\|invite\|trusted-invite\|admin-invite) |
| `PCP_DEFAULT_QUOTA` | 10 GiB | per-user quota in bytes (0 = unlimited) |
| `PCP_MAX_UPLOAD` | 5 GiB | max single request body in bytes |
| `PCP_GIT_GC_DEBOUNCE` | 30s | delay before the automatic repo GC after a force-push/branch delete (Go duration) |
| `PCP_GIT_SSH_ADDR` | `:4222` | git-over-SSH listen address (empty = SSH transport off) |

## Escape hatches

Your data is never trapped: the REST API covers files, mail, calendars,
contacts, and media with per-scope keys; downloads are plain HTTP
(Range-capable) and folders zip on the fly; documents export to
HTML/text, spreadsheets to CSV/XLSX, diagrams to SVG/PNG; messages
fetch as raw RFC 822; calendars speak ICS. Databox itself is yours —
`databox console -e 'list /pcp/'`.

## Layout

```
cmd/{pcp,postoffice,cloudferry}   the three binaries
cmd/smoke                         chained live-smoke harness (phases 3–8)
pkg/kernel/                       auth, sessions, routing, chrome, audit, SSE
pkg/domain/                       databox access, one package per domain (kvx
                                  holds the canonical key table)
pkg/apps/                         one package per app (handlers + templates + JS)
pkg/ui/                           design system: tokens, fonts, components
pkg/wire/                         sealing + signing shared by both gateways
pkg/postoffice + pkg/poclient     mail gateway + PCP-side client
pkg/cloudferry + pkg/cloudferryclient  web gateway + PCP-side client
pkg/mailer, pkg/ferry, pkg/health backgrounds: mail loops, gateway sync, health
charts/pcp/                       Helm chart
docs/                             deployment, usage, admin, api, postoffice, cloudferry
```

Tests: `go test ./...` — domain logic against an in-memory databox
fake, wire-format and trust-boundary suites for both gateways
(`pkg/{postoffice,cloudferry}/trustboundary_test.go`), API response
shapes, template sets. `cmd/smoke` drives the real binaries against a
real databox node end to end.

## License

[![GNU AGPLv3](AGPLv3_Logo.svg)](https://www.gnu.org/licenses/agpl-3.0.html)

[Third-party dependency license report](../../pkg/licenses/LICENSE-REVIEW.md) (also served in-app at `/licenses`)
