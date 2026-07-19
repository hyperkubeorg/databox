# Git Services

GitHub-style git hosting inside PCP: repositories under user and
organization namespaces, teams and per-repo grants, issues and merge
requests, real `git clone`/`push` over smart HTTP and SSH, and public
repos served to the anonymous internet through cloudferry. Everything lives in
databox under `/pcp/git/` — no filesystem, no extra daemon.

## Enabling it

Off by default. **Admin → Site → Git Services** has the switches:

- **Enable Git Services** — off hides Git from the launcher/switcher and
  404s every `/git/` route (web, API, wire, anonymous alike).
- **Allow public repositories** — on by default. Off forces every repo
  private and drops all anonymous pages and clones to 404: an
  internal-only instance.
- **Max git push/fetch body** — the tunnel-side wire body cap (default
  1 GiB). Each web gateway carries a matching edge cap (below).

## Namespaces, organizations, teams, roles

Users and organizations share one namespace: `sam/dotfiles` and
`hyperkube/tools` live in the same URL space, and signup checks the org
registry so names never collide. Names are immutable in v1.

- **Organizations** are self-serve inside the Git app (`/git/orgs/new`):
  members with `owner`/`member` roles, flat **teams** (≤500 members),
  org settings for default member permission (`none|read|write`),
  member-list visibility, and whether members may create repos. The last
  owner can never demote or remove themself. Org deletion requires zero
  repos.
- **Repo roles** — three, each including the ones below it:

| Role | Grants |
|---|---|
| `read` | clone/fetch, browse, open and comment on issues/MRs |
| `write` | push, edit files in the browser, merge MRs, triage (labels, assignees, close/reopen), delete branches |
| `admin` | settings, visibility, grants, delete |

A viewer's role is the **maximum** of: public visibility (`read`, even
anonymous), personal ownership (`admin`), org owner (`admin`) / org
default member permission, team grants, and direct user grants — one
resolution function, exhaustively tested (`pkg/domain/git/roles_test.go`).
No access always answers **404, never 403**: private repos are
unconfirmable on web, API, and wire alike.

## Quotas

Pushes charge the **owning namespace**: personal repos charge the user's
normal storage quota; org repos charge the org, which has its own
tier/override/usage (**Admin → Storage → Git organizations**). A push is
pre-charged, reconciled to actual stored bytes, and fully refunded if
rejected — over-quota pushes move no refs and leave no bytes
(`cmd/smoke` phase 11 proves the round trip; `pkg/apps/git/wire_test.go`
covers the paths).

## Cloning and pushing

Repo URLs double as clone URLs (a trailing `.git` is accepted):

```
https://<host>/git/<ns>/<name>[.git]
```

Git authenticates with **Basic**: your username plus a git credential as
the password. Mint one in **Git → Settings** ("Mint a git credential") —
it's a platform API key with scopes `git:read` + `git:write`, shown
once. Then:

```
git clone https://sam@pcp.example.com/git/sam/dotfiles.git
# password: the minted token — or store it:
git config --global credential.helper store
```

Limits on the wire: request body ≤ the git body cap (1 GiB default),
one object ≤ 256 MiB, one push ≤ 100,000 objects. Pushes are atomic —
all ref updates land in one transaction or none do
(`pkg/domain/git/repos_test.go`, `wire_test.go`).

## Git over SSH

The same repositories also serve over SSH — no token in the URL, the
key is the identity. HTTP and SSH drive one wire-protocol core
(`pkg/gitwire`), so quotas, caps, atomic pushes, automatic GC, and MR
head refresh behave identically on both.

**Key setup:**

1. Have a key (or make one): `ssh-keygen -t ed25519`. Accepted types:
   ed25519, RSA ≥ 3072 bits, ECDSA. One key belongs to ONE account —
   registering it twice (any account) is refused.
2. Paste the `.pub` line in **Git → Settings → SSH keys** (adds and
   removals are audited; the card shows fingerprint + last used).
3. Test: `ssh -p 4222 git@<host>` answers
   *"Hi you! You've successfully authenticated…"* and exits — there is
   no shell.

**Clone URLs** (the repo page's clone box has an HTTPS/SSH toggle):

```
ssh://git@<host>:4222/<ns>/<name>.git
```

The username is `git` by convention (your own username also works —
with your own key only). Publickey is the ONLY auth method: no
passwords, and **no anonymous SSH** — anonymous clones of public repos
stay HTTPS-only. A key that isn't registered, a banned owner, or a repo
you can't see all answer the same way ("repository not found" /
permission denied): private repos stay unconfirmable, exactly like the
HTTP wire's 404-never-403 rule.

**The 4222 convention:** `PCP_GIT_SSH_ADDR` defaults to `:4222` on the
PCP host (empty disables the transport, which also hides the SSH clone
UI). The host key is generated once and shared by every replica
(`/pcp/git/sshhostkey`); its fingerprint logs at startup. On the dev
kind cluster the chart publishes NodePort 30422 and `make relay-ssh`
serves it at `ssh://git@localhost:4222`.

