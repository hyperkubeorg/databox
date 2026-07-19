// featurepurge.go — the per-feature data-purge backend for the Services
// page (Draft 004 §9). Purge deletes a feature's OWN keyspace across the
// domains the site package can't import, so it lives here at the app
// layer and keys off the same feature ids (site.Feature*). The Services
// detail handler (servicespage.go) calls purgeParts/purgeOrphans up front
// for the ten-click gauntlet, and purgeFeature on the final click.
//
// Every feature's key families are hardcoded here (the domain packages
// keep them unexported) — the map below is the single place they live for
// purge. Blob-bearing families (large values in the separate blob store)
// are swept with StatBlob/DeleteBlob so bytes are freed, not just their
// index rows. Calendar/Contacts have (almost) no keyspace of their own —
// their data is .pccal/.pccard FILES inside drives — so those purges walk
// every drive and delete the files by extension. Video and Music SHARE
// the /pcp/media/ keyspace: their purge is kind-scoped so removing one
// leaves the other's registrations and catalogs intact.
package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hyperkubeorg/databox/pkg/client"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// drivesPrefix roots the drive records; a drive id is the one key segment
// after it (drives.driveKey), so scanning it enumerates every drive on the
// site — the calendar/contacts file sweeps' drive list.
const drivesPrefix = "/pcp/drives/"

// kvFamilies maps a feature to the pure key/value prefixes its purge
// range-deletes. Blob-bearing prefixes are listed separately in
// blobFamilies. Calendar's file sweep and the media kind-scoped sweep add
// their own logic on top; drive is the nuclear superset.
var kvFamilies = map[string][]string{
	site.FeatureDrive: {
		"/pcp/drives/", "/pcp/members/", "/pcp/userdrives/", // drives domain
		"/pcp/nodes/", "/pcp/noderef/", "/pcp/versions/", // nodes domain (tree)
		"/pcp/shares/", "/pcp/sharesess/", "/pcp/nodeshares/", // shares domain
		"/pcp/grants/", "/pcp/nodegrants/",
		"/pcp/media/registry/", "/pcp/media/catalog/", // registrations die with the drive
	},
	site.FeatureMail: {
		"/pcp/mail/domains/", "/pcp/mail/dkim/", "/pcp/mail/postoffices/",
		"/pcp/mail/podomains/", "/pcp/mail/domainpos/", "/pcp/mail/addrs/",
		"/pcp/mail/useraddrs/", "/pcp/mail/boxes/", "/pcp/mail/threads/",
		"/pcp/mail/threadidx/", "/pcp/mail/sentidx/", "/pcp/mail/starred/",
		"/pcp/mail/msgs/", "/pcp/mail/msgref/", "/pcp/mail/labels/",
		"/pcp/mail/bylabel/", "/pcp/mail/drafts/", "/pcp/mail/folders/",
		"/pcp/mail/searchtext/", "/pcp/mail/outq/", "/pcp/mail/pocursors/",
		"/pcp/mail/sendlog/", "/pcp/mail/welcome/", "/pcp/mail/recents/",
		"/pcp/mail/idem/", "/pcp/mail/gcpending/", "/pcp/mail/serial",
		"/pcp/mail/gccursor",
		// NOTE: /pcp/mail/icsrsvp/ is Calendar's inbound-RSVP family (it
		// only rents the mail namespace) — Calendar's purge owns it.
	},
	site.FeatureMessenger: {
		"/pcp/msg/servers/", "/pcp/msg/members/", "/pcp/msg/usermembers/",
		"/pcp/msg/roles/", "/pcp/msg/channels/", "/pcp/msg/discover/",
		"/pcp/msg/convos/", "/pcp/msg/msgs/", "/pcp/msg/msgref/",
		"/pcp/msg/read/", "/pcp/msg/unread/", "/pcp/msg/notified/",
		"/pcp/msg/mentions/", "/pcp/msg/dmidx/", "/pcp/msg/dms/",
		"/pcp/msg/groupmembers/", "/pcp/msg/presence/", "/pcp/msg/online/",
		"/pcp/msg/typing/", "/pcp/msg/search/", "/pcp/msg/authoridx/",
		"/pcp/msg/invites/", "/pcp/msg/serverinvites/", "/pcp/msg/profiles/",
	},
	site.FeatureGit: {
		"/pcp/git/ns/", "/pcp/git/profiles/", "/pcp/git/orgs/",
		"/pcp/git/orgmembers/", "/pcp/git/userorgs/", "/pcp/git/teams/",
		"/pcp/git/grants/", "/pcp/git/usergrants/", "/pcp/git/teamgrants/",
		"/pcp/git/repo/", "/pcp/git/reponame/", "/pcp/git/forks/",
		"/pcp/git/refs/", "/pcp/git/obj/", "/pcp/git/seq/",
		"/pcp/git/issues/", "/pcp/git/issueidx/", "/pcp/git/assigned/",
		"/pcp/git/comments/", "/pcp/git/labels/", "/pcp/git/merges/",
		"/pcp/git/mergeidx/", "/pcp/git/mrsrc/", "/pcp/git/sshkeys/",
		"/pcp/git/sshfp/", "/pcp/git/sshhostkey",
	},
	site.FeatureCalendar: {
		"/pcp/calsubs/",      // subscription/visibility overrides
		"/pcp/mail/icsrsvp/", // inbound RSVP replies to foreign invites
	},
	// Smart Home owns its keyspace outright (Draft 005 §6.1) — one prefix
	// covers spaces, members, the allowlist, and every later family
	// (cameras, agents, segments, events, clips). Segment/thumbnail BLOB
	// families join blobFamilies when ingest lands (phase 3).
	site.FeatureSmartHome: {"/pcp/smarthome/"},
}

