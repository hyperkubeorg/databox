// session.go — the stream-backed Reporter (Draft 003 §6.2): each report
// the DAG driver makes opens ONE fresh yamux stream back to PCP, writes a
// framed message (and, for artifacts, the bytes), and closes it. This
// mirrors the "one stream, one exchange" shape the cloudferry data plane
// uses, so a slow or stuck report never wedges the control session.
package runner

import (
	"bufio"
	"encoding/base64"
	"io"

	"github.com/hashicorp/yamux"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
)

// sessionReporter opens report streams on a live yamux session.
type sessionReporter struct {
	sess *yamux.Session
}

// PhaseStatus opens a stream and writes one phase-status frame.
func (r *sessionReporter) PhaseStatus(ps buildproto.PhaseStatus) error {
	return r.oneMessage(buildproto.TypePhaseStatus, ps)
}

// BuildStatus opens a stream and writes one build-status frame.
func (r *sessionReporter) BuildStatus(bs buildproto.BuildStatus) error {
	return r.oneMessage(buildproto.TypeBuildStatus, bs)
}

// Log opens a stream and writes a log-chunk header frame followed by the
// bytes frame (§3.3).
func (r *sessionReporter) Log(repoID string, n int, phase string, offset int64, b []byte) error {
	stream, err := r.sess.Open()
	if err != nil {
		return err
	}
	defer stream.Close()
	meta := buildproto.LogChunk{RepoID: repoID, N: n, Phase: phase, Offset: offset, Len: len(b)}
	if err := buildproto.WriteMessage(stream, buildproto.TypeLog, meta); err != nil {
		return err
	}
	return buildproto.WriteFrame(stream, buildproto.TypeLog, b)
}

// UploadArtifact opens a stream, writes the metadata frame, then streams
// the artifact bytes (§5.4).
func (r *sessionReporter) UploadArtifact(meta buildproto.ArtifactUpload, src io.Reader) error {
	stream, err := r.sess.Open()
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := buildproto.WriteMessage(stream, buildproto.TypeArtifactPut, meta); err != nil {
		return err
	}
	return buildproto.WriteBlobStream(stream, src)
}

// FetchArtifact opens a stream, requests an input artifact, and copies
// PCP's answering byte stream into w (§5.4).
func (r *sessionReporter) FetchArtifact(repoID string, n int, name string, w io.Writer) error {
	stream, err := r.sess.Open()
	if err != nil {
		return err
	}
	defer stream.Close()
	req := buildproto.ArtifactRequest{RepoID: repoID, N: n, Name: name}
	if err := buildproto.WriteMessage(stream, buildproto.TypeArtifactGet, req); err != nil {
		return err
	}
	_, err = buildproto.ReadBlobStream(bufio.NewReader(stream), w)
	return err
}

// oneMessage opens a stream, writes one framed message, and closes it.
func (r *sessionReporter) oneMessage(msgType string, v any) error {
	stream, err := r.sess.Open()
	if err != nil {
		return err
	}
	defer stream.Close()
	return buildproto.WriteMessage(stream, msgType, v)
}

// decodeSealed base64-decodes a stored sealed-secret ciphertext.
func decodeSealed(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
