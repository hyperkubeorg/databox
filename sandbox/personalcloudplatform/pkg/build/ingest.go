// ingest.go — persistence for everything a runner reports over the
// buildwire session (Draft 003 §3.2, §3.3, §3.4, §6.2). Each runner-
// opened stream carries one report: a phase/step status, a log span, an
// artifact upload, or an artifact request (for a declared input). The
// listener's accept loop hands each stream here.
package build

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
)

// ingest reads one runner-opened stream and persists its report.
func (l *Listener) ingest(ctx context.Context, stream net.Conn) {
	defer stream.Close()
	br := bufio.NewReader(stream)
	msgType, payload, err := buildproto.ReadFrame(br)
	if err != nil {
		return
	}
	switch msgType {
	case buildproto.TypePhaseStatus:
		l.ingestPhase(ctx, payload)
	case buildproto.TypeBuildStatus:
		l.ingestBuild(ctx, payload)
	case buildproto.TypeLog:
		l.ingestLog(ctx, payload, br)
	case buildproto.TypeArtifactPut:
		l.ingestArtifact(ctx, payload, br)
	case buildproto.TypeArtifactGet:
		l.serveArtifact(ctx, payload, stream)
	}
}

// ingestPhase writes a reported phase transition (§3.2).
func (l *Listener) ingestPhase(ctx context.Context, payload []byte) {
	var ps buildproto.PhaseStatus
	if json.Unmarshal(payload, &ps) != nil {
		return
	}
	steps := make([]dbuild.Step, len(ps.Steps))
	for i, s := range ps.Steps {
		steps[i] = dbuild.Step{
			Name: s.Name, Command: s.Command, Args: s.Args,
			ExitOnFailure: s.ExitOnFailure, State: s.State, ExitCode: s.ExitCode,
			StartedAt: s.StartedAt, FinishedAt: s.FinishedAt,
		}
	}
	phase := dbuild.Phase{
		RepoID: ps.RepoID, N: ps.N, Name: ps.Phase, Image: ps.Image,
		RequiresPhase: ps.Requires, Inputs: ps.Inputs, Outputs: ps.Outputs,
		State: ps.State, ExitCode: ps.ExitCode,
		StartedAt: ps.StartedAt, FinishedAt: ps.FinishedAt, Steps: steps,
	}
	if err := l.Build.RecordPhase(ctx, phase); err != nil {
		l.Log.Warn("buildwire: record phase failed", "repo", ps.RepoID, "n", ps.N, "phase", ps.Phase, "err", err)
	}
}

// ingestBuild applies a reported build-state transition (§8.1).
func (l *Listener) ingestBuild(ctx context.Context, payload []byte) {
	var bs buildproto.BuildStatus
	if json.Unmarshal(payload, &bs) != nil {
		return
	}
	if !dbuild.ValidBuildState(bs.State) {
		return
	}
	if _, err := l.Build.SetBuildState(ctx, bs.RepoID, bs.N, bs.State); err != nil {
		l.Log.Warn("buildwire: set build state failed", "repo", bs.RepoID, "n", bs.N, "state", bs.State, "err", err)
	}
}

// ingestLog appends a log span (§3.3): the header frame is followed by the
// raw bytes frame on the same stream.
func (l *Listener) ingestLog(ctx context.Context, payload []byte, br *bufio.Reader) {
	var lc buildproto.LogChunk
	if json.Unmarshal(payload, &lc) != nil {
		return
	}
	_, b, err := buildproto.ReadFrame(br)
	if err != nil {
		return
	}
	if _, err := l.Build.AppendLog(ctx, lc.RepoID, lc.N, lc.Phase, b); err != nil {
		l.Log.Warn("buildwire: append log failed", "repo", lc.RepoID, "n", lc.N, "phase", lc.Phase, "err", err)
	}
}

// ingestArtifact stores an uploaded artifact (§3.4, §5.4): the metadata
// header frame is followed by the artifact's byte stream.
func (l *Listener) ingestArtifact(ctx context.Context, payload []byte, br *bufio.Reader) {
	var meta buildproto.ArtifactUpload
	if json.Unmarshal(payload, &meta) != nil {
		return
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := buildproto.ReadBlobStream(br, pw)
		pw.CloseWithError(err)
	}()
	art := dbuild.Artifact{
		RepoID: meta.RepoID, N: meta.N, Name: meta.Name, Phase: meta.Phase,
		Size: meta.Size, Sha256: meta.Sha256,
	}
	if err := l.Build.PutArtifact(ctx, art, pr); err != nil {
		_, _ = io.Copy(io.Discard, pr) // drain so the reader goroutine unblocks
		l.Log.Warn("buildwire: put artifact failed", "repo", meta.RepoID, "n", meta.N, "name", meta.Name, "err", err)
	}
}

// serveArtifact streams a requested input artifact back to the runner
// (§5.4): read the request, then write the bytes as a blob stream.
func (l *Listener) serveArtifact(ctx context.Context, payload []byte, stream net.Conn) {
	var req buildproto.ArtifactRequest
	if json.Unmarshal(payload, &req) != nil {
		return
	}
	pr, pw := io.Pipe()
	go func() {
		err := l.Build.ReadArtifact(ctx, req.RepoID, req.N, req.Name, pw)
		pw.CloseWithError(err)
	}()
	if err := buildproto.WriteBlobStream(stream, pr); err != nil {
		l.Log.Warn("buildwire: serve artifact failed", "repo", req.RepoID, "n", req.N, "name", req.Name, "err", err)
	}
	_, _ = io.Copy(io.Discard, pr)
}