// blobFamilies maps a feature to the prefixes whose keys may hold blobs
// (or a mix of blobs and ordinary rows, like the collab /pcp/docs/ doc
// space). These are swept with StatBlob+DeleteBlob so the blob storage is
// freed, then range-cleared for any residual index rows.
var blobFamilies = map[string][]string{
	site.FeatureDrive: {
		"/pcp/blobs/", "/pcp/thumbs/", "/pcp/tmp/", // file, thumbnail, upload-assembly blobs
		"/pcp/docs/",      // collab op-log rows + snapshot blobs (calendar ledgers ride here)
		"/pcp/media/art/", // harvested cover-art blobs
	},
	site.FeatureMail:      {"/pcp/mail/blobs/"},
	site.FeatureMessenger: {"/pcp/msg/blobs/"},
	site.FeatureGit:       {"/pcp/git/objblob/"},
	site.FeatureSmartHome: {"/pcp/smarthome/segblob/", "/pcp/smarthome/thumbblob/"},
}

// purgeFeature irreversibly deletes feature id's data from databox,
// returning how many records and bytes were removed. Allowed whether the
// feature is on or off. id is one of the site.Feature* ids.
func purgeFeature(ctx context.Context, k *kernel.App, id string) (records int, bytes int64, err error) {
	db := appDB(k)
	if db == nil {
		return 0, 0, fmt.Errorf("purge %s: no databox client available", id)
	}
	var firstErr error
	note := func(e error) {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}

	switch id {
	case site.FeatureVideo, site.FeatureMusic:
		r, b, e := purgeMedia(ctx, k, db, id)
		return r, b, e
	case site.FeatureCalendar:
		// Own key families first, then the .pccal files in every drive.
		records, bytes, err = sweepKV(ctx, db, kvFamilies[id])
		note(err)
		fr, fb, fe := purgeDriveFiles(ctx, k, db, ".pccal")
		records += fr
		bytes += fb
		note(fe)
		return records, bytes, firstErr
	case site.FeatureContacts:
		return purgeDriveFiles(ctx, k, db, ".pccard")
	case site.FeatureDrive, site.FeatureMail, site.FeatureMessenger, site.FeatureGit, site.FeatureSmartHome:
		// Free blob storage BEFORE the range-deletes remove the keys that
		// make the blobs enumerable.
		br, bb, be := sweepBlobs(ctx, db, blobFamilies[id])
		note(be)
		kr, kb, ke := sweepKV(ctx, db, kvFamilies[id])
		note(ke)
		return br + kr, bb + kb, firstErr
	default:
		return 0, 0, fmt.Errorf("unknown feature %q", id)
	}
}