**Production exposure:** don't open 4222 to the internet yourself —
pair a cloudferry and add a **TCP relay** (edge port `22` → target
`4222`) on the gateway's admin page; the gateway relays raw bytes and,
since SSH encrypts end to end, stays blind in flight as well as at rest
(docs/cloudferry.md → "TCP relays"). Clone URLs then drop the port:
`ssh://git@code.example.org/ns/repo.git` (or plain
`git@code.example.org:ns/repo.git`).

Wire limits are the HTTP table's, minus the body caps that only exist
because HTTP has request bodies; SSH pushes always use the incremental
quota-accrual path. SSH-specific guards: per-IP concurrent-connection
cap, a per-IP auth-failure budget, an idle timeout, one session per
connection, and no port-forwarding/agent/X11/PTY services.

## Editing files in the browser

Writers get an in-service editor (Ace, vendored — no CDN): **Edit** and
**Delete file** on any blob viewed at a **branch** head, **New file** on
the repo home and tree pages, and a create-in-the-browser path on the
empty-repo quick-setup block. Tags and bare commits are read-only — the
editor routes 404 there, and the blob view shows a "switch to a branch"
hint instead of the button.

Each save is one real commit built through the same storer, quota, and
atomic ref machinery pushes use: bytes are charged to the owning
namespace (over-quota saves are rejected with your content kept in the
form), and open merge requests sourced from the branch refresh their
heads exactly as a push would. The path field is editable — changing it
renames (delete + add) in the same commit. Deleting is a confirm dialog
that produces a deletion commit.

**Concurrency (CAS):** the editor captures the branch head when the page
opens. If the branch moves before you commit:

- your file untouched by the new commits → your save **rebases
  transparently** onto the new head;
- your file also changed there → the save is rejected with a friendly
  "branch moved" message and the editor re-renders with everything you
  typed intact — copy, reload, reconcile.

Editor commits sign as `Display Name <username@pcp.local>`, the same
identity as README initialization. Line endings are stored as LF.
Files above the 1 MiB render cap (and binaries) are not editable in the
browser — clone instead. (`pkg/domain/git/webcommit_test.go`,
`pkg/apps/git/editor_web_test.go`.)

The API twin is `POST /api/v1/git/repos/{ns}/{name}/contents`
(`git:write` + repo write role): `{branch, path, content (base64),
message, baseSha}` upserts; add `fromPath` to rename or `"delete": true`
to delete; a real CAS conflict answers **409**.

## Syntax highlighting

Blob views and markdown code fences (READMEs, issues, MR bodies,
comments — they share one renderer) highlight client-side as a
progressive enhancement: no JS, no highlighting, same escaped text.
Blob language comes from the file extension; fences from the
```` ```lang ```` info string — both through a **whitelist**
(`pkg/apps/git/highlight.go`), so a hostile fence string can never
inject a class attribute. Files above the render cap stay plain.

Vendored libraries (local to the project, pinned, no CDN):

| Library | Version | License | Where |
|---|---|---|---|
| highlight.js (common bundle + dockerfile) | 11.11.1 | BSD-3-Clause | `pkg/apps/git/assets/vendor/highlightjs/11.11.1/` |
| Ace (ace-builds, src-min-noconflict) | 1.44.0 | BSD-3-Clause | `pkg/apps/git/assets/vendor/ace/1.44.0/` |

Each directory carries the verbatim upstream `LICENSE` and a `VENDOR.md`
with exact download URLs and the "how to add a language" recipe: drop in
the language/mode file for the pinned version, then extend the whitelist
maps (`highlight.go` for highlight.js, the `aceModeByExt` map in
`git_repo_edit.tpl` for Ace). Covered languages: C, C++, C#, Ruby,
Python, Go, PHP, JavaScript, TypeScript, Java, Rust, bash/shell,
HTML/XML, CSS, JSON, YAML, Markdown, SQL, Kotlin, Swift, diff,
Dockerfile, INI/TOML, Makefile (and the rest of the hljs common bundle).

Token colors are our own tiny stylesheet on the platform design tokens
(`git_styles.tpl`) — one palette that follows the dark and light themes;
no stock theme CSS ships. The editor's Ace theme (`ace/theme/pcp`) reads
the same tokens. Assets serve from `/git/-/assets/…` (`-` is a reserved
name, so the literal route can never collide with a namespace) with
immutable caching — versions ride the directory names.

**Diff views are deliberately unhighlighted** (correct-first): unified
diffs splice line fragments from two file versions, and lexing fragments
mislabels tokens. Future work.

## Issues and merge requests

Per repo, sharing one `#N` sequence so references are unambiguous.
`read` may open issues and comment; `write` triages. Markdown bodies
autolink `#N` and `@user`. Merge requests come from any branch of the
repo or a fork in its network; the diff renders per-file unified with
binary/too-large fallbacks. Merging fast-forwards when possible, else
builds a merge commit from a **file-level** three-way merge — files
changed on both sides conflict and block the button with a
resolve-locally message. Assignments feed the launcher card and the
`/git` dashboard; events notify in-app, plus by mail if you opt in
(Git → Settings) and Mail is enabled.

