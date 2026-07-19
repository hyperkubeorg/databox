// range.go — ranged blob reads: stream [offset, offset+length) of a blob
// without touching the chunks (or EC stripes) outside the window. The
// manifest records every chunk's size, so seeking is pure arithmetic —
// this is what makes HTTP Range serving (video seeking) cheap for the
// apps built on databox.
package blob

import (
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
)

// ReadRange streams length bytes starting at offset to w, verifying
// every chunk it actually touches (skipped chunks are never fetched).
// length < 0 means "to the end". Reads past EOF are clamped; an offset
// beyond the blob writes nothing.
func (e *Engine) ReadRange(m *Manifest, w io.Writer, offset, length int64) error {
	if offset < 0 {
		return fmt.Errorf("negative offset %d", offset)
	}
	if length < 0 || offset+length > m.Size {
		length = m.Size - offset
	}
	if offset >= m.Size || length <= 0 {
		return nil
	}
	end := offset + length
	switch m.Mode {
	case "replica":
		pos := int64(0)
		for _, ref := range m.Chunks {
			next := pos + ref.Size
			if next <= offset {
				pos = next
				continue // wholly before the window: never fetched
			}
			if pos >= end {
				break
			}
			data, err := e.fetch(ref)
			if err != nil {
				return err
			}
			lo, hi := int64(0), ref.Size
			if offset > pos {
				lo = offset - pos
			}
			if end < next {
				hi = end - pos
			}
			if _, err := w.Write(data[lo:hi]); err != nil {
				return err
			}
			pos = next
		}
		return nil
	case "ec":
		enc, err := reedsolomon.New(m.DataShards, m.ParityShards)
		if err != nil {
			return err
		}
		pos := int64(0)
		for _, stripe := range m.Stripes {
			next := pos + stripe.DataLen
			if next <= offset {
				pos = next
				continue // stripe never gathered or reconstructed
			}
			if pos >= end {
				break
			}
			lo, hi := int64(0), stripe.DataLen
			if offset > pos {
				lo = offset - pos
			}
			if end < next {
				hi = end - pos
			}
			// Reconstruct the stripe (only the ones the window touches),
			// slicing the wanted bytes out as they stream through.
			sw := &sliceWriter{w: w, lo: lo, hi: hi}
			if err := e.readStripe(enc, m.DataShards, stripe, sw); err != nil {
				return err
			}
			pos = next
		}
		return nil
	default:
		return fmt.Errorf("unknown blob mode %q", m.Mode)
	}
}

// sliceWriter forwards only the bytes whose stream positions fall in
// [lo, hi) — the per-stripe window cut for EC ranged reads.
type sliceWriter struct {
	w      io.Writer
	pos    int64
	lo, hi int64
}

func (s *sliceWriter) Write(p []byte) (int, error) {
	n := len(p)
	start, end := s.pos, s.pos+int64(n)
	s.pos = end
	cutLo, cutHi := max(start, s.lo), min(end, s.hi)
	if cutLo >= cutHi {
		return n, nil
	}
	if _, err := s.w.Write(p[cutLo-start : cutHi-start]); err != nil {
		return 0, err
	}
	return n, nil
}
