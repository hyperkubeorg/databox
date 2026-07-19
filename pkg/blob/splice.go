// splice.go implements manifest splicing (§25: "Completed
// multipart uploads splice part chunk-maps into one blob metadata record"):
// building one destination manifest whose chunk map is the concatenation of
// several source blobs' chunk maps, WITHOUT copying the underlying bytes.
//
// # Why concatenation is safe
//
// Chunks are content-addressed and immutable, and the repair loop's GC
// keeps any chunk referenced by ANY manifest (pkg/server/repair.go builds
// its referenced set from every manifest's AllRefs). Two manifests sharing
// chunk references is therefore already a supported state — splice just
// creates it deliberately. Deleting the source manifests afterwards is the
// caller's job and is safe the moment the destination manifest commits.
//
// # Manifest shape: flat, single-mode — not segmented
//
// A spliced manifest keeps the exact same shape as a written one: Mode is
// "replica" (ordered Chunks) or "ec" (ordered Stripes with one shared
// DataShards/ParityShards geometry). No new segment structure is
// introduced, because every existing consumer — Engine.Read, Append's tail
// surgery, the repair loop's per-mode switch, ReconstructStripeShards, and
// GC's AllRefs — already understands this shape completely and needs zero
// changes to keep spliced blobs readable, repairable, and GC-safe.
//
// The costs of staying flat are two relaxed invariants plus one bounded
// re-encode, all verified against every consumer:
//
//   - Replica mode may now contain SHORT CHUNKS MID-BLOB (each source's
//     tail chunk). Read concatenates fetched chunks verbatim and repair
//     ignores sizes, so both are unaffected. Only Append cared (it rebuilds
//     the final tail), and Append is refused on spliced manifests anyway.
//
//   - EC mode may now contain SHORT STRIPES MID-BLOB (each source's tail
//     stripe). Every stripe self-describes its real byte length (DataLen),
//     and readStripe/ReconstructStripeShards operate strictly per-stripe,
//     so short stripes anywhere in the sequence read and repair correctly.
//
//   - MIXED-MODE sources (or EC sources with mismatched geometry) cannot
//     be represented flat, so those sources — and only those — are
//     re-encoded into the destination's mode by streaming their bytes
//     through the normal store path. In the S3 multipart reality that
//     produced this feature, parts under one upload share one durability
//     policy: either everything is replica (pure concat, zero bytes move)
//     or the large parts are same-geometry EC and at most the small
//     single-chunk parts re-encode — a few chunks, not the object.
//
// # The composite hash
//
// A true whole-blob SHA-256 of the destination would require reading every
// byte of every source. Instead the destination records a composite hash
// (Manifest.Composite): the comma-joined per-source content digests, in
// order — the same trade S3 itself makes with its "<md5>-<n>" multipart
// ETags. Per-chunk content addressing still verifies every byte on every
// read, and each component still pins its source's entire content, so
// integrity is not weakened; see the Composite field docs in blob.go for
// the exact format. Append refuses composite manifests (append.go) because
// no whole-blob hash midstate exists to resume.
package blob

import (
	"fmt"
	"io"
	"strings"
)

// HashComponents returns the manifest's content digest list: the single
// whole-blob digest for a plain manifest, or the per-source components of
// a composite one (splice inlines nested composites, so components are
// always one flat, ordered list covering the blob left to right).
func (m *Manifest) HashComponents() []string {
	if !m.Composite {
		return []string{m.SHA256}
	}
	return strings.Split(m.SHA256, ",")
}