## Public repos and the ferry

With public repos allowed, `public` repositories render to anonymous
visitors — through a cloudferry hostname or the LAN address — and clone
anonymously (fetch only; push always authenticates). Anonymous pages are
strictly read-only: no comment box, no forms, a login link instead of
the app switcher. Public **profiles** are opt-in (Git → Settings): until
you create one and mark it public, `/git/you` is a 404 to the world.
An org's member list shows publicly only if the org opted in.

Leak rules, tested in `pkg/apps/git/public_web_test.go`: anonymous pages
show usernames/display names/avatars of participants — never email
addresses, never non-opted-in member lists, never a private repo's name
or existence (404 everywhere, including fork backlinks: a private fork
simply doesn't appear, and a public fork of a private parent shows no
"forked from").

Anonymous traffic rides a stricter rate tier (60 requests/min per IP on
public pages and anonymous fetches) behind cloudferry's own per-IP edge
limiter.

## Caps & limits knobs

| Knob | Where | Default |
|---|---|---|
| Git wire body (tunnel-side) | Admin → Site → Git Services | 1 GiB |
| Git wire body (edge) | Admin → Web access → gateway → Edge limits | 1 GiB |
| General request body (edge) | same page | 5 GiB |
| Per-object size | fixed (v1) | 256 MiB |
| Objects per push | fixed (v1) | 100,000 |
| Anonymous requests/min/IP | fixed (v1) | 60 |

The two git body caps are a pair: the edge rejects oversized pushes
before they cross the tunnel; PCP enforces the same bound for direct-LAN
pushes.

## Garbage collection

Deleting branches or force-pushing never refunds storage eagerly — a
collection pass does: a reachability walk from all branches/tags, open
merge-request heads, and every fork's refs (forks read through their
parent, so nothing a fork needs is ever collected), then unreachable
objects are deleted and the bytes refunded to the owning namespace
(`pkg/domain/git/gc_test.go`).

Maintenance is **fully automatic** — there is no GC button for users or
admins, by design: reachability is an internal invariant, not a judgment
call to hand people. Two triggers:

- **After a push or branch delete** — any ref deletion or
  non-fast-forward move schedules a debounced background pass for that
  repo (runs `PCP_GIT_GC_DEBOUNCE` — default 30s — after the last
  trigger, so a burst of force-pushes collects once). Fast-forward
  pushes and web edits never orphan anything and schedule nothing.
- **Nightly sweep** — a databox-lock-singleton worker
  (`/pcp/system/loops/gitgc` on the admin Workers page) catches
  stragglers roughly every 24h, skipping any repo that is mid-push.

Both paths take the per-repo push lock, so a push and a collection never
run concurrently on one repo, and each completed pass logs objects and
bytes freed with actor `system`.

## API

Scopes `git:read` / `git:write` — the same credential story as the wire
protocol. Raw git data rides smart HTTP, not JSON. Endpoints (all under
`/api/v1/git`, bearer-authed, RoleFor-gated after the scope check):

- repos: `GET/POST /repos`, `GET/PATCH/DELETE /repos/{ns}/{name}`,
  `POST …/fork`, grants `GET/PUT /repos/{ns}/{name}/grants`,
  `DELETE …/grants/{subject}`
- orgs: `GET/POST /orgs`, `GET/PATCH /orgs/{org}`, members
  `GET/POST /orgs/{org}/members`, `DELETE …/members/{username}`, teams
  `GET/POST /orgs/{org}/teams`, `PATCH/DELETE …/teams/{team}`,
  `POST/DELETE …/teams/{team}/members[/{username}]`
- issues: `GET/POST /repos/{ns}/{name}/issues`, `GET …/issues/{n}`,
  comments `POST/PATCH/DELETE …/issues/{n}/comments[/{id}]`, state,
  labels (`PUT …/issues/{n}/labels`, label CRUD under `…/labels`),
  assignees
- merges: `GET/POST /repos/{ns}/{name}/merges`, `GET …/merges/{n}`,
  comments, state, `POST …/merges/{n}/merge`, assignees, labels
- contents: `POST /repos/{ns}/{name}/contents` (the browser editor's
  API twin — create/update/rename/delete one file on a branch, CAS on
  `baseSha`, 409 on conflict)
- profile: `GET/PUT /profile` (the git profile)

Every route sits in the route→scope audit table
(`pkg/apps/api/scopes_test.go`).

## Not in v1

git-LFS, line-level conflict resolution, diff-view syntax highlighting
(blob/fence highlighting shipped — see above), webhooks, CI/CD, wikis,
releases, code search, branch protection, fork-detach, rename/transfer.
The wire protocol is smart HTTP + SSH — no dumb HTTP, no git://.
