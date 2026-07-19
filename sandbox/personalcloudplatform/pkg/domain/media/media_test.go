package media

import (
	"context"
	"strings"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
)

func TestRegistry(t *testing.T) {
	s := &Store{DB: kvxtest.New(t)}
	ctx := context.Background()
	drive, folderA, folderB := kvx.NewID(), kvx.NewID(), kvx.NewID()

	if err := s.Register(ctx, drive, folderA, "podcast", "ada"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := s.Register(ctx, drive, folderA, KindMusic, "ada"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.Register(ctx, drive, folderB, KindVideo, "bob"); err != nil {
		t.Fatalf("register: %v", err)
	}
	reg, found, err := s.Get(ctx, drive, folderA)
	if err != nil || !found || !reg.Has(KindMusic) || reg.Has(KindVideo) || reg.By != "ada" {
		t.Fatalf("get = %+v found=%v (%v)", reg, found, err)
	}
	// Kinds are a SET: registering the other kind ADDS it — one mixed
	// folder feeds both apps.
	if err := s.Register(ctx, drive, folderA, KindVideo, "ada"); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if reg, _, _ := s.Get(ctx, drive, folderA); !reg.Has(KindVideo) || !reg.Has(KindMusic) {
		t.Fatalf("dual registration failed: %+v", reg)
	}
	// Registering an already-held kind never duplicates it.
	if err := s.Register(ctx, drive, folderA, KindVideo, "ada"); err != nil {
		t.Fatalf("repeat register: %v", err)
	}
	if reg, _, _ := s.Get(ctx, drive, folderA); len(reg.Kinds) != 2 {
		t.Fatalf("kinds not deduped: %+v", reg)
	}
	rows, err := s.ListRegistered(ctx, drive)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list = %+v (%v)", rows, err)
	}
	// Unregistering ONE kind keeps the row (and the other kind).
	if err := s.Unregister(ctx, drive, folderA, KindMusic); err != nil {
		t.Fatalf("unregister music: %v", err)
	}
	if reg, found, _ := s.Get(ctx, drive, folderA); !found || reg.Has(KindMusic) || !reg.Has(KindVideo) {
		t.Fatalf("kind-scoped unregister = %+v found=%v", reg, found)
	}
	// Unregistering the LAST kind deletes the row. Idempotent.
	if err := s.Unregister(ctx, drive, folderA, KindVideo); err != nil {
		t.Fatalf("unregister video: %v", err)
	}
	if _, found, _ := s.Get(ctx, drive, folderA); found {
		t.Fatal("row survived its last kind")
	}
	if err := s.Unregister(ctx, drive, folderA, KindVideo); err != nil {
		t.Fatalf("re-unregister: %v", err)
	}
	if rows, _ := s.ListRegistered(ctx, drive); len(rows) != 1 || rows[0].FolderID != folderB {
		t.Fatalf("list after unregister = %+v", rows)
	}
	// Unregister-all (kind "") drops a row in one call.
	if err := s.Unregister(ctx, drive, folderB, ""); err != nil {
		t.Fatalf("unregister all: %v", err)
	}
	if rows, _ := s.ListRegistered(ctx, drive); len(rows) != 0 {
		t.Fatalf("unregister-all left rows: %+v", rows)
	}

	// Drive purge drops everything.
	if err := s.Register(ctx, drive, folderA, KindMusic, "ada"); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeDrive(ctx, drive); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if rows, _ := s.ListRegistered(ctx, drive); len(rows) != 0 {
		t.Fatalf("registry survived the purge: %+v", rows)
	}
}

// TestRegistrationDecodeCompat proves rows written by the single-kind
// registry shape ({"kind":"music"}) still decode — users have live
// registries.
func TestRegistrationDecodeCompat(t *testing.T) {
	s := &Store{DB: kvxtest.New(t)}
	ctx := context.Background()
	drive, folder := kvx.NewID(), kvx.NewID()

	old := `{"kind":"music","by":"ada","at":"2026-07-01T12:00:00Z"}`
	if _, err := s.DB.Set(ctx, registryKey(drive, folder), []byte(old)); err != nil {
		t.Fatalf("seed old row: %v", err)
	}
	reg, found, err := s.Get(ctx, drive, folder)
	if err != nil || !found {
		t.Fatalf("get = %v found=%v", err, found)
	}
	if !reg.Has(KindMusic) || reg.Has(KindVideo) || reg.By != "ada" || reg.At.IsZero() {
		t.Fatalf("old row decoded wrong: %+v", reg)
	}
	// A registration on top of the old row folds into the set.
	if err := s.Register(ctx, drive, folder, KindVideo, "bob"); err != nil {
		t.Fatalf("register over old row: %v", err)
	}
	reg, _, _ = s.Get(ctx, drive, folder)
	if !reg.Has(KindMusic) || !reg.Has(KindVideo) {
		t.Fatalf("upgrade lost a kind: %+v", reg)
	}
	rows, err := s.AllRegistrations(ctx)
	if err != nil || len(rows) != 1 || strings.Join(rows[0].Kinds, ",") != "music,video" {
		t.Fatalf("all registrations = %+v (%v)", rows, err)
	}
}
