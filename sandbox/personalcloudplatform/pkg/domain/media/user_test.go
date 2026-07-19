package media

import (
	"testing"
)

func TestForUserMembershipUnionAndHidden(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	vid := f.registerFolder(t, ctx, "Video", KindVideo)
	mus := f.registerFolder(t, ctx, "Music", KindMusic)

	// ada (member) sees both registered folders.
	folders, err := f.media.ForUser(ctx, "ada")
	if err != nil || len(folders) != 2 {
		t.Fatalf("ForUser(ada) = %v (%v)", folders, err)
	}
	if folders[0].Kind != KindMusic || folders[1].Kind != KindVideo {
		t.Errorf("sort order = %v", folders)
	}
	if folders[0].Role != "owner" || folders[0].DriveName != "Media" {
		t.Errorf("folder meta = %+v", folders[0])
	}

	// erin is not a member: nothing.
	if _, err := f.users.CreateUser(ctx, "erin", "Erin", "password123"); err != nil {
		t.Fatal(err)
	}
	if folders, _ := f.media.ForUser(ctx, "erin"); len(folders) != 0 {
		t.Fatalf("non-member sees folders: %v", folders)
	}

	// Membership IS the subscription: join → both appear instantly.
	if err := f.drives.SetMember(ctx, f.drive.ID, "erin", "viewer"); err != nil {
		t.Fatal(err)
	}
	if folders, _ := f.media.ForUser(ctx, "erin"); len(folders) != 2 {
		t.Fatalf("member union = %v", folders)
	}

	// Hidden is per-user: erin hides Video; ada's view is untouched.
	if err := f.media.SetHidden(ctx, "erin", f.drive.ID, vid.ID, true); err != nil {
		t.Fatal(err)
	}
	ef, _ := f.media.ForUser(ctx, "erin")
	hidden := 0
	for _, fo := range ef {
		if fo.Hidden {
			hidden++
			if fo.FolderID != vid.ID {
				t.Errorf("wrong folder hidden: %+v", fo)
			}
		}
	}
	if hidden != 1 {
		t.Errorf("erin hidden count = %d", hidden)
	}
	af, _ := f.media.ForUser(ctx, "ada")
	for _, fo := range af {
		if fo.Hidden {
			t.Errorf("ada inherited erin's hidden flag: %+v", fo)
		}
	}
	// Unhide restores.
	_ = f.media.SetHidden(ctx, "erin", f.drive.ID, vid.ID, false)
	ef, _ = f.media.ForUser(ctx, "erin")
	for _, fo := range ef {
		if fo.Hidden {
			t.Errorf("unhide did not stick: %+v", fo)
		}
	}

	// Leave → gone.
	if err := f.drives.RemoveMember(ctx, f.drive.ID, "erin"); err != nil {
		t.Fatal(err)
	}
	if folders, _ := f.media.ForUser(ctx, "erin"); len(folders) != 0 {
		t.Fatalf("ex-member still sees folders: %v", folders)
	}
	_ = mus
}

