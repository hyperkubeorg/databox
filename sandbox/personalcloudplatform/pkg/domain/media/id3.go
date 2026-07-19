// id3.go — a minimal, self-contained ID3v2 reader: exactly the frames
// the media indexer needs (title, artist, album, track, year, embedded
// cover art), nothing else. Tags sit at the FRONT of an MP3, so the
// indexer reads them with a ranged blob read — a 40MB album track costs
// a few hundred KB of tag bytes to catalog, not a full download.
//
// Supports v2.2 (3-byte frames), v2.3, and v2.4 (syncsafe sizes) with
// latin-1, UTF-16 (BOM/BE), and UTF-8 text encodings — the combinations
// real-world files actually carry. Ported from PCD.
package media

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ID3Info is what the indexer extracts from one file.
type ID3Info struct {
	Title  string
	Artist string
	Album  string
	Track  int
	Year   int
	// Cover is the APIC frame's image bytes (nil = none); CoverMIME its
	// declared type (sniffed again before it's ever served).
	Cover     []byte
	CoverMIME string
}

// id3MaxTag caps how much tag we read — art-heavy tags run ~1MB.
const id3MaxTag = 4 << 20

// ReadID3 fetches and parses the leading ID3v2 tag of the blob at key.
// found=false for files without one (untagged MP3s, non-MP3 audio) —
// never an error; the indexer falls back to filename conventions.
func (s *Store) ReadID3(ctx context.Context, blobKey string) (ID3Info, bool) {
	var head bytes.Buffer
	if err := s.DB.GetBlobRange(ctx, blobKey, 0, 10, &head); err != nil || head.Len() < 10 {
		return ID3Info{}, false
	}
	h := head.Bytes()
	if string(h[:3]) != "ID3" || h[3] > 4 {
		return ID3Info{}, false
	}
	version := int(h[3])
	size := syncsafe(h[6:10])
	if size <= 0 || size > id3MaxTag {
		return ID3Info{}, false
	}
	var tag bytes.Buffer
	tag.Grow(size)
	if err := s.DB.GetBlobRange(ctx, blobKey, 10, int64(size), &tag); err != nil {
		return ID3Info{}, false
	}
	return parseID3Frames(tag.Bytes(), version, h[5])
}

// syncsafe decodes a 4-byte syncsafe integer (7 bits per byte).
func syncsafe(b []byte) int {
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

// parseID3Frames walks the frame list, harvesting the wanted ones.
func parseID3Frames(data []byte, version int, flags byte) (ID3Info, bool) {
	var info ID3Info
	// An extended header (v2.3/2.4 flag bit 6) prefixes the frames.
	if flags&0x40 != 0 && len(data) >= 4 {
		ext := 0
		if version == 4 {
			ext = syncsafe(data[:4])
		} else {
			ext = int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3]) + 4
		}
		if ext > 0 && ext < len(data) {
			data = data[ext:]
		}
	}
	idLen, szLen, hdrLen := 4, 4, 10
	if version == 2 {
		idLen, szLen, hdrLen = 3, 3, 6
	}
	got := false
	for len(data) >= hdrLen {
		id := string(data[:idLen])
		if id == strings.Repeat("\x00", idLen) {
			break // padding
		}
		var size int
		switch {
		case version == 2:
			size = int(data[3])<<16 | int(data[4])<<8 | int(data[5])
		case version == 4:
			size = syncsafe(data[idLen : idLen+szLen])
		default:
			size = int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		}
		if size < 0 || hdrLen+size > len(data) {
			break
		}
		body := data[hdrLen : hdrLen+size]
		switch id {
		case "TIT2", "TT2":
			info.Title, got = id3Text(body), true
		case "TPE1", "TP1":
			info.Artist, got = id3Text(body), true
		case "TALB", "TAL":
			info.Album, got = id3Text(body), true
		case "TRCK", "TRK":
			t := id3Text(body)
			if i := strings.IndexByte(t, '/'); i >= 0 {
				t = t[:i]
			}
			info.Track, _ = strconv.Atoi(strings.TrimSpace(t))
			got = true
		case "TYER", "TYE", "TDRC":
			y := id3Text(body)
			if len(y) >= 4 {
				info.Year, _ = strconv.Atoi(y[:4])
			}
			got = true
		case "APIC", "PIC":
			if mime, img := id3Picture(body, version); img != nil && info.Cover == nil {
				info.Cover, info.CoverMIME, got = img, mime, true
			}
		}
		data = data[hdrLen+size:]
	}
	return info, got
}

// id3Text decodes a text frame: encoding byte + payload.
func id3Text(body []byte) string {
	if len(body) < 2 {
		return ""
	}
	s := decodeID3String(body[0], body[1:])
	return strings.TrimSpace(strings.Trim(s, "\x00"))
}

// decodeID3String handles the four ID3 text encodings.
func decodeID3String(enc byte, b []byte) string {
	switch enc {
	case 0: // latin-1
		out := make([]rune, 0, len(b))
		for _, c := range b {
			if c != 0 {
				out = append(out, rune(c))
			}
		}
		return string(out)
	case 1, 2: // UTF-16 with BOM / UTF-16BE
		be := enc == 2
		if len(b) >= 2 && enc == 1 {
			if b[0] == 0xfe && b[1] == 0xff {
				be, b = true, b[2:]
			} else if b[0] == 0xff && b[1] == 0xfe {
				be, b = false, b[2:]
			}
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if be {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		return string(utf16.Decode(u))
	default: // 3 = UTF-8
		return string(b)
	}
}

// id3Picture unpacks an APIC (v2.3/2.4) or PIC (v2.2) frame.
func id3Picture(body []byte, version int) (mime string, img []byte) {
	if len(body) < 4 {
		return "", nil
	}
	enc := body[0]
	rest := body[1:]
	if version == 2 { // PIC: 3-byte image format, no MIME string
		if len(rest) < 4 {
			return "", nil
		}
		mime = "image/" + strings.ToLower(string(rest[:3]))
		rest = rest[3:]
	} else {
		i := bytes.IndexByte(rest, 0)
		if i < 0 {
			return "", nil
		}
		mime = string(rest[:i])
		rest = rest[i+1:]
	}
	if len(rest) < 1 {
		return "", nil
	}
	rest = rest[1:] // picture type byte
	// Description: null-terminated in the frame's text encoding.
	if enc == 1 || enc == 2 {
		for i := 0; i+1 < len(rest); i += 2 {
			if rest[i] == 0 && rest[i+1] == 0 {
				rest = rest[i+2:]
				break
			}
		}
	} else {
		if i := bytes.IndexByte(rest, 0); i >= 0 {
			rest = rest[i+1:]
		}
	}
	if len(rest) == 0 {
		return "", nil
	}
	return mime, rest
}
