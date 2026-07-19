// Package assets holds the static files (CSS, and any future JS/images)
// served by the web portal at /assets/ (§3, §4).
// Everything is embedded into the binary so the portal works air-gapped:
// no CDN, no external fonts, no files on disk.
//
// Security note (§4 "Asset Security"): the embed pattern below is `*`,
// which also embeds this very file. The /assets/ HTTP handler in
// pkg/routes/frontend therefore explicitly answers 404 for
// /assets/embed.go (and any other .go file) so source never leaves the
// binary through the asset route.
package assets

import "embed"

// FS exposes the static files as an embedded read-only filesystem.
//
//go:embed *
var FS embed.FS
