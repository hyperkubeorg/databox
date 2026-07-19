# The admin console

The management reference for the household admin. See also
[README](../README.md) · [deployment.md](deployment.md) ·
[usage.md](usage.md) · [api.md](api.md) ·
[postoffice.md](postoffice.md) · [cloudferry.md](cloudferry.md).

## Who this is for

You're the household admin: the most capable *user* of this install,
not a sysadmin. The console assumes that. It's built so you spot and
fix a problem **before someone tells you about it** — one task type per
page, wizards for anything multi-step, and health in plain sentences.
Nothing here needs a shell.

Running the machines, wiring databox, and setting the environment are
in [deployment.md](deployment.md); day-to-day member usage is in
[usage.md](usage.md).

## Getting in

The console is at **`/admin`** (every route is admin-only). You become
an admin one of two ways:

- **First account wins.** The first person to sign up on a fresh deploy
  is made admin automatically (decided inside the signup transaction, so
  racing first signups resolve to exactly one admin).
- **`PCP_ADMIN=username`** promotes a named account to admin as soon as
  it exists — the process polls, so you can set the variable *before*
  that person signs up. Handy when open signup created the first account
  and it's the wrong one.

After that, admins promote and demote each other from **People → Users**
(you can't demote yourself). There is no separate "root" — admin is a
bit on an ordinary account.

## Console layout

A left rail of task pages. Each page does **one category of task**;
entities (a user, a domain, a gateway) get their own detail pages. The
rail badges **Home** with the count of open warn/critical problems.

| Section | Pages |
|---|---|
| **Home** | Health overview: open problems first, then a traffic-light row per area |
| **People** | Users (+ per-user detail) · Invites |
| **Storage** | Tiers & quotas · Usage · Git organizations |
| **Site** | Branding · signup mode · Git Services |
| **Mail** | Domains · Post offices · Addresses · Aliases · Distribution lists · Welcome messages · Sending policy (seven pages) |
| **Web access** | Gateways · Hostnames & certificates · Offline page |
| **Security** | Audit log · IP bans |
| **System** | Workers · Databox (view-only) · Problems |

Contrast the **seven separate Mail pages** with a single crammed tab:
each is a short focused form. Anything multi-step is a **guided wizard**
that verifies each step live — a mail domain shows its MX/SPF/DKIM/DMARC
records with copy buttons and checks them against real DNS; a gateway
pairing walks code → verify → hostname → TLS mode → DNS check →
first-request probe. DNS lookups run behind a resolver seam, so a deploy
whose resolver is blocked degrades to an honest "couldn't look this up
from here" notice instead of a wall of red.

Every mutation is CSRF-checked and written to the audit log. Success and
error surface as a flash line at the top of the page.

## People

### Users

**Users** (`/admin/users`) is the account directory — searchable by
username or display name. Each account opens a **detail page**
(`/admin/users/{user}`) with the full admin view:

| Panel | What you can do |
|---|---|
| Profile | Display name, admin status, ban status, invite lineage |
| Drives & usage | The personal drive and any shared drives, with bytes used |
| Sessions | Every active login; **revoke all** signs the account out everywhere |
| Connected-from IPs | Addresses this account has logged in from |
| Capabilities | The capability checkboxes (currently just **invite** — mint signup codes) |
| Tier | Assign a storage tier (or none) |
| Quota override | Per-account quota: blank = use tier/default, `unlimited`, or a byte count |
| Email allowance | How many email accounts this member may create — blank = site default, `none` = zero, or a number (0–100) |
| API keys | The member's scoped bearer keys; **revoke** kills one immediately (next request 401s) |

Actions on the account:

- **Ban / unban.** Banning revokes the account's sessions immediately.
  Tick *also ban IPs* to fan the ban out to every address the account
  has connected from (unban reverses it).
- **Promote / demote** admin (not on your own account).
- **Impersonate.** Mints a session *as* the member and drops your own —
  one browser, one identity. A banner shows you're impersonating, and
  the start, the stop, and everything auditable in between are logged.
  **Stop impersonating** returns you to your own account.
- **Delete.** Type the username to confirm. Deletion purges, in order:
  the member's mail addresses (so mail stops arriving), then the
  personal drive's files, sharing, and media catalogs, then API keys,
  then the account itself.

Quota and email-allowance resolution is covered under **Storage** and
**Mail → Addresses** below.

### Invites

**Invites** (`/admin/invites`) lists every invite on the site with its
redemption ledger (who signed up, when, from where) and its live status.
Three kinds:

- **Quantity** — admits a fixed number of signups (1–10000), first come
  first served.
- **Time** — admits any number until an expiry you set.
- **Permanent** — admits until revoked. A standing door; **admins only**.

Any member is capped at **100 invite records** (revoked and spent ones
count — the cap bounds the record set). Admin-minted invites **require a
description** — a standing door needs a written "why". You can revoke
anyone's code from here.

**How invites interact with signup modes** (set on **Site**): admins can
always mint, in every mode (so codes can be staged before you flip the
gate). Ordinary members can mint only in **invite** mode, and in
**trusted-invite** mode only if they hold the **invite** capability. In
**open** and **admin-invite** modes, members can't mint at all.

## Storage

**Tiers & quotas** (`/admin/tiers`) is where storage limits live:

- **Tiers** are named quota levels (a tier needs a quota — a byte count
  or `unlimited`). The page shows how many accounts sit on each tier
  before you remove one. Assign a tier to a member on their user page.
- **Site default quota** applies to anyone with no tier and no override.
- **Max upload** (`PCP_MAX_UPLOAD`, default 5 GiB) caps a single request
  body; set an override here.

A member's **effective quota** resolves in order: **per-user override →
tier → site default → the `PCP_DEFAULT_QUOTA` bootstrap value**. The
first level that's set wins; `unlimited` at any level means no limit.
Changes are live on the next request — nothing re-provisions.

**Usage** (`/admin/usage`) is the overview: total bytes stored, member
count, total promised quota, and the top 25 accounts by usage with a
percent-of-quota bar. Git organizations appear as their own rows —
they are quota-bearing like accounts. A member's `UsedBytes` already
includes their personal git repositories.

**Git organizations** (`/admin/gitorgs`, shown while Git Services is
enabled) sets each org's quota tier or byte override — the same
resolution order as user quotas. Org membership and settings are
self-serve inside the Git app; only the quota levers live here. Changes
are audited.

## Site

**Branding & signup** (`/admin/site`):

- **Site name** — the brand shown in the launcher, page titles, and auth
  screens (≤40 characters; defaults to "Personal Cloud Platform").
- **Signup mode**, each explained in plain language so the choice is a
  sentence, not a constant:

| Mode | Meaning |
|---|---|
| **Open** | Anyone who can reach the site may create an account. |
| **Invite** | New accounts need an invite code — any member can mint one. |
| **Trusted invite** | Need a code — only members you've granted the *invite* capability (and admins) can mint. |
| **Admin invite** | Need a code — only admins can mint. |

(`PCP_SIGNUP_MODE` seeds the mode on a *fresh* deploy only; a saved
choice here always wins over the environment.)

**Git Services** (`/admin/site/git`): the master switch (off by
default — off makes every `/git/` route indistinguishable from
unbuilt), the public-repositories switch (off forces all repos private
and 404s every anonymous git page and clone), and the tunnel-side git
push/fetch body cap (default 1 GiB — keep it consistent with each
gateway's edge cap under Web access). Full feature guide:
[gitservices.md](gitservices.md).

## Mail administration

Mail is a gateway architecture: PCP runs behind NAT and can't speak SMTP
to the internet, so a **post office** binary on a public host carries the
mail. It's blind at rest — only PCP can read what it holds. The gateway
internals, the pairing protocol, and the on-host runbook are in
[postoffice.md](postoffice.md); this section is the console side. Seven
pages:

### Domains

**`/admin/mail/domains`** lists your hosted domains; adding one drops you
straight into its **setup wizard** (`/admin/mail/domains/{domain}`). The
wizard is a DNS record sheet built from what PCP actually knows —
authorized post offices, their reported public IPs, and the domain's
DKIM key — with **copy buttons** and **live verification** (a *check*
button resolves each record and marks it ✓, missing, or differing):

- **MX** — one per authorized post office (priority ascends per gateway).
- **SPF** (TXT) — lists every gateway's real public IPv4 **and** IPv6,
  falling back to `a:<host>` until a gateway has been polled.
- **DKIM** (TXT at `<selector>._domainkey`) — the domain's signing key.
- **DMARC** (TXT at `_dmarc`) — a `p=quarantine` starting policy; any
  valid DMARC record you publish counts.
- **Reverse DNS (PTR)** — per sending IP, forward-confirmed (the name
  must resolve back to the same IP). Set it in your server provider's
  panel; major mailbox providers reject mail from IPs with no PTR.

Enable the domain from here once the records verify.

### Post offices

**`/admin/mail/postoffices`** lists paired gateways; each opens a detail
page. While **pending**, that page is the **pairing wizard** — it shows
a paste-me setup code; you paste the gateway's completion code back to
finish. Once **active** it's the operations dashboard:

- **Live status** polled on load (self-reported summary — never stale).
- **Sparklines** from stored status samples: spool depth, outbound
  queue, pending events, recent-error count.
- **Config drift** flag when the running serial lags what PCP pushed.
- **Re-push** — clears the recorded push fingerprint so the next sync
  sweep re-sends config *and* DKIM keys (the fix for "keys awaiting
  re-push").
- **Domain authorization** with per-domain MX priority.
- **Endpoint** and **spool cap** settings.
- **Disable** (stop routing through it) and **Repair** (mint a fresh
  pairing code — revokes the old identity, for when a gateway is lost).
- **Delete** (also drops its history samples).

### Addresses

**`/admin/mail/addresses`** lists every mailbox on the site. You can
**assign** a mailbox to a member directly — but this respects their
**email-account allowance**: if they've used all their allowed accounts,
you raise the allowance on their user page first (blank = site default,
a number = that many, `none` = zero). The site-wide default lives on
**Sending policy**. This is the **self-service** hinge: members create
their own email accounts up to their allowance, so you set a limit once
instead of hand-assigning every address. Deleting an address removes the
email account and its message store.

### Aliases

**`/admin/mail/aliases`** — forwarding addresses that point at a target,
assigned to a member (capped per user by the alias limit on Sending
policy). Retarget or delete in place.

### Distribution lists

**`/admin/mail/distros`** — a list address with **members** (who
receives) and **allowed senders** (who may post to it). Create, update
members/senders, or delete.

### Welcome messages

**`/admin/mail/welcome`** — automatic messages delivered to new
mailboxes, with a scope (all, or a specific domain), from-address,
subject, body, an enabled toggle, and an ordering.

### Sending policy

**`/admin/mail/sending`** is the mail feature switch plus every
site-wide limit. Turning it **off** hides the Email app and stops the
intake/outbound/sync loops. Fields (blank reads as the default shown):

| Setting | Default | Meaning |
|---|---|---|
| Default mailbox allowance | 0 | Email accounts every member may create (0 = grant-only) |
| Max aliases | 10 | Aliases per member |
| Max message size | 25 MiB | One message, attachments included |
| Send per day | 500 | Outbound cap per member per day |
| Send burst | 20 | Outbound cap per member per minute |
| Trash retention | 30 days | Auto-purge window for Trash |
| Spam tag / reject | 5 / 15 | spamd scores: ≥ tag → Spam folder, ≥ reject → refused at SMTP |
| RBL zones | — | DNSBL zones the gateways query at connect (e.g. `zen.spamhaus.org`) |
| spamd address | — | Optional SpamAssassin `host:port` the gateways score through |

## Web access (cloudferry)

Public web access uses the same blind-gateway model as mail: a
**cloudferry** binary on a public host, dialed out from PCP, carries
visitors and serves TLS with certificates PCP issues. Gateway internals
and the on-host runbook are in [cloudferry.md](cloudferry.md). Three
pages:

### Gateways

**`/admin/webaccess/gateways`** lists gateways; each opens a detail page
that is the **pairing wizard** while pending (**code → verify →
hostname → TLS mode → DNS A/AAAA check → first-request probe**, walkable
on the one page) and the dashboard once active:

- **Live status**, current **tunnels**, and this replica's local tunnel
  count.
- **Sparklines**: tunnels, requests, 5xx responses, recent errors.
- **Config drift** flag and **Re-push** (re-sends config *and* serving
  certificates).
- **Edge limits** — max concurrent connections, per-IP requests per
  minute, max request body (MiB), and max **git** push/fetch body (MiB,
  default 1 GiB — applies to `/git/…/git-upload-pack` and
  `…/git-receive-pack` instead of the general cap, and pairs with the
  tunnel-side cap on Site → Git Services); blank/0 = the gateway's
  defaults.
- **TCP relays** — raw port passthrough: public edge port → target port
  on the PCP host (e.g. SSH: edge `22` → a local sshd/git daemon on
  `4222`). Rows show the live listener state, active connections, and
  relayed bytes from the gateway's self-report; add (edge port, target
  port, label) and remove push within seconds. The configured list is
  also the PCP-side dial allowlist. See
  [cloudferry.md](cloudferry.md#tcp-relays) for limits, blindness, and
  the privileged-port (`CAP_NET_BIND_SERVICE`) note.
- **ACME directory** URL, **disable/enable**, **repair**, **delete**.

### Hostnames & certificates

**`/admin/webaccess/hostnames`** is the routing table: each hostname
maps to a gateway with a **TLS mode**:

- **acme** — automatic certificates (Let's Encrypt-style); renewals are
  hands-off.
- **selfsigned** — a self-issued cert (dev / internal).
- **custom** — you upload cert + key PEM (switch the hostname to custom
  first, then upload).

Rows show **certificate expiry and source**, and a **force-HTTPS**
toggle redirects plain HTTP. Edge limits are per-gateway (above).

### Offline page

**`/admin/webaccess/offline`** edits the HTML visitors see when a
gateway is up but PCP is unreachable, with a live preview.

## Security

**Audit log** (`/admin/audit`) is every mutation the console (and the
API) performs — paged, **filterable by actor and action**, with a **CSV
export** (columns: time, actor, actor-is-admin, action, target, detail,
IP, impersonating). Entries are immutable through the app; the only
pruning is **retention**, tunable here by **age** (days; default 90) and
**entry count** — a shrunk window applies immediately.

**IP bans** (`/admin/ipbans`) lists banned addresses; ban or unban a
single IP. Per-user IPs live on the user detail page, and banning an
account can fan out to all its addresses (see People → Users).

**Two-factor auth** is member-managed (Settings → Two-factor
authentication; TOTP with one-time recovery codes). The user detail page
shows whether it's on and carries **Reset two-factor auth** — the lever
for a member who lost both their authenticator and their recovery codes.
The reset clears everything 2FA (they can re-enroll from Settings) and
is audited (`user.totp.reset`), as are member-side enables and disables
(`user.totp.enable` / `user.totp.disable`).

## Observability & health

### The health worker and the problems model

A background **health worker** re-evaluates every check on a **60-second
cadence** (a databox-lock singleton — one replica sweeps at a time). It
never polls gateways itself: the mail and web sync loops persist a status
**sample** on every poll, and the worker reads the stored records and
samples.

A failing check becomes a **problem**: a **severity**
(**info / warn / critical**), one sentence saying **what's wrong**, one
saying **what to do**, and a link to the page that fixes it. Problems:

- **surface** on Admin **Home** (open problems first, then a
  traffic-light row per area), badge the launcher's **Admin card** and
  the rail's Home entry with the open warn+critical count, and **notify
  every admin** through normal notifications when something
  warn-or-worse *opens* (deduped per problem per 24h so a flapping check
  can't page you repeatedly);
- **auto-resolve** the moment the check passes, kept 24h as a "recently
  resolved" tombstone (for context), then pruned;
- keep their **"since"** across re-raises, so *how long* it's been wrong
  survives.

The honest workflow: do nothing until notified, then read the sentence
and follow it. **Problems** (`/admin/system/problems`) lists open and
recently-resolved findings and has a **re-check now** button.

### What each check means

| Check | Severity | Meaning / action |
|---|---|---|
| Post office unreachable | critical | No status poll for ~90s — mail through it is stopped. Check the machine is up; sync reconnects on its own. |
| Web gateway unreachable | critical | Same, for web — its hostnames show the offline page. |
| No live tunnels | critical | Gateway is up but no PCP replica holds a tunnel — visitors get the offline page. Check PCP can reach the tunnel endpoint. |
| Config drift | warn | Running an older config than PCP pushed for >5 min — use **Re-push** on its page. |
| Keys awaiting re-push | warn | Restarted and lost RAM-only keys (DKIM / serving certs) — re-push is automatic within a minute. |
| Queues growing | warn (mail) | Spool or outbound queue grew three polls running — read the post office's recent errors; delivery may be failing downstream. |
| New errors every poll | info | The gateway's error ring is filling — read its recent-errors list. |
| Certificate expiring | warn → critical | Warn 14 days out, critical inside 3 days or past expiry. ACME renews automatically (a countdown that keeps counting usually means DNS no longer points at the gateway); custom certs need a fresh upload. |
| Replica stopped heartbeating | warn | A PCP process missed its heartbeats (>2 min) — it likely crashed (a clean shutdown removes its own record). Check the process/pod. |
| Background loop failing | warn | A loop's newest run errored; its own message is quoted. Loops retry on their cadence. |
| Media scan stale | warn | Registered media folders haven't re-scanned in 15 min — new files won't appear in Video/Music. Check the mediascan loop. |
| Storage near cap | warn | Members have used ≥90% of the storage the site promised — raise tiers, trim quotas, or add databox capacity. |

**Databox checks** (only when PCP's databox user may read cluster
metadata — see below): node reported offline by databox's own liveness
verdict (critical); raft group with
no recent leader (warn); group under-replicated below the target replica
count (warn); shard mid-split, escalating to warn past 30 min (info);
automation paused (info); plus any alert the cluster itself raises. PCP
**never mutates** the cluster — each of these names the exact `databox`
CLI command that acts on it.

### Workers

**`/admin/system/workers`** is one row per moving part: each paired
gateway (reachability, config serial / drift, key freshness, a spark),
every background loop (last run / last success / last error), and every
PCP replica (heartbeat). Gateway rows open the detail page with the full
sparklines — built from the samples PCP already stores (a day of
history), enough for "when did this start" without a metrics stack.

### The Databox panel

**`/admin/system/databox`** renders databox's own cluster metadata —
node/group/shard tables, a summary line (nodes · groups · shards ·
bytes), offline-node count (databox's liveness verdict), and admin pause
flags — strictly **read-only**.
PCP performs **no databox mutations**; every condition names the
`databox` CLI command to run, and the health checks above point here.

Most deploys run PCP as a **scoped databox user** that *cannot* read the
cluster's system view. That's expected: the panel then shows a
plain-language notice ("grant it databox admin, or use `databox cluster
status` directly") instead of failing, and the databox health checks
simply skip. For acting on cluster problems, see the databox admin docs
at [`docs/admin/`](../../../docs/admin/).

## Backups & data

PCP is stateless — every user, session, file byte, mail message, and
calendar event is a record or blob under **`/pcp/`** in databox.
Replicas are interchangeable and hold nothing durable. **There is no
separate PCP backup:** to protect the install, **back up databox**. See
the databox backup and restore guide at
[`docs/admin/backup-restore.md`](../../../docs/admin/backup-restore.md).
The blind gateways (post office, cloudferry) hold only transient,
at-rest-encrypted spool and no long-term state — they need no backup of
their own.
