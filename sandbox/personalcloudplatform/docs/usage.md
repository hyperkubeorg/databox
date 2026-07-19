# Using Personal Cloud Platform

This is the guide for people who *use* PCP — the household members, not
the admin who runs it. Anything admin-controlled (email allowances, mail
domains, media scanning, tiers) is flagged and points at
[docs/admin.md](admin.md). For the platform overview see
[README.md](../README.md); to drive PCP from your own software see
[docs/api.md](api.md).

## Signing in & the launcher

Open the site and **sign up** (`/signup`). The **first account created
becomes the admin**; everyone after is a normal member. Whether new
signups are open or need an invite code is the admin's choice — if the
form asks for a code, ask them for one.

After you sign in, the **launcher** (`/`) greets you with a grid of app
cards — Drive, Email, Calendar, Video, Music (and an **Admin** card if
you're the admin). Each card shows a live one-liner: storage used,
unread count, your next event, what you're mid-watch on.

- **App switcher** — the **grid icon at the top-left** of every page
  opens a popover to jump between Launcher, Drive, Email, Calendar,
  Contacts, Video, and Music. (Contacts lives here, not on the launcher
  grid.)
- **Theme** — the **sun button** (top-right of the app bar, and the logo
  tile on the launcher) flips light/dark instantly. In Email you can
  also press **T**.
- **Settings** (`/settings`, or your avatar at top-right) — change your
  **display name**, **password**, and **theme**; toggle auto-subscribe
  to shared-drive calendars; see your **storage usage / quota / tier**;
  turn on **two-factor authentication**; review **sessions**; and manage
  **API keys** (see the last section). Your username is fixed.
- **Sessions** (`/settings/sessions`) — everywhere you're signed in,
  with a device label (browser + OS, parsed from each login), address,
  sign-in time, and expiry. The session you're looking from is marked
  **this device**; every other row has its own **Sign out** button, so
  one stale or unrecognized login can be revoked without touching the
  rest. Don't recognize a session? Sign it out, then change your
  password.
- **Two-factor authentication** — Settings → Two-factor authentication →
  **Turn on**. Add the shown secret to any authenticator app (open the
  `otpauth://` link or type it in), then confirm with the app's current
  code. Already have a secret (migrating from another setup, or a
  fixed-key token)? Paste its base32 form into the optional field before
  turning on and it's imported instead of generating a new one — the
  confirm step still proves a code first. You get **8 one-time recovery
  codes, shown exactly once** — save them. From then on, signing in asks for a 6-digit code (or a recovery
  code) after your password. Each code works once — if a code is refused
  right after enrolling, wait for the app's next one. Turning 2FA off
  requires your password. Lost the phone AND the codes? An admin can
  reset your 2FA from your user page.

## Drive

Drive opens in your **personal drive**. The left rail lists your drives,
**Shared with me**, a storage meter, and a search box (press **/** to
focus it).

**Browsing.** Folders and files show as a **grid** or **rows** — toggle
with the view button. Filter by kind (Folders, Photos, Videos, Music,
Documents, Other) and change the sort order. **Breadcrumbs** across the
top walk you back up the tree. On the desktop the browser behaves like a
file manager: **right-click** for a context menu, click/shift-click to
select, drag to move, double-click to open, and rename inline. The list
refreshes live as others make changes. (Everything also works without
JavaScript through each item's **Details** page.)

**Getting files in.** Click **Upload** (or **Upload folder**), or just
**drag files onto the window**. Uploads are **resumable** — big files
upload in chunks and the queue survives a reload, picking up where it
left off. **New folder** makes a folder.

**Getting files out.** Open a file to view it, or **Download** it.
**Download all** zips the current folder; select several items and
**Download** zips just those. Deletes are **permanent — there is no
trash.**

**Versions.** Every save keeps history. On a file's Details page,
**Version history** lists each revision; you can **Download** an old one,
or open it as a **read-only preview** in the editor and **Restore** it
(restoring writes a *new* version — history only moves forward).

**Sharing.** On a file or folder's Details page:

- **Public links** — anyone with the URL gets in (`/s/…`). Choose **View
  only** or **View & download**, and optionally set a **password** and an
  **expiry** (1 day / 1 week / 30 days). Revoke any link anytime.
- **Share with people** — grant a specific member **viewer** or
  **editor**; it appears in their *Shared with me*. Remove access
  anytime.

**Shared drives.** **New shared drive** makes a space you work in
together. In its **Drive settings**, the owner adds members as
**owner / editor / viewer**, renames, or deletes the drive (you must
type its name to confirm). Any member can **leave** a drive from the same
page.

**Collaborative editors.** PCP has five built-in editors, all with
**live multi-user editing** (you see others' cursors and changes in real
time):

| Editor | Make one (right-click → New…) | Exports |
|---|---|---|
| Spreadsheet | New spreadsheet | CSV, XLSX |
| Document | New document | HTML, plain text |
| Markdown | New markdown | HTML, plain text |
| Diagram | New diagram | SVG, PNG |
| Kanban board | New kanban board | — |

Click a document's **title** to rename it. Each **Export** can either
download the file or **save it back into the drive** as a sibling. A
`.csv` file can be **imported** into a live spreadsheet from its
right-click menu.

**Feeding Video & Music.** On a *folder's* Details page, **Use as Video
content** / **Use as Music content** registers it (the two are
independent — a mixed folder can feed both). Once registered, the folder
shows up in the Video/Music apps for **everyone on that drive**, and
**Rescan now** rebuilds its catalog on demand. You need **editor** access
on the drive to register a folder.

## Email

**Your address.** Email needs an address, and your admin sets how many
you may have (an **allowance** — see [docs/admin.md](admin.md)). Open
Email; if you have none or several, you land on the **account chooser**.
While your allowance lasts, **New address** lets you pick a name and a
domain (the picker checks availability as you type) and claims it
yourself — no admin round-trip. You can hold **several addresses** and
switch between them with the mailbox dropdown in the sidebar. Deleting an
address is admin-only.

**Layout.** Three panes: the **sidebar** (folders + labels), the
**message list**, and the **reading pane**. Conversations are
**threaded** — a whole back-and-forth reads as one item, collapsed
messages expand on click.

**Organizing.** System folders are **Inbox, Starred, Sent, Drafts,
Archive, Spam, Trash**; you can add your own **custom folders** (in Mail
settings). **Labels** are colored tags orthogonal to folders — a thread
can wear several. **Star** anything. The filter chips above the list
narrow to **All / Unread / Starred / Has files**.

**Search.** Type in the search box (or press **/**). Free words
AND-match; these operators refine:

- `from:` — sender · `to:` — recipient · `label:` — a label name
- `in:` — a folder (system name, a custom folder's name, or `starred` /
  `sent`) · `has:file` — has an attachment

**Compose.** The **Compose** button opens a docked window: **rich text**
(bold/italic/underline/strikethrough, lists, quote, links), **To** plus
**Cc/Bcc**, and a subject. Attach files **from this computer** or
**from Drive**. Incoming attachments have a **Save to Drive** button that
drops them into a folder you pick. Drafts save as you type; minimize or
discard the window anytime.

**Replying.** Each message has **Reply / Reply all / Forward**.

**Undo send.** If you've turned it on (Mail settings → Undo send: 10 or
30 seconds), a sent message is **held** for that window so you can pull it
back before it leaves.