// sweepKV counts and range-deletes pure key/value prefixes: it streams
// each prefix accumulating records and value bytes, then DeleteRanges it.
// A prefix with no keys contributes nothing. Returns the first hard error
// but always attempts every prefix.
func sweepKV(ctx context.Context, db *client.Client, prefixes []string) (records int, bytes int64, firstErr error) {
	for _, p := range prefixes {
		if e := kvx.ScanPrefix(ctx, db, p, func(_ string, value []byte) error {
			records++
			bytes += int64(len(value))
			return nil
		}); e != nil && firstErr == nil {
			firstErr = e
		}
		if e := kvx.DeletePrefix(ctx, db, p); e != nil && firstErr == nil {
			firstErr = e
		}
	}
	return records, bytes, firstErr
}

// sweepBlobs handles prefixes whose keys may be blobs (large values in the
// separate blob store) or ordinary rows (the collab doc space mixes both).
// Per key it counts a record, and: if the key holds a blob, adds its real
// size (StatBlob) and frees it (DeleteBlob); otherwise adds the row's
// value bytes and deletes the row. A final range-delete clears any residue.
// Deletes are best-effort (a leaked blob wastes space, nothing else) so a
// single failure never aborts the sweep.
func sweepBlobs(ctx context.Context, db *client.Client, prefixes []string) (records int, bytes int64, firstErr error) {
	for _, p := range prefixes {
		if e := kvx.ScanPrefix(ctx, db, p, func(key string, value []byte) error {
			records++
			if size, _, found, _ := db.StatBlob(ctx, key); found {
				bytes += size
				_ = db.DeleteBlob(ctx, key)
			} else {
				bytes += int64(len(value))
				_ = db.Delete(ctx, key)
			}
			return nil
		}); e != nil && firstErr == nil {
			firstErr = e
		}
		if e := kvx.DeletePrefix(ctx, db, p); e != nil && firstErr == nil {
			firstErr = e
		}
	}
	return records, bytes, firstErr
}

// purgeDriveFiles deletes every file with suffix (".pccal"/".pccard")
// across every drive on the site — the calendar/contacts purge, whose data
// lives as ordinary Drive files, not a keyspace of its own. It uses the
// composed shares.DeleteNode (which sweeps share links + grants and refunds
// quota) when available, falling back to nodes.DeleteForever. Files in
// drives the sweep cannot read are simply left in place (best-effort).
func purgeDriveFiles(ctx context.Context, k *kernel.App, db *client.Client, suffix string) (records int, bytes int64, firstErr error) {
	if k.Nodes == nil {
		return 0, 0, nil
	}
	var driveIDs []string
	if e := kvx.ScanPrefix(ctx, db, drivesPrefix, func(key string, _ []byte) error {
		id := key[len(drivesPrefix):]
		if kvx.ValidID(id) {
			driveIDs = append(driveIDs, id)
		}
		return nil
	}); e != nil {
		firstErr = e
	}
	for _, d := range driveIDs {
		files, e := k.Nodes.FindBySuffix(ctx, d, suffix)
		if e != nil {
			if firstErr == nil {
				firstErr = e
			}
			continue
		}
		for _, n := range files {
			var freed int64
			var de error
			if k.Shares != nil {
				freed, de = k.Shares.DeleteNode(ctx, d, n.ID)
			} else {
				freed, de = k.Nodes.DeleteForever(ctx, d, n.ID, nil)
			}
			if de != nil {
				if firstErr == nil {
					firstErr = de
				}
				continue
			}
			records++
			bytes += freed
		}
	}
	return records, bytes, firstErr
}