// Splice builds the manifest for dstKey whose content is the concatenation
// of the sources' contents, in order. Chunk maps are concatenated by
// reference — no source bytes are read or copied — except for sources
// whose encoding cannot join the destination's mode (see canConcat), which
// are re-encoded through the normal store path. Source manifests are never
// modified; committing the result (and deleting the sources) is the
// caller's job.
//
// The destination is composite-hashed (see package comment) unless there
// is exactly one source, in which case the source manifest is returned as
// a verbatim copy — same content, so its true hash, hash midstate, and
// appendability all remain valid.
func (e *Engine) Splice(dstKey string, srcs []*Manifest, contentType string) (*Manifest, error) {
	if len(srcs) == 0 {
		return nil, fmt.Errorf("splice requires at least one source blob")
	}
	for i, src := range srcs {
		if src.Mode != "replica" && src.Mode != "ec" {
			return nil, fmt.Errorf("splice source %d has unknown blob mode %q", i, src.Mode)
		}
	}
	// Single source: the destination IS the source's content, so its plain
	// manifest transfers wholesale (chunk refs are shareable by design).
	if len(srcs) == 1 {
		cp := *srcs[0]
		cp.Chunks = append([]Ref(nil), srcs[0].Chunks...)
		cp.Stripes = append([]Stripe(nil), srcs[0].Stripes...)
		cp.ContentType = contentType
		return &cp, nil
	}

	// Destination mode: replica iff every source is replica (pure concat);
	// otherwise EC with the first EC source's geometry — in practice the
	// shared policy geometry of every large part (see package comment).
	m := &Manifest{Mode: "replica", ContentType: contentType, Composite: true}
	for _, src := range srcs {
		if src.Mode == "ec" {
			m.Mode, m.DataShards, m.ParityShards = "ec", src.DataShards, src.ParityShards
			break
		}
	}

	// Concatenate: sizes sum, hash components join in order, and each
	// source's chunk map either transfers by reference or re-encodes.
	var components []string
	for i, src := range srcs {
		m.Size += src.Size
		components = append(components, src.HashComponents()...)
		if canConcat(m, src) {
			if src.Mode == "replica" {
				m.Chunks = append(m.Chunks, src.Chunks...)
			} else {
				m.Stripes = append(m.Stripes, src.Stripes...)
			}
			continue
		}
		// Boundary case: this source's encoding cannot join the flat
		// destination manifest, so its bytes are re-read (server-side,
		// chunk-local where possible) and stored in the destination mode.
		// Re-encoding changes the representation, never the content, so
		// the source's hash component above stays truthful.
		if err := e.reencode(dstKey, m, src); err != nil {
			return nil, fmt.Errorf("re-encode splice source %d: %w", i, err)
		}
	}
	// The composite hash format is documented on Manifest.Composite. No
	// hash midstate: there is no single running whole-blob hash to resume.
	m.SHA256 = strings.Join(components, ",")
	m.HashState = nil
	return m, nil
}

// canConcat reports whether src's chunk map can transfer into dst by pure
// reference concatenation: same mode, and for EC the exact same
// DataShards/ParityShards geometry (a manifest carries only one geometry,
// which Read/repair/reconstruct apply to every stripe).
func canConcat(dst, src *Manifest) bool {
	if dst.Mode != src.Mode {
		return false
	}
	return dst.Mode == "replica" ||
		(dst.DataShards == src.DataShards && dst.ParityShards == src.ParityShards)
}

// reencode streams one source's content through the destination's storage
// mode, appending the resulting chunk map to dst. The stream is re-chunked
// at the engine's chunk size from the source's start, so within this
// source's span every chunk is full-size except its last — the same layout
// a fresh write produces, which keeps EC stripe padding strictly at each
// stripe's end (readStripe trims buf[:DataLen], so padding anywhere else
// would corrupt reads). Memory use is bounded to one stripe.
func (e *Engine) reencode(dstKey string, dst *Manifest, src *Manifest) error {
	// Stream the source through a pipe: Read verifies every chunk hash and
	// reconstructs missing EC shards exactly as a client download would.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(e.Read(src, pw)) }()
	defer pr.Close()

	pol := e.PolicyFor(dstKey)
	buf := make([]byte, e.ChunkSize)
	var stripePending [][]byte // EC: chunks accumulating toward a full stripe
	flushStripe := func() error {
		if len(stripePending) == 0 {
			return nil
		}
		stripe, err := e.storeStripe(stripePending, dst.DataShards, dst.ParityShards, e.Peers.ActiveNodes())
		if err != nil {
			return err
		}
		dst.Stripes = append(dst.Stripes, stripe)
		stripePending = nil
		return nil
	}
	for {
		n, rerr := io.ReadFull(pr, buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			switch dst.Mode {
			case "replica":
				// New copies must meet the destination key's replica
				// quorum, exactly like a fresh write (§11).
				ref, err := e.storeReplicated(chunk, pol.Replicas)
				if err != nil {
					return err
				}
				dst.Chunks = append(dst.Chunks, ref)
			case "ec":
				stripePending = append(stripePending, chunk)
				if len(stripePending) == dst.DataShards {
					if err := flushStripe(); err != nil {
						return err
					}
				}
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return flushStripe()
}