**Keyboard shortcuts.** `/` search · **C** compose · **J** / **K** move
through the list · **E** archive the open thread · **S** star it · **T**
toggle theme.

**Calendar invites.** An invitation email shows an **invite card** — RSVP
**Accept / Maybe / Decline** right from the message, and it syncs to the
event.

**Mail settings** (`/mail/settings`) also holds per-mailbox
**signatures**, **label** and **custom folder** management, and shows the
platform-wide **Trash retention** period.

## Calendar

Switch between **Month / Week / Day**, and use **← / Today / →** to move.

**New event** captures a title, start/end date and time (or **all day**),
which calendar it lands on, location, and notes. On the Week and Day
views the form docks beside the planner and the draft is drawn as a
live block on the grid: drag the block to move it, pull its bottom
edge to change the end, or draw a fresh range to re-pick — the form
follows every gesture. Two people fields:

- **Invite people** — members get an in-app invite and **RSVP**; external
  **email addresses** get a real ICS invitation in their inbox (and
  updates/cancellations follow).
- **Tag people** — mentions a member without asking them to RSVP.

Open any event to **RSVP** (Yes / Maybe / No), **Edit**, or **Delete**.

**Multiple calendars.** Everyone starts with a personal **Calendar**
(created on first visit). The rail splits **My calendars** (personal) from
**Shared calendars** (one per shared drive you're in). **Add** makes a new
personal calendar. Tick a calendar to **show** it, untick to **hide** it.
Calendars are just files in your drives — the **file** link jumps to it in
Drive. Whether shared calendars appear ticked by default is your
**Settings → Calendar** auto-subscribe preference.

## Contacts

Open **Contacts** from the app switcher. **New contact** captures name,
emails, phones, org, title, and notes — plus any number of **custom
fields**, vault-style: each is a label, a type, and a value. Types:
**Text**, **Secret** (masked until you hit Reveal; Copy without
showing), **Note** (multiline), **Link**, **Date**. Use them for door
codes, account numbers, backup emails — anything worth remembering
about a person. **Search** filters the list (secret values are never
searched); click a card to edit or delete. New cards save to a drive
you choose (your personal drive by default) — **shared drives act as
shared address books**. Your contacts feed **Email's recipient
typeahead** as you compose.

## Video

Video is a streaming-style view over Drive folders **registered as Video
content** (see *Feeding Video & Music* above). **Join a shared drive and
its registered content appears here automatically**; leave and it's gone.

The home page has shelves for **Continue watching**, **My list**,
**Favorites**, and each registered folder. Browsing a folder splits into
**Movies** and **Series**; a title page offers **Play / Resume**, **Mark
watched**, **+ My list**, **Favorite**, and (for series) a season/episode
list with a **Next up** button. Your **resume position** rides with you —
progress bars on posters, resume on play.

Catalogs come from the files themselves: names like `Show S01E05.mkv` and
`Movie (2023).mp4`, with a `poster.jpg` for art. If you have **editor**
access, the player has a **Capture frame** tool — freeze the current
frame and **Use as poster** (the show/movie's `poster.jpg`) or **Use as
preview** (that episode's own thumbnail).

**Hide from my Video** removes a folder from **your** view only (others
are unaffected), and **Clear watch history** forgets your positions. If a
registered folder shows *Nothing indexed*, its files may not match the
naming pattern, or the scan hasn't run yet — **Rescan** from the folder,
or ask your admin if scans are stale (see [docs/admin.md](admin.md)).

## Music

Music catalogs Drive folders **registered as Music content**, richest
from tagged MP3s (Artist/Album folders also work). Browse **albums**,
**artists**, and **tracks**; **Recently played** and **Favorites** sit on
the home page.

**Playlists** — create one from the home page, **rename** or **delete**
it, **reorder** tracks (↑ / ↓), remove tracks, and **Add to playlist**
from any album's track rows.

The **mini-player** at the bottom **persists as you navigate** — start an
album or **Play all** a playlist and it keeps playing while you browse,
with a **queue** and **shuffle**. **Search** finds albums and artists.
Like Video, **Hide** removes a folder from your own view.

## API keys & escape hatches

**API keys** (Settings → **API keys**) let your own software — a mail
app, a file-sync tool — act as you over the REST API at `/api/v1/`. Give
a key a name, tick the **scopes** it needs, and pick an expiry
(Never / 30 days / 90 days / 1 year). The full token
(`pcp_<id>_<secret>`) is **shown exactly once** at creation — copy it
then; only a digest is stored. A key is always **capped by your own
access**, never grants the web UI, and you can **Revoke** it anytime.

Scopes, briefly:

| Scope | Lets the key… |
|---|---|
| `profile:read` | read your username, display name, quota/usage |
| `drive:read` / `drive:write` | list & download files / create, upload, move, delete, share |
| `mail:read` / `mail:write` / `mail:send` | read mail / flags, labels, drafts / send mail |
| `calendar:read` / `calendar:write` | read events / create, edit, RSVP |
| `contacts:read` / `contacts:write` | read / edit contacts |
| `media:read` | read media catalogs, art, and stream (read-only) |

**Your data is never trapped.** Beyond the API, every export is a plain
file: downloads are ordinary HTTP (resumable), folders **zip** on
demand, spreadsheets export to **CSV/XLSX**, documents to **HTML/text**,
diagrams to **SVG/PNG**, mail messages fetch as **raw RFC 822**, and
calendars speak **ICS**. Full endpoint reference: [docs/api.md](api.md).
