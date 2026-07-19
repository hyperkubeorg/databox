# Messenger

A group chat app inside PCP: **servers** (joinable groups) with
**channels**, per-server **roles**, **direct** and **group** messages,
presence, mentions, safe markdown, attachments, and search. Built on the
same databox substrate as the rest of PCP; no extra infrastructure.

## Enabling

On by default — Messenger needs no gateway or external service. An admin can
turn it off in **Admin → Site → Messenger**; off hides it from the launcher
and the app switcher and 404s its routes.

## Servers, channels, roles

- **Servers** are **open** (discoverable via the compass in the rail —
  `/messenger/browse` — anyone may join) or **invite-only** (hidden; join
  via an invite code). The creator is the owner.
- The **servers rail** (Direct Messages, your servers, create, discover,
  your status) persists on every messenger surface — discovery, search,
  settings, profiles, and invite pages render beside it, so you never
  lose your place.
- **Server-level actions** cluster under the server's name: click it to
  open the menu with **Invite people**, **Server settings** (management
  permissions only), and **Leave server**.
- **Channels** group discussion; a channel may be **private** to a set of
  roles. Every server keeps at least one channel.
- **Roles** are per-server permission grants. The base `@everyone` role is
  created with the server. A member's effective permissions are the union of
  their roles. Permission bits: view channel, send, manage messages, attach
  files, embed links, mention-everyone (`@here`/`@channel`), create invite,
  manage channels, manage roles, kick, ban, manage server. The **owner** and
  any **PCP admin** hold everything — admins can moderate any server they
  visit, and every privileged action is audited.
- **Server settings** (in the server-name menu, shown to anyone holding a
  management permission) is the admin console: rename the server,
  edit its description and visibility, rename/retopic/delete channels, edit
  roles and their permission grants, assign roles to members, **kick / ban /
  unban**, list and **revoke invites**, and — owner only — **transfer
  ownership** or **delete the server**. Each section appears only when your
  permissions unlock it, and every mutation re-checks server-side.

## Messages

Safe markdown: **bold**, *italic*, ~~strike~~, `code`, fenced code blocks,
> blockquotes, - lists, ||spoilers||, autolinked URLs, and `@mentions`. All
input is HTML-escaped first and only known-safe tags are emitted — no
sanitizer round-trip needed. Edit is author-only; delete tombstones the
message (moderators with *manage messages* can delete anyone's).

Attach files from your PC or from Drive; images render inline, everything
else as a download chip. Attachment bytes are charged to your quota.

## Direct & group messages

DMs open lazily on first message (a deterministic id, so either party
reaches the same conversation). Start one from the Home column's **+ New**,
or — while chatting in a server — **click anyone in the member list**: the
popover offers **Message** (jumps straight into the DM), **View profile**,
and, for moderators, **Kick / Ban**. Group DMs have an explicit roster and
an editable name; leave at any time.

The composer sends on **Enter** (Shift+Enter for a newline) — there is no
Send button.

## Presence

Set your status to **Online**, **Away**, **Do Not Disturb**, **Invisible**,
or **Offline**. Invisible and Offline appear offline to others; DND
suppresses your own mention notifications. Connection is tracked by the live
stream's heartbeat, so presence self-heals if a tab closes. Member lists pull
online people to the top.

## Mentions & notifications

`@username` pings one member; `@here` pings connected members; `@channel`
(and `@everyone`) ping all — the last two require the *mention everyone*
permission. A mention flags the recipient's unread badge red and raises a
platform notification (suppressed while they're on Do Not Disturb).

## Unread & real-time

Every message fans out a tiny unread bump to each member, so badges update
live over one SSE stream per open page. Very large servers fall back to
derived unread (the cap is logged, never silent). The stream also carries
live message delivery, typing indicators, and presence changes.

## Search

A maintained inverted index — each message write emits term postings — powers
`/messenger/search`. Scope to **this channel**, **this server**, **all my
servers & DMs**, or **DMs**. Operators: `from:user`, `has:file`,
`before:`/`after:YYYY-MM-DD`. Search only ever reads conversations you're in.

## Connections

A user's profile (`/messenger/u/<name>`) shows their card and the **servers
you have in common** — the intersection of your memberships, hiding
invite-only servers you're not in.

## Invites

Members with the *create invite* permission mint codes (optional expiry and
use limit) and can drop them into any conversation as a Join embed. Redeeming
joins the server.

## API & future clients

Everything above is available over the bearer API (`messenger:read` /
`messenger:write`) under `/api/v1/messenger/` — see `docs/api.md`. The API is
a first-class peer of the web app, sized so a native phone client is a
straightforward consumer: list servers/channels/DMs, page messages, send,
mark read, set status, search, and redeem invites. Live updates ride the
`/messenger/events` SSE stream.