// purgeMedia is the kind-scoped Video/Music purge. Video and Music share
// the /pcp/media/ keyspace, so a blanket sweep would nuke both. Instead it
// walks every registration, and for each that covers this kind it removes
// ONLY that kind's registration slice, derived catalog, and harvested art
// (media.Unregister does exactly this, leaving the other kind intact), then
// sweeps the per-user rows this kind owns (progress, watchlist/favorites,
// and — music only — playlists). The underlying media FILES are the user's
// Drive content and are never touched. Hidden overrides are not
// kind-tagged and are left (they resolve live and drop out on their own).
func purgeMedia(ctx context.Context, k *kernel.App, db *client.Client, kind string) (records int, bytes int64, firstErr error) {
	note := func(e error) {
		if e != nil && firstErr == nil {
			firstErr = e
		}
	}
	if k.Media != nil {
		regs, e := k.Media.AllRegistrations(ctx)
		note(e)
		for _, r := range regs {
			if !r.Has(kind) {
				continue
			}
			records++ // the registration row's slice for this kind
			for _, ck := range media.CatKinds(kind) {
				entries, ce := k.Media.ListCatalog(ctx, r.DriveID, r.FolderID, ck)
				if ce != nil {
					note(ce)
					continue
				}
				for _, en := range entries {
					records++
					if en.ArtBlob != "" {
						if size, _, found, _ := db.StatBlob(ctx, media.ArtKey(r.DriveID, r.FolderID, en.ArtBlob)); found {
							bytes += size
						}
					}
				}
			}
			// The real deletion (catalog rows + art blobs + registry slice),
			// leaving the other kind's data intact.
			note(k.Media.Unregister(ctx, r.DriveID, r.FolderID, kind))
		}
	}

	// Per-user rows carry the kind in their VALUE, so filter on decode.
	progKind := media.ProgVideo
	if kind == site.FeatureMusic {
		progKind = media.ProgMusic
	}
	r, b := sweepMatching(ctx, db, "/pcp/media/progress/", &firstErr, func(value []byte) bool {
		var p media.Progress
		return json.Unmarshal(value, &p) == nil && p.Kind == progKind
	})
	records += r
	bytes += b

	listKindOK := func(value []byte) bool {
		var it media.ListItem
		if json.Unmarshal(value, &it) != nil {
			return false
		}
		if kind == site.FeatureMusic {
			return it.Kind == media.CatAlbum || it.Kind == media.CatArtist
		}
		return it.Kind == media.CatMovie || it.Kind == media.CatSeries
	}
	for _, prefix := range []string{"/pcp/media/watchlist/", "/pcp/media/favorites/"} {
		r, b := sweepMatching(ctx, db, prefix, &firstErr, listKindOK)
		records += r
		bytes += b
	}

	// Playlists are a Music-only construct.
	if kind == site.FeatureMusic {
		r, b := sweepMatching(ctx, db, "/pcp/media/playlists/", &firstErr, func([]byte) bool { return true })
		records += r
		bytes += b
	}
	return records, bytes, firstErr
}

// sweepMatching streams a prefix and deletes only the rows match accepts,
// counting them. Deletes are best-effort; the first error is recorded via
// firstErr but the scan continues.
func sweepMatching(ctx context.Context, db *client.Client, prefix string, firstErr *error, match func(value []byte) bool) (records int, bytes int64) {
	if e := kvx.ScanPrefix(ctx, db, prefix, func(key string, value []byte) error {
		if !match(value) {
			return nil
		}
		records++
		bytes += int64(len(value))
		if e := db.Delete(ctx, key); e != nil && *firstErr == nil {
			*firstErr = e
		}
		return nil
	}); e != nil && *firstErr == nil {
		*firstErr = e
	}
	return records, bytes
}

// appDB returns the databox client shared by every store (they all wrap
// the same *client.Client). Site is always wired; Users is the fallback.
func appDB(k *kernel.App) *client.Client {
	if k.Site != nil {
		return k.Site.DB
	}
	if k.Users != nil {
		return k.Users.DB
	}
	return nil
}