func TestProgressCRUD(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	node := f.putFile(t, ctx, "Movie (2023).mp4", "video/mp4", []byte("x"))

	err := f.media.SetProgress(ctx, "ada", Progress{
		DriveID: f.drive.ID, NodeID: node.ID, Kind: ProgVideo, Title: "Movie", Pos: 120, Dur: 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, found, err := f.media.GetProgress(ctx, "ada", node.ID)
	if err != nil || !found || p.Pos != 120 || p.Title != "Movie" || p.Watched() {
		t.Fatalf("progress = %+v found=%v (%v)", p, found, err)
	}

	// Continue-watching shelf: in-flight only, and per-kind.
	recent, err := f.media.RecentProgress(ctx, "ada", ProgVideo, 10, true)
	if err != nil || len(recent) != 1 {
		t.Fatalf("recent = %v (%v)", recent, err)
	}
	if recent, _ := f.media.RecentProgress(ctx, "ada", ProgMusic, 10, false); len(recent) != 0 {
		t.Errorf("video progress leaked into music: %v", recent)
	}

	// A manual Done mark survives later heartbeats.
	if err := f.media.MarkWatched(ctx, "ada", f.drive.ID, node.ID, true); err != nil {
		t.Fatal(err)
	}
	_ = f.media.SetProgress(ctx, "ada", Progress{DriveID: f.drive.ID, NodeID: node.ID, Pos: 5, Dur: 3600})
	if p, _, _ := f.media.GetProgress(ctx, "ada", node.ID); !p.Done {
		t.Errorf("Done mark lost to a heartbeat: %+v", p)
	}
	if recent, _ := f.media.RecentProgress(ctx, "ada", ProgVideo, 10, true); len(recent) != 0 {
		t.Errorf("watched item on the continue shelf: %v", recent)
	}

	// Unmark clears the row entirely.
	_ = f.media.MarkWatched(ctx, "ada", f.drive.ID, node.ID, false)
	if _, found, _ := f.media.GetProgress(ctx, "ada", node.ID); found {
		t.Errorf("unmark kept the row")
	}

	// Clear-all.
	_ = f.media.SetProgress(ctx, "ada", Progress{DriveID: f.drive.ID, NodeID: node.ID, Pos: 30, Dur: 60})
	if err := f.media.ClearProgress(ctx, "ada", nil); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := f.media.GetProgress(ctx, "ada", node.ID); found {
		t.Errorf("clear-all kept the row")
	}
}

// TestRecentCoveredProgress: unregistering a folder hides its progress
// rows from every Continue-watching surface WITHOUT deleting them —
// re-registering (and rescanning) brings the positions back.
func TestRecentCoveredProgress(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Video", KindVideo)
	movie := f.putFile(t, ctx, "Video/Movie (2023).mp4", "video/mp4", []byte("x"))
	if _, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.media.SetProgress(ctx, "ada", Progress{
		DriveID: f.drive.ID, NodeID: movie.ID, Kind: ProgVideo, Title: "Movie", Pos: 120, Dur: 3600,
	}); err != nil {
		t.Fatal(err)
	}

	recent, err := f.media.RecentCoveredProgress(ctx, "ada", ProgVideo, 10, true)
	if err != nil || len(recent) != 1 || recent[0].Pos != 120 {
		t.Fatalf("covered = %v (%v)", recent, err)
	}

	// A HIDDEN folder's rows leave the shelf too (per-user view).
	_ = f.media.SetHidden(ctx, "ada", f.drive.ID, folder.ID, true)
	if recent, _ := f.media.RecentCoveredProgress(ctx, "ada", ProgVideo, 10, true); len(recent) != 0 {
		t.Errorf("hidden folder still on the shelf: %v", recent)
	}
	_ = f.media.SetHidden(ctx, "ada", f.drive.ID, folder.ID, false)

	// Unregister: the shelf empties, but the PROGRESS ROW SURVIVES.
	if err := f.media.Unregister(ctx, f.drive.ID, folder.ID, KindVideo); err != nil {
		t.Fatal(err)
	}
	if recent, _ := f.media.RecentCoveredProgress(ctx, "ada", ProgVideo, 10, true); len(recent) != 0 {
		t.Fatalf("unregistered folder still on the shelf: %v", recent)
	}
	if p, found, _ := f.media.GetProgress(ctx, "ada", movie.ID); !found || p.Pos != 120 {
		t.Fatalf("progress row deleted by unregister: %+v found=%v", p, found)
	}

	// Re-register + rescan: the position comes back exactly.
	if err := f.media.Register(ctx, f.drive.ID, folder.ID, KindVideo, "ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.media.Rescan(ctx, f.drive.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	recent, _ = f.media.RecentCoveredProgress(ctx, "ada", ProgVideo, 10, true)
	if len(recent) != 1 || recent[0].Pos != 120 {
		t.Fatalf("resume position lost across re-register: %v", recent)
	}
}

func TestWatchlistAndFavorites(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	folder := f.registerFolder(t, ctx, "Video", KindVideo)
	it := ListItem{DriveID: f.drive.ID, FolderID: folder.ID, Kind: CatMovie, Slug: "heat-1995-abcd", Title: "Heat"}

	if f.media.OnList(ctx, "ada", ListWatch, it) {
		t.Fatal("empty list claims membership")
	}
	if err := f.media.SetListItem(ctx, "ada", ListWatch, it, true); err != nil {
		t.Fatal(err)
	}
	if !f.media.OnList(ctx, "ada", ListWatch, it) {
		t.Fatal("added item not on list")
	}
	if f.media.OnList(ctx, "ada", ListFavs, it) {
		t.Fatal("watchlist add leaked into favorites")
	}
	items, err := f.media.ListItems(ctx, "ada", ListWatch)
	if err != nil || len(items) != 1 || items[0].Title != "Heat" {
		t.Fatalf("items = %v (%v)", items, err)
	}
	// Per-user: erin's list is empty.
	if items, _ := f.media.ListItems(ctx, "erin", ListWatch); len(items) != 0 {
		t.Errorf("lists leaked across users")
	}
	if err := f.media.SetListItem(ctx, "ada", ListWatch, it, false); err != nil {
		t.Fatal(err)
	}
	if f.media.OnList(ctx, "ada", ListWatch, it) {
		t.Fatal("removed item still on list")
	}
	// Junk shapes never become keys.
	if err := f.media.SetListItem(ctx, "ada", ListWatch, ListItem{DriveID: "../x", Kind: CatMovie, Slug: "a"}, true); err == nil {
		t.Fatal("junk item accepted")
	}
}

func TestPlaylistsCRUD(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pl, err := f.media.CreatePlaylist(ctx, "ada", "Road trip")
	if err != nil {
		t.Fatal(err)
	}
	add := func(title string) {
		t.Helper()
		if err := f.media.UpdatePlaylist(ctx, "ada", pl.ID, func(p *Playlist) error {
			p.Tracks = append(p.Tracks, PlaylistTrack{DriveID: f.drive.ID, NodeID: "AAAAAAAAAAAA", Title: title})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("One")
	add("Two")
	add("Three")

	// Reorder: move index 2 up.
	if err := f.media.UpdatePlaylist(ctx, "ada", pl.ID, func(p *Playlist) error {
		p.Tracks[1], p.Tracks[2] = p.Tracks[2], p.Tracks[1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, found, _ := f.media.GetPlaylist(ctx, "ada", pl.ID)
	if !found || len(got.Tracks) != 3 || got.Tracks[1].Title != "Three" {
		t.Fatalf("reorder = %+v", got.Tracks)
	}

	// Rename.
	if err := f.media.UpdatePlaylist(ctx, "ada", pl.ID, func(p *Playlist) error {
		p.Name = "Long drive"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pls, _ := f.media.Playlists(ctx, "ada")
	if len(pls) != 1 || pls[0].Name != "Long drive" {
		t.Fatalf("playlists = %v", pls)
	}
	// Per-user.
	if pls, _ := f.media.Playlists(ctx, "erin"); len(pls) != 0 {
		t.Errorf("playlists leaked across users")
	}
	// Delete.
	if err := f.media.DeletePlaylist(ctx, "ada", pl.ID); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := f.media.GetPlaylist(ctx, "ada", pl.ID); found {
		t.Errorf("deleted playlist survived")
	}
}
