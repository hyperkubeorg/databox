// Package kvx holds the storage helpers every domain package shares —
// inverted-timestamp IDs, random IDs, OCC helpers, key-segment
// validation, and JSON get/set plumbing over the databox client — plus
// the canonical key table below, which is the review gate for any new
// key the app writes (§12).
//
// Everything PCP stores lives under /pcp/, so one grant covers the app:
//
//	databox grant add pcp allow /pcp list,read,write,delete
//
// # Canonical key table
//
// Active in phase 1:
//
//	/pcp/users/<username>          → users.User
//	/pcp/sessions/<token>          → users.Session (lazy TTL; token is the credential)
//	/pcp/meta/site-config          → site.Config
//	/pcp/meta/first-admin          → {username} (OCC first-signup winner)
//	/pcp/audit/<invTs>-<rand>      → kernel.AuditEntry (newest-first)
//
// Active from phase 2a (API v1 keys, spec §12.1):
//
//	/pcp/apikeys/<keyID>           → apikeys.Key (SHA-256 digest — never the secret)
//	/pcp/userkeys/<user>/<keyID>   → reverse index for per-user listing
//
// Active from phase 2b (Drive core):
//
//	/pcp/drives/<driveID>                       → drives.Drive
//	/pcp/members/<driveID>/<username>           → drives.Member (role in the drive)
//	/pcp/userdrives/<username>/<driveID>        → drives.Member (reverse index)
//	/pcp/nodes/<driveID>/<parentID>/<nameLower> → nodes.Node (one folder's children;
//	                                              one prefix List = the folder)
//	/pcp/noderef/<driveID>/<nodeID>             → nodes.NodeRef (id → current location)
//	/pcp/blobs/<driveID>/<blobID>               → file BLOB (immutable id per version)
//	/pcp/tmp/<username>/<uploadID>[.meta]       → upload-assembly BLOB + meta (lazy GC)
//	/pcp/thumbs/<driveID>/<blobID>              → thumbnail BLOB (content-addressed)
//	/pcp/versions/<driveID>/<nodeID>/<rev>      → nodes.Version (rev sorts newest-first)
//	/pcp/shares/<token>                         → shares.Share (public link)
//	/pcp/sharesess/<id>                         → shares.ShareSession (password pass)
//	/pcp/nodeshares/<driveID>/<nodeID>/<token>  → {} (reverse index of shares)
//	/pcp/grants/<username>/<driveID>/<nodeID>   → shares.Grant ("shared with me")
//	/pcp/nodegrants/<driveID>/<nodeID>/<user>   → shares.Grant (reverse index / ACL)
//	/pcp/media/registry/<driveID>/<folderID>    → media.Registration (drive-level
//	                                              registered folder, spec §9)
//
// Active from phase 2c (collab editors, spec §2/§6):
//
//	/pcp/docs/<driveID>/<nodeID>/ops/<hlc>       → one op (LWW; HLC embeds the actor)
//	/pcp/docs/<driveID>/<nodeID>/snapshot        → BLOB: folded doc + HLC watermark
//	/pcp/docs/<driveID>/<nodeID>/presence/<user> → collab.Presence (lazy TTL)
//
// Active from phase 3 (email backend, spec §7):
//
//	/pcp/mail/domains/<domain>                  → mail.Domain (+ DKIM public key)
//	/pcp/mail/dkim/<domain>                     → DKIM private PEM (push-only; no UI)
//	/pcp/mail/postoffices/<poID>                → mail.PostOffice (pairing + PCP keys)
//	/pcp/mail/podomains/<poID>/<domain>         → {priority} (gateway → domains)
//	/pcp/mail/domainpos/<domain>/<poID>         → {priority} (reverse index)
//	/pcp/mail/addrs/<domain>/<local>            → mail.Address (mailbox|alias|distro)
//	/pcp/mail/useraddrs/<user>/<domain>/<local> → {type} (reverse index: "my addresses")
//	/pcp/mail/boxes/<user>/<boxID>              → mail.Mailbox (store + signature)
//	/pcp/mail/threads/<user>/<box>/<threadID>   → mail.ThreadMeta (canonical)
//	/pcp/mail/threadidx/<user>/<box>/<folder>/<invTs>-<threadID> → ThreadMeta copy
//	                                              (the ONE folder listing; re-filed
//	                                              atomically on every activity)
//	/pcp/mail/sentidx/<user>/<box>/<invTs>-<threadID>   → ThreadMeta copy (Sent facet)
//	/pcp/mail/starred/<user>/<box>/<invTs>-<threadID>   → ThreadMeta copy (Starred)
//	/pcp/mail/bylabel/<user>/<labelID>/<invTs>-<threadID> → ThreadMeta copy (labels)
//	/pcp/mail/msgs/<user>/<box>/<threadID>/<ts>-<msgID> → mail.MsgMeta (ascending)
//	/pcp/mail/msgref/<user>/<msgID>             → mail.MsgRef (id → location; the
//	                                              idempotent-delivery existence check)
//	/pcp/mail/labels/<user>/<labelID>           → mail.Label {name,color,order}
//	/pcp/mail/drafts/<user>/<box>/<draftID>     → mail.Draft (mutable; not a thread)
//	/pcp/mail/folders/<user>/<box>/<folderID>   → mail.Folder (custom folders, ≤50)
//	/pcp/mail/blobs/<user>/<blobID>             → raw RFC 822 BLOB ("+sys" = system)
//	/pcp/mail/searchtext/<user>/<blobID>        → zlib search text (≤2 MiB guarded)
//	/pcp/mail/outq/<invTs>-<outID>              → mail.OutMsg (held|pending|submitted)
//	/pcp/mail/pocursors/<poID>                  → delivery-event cursor
//	/pcp/mail/sendlog/<user>/<invTs>            → {} (send-rate window)
//	/pcp/mail/welcome/<id>                      → mail.Welcome
//	/pcp/mail/recents/<user>/<addrHash>         → mail.Recent (compose typeahead)
//	/pcp/mail/serial                            → manifest serial (monotonic counter)
//
// Active from phase 4 (email UI + Mail API, spec §7/§12.2):
//
//	/pcp/mail/blobs/<user>/att-<id>             → staged draft-attachment BLOB
//	                                              (dies with its draft, or at
//	                                              outbound release)
//	/pcp/mail/idem/<user>/<keyHash>             → mail.idemRecord (Idempotency-Key
//	                                              send ledger, 24h TTL)
//	/pcp/notif/<user>/<invTs>                   → notify.Notification (newest-first)
//	/pcp/system/loops/<name>                    → system.LoopRecord (§11.3)
//
// Active from phase 5 (calendar + contacts, spec §8/§7.6; contact
// cards are ordinary node/blob rows — no keys of their own):
//
//	/pcp/calsubs/<user>/<driveID>/<nodeID>             → calendar.CalSub (visibility
//	                                                     override; grants nothing)
//	/pcp/docs/<d>/<n>/notifsent/<event>/<suffix>       → {} (invite/tag/RSVP dedup
//	                                                     ledger; dies with the doc)
//	/pcp/docs/<d>/<n>/icsmail/<event>                  → calendar.icsLedger (outbound
//	                                                     invite UID/SEQUENCE/recipients)
//	/pcp/mail/icsrsvp/<user>/<uidHash>                 → calendar.InboundRSVP (answers
//	                                                     to foreign invites)
//
// Active from phase 6 (Video & Music, spec §9):
//
//	/pcp/media/catalog/<driveID>/<folderID>/<kind>/<slug> → media.CatalogEntry
//	                                              (rebuildable cache; kind is the
//	                                              fixed catalog vocabulary, plus the
//	                                              reserved "meta" row = media.ScanInfo)
//	/pcp/media/art/<driveID>/<folderID>/<slug>  → BLOB (harvested ID3 APIC cover art;
//	                                              dies with the catalog)
//	/pcp/media/progress/<user>/<nodeID>         → media.Progress (playback position)
//	/pcp/media/watchlist/<user>/<itemKey>       → media.ListItem (itemKey =
//	                                              drive:folder:kind:slug — separator-free
//	                                              segments)
//	/pcp/media/favorites/<user>/<itemKey>       → media.ListItem
//	/pcp/media/playlists/<user>/<plID>          → media.Playlist (ordered track refs)
//	/pcp/media/hidden/<user>/<driveID>/<folderID> → {} (per-user view override)
//
// Active from phase 7 (cloudferry web gateway, spec §10):
//
//	/pcp/cloudferry/gateways/<gwID>       → ferry.Gateway (pairing + PCP keys)
//	/pcp/cloudferry/hosts/<hostname>      → ferry.Host (gateway, TLS mode, forceHTTPS)
//	/pcp/cloudferry/certs/<hostname>      → ferry.HostCert (cert+key PEM — databox IS
//	                                        the private store; pushed sealed, held
//	                                        RAM-only on the gateway)
//	/pcp/cloudferry/acme/account          → ferry.ACMEAccount (key + directory URL)
//	/pcp/cloudferry/acme/challenges/<tok> → HTTP-01 keyAuth (replica-safe; retired
//	                                        after issuance)
//	/pcp/cloudferry/offlinepage           → {html} (503 page pushed to gateways)
//	/pcp/cloudferry/serial                → config serial (monotonic counter)
//
// Active from phase 8 (admin console + observability, spec §11):
//
//	/pcp/invites/<code>                  → invites.Invite (keyed by the code)
//	/pcp/invitesbyuser/<user>/<code>     → {} (reverse index: "my invites")
//	/pcp/inviteuses/<code>/<user>        → invites.InviteUse (redemption ledger)
//	/pcp/userips/<user>/<ip>             → users.UserIP (first/last seen + logins)
//	/pcp/ipbans/<ip>                     → users.IPBan (refused at login AND signup)
//	/pcp/system/samples/<workerID>/<invTs> → system.Sample (gateway self-report
//	                                       history; pruned to the newest 288)
//	/pcp/system/replicas/<id>            → system.Replica (30s heartbeat, lazy TTL)
//	/pcp/system/problems/<id>            → system.Problem (raise/auto-resolve;
//	                                       resolved kept 24h as tombstones)
//	/pcp/system/notified/<problemID>     → {at} (admin-notification dedup, 24h)
//
// Active from phase 9 (hardening):
//
//	/pcp/mail/gcpending/<user>/<blobID>  → RFC3339 first-seen stamp (orphaned-blob
//	                                       GC candidate; reclaimed past grace)
//	/pcp/mail/gccursor                   → {blobs, search} scan cursors (GC paging)
//
// Active from the Messenger app (the Messenger spec). A conversation id
// <cid> unifies message storage: a server channel's cid IS its channelID;
// a DM's cid is dm_<userLo>_<userHi>; a group DM's cid is a random g<id>.
//
//	/pcp/msg/servers/<serverID>                  → messenger.Server
//	/pcp/msg/members/<serverID>/<username>       → messenger.Member (roleIDs, nick)
//	/pcp/msg/usermembers/<username>/<serverID>   → messenger.Member (reverse; same txn)
//	/pcp/msg/roles/<serverID>/<roleID>           → messenger.Role (name, color, perms, pos)
//	/pcp/msg/channels/<serverID>/<channelID>     → messenger.Channel (channelID IS the cid)
//	/pcp/msg/discover/<invTs>-<serverID>         → {} (open servers; the server browser index)
//	/pcp/msg/convos/<cid>                         → messenger.Convo (kind, serverID|-, lastMsgTs)
//	/pcp/msg/msgs/<cid>/<ts>-<msgID>             → messenger.Message (ascending in a channel)
//	/pcp/msg/msgref/<msgID>                       → {cid} (id → location; edit/delete/idempotency)
//	/pcp/msg/read/<username>/<cid>               → {lastReadTs, lastReadMsgID}
//	/pcp/msg/unread/<username>/<cid>             → messenger.Unread {count, lastTs, mention}
//	                                               (fan-out badge; the ONE SSE-watched prefix)
//	/pcp/msg/mentions/<username>/<invTs>-<msgID> → {cid} (unresolved-mention ledger; red badge)
//	/pcp/msg/dmidx/<username>/<invTs>-<cid>      → messenger.DMRef (DM/group list, newest-first)
//	/pcp/msg/dms/<username>/<cid>                → {} (membership/existence; reverse of dmidx)
//	/pcp/msg/groupmembers/<cid>/<username>       → {} (group-DM roster)
//	/pcp/msg/presence/<username>                  → messenger.Presence (chosen status + statusMsg)
//	/pcp/msg/online/<username>/<replicaOrSess>   → {at} (connection heartbeat, lazy TTL)
//	/pcp/msg/typing/<cid>/<username>             → {at} (~6s TTL)
//	/pcp/msg/search/<cid>/<term>/<invTs>-<msgID> → {author} (term posting; author for from: filter)
//	/pcp/msg/authoridx/<cid>/<username>/<invTs>-<msgID> → {} (author-only queries)
//	/pcp/msg/blobs/<cid>/<blobID>                → BLOB (immutable attachment; Range-served)
//	/pcp/msg/invites/<code>                       → messenger.Invite (serverID, by, expiry, uses)
//	/pcp/msg/serverinvites/<serverID>/<code>     → {} (reverse; this server's invites)
//	/pcp/msg/profiles/<username>                  → messenger.Profile (bio, pronouns, accent, banner)
//
// Active from Git Services phase 1 (PROJECT-DRAFT-002 §14; repo/object/
// issue/MR families register with their build phases):
//
//	/pcp/git/ns/<name>                    → git.NS (shared user|org namespace registry)
//	/pcp/git/profiles/<user>              → git.Profile (opt-in; absence = never created)
//	/pcp/git/orgs/<org>                   → git.Org (incl. quota fields)
//	/pcp/git/orgmembers/<org>/<user>      → git.OrgMember (owner|member)
//	/pcp/git/userorgs/<user>/<org>        → git.OrgMember (reverse index; same txn)
//	/pcp/git/teams/<org>/<teamID>         → git.Team (flat member list, cap 500)
//	/pcp/git/grants/<repoID>/<subject>    → git.Grant (subject u:<user> | t:<org>/<teamID>)
//	/pcp/git/usergrants/<user>/<invTs>-<repoID> → {repoID, subject} ("shared with you";
//	                                        one entry per grant SOURCE — matched by
//	                                        value, never key suffix; same txn as the
//	                                        grant / team-membership change)
//	/pcp/git/teamgrants/<org>/<teamID>/<repoID> → git.Grant (reverse of team-subject
//	                                        grants; what membership changes fan out)
//
// Active from Git Services phase 2 (Draft 002 §5.1/§6.2 — the git core):
//
//	/pcp/git/repo/<repoID>                → git.Repo (repoID is stable across
//	                                        rename/transfer; object keys never move)
//	/pcp/git/reponame/<ns>/<name>         → repoID (per-ns name index; same txn)
//	/pcp/git/forks/<parentID>/<childID>   → childID (fork reverse index; blocks
//	                                        parent deletion while children live)
//	/pcp/git/refs/<repoID>/<refname>      → commit hash hex (raw refname suffix —
//	                                        validated to printable-ASCII git rules,
//	                                        so keys stay charset-safe and ordered;
//	                                        ref updates are ONE OCC txn per push)
//	/pcp/git/obj/<repoID>/<sha>           → loose object < 256 KiB encoded
//	                                        ("<type> <size>\n" + zlib content)
//	/pcp/git/objblob/<repoID>/<sha>       → BLOB, same encoding, ≥ 256 KiB (ranged
//	                                        reads serve the header; §6.2 tiering)
//
// Active from Git Services phase 4 (Draft 002 §8 — issues):
//
//	/pcp/git/seq/<repoID>                 → next issue/MR number (OCC counter,
//	                                        SHARED by issues and MRs — #N is
//	                                        unambiguous repo-wide)
//	/pcp/git/issues/<repoID>/<n>          → git.Issue (canonical record)
//	/pcp/git/issueidx/<repoID>/<state>/<invTs>-<n> → git.Issue copy (the list view;
//	                                        invTs = inverted last-activity, re-filed
//	                                        in the same tx as every record change)
//	/pcp/git/assigned/<user>/<invTs>-<repoID>:<n> → git.AssignedRef (OPEN items only;
//	                                        feeds the launcher count + dashboard;
//	                                        removal matches the VALUE, never the key
//	                                        suffix; MRs share it via Kind)
//	/pcp/git/comments/<repoID>/<n>/<ts>-<id> → git.Comment (ascending; MRs reuse)
//	/pcp/git/labels/<repoID>/<labelID>    → git.Label {name, color} (issue records
//	                                        keep dangling ids after a label delete —
//	                                        readers filter lazily)
//
// Active from Git Services phase 5 (Draft 002 §9 — merge requests):
//
//	/pcp/git/merges/<repoID>/<n>          → git.Merge (repoID = TARGET repo; n from
//	                                        the SHARED seq/ counter, so comments/
//	                                        assigned reuse §8's families verbatim)
//	/pcp/git/mergeidx/<repoID>/<state>/<invTs>-<n> → git.Merge copy (issueidx's twin;
//	                                        states open|merged|closed, re-filed in
//	                                        the same tx as every record change)
//	/pcp/git/sshkeys/<user>/<keyID>       → git.SSHKey (validated authorized_keys
//	                                        line + SHA256 fingerprint + last-used)
//	/pcp/git/sshfp/<hexFingerprint>       → {user, keyID} (OCC-claimed with the key
//	                                        record — one key, one account; the SSH
//	                                        server's auth lookup; hex because base64
//	                                        fingerprints contain '/')
//	/pcp/git/sshhostkey                   → ed25519 host key PEM (OCC-claimed once;
//	                                        replicas share one SSH identity)
//	/pcp/git/mrsrc/<sourceRepoID>/<targetRepoID>:<n> → {sourceBranch, target, n}
//	                                        (receive-pack's open-MR head-refresh
//	                                        lookup + the outbound-open-MR delete
//	                                        block; the BRANCH lives in the value —
//	                                        branch names contain "/" and would
//	                                        break key-prefix isolation; row lives
//	                                        exactly while the MR is open)
//	/pcp/git/lastcommit/<repoID>/<sha>/<dirKey> → map name → {sha, subject, when}
//	                                        (tree-listing last-touch attribution,
//	                                        §5.2 — a REBUILDABLE cache in the
//	                                        media-catalog mold, immutable per
//	                                        commit so it never invalidates; dirKey
//	                                        is "root" or a 16-hex path hash — dir
//	                                        paths carry slashes; results over
//	                                        128 KiB simply aren't cached)
//
// Active from Builds CI/CD (PROJECT-DRAFT-003 §14 — runner records
// register in phase 2, build/phase/release/secret families with their
// build phases; all under /pcp/build/):
//
//	/pcp/build/runners/<id>                     → build.Runner (pairing twin of ferry)
//	/pcp/build/runnersby/<scope>/<id>           → id (per-scope runner index;
//	                                              scope = system | org:<org> | repo:<repoID>)
//	/pcp/build/access/<subject>                 → compute allowlist entry (§4.4;
//	                                              subject u:/o:/t:/r:)
//	/pcp/build/profiles/<id>                    → execution profile (§7.2)
//	/pcp/build/profilebind/<scope>              → profile binding (global|org|user|repo)
//	/pcp/build/seq/<repoID>                     → per-repo build counter (OCC)
//	/pcp/build/builds/<repoID>/<n>              → build.Build
//	/pcp/build/buildidx/<repoID>/<class>/<invTs>-<n> → build.Build copy (active|done
//	                                              list views; re-filed in the same txn
//	                                              as every state change)
//	/pcp/build/phases/<repoID>/<n>/<phase>      → build.Phase (inline steps)
//	/pcp/build/logs/<repoID>/<n>/<phase>        → per-phase log stream (KV chunks)
//	/pcp/build/logblob/<repoID>/<n>/<phase>/<seq> → log overflow BLOB (ranged tail)
//	/pcp/build/artifacts/<repoID>/<n>/<name>    → artifact metadata
//	/pcp/build/artblob/<repoID>/<n>/<name>      → artifact bytes (BLOB, ranged)
//	/pcp/build/releases/<repoID>/<releaseID>    → build.Release
//	/pcp/build/releaseidx/<repoID>/<invTs>-<releaseID> → build.Release copy (newest-first)
//	/pcp/build/reltag/<repoID>/<tag>            → releaseID (tag uniqueness claim, same txn)
//	/pcp/build/relblob/<repoID>/<releaseID>/<name> → durable release artifact (BLOB;
//	                                              cleanup-exempt copy)
//	/pcp/build/secrets/<scope>/<name>           → build.Secret (sealed ciphertext only —
//	                                              PCP never holds plaintext; scope
//	                                              repo/<repoID> | org/<org>)
//
// Active from Smart Home phase 1 (PROJECT-DRAFT-005 §14; camera/agent/
// segment/event/clip families register with their build phases):
//
//	/pcp/smarthome/access/<subject>              → smarthome.AccessEntry (u:<user>
//	                                               allowlist — who may CREATE spaces
//	                                               and pair agents, §3.1)
//	/pcp/smarthome/space/<spaceID>               → smarthome.Space
//	/pcp/smarthome/members/<spaceID>/<username>  → smarthome.Member (role in the space)
//	/pcp/smarthome/userspaces/<username>/<spaceID> → smarthome.Member (reverse; same txn)
//
// Active from Smart Home phase 3 (agents + ingest, §4/§5):
//
//	/pcp/smarthome/paircode/<code>               → smarthome.Pairing (10-min TTL,
//	                                               deleted at redeem in the same txn
//	                                               that mints the agent)
//	/pcp/smarthome/agent/<agentID>               → smarthome.Agent (token SHA-256
//	                                               digest — never the secret;
//	                                               LastSeen heartbeat)
//	/pcp/smarthome/cam/<spaceID>/<camID>         → smarthome.Camera
//	/pcp/smarthome/camrev/<spaceID>              → {rev} (config revision; the
//	                                               command long-poll's wake signal)
//	/pcp/smarthome/seg/<camID>/<tskey>           → smarthome.Segment (index row;
//	                                               tskey = kvx.TSKey forward time,
//	                                               so a timeline window is one
//	                                               cursor List)
//	/pcp/smarthome/segblob/<camID>/<tskey>       → BLOB (fMP4 bytes, ranged reads)
//	/pcp/smarthome/thumbblob/<camID>/<tskey>     → BLOB (JPEG poster per segment)
//	/pcp/smarthome/event/<camID>/<invID>         → smarthome.Event (newest-first)
//	/pcp/smarthome/eventidx/<spaceID>/<invID>    → smarthome.Event copy (space feed;
//	                                               same txn)
//	/pcp/smarthome/counters/<spaceID>            → smarthome.Counters (running
//	                                               bytes/segments per camera)
//
// Active from Smart Home phases 4–6 (watching + clips, §7/§9):
//
//	/pcp/smarthome/boost/<camID>                 → {until_ms} (live-boost lease;
//	                                               written with a camrev bump so
//	                                               the command poll wakes)
//	/pcp/smarthome/clip/<spaceID>/<invID>        → smarthome.Clip (pins its range
//	                                               against retention; newest-first)
//	/pcp/smarthome/clipshare/<token>             → smarthome.ClipShare (public
//	                                               link; expiry lazy-TTL; the live
//	                                               token also rides the Clip record)
//
// Read-only foreign namespace (spec §11.4 — not /pcp/, not ours): the
// databox `.databox/` system view (nodes/, groups/, shards/,
// stats/groups/, alerts/, admin/pause/), consumed by domain/clusterview
// with graceful degradation when the databox user may not read it.
package kvx