// purgeParts returns human-readable lines describing what purging id
// destroys (shown in the confirmation gauntlet up front).
func purgeParts(id string) []string {
	switch id {
	case site.FeatureDrive:
		return []string{
			"Every drive, folder, and file — all uploaded blobs, every version, and thumbnails.",
			"All share links and access grants.",
			"Calendar events and Contacts cards live as Drive files and are destroyed with the Drive.",
			"Video and Music registrations, catalogs, and cover art (the media files are Drive files, destroyed here too).",
			"The nuclear option: nothing that lives in a Drive survives, and there is no undo and no backup.",
		}
	case site.FeatureMail:
		return []string{
			"Every mailbox, thread, and message, and all message blobs (attachments and raw MIME).",
			"Mail addresses and aliases, drafts, folders, labels, starred flags, and search text.",
			"Mail domains, DKIM keys, and postoffice pairings — outbound sending stops until reconfigured.",
		}
	case site.FeatureCalendar:
		return []string{
			"Every .pccal calendar file across all drives (a best-effort per-drive sweep).",
			"Calendar subscription overrides and inbound RSVP replies.",
			"The .pccal files live inside users' drives — a file in a drive the sweep cannot read is left in place.",
		}
	case site.FeatureContacts:
		return []string{
			"Every .pccard contact card file across all drives (a best-effort per-drive sweep).",
			"Contacts keeps no keyspace of its own — the .pccard files live inside users' drives, so any the sweep cannot reach are left in place.",
		}
	case site.FeatureVideo:
		return []string{
			"Video folder registrations, their derived catalogs, and harvested cover art.",
			"Per-user video playback progress, watchlist, and favorites.",
			"The media registry is SHARED with Music: this removes only VIDEO registrations and catalogs and leaves Music untouched.",
			"The underlying video files are your Drive content and are NOT deleted.",
		}
	case site.FeatureMusic:
		return []string{
			"Music folder registrations, their derived catalogs, and harvested cover art.",
			"Per-user music playback progress, watchlist, favorites, and playlists.",
			"The media registry is SHARED with Video: this removes only MUSIC registrations and catalogs and leaves Video untouched.",
			"The underlying music files are your Drive content and are NOT deleted.",
		}
	case site.FeatureMessenger:
		return []string{
			"Every server, channel, and direct-message conversation, with all messages and attachment blobs.",
			"Memberships, roles, invites, presence, read state, and mentions.",
		}
	case site.FeatureGit:
		return []string{
			"The entire Git keyspace: repositories, git objects and refs, organizations, teams, and access grants.",
			"All issues, merge requests, comments, and labels.",
			"SSH public keys and the cluster-shared host key.",
		}
	case site.FeatureSmartHome:
		return []string{
			"Every space, its members and roles, and the admin creation allowlist.",
			"All cameras, paired agents (their tokens revoked), recorded footage, thumbnails, events, and clips.",
			"Any pcp-camd agents still running will fail to ingest until re-paired.",
		}
	default:
		return nil
	}
}

// purgeOrphans returns names of cross-feature data a purge of id would
// leave dangling (e.g. purging Git orphans a Builds job that referenced a
// repo). The list sharpens the gauntlet's warning so the admin decides
// deliberately.
func purgeOrphans(sc site.Config, id string) []string {
	switch id {
	case site.FeatureDrive:
		var out []string
		if sc.FeatureEnabled(site.FeatureCalendar) {
			out = append(out, "Calendar is enabled — its events live in Drive files being destroyed.")
		}
		if sc.FeatureEnabled(site.FeatureContacts) {
			out = append(out, "Contacts is enabled — its cards live in Drive files being destroyed.")
		}
		if sc.FeatureEnabled(site.FeatureVideo) {
			out = append(out, "Video is enabled — its registrations point at folders being destroyed.")
		}
		if sc.FeatureEnabled(site.FeatureMusic) {
			out = append(out, "Music is enabled — its registrations point at folders being destroyed.")
		}
		return out
	case site.FeatureGit:
		return []string{
			"Builds jobs that referenced a repository become orphans — Builds keeps its own /pcp/build/* keyspace and is not purged here.",
		}
	default:
		return nil
	}
}
