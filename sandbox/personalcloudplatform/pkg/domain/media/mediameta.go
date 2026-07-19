// mediameta.go — the sidecar metadata format (spec §9): plain-JSON
// files that classify and describe media with NO third-party services.
// `info.pcmeta` describes a folder's show/movie/album; `<file>.pcmeta`
// describes one file. Hand-editable, rescan-stable, travels with the
// files. Ported from PCD — the format string stays "pcd-meta/1" so
// sidecars written there keep working here.
package media

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Sidecar constants.
const (
	MediaMetaFormat = "pcd-meta/1"
	MediaMetaExt    = ".pcmeta"
	// MediaMetaFolderName is the folder-level sidecar's filename.
	MediaMetaFolderName = "info.pcmeta"
	mediaMetaMax        = 64 << 10
)

// Media types a sidecar can pin (overriding filename heuristics).
const (
	MediaTypeSeries = "series"
	MediaTypeParts  = "parts"
	MediaTypeMovie  = "movie"
)

// MediaMeta is one sidecar's content.
type MediaMeta struct {
	Format      string   `json:"format"`
	Type        string   `json:"type,omitempty"` // series | parts | movie
	Title       string   `json:"title,omitempty"`
	Year        int      `json:"year,omitempty"`
	Description string   `json:"description,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	// Music fields. On a track's own sidecar they relabel that one audio
	// file; on a folder's info.pcmeta, Album/Artist/Year pin the album
	// details for every track in the folder. Explicit metadata always
	// beats ID3 tags and filename conventions.
	Artist  string `json:"artist,omitempty"`
	Album   string `json:"album,omitempty"`
	TrackNo int    `json:"track_no,omitempty"`
}

// ValidMediaMeta is the shape gate the indexer and any writer share.
func ValidMediaMeta(m MediaMeta) error {
	switch m.Type {
	case "", MediaTypeSeries, MediaTypeParts, MediaTypeMovie:
	default:
		return fmt.Errorf("bad media type %q", m.Type)
	}
	if len(m.Title) > 200 || len(m.Description) > 4000 {
		return fmt.Errorf("title/description too long")
	}
	if m.Year < 0 || m.Year > 3000 {
		return fmt.Errorf("bad year")
	}
	if len(m.Genres) > 12 {
		return fmt.Errorf("at most 12 genres")
	}
	if len(m.Artist) > 300 || len(m.Album) > 200 {
		return fmt.Errorf("artist/album too long")
	}
	if m.TrackNo < 0 || m.TrackNo > 999 {
		return fmt.Errorf("bad track number")
	}
	for _, g := range m.Genres {
		if g == "" || len(g) > 40 {
			return fmt.Errorf("bad genre")
		}
	}
	return nil
}

// ParseMediaMeta decodes a sidecar; junk is a plain miss (the indexer
// falls back to filenames), never an error surfaced to a scan.
func ParseMediaMeta(raw []byte) (MediaMeta, bool) {
	if len(raw) > mediaMetaMax {
		return MediaMeta{}, false
	}
	var m MediaMeta
	if json.Unmarshal(raw, &m) != nil || m.Format != MediaMetaFormat || ValidMediaMeta(m) != nil {
		return MediaMeta{}, false
	}
	return m, true
}

// EncodeMediaMeta renders a sidecar for writing (indented — these files
// are meant to be hand-editable).
func EncodeMediaMeta(m MediaMeta) []byte {
	m.Format = MediaMetaFormat
	raw, _ := json.MarshalIndent(m, "", "  ")
	return append(raw, '\n')
}

// IsMediaMetaFile reports whether a filename is a sidecar.
func IsMediaMetaFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), MediaMetaExt)
}
