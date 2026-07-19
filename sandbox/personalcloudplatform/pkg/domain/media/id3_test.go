package media

import (
	"bytes"
	"context"
	"testing"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
)

// --- fixture builders: minimal but REAL ID3v2 tags -------------------------

// id3TextFrame renders one v2.3/v2.4 text frame (encoding 0 latin-1).
func id3TextFrame(id, text string, v4 bool) []byte {
	body := append([]byte{0}, []byte(text)...)
	return id3RawFrame(id, body, v4)
}

// id3RawFrame renders one v2.3/v2.4 frame with the given body.
func id3RawFrame(id string, body []byte, v4 bool) []byte {
	out := []byte(id)
	n := len(body)
	if v4 {
		out = append(out, byte(n>>21&0x7f), byte(n>>14&0x7f), byte(n>>7&0x7f), byte(n&0x7f))
	} else {
		out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	out = append(out, 0, 0) // flags
	return append(out, body...)
}

// id3utf16Frame renders a UTF-16LE-with-BOM text frame (encoding 1).
func id3utf16Frame(id, text string) []byte {
	body := []byte{1, 0xff, 0xfe}
	for _, r := range text {
		body = append(body, byte(r), byte(r>>8))
	}
	return id3RawFrame(id, body, false)
}

// apicFrame renders an APIC frame carrying img as image/jpeg.
func apicFrame(img []byte) []byte {
	body := []byte{0}                            // text encoding
	body = append(body, "image/jpeg"...)         //
	body = append(body, 0, 3)                    // mime NUL + picture type (front cover)
	body = append(body, 'c', 'o', 'v', 'e', 'r') // description
	body = append(body, 0)                       // description NUL
	body = append(body, img...)
	return id3RawFrame("APIC", body, false)
}

// id3Tag assembles a whole ID3v2 tag (version 3 or 4) from frames.
func id3Tag(version byte, frames ...[]byte) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	body = append(body, make([]byte, 32)...) // padding
	n := len(body)
	head := []byte{'I', 'D', '3', version, 0, 0,
		byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	return append(head, body...)
}

// mp3Fixture is a tag plus a token of MPEG frame bytes — enough for
// content sniffing to say audio/mpeg.
func mp3Fixture(tag []byte) []byte {
	return append(append([]byte{}, tag...), 0xff, 0xfb, 0x90, 0x00, 1, 2, 3, 4)
}

func TestParseID3v23(t *testing.T) {
	cover := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	tag := id3Tag(3,
		id3TextFrame("TIT2", "Karma Police", false),
		id3TextFrame("TPE1", "Radiohead", false),
		id3TextFrame("TALB", "OK Computer", false),
		id3TextFrame("TRCK", "6/12", false),
		id3TextFrame("TYER", "1997", false),
		apicFrame(cover),
	)
	info, ok := parseID3Frames(tag[10:], 3, tag[5])
	if !ok {
		t.Fatal("parse failed")
	}
	if info.Title != "Karma Police" || info.Artist != "Radiohead" || info.Album != "OK Computer" {
		t.Errorf("text frames = %+v", info)
	}
	if info.Track != 6 || info.Year != 1997 {
		t.Errorf("track/year = %d/%d", info.Track, info.Year)
	}
	if !bytes.Equal(info.Cover, cover) || info.CoverMIME != "image/jpeg" {
		t.Errorf("cover = % x mime=%q", info.Cover, info.CoverMIME)
	}
}

func TestParseID3v24SyncsafeAndUTF16(t *testing.T) {
	tag := id3Tag(4,
		id3TextFrame("TIT2", "Song", true),
		id3TextFrame("TDRC", "2004-05-01", true),
	)
	info, ok := parseID3Frames(tag[10:], 4, tag[5])
	if !ok || info.Title != "Song" || info.Year != 2004 {
		t.Fatalf("v2.4 parse = %+v ok=%v", info, ok)
	}

	utf := id3Tag(3, id3utf16Frame("TPE1", "Björk"))
	info, ok = parseID3Frames(utf[10:], 3, utf[5])
	if !ok || info.Artist != "Björk" {
		t.Fatalf("utf-16 artist = %q ok=%v", info.Artist, ok)
	}
}

func TestReadID3RangedFromBlobStore(t *testing.T) {
	db := kvxtest.New(t)
	s := &Store{DB: db}
	ctx := context.Background()
	blob := mp3Fixture(id3Tag(3,
		id3TextFrame("TIT2", "Track A", false),
		id3TextFrame("TPE1", "Artist One", false),
		id3TextFrame("TALB", "Album Alpha", false),
	))
	if err := db.PutBlob(ctx, "/pcp/blobs/test/x", bytes.NewReader(blob), "audio/mpeg"); err != nil {
		t.Fatal(err)
	}
	info, ok := s.ReadID3(ctx, "/pcp/blobs/test/x")
	if !ok || info.Title != "Track A" || info.Artist != "Artist One" || info.Album != "Album Alpha" {
		t.Fatalf("ReadID3 = %+v ok=%v", info, ok)
	}

	// An untagged blob is a plain miss.
	_ = db.PutBlob(ctx, "/pcp/blobs/test/y", bytes.NewReader([]byte{0xff, 0xfb, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}), "audio/mpeg")
	if _, ok := s.ReadID3(ctx, "/pcp/blobs/test/y"); ok {
		t.Fatal("untagged blob parsed as tagged")
	}
}
