// thumbs.go — the thumbnail cache: 256px JPEG previews for image files,
// generated lazily on first request and stored content-addressed at
// thumbs/<driveID>/<blobID>. Blob ids are immutable, so a cached
// thumbnail never needs invalidation — it simply dies with its blob
// (purge deletes both).
//
// Uploaded bytes are never trusted: the source must SNIFF as a raster
// image type, DecodeConfig gates dimensions BEFORE the real decode
// (decompression-bomb defense), and oversized sources are simply
// skipped — the browser falls back to the type icon. Soft-fail
// everywhere: thumbnails are chrome, never worth an error page.
package nodes

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg" // decoder + the output encoder
	"net/http"

	_ "image/gif" // registers the GIF decoder
	_ "image/png" // registers the PNG decoder

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// Thumbnail limits.
const (
	// ThumbEdge is the long-edge pixel count of a generated thumbnail.
	ThumbEdge = 256
	// thumbMaxSource caps the bytes read for decoding — a personal-cloud
	// photo fits comfortably; a 500MB TIFF is not thumbnail material.
	thumbMaxSource = 48 << 20
	// thumbMaxPixels caps width×height BEFORE decoding (DecodeConfig
	// reads only the header): 60 MP covers any camera, and bounds the
	// decoder's memory.
	thumbMaxPixels = 60 << 20
)

// thumbImageTypes are the content types the generator accepts (sniffed
// from real bytes, never declared ones). SVG is deliberately absent —
// it's a script vector, not a raster.
var thumbImageTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
}

// GenerateThumb reads the source blob, decodes it under the caps, scales
// to ThumbEdge, JPEG-encodes, and stores the result at the thumb key.
// Returns the encoded bytes. Errors mean "no thumbnail" — callers fall
// back to icons, never to failures.
func (s *Store) GenerateThumb(ctx context.Context, driveID, blobID string, sourceSize int64) ([]byte, error) {
	if !kvx.ValidID(driveID) || !kvx.ValidID(blobID) {
		return nil, users.ErrNotFound
	}
	if sourceSize <= 0 || sourceSize > thumbMaxSource {
		return nil, fmt.Errorf("source too large for a thumbnail (%d bytes)", sourceSize)
	}
	var buf bytes.Buffer
	buf.Grow(int(sourceSize))
	if err := s.DB.GetBlob(ctx, BlobKey(driveID, blobID), &buf); err != nil {
		return nil, err
	}
	raw := buf.Bytes()
	// Gate 1: the real bytes must sniff as a raster image.
	head := raw
	if len(head) > 512 {
		head = head[:512]
	}
	if !thumbImageTypes[http.DetectContentType(head)] {
		return nil, fmt.Errorf("not a supported image")
	}
	// Gate 2: header-only dimension check before the real decode.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > thumbMaxPixels {
		return nil, fmt.Errorf("image dimensions out of bounds (%dx%d)", cfg.Width, cfg.Height)
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	// Scale so the long edge is ThumbEdge (never upscale).
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := 1.0
	if w >= h && w > ThumbEdge {
		scale = float64(ThumbEdge) / float64(w)
	} else if h > w && h > ThumbEdge {
		scale = float64(ThumbEdge) / float64(h)
	}
	tw, th := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	if err := s.DB.PutBlob(ctx, ThumbKey(driveID, blobID), bytes.NewReader(out.Bytes()), "image/jpeg"); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
