# Services — feature enablement

PCP ships every launcher app **off**. An admin turns features on, one at a time,
from **Admin → Services** (`/admin/services`). Services is the only place a
feature is enabled or disabled; each feature's own settings page keeps its policy
(mail sending, git public repos) and links back here for the master switch.

## The model

Every launcher app is a *feature* with a master switch on the site-config record.
A single registry (`pkg/domain/site/features.go`) is the source of truth for the
feature list, each feature's requirements, and its enabled state; the launcher,
the app switcher, every route gate, and this page all read it, so they can't
drift. A disabled feature is invisible and inert: no launcher card, no switcher
entry, and every one of its routes (web and API) returns 404 — indistinguishable
from an unbuilt route.

## Requirements

Some features store their data inside Drive, so they require Drive to be enabled
first:

| Feature | Requires |
| --- | --- |
| Drive, Email, Messenger, Git | — |
| Calendar | Drive |
| Contacts | Drive |
| Music | Drive |
| Video | Drive |

Requirements are enforced in both directions:

- **Enabling** a feature is refused until every requirement is on. The Services
  row greys the toggle and names what to enable first.
- **Disabling** a feature is refused while any enabled feature still depends on
  it, naming them. Turn the dependents off first — there is no silent cascade.

A fresh instance has nothing enabled, so the first step is always **enable
Drive**, then enable what builds on it.

## Disabling

Disabling hides the app and 404s its routes immediately (within one config read).
Data is retained — disable is reversible. To remove data, purge (below).

## Purging data

Each feature has a **danger zone** on its Services detail page that permanently
deletes all of that feature's stored data. This is irreversible: there is no undo
and no backup. It is meant for reclaiming space, decommissioning a feature, or
scrubbing data that must not persist.

Purge is deliberately hard to trigger:

- The page states up front exactly what will be destroyed and its size.
- You must click the confirmation **ten times**, each step restating that the
  action is permanent.
- You must type the feature's id to arm the final delete.

Purge is allowed whether the feature is on or off; if it is on, active users lose
their data immediately. Cross-feature orphans (e.g. a build that referenced a
purged repository) are named before you confirm. Every purge is audited — the
start with the intended target, and completion with the record and byte counts
removed.

Note on Drive: purging Drive destroys the files other features store inside it —
including Calendar events and Contacts cards. The confirmation says so.
