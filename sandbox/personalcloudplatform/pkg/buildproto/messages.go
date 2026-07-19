// messages.go — the buildwire transport vocabulary (Draft 003 §6.2),
// shared by pkg/build (PCP's dispatch/ingest loop), pkg/runner (the
// runner-side library), and pkg/kernel's buildwire listener. One package
// defines both directions so the two halves can never drift.
//
// The wire is a single yamux session per paired runner (§6.2: the runner
// dials PCP). PCP is the yamux SERVER: it OPENS streams to push config
// and dispatch jobs (control → runner); the runner OPENS streams back to
// report phase/step status, append logs, and up/download artifacts. Each
// stream carries one framed message: a JSON header line naming the type
// and payload length, then the payload bytes. Status/config/dispatch
// payloads are JSON; log and artifact payloads are raw bytes.
package buildproto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Frame message types. Control frames (PCP → runner) open on a
// PCP-initiated stream; report frames (runner → PCP) open on a
// runner-initiated stream.
const (
	// PCP → runner (control).
	TypeConfig   = "config"   // ConfigPush
	TypeDispatch = "dispatch" // DispatchJob
	TypeCancel   = "cancel"   // CancelJob

	// runner → PCP (reports).
	TypePhaseStatus  = "phase_status"  // PhaseStatus
	TypeStepStatus   = "step_status"   // StepStatus
	TypeLog          = "log"           // LogChunk (Bytes carried in the frame payload)
	TypeBuildStatus  = "build_status"  // BuildStatus
	TypeArtifactPut  = "artifact_put"  // ArtifactUpload header; artifact bytes follow as a second frame
	TypeArtifactGet  = "artifact_get"  // ArtifactRequest; PCP answers with TypeArtifactData
	TypeArtifactData = "artifact_data" // raw artifact bytes (answer to TypeArtifactGet)
)

// The signed hello is a line-delimited JSON exchange BEFORE yamux takes
// over (mirroring ferry): the runner signs HelloMethod+HelloPath with
// its control key, PCP verifies against the paired RunnerPub and signs
// the reply so the runner can verify it reached the right PCP.
const (
	HelloMethod    = "HELLO"
	HelloPath      = "/buildwire/hello"
	HelloReplyPath = "/buildwire/hello-reply"
)

// RunnerHello is the first line the runner writes on a fresh connection.
// Auth is wire.SignRequest(runnerControlPriv, HelloMethod, HelloPath, nil)
// — PCP looks the runner up by RunnerID and verifies against its stored
// RunnerPub, and separately pins the TLS client-cert fingerprint.
type RunnerHello struct {
	V        int    `json:"v"`
	RunnerID string `json:"runner_id"`
	Kind     string `json:"kind"`               // k8s | baremetal (reported live)
	Capacity int    `json:"capacity,omitempty"` // free slots the runner reports
	Auth     string `json:"auth"`               // wire.SignRequest(...)
}

// HelloReply is PCP's one-line answer. Auth is
// wire.SignRequest(pcpControlPriv, HelloMethod, HelloReplyPath, nil) so
// the runner can confirm PCP's identity against the setup blob's control
// key; only an OK+valid reply proceeds to yamux.
type HelloReply struct {
	V     int    `json:"v"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Auth  string `json:"auth,omitempty"`
}

// ExecutionProfile is the resolved compute policy PCP pushes for a runner
// (Draft 003 §7.2): what the runtime is allowed to do that the YAML
// cannot set itself. Both executor kinds read the fields that apply to
// them; the other side ignores the rest.
type ExecutionProfile struct {
	ID string `json:"id,omitempty"`
	// Bare-metal: extra container flags (--gpus, --device, ulimits, cpu/
	// memory caps, extra mounts) applied verbatim to podman/docker run.
	ContainerFlags []string `json:"container_flags,omitempty"`
	// UserPolicy constrains a phase's `user:` (§5.5): "" = allow any,
	// "forbid-root" = reject uid 0, or a pinned uid string forcing that
	// value regardless of the phase request.
	UserPolicy string `json:"user_policy,omitempty"`
	// PodOverlay is a k8s pod-spec fragment (resources incl.
	// nvidia.com/gpu, runtimeClassName, nodeSelector, tolerations,
	// securityContext) merged onto each phase Pod. Opaque here — the k8s
	// executor merges it.
	PodOverlay json.RawMessage `json:"pod_overlay,omitempty"`
}

// UserPolicy values.
const (
	UserPolicyForbidRoot = "forbid-root"
)

// ConfigPush is the runner's live config (Draft 003 §7): the concurrency
// cap and the resolved execution profile. Serial versions the push so an
// unchanged content-hash skips it (ferry push.go pattern).
type ConfigPush struct {
	Serial        uint64           `json:"serial"`
	MaxConcurrent int              `json:"max_concurrent"`
	Profile       ExecutionProfile `json:"profile"`
}

// SealedSecret is one build secret in flight (Draft 003 §5.3): the runner
// opens Sealed with its private seal key; PCP only ever holds ciphertext.
type SealedSecret struct {
	Name   string `json:"name"`
	Sealed string `json:"sealed"` // base64 wire.Seal ciphertext
}

// DispatchJob is one build handed to a runner (Draft 003 §6.2). SpecYAML
// is the exact `.pcp-builder.yaml` bytes from the triggering commit so
// the runner parses and drives the DAG itself; Secrets are sealed to this
// runner; Profile is the resolved execution policy. Phase is optional: an
// empty Phase means "run the whole pipeline" (v1); a set Phase reserves a
// future per-phase dispatch mode.
type DispatchJob struct {
	RepoID   string           `json:"repo_id"`
	N        int              `json:"n"`
	Commit   string           `json:"commit,omitempty"`
	Ref      string           `json:"ref,omitempty"`
	Trigger  string           `json:"trigger,omitempty"`
	SpecYAML []byte           `json:"spec_yaml"`
	Secrets  []SealedSecret   `json:"secrets,omitempty"`
	Profile  ExecutionProfile `json:"profile"`
	Phase    string           `json:"phase,omitempty"`
}

// CancelJob asks the runner to tear down a live build's containers/Pods
// (Draft 003 §8.2).
type CancelJob struct {
	RepoID string `json:"repo_id"`
	N      int    `json:"n"`
}

// PhaseStatus reports one phase's state transition (Draft 003 §3.2). The
// runner sends it at phase start, on each step, and at completion.
type PhaseStatus struct {
	RepoID     string       `json:"repo_id"`
	N          int          `json:"n"`
	Phase      string       `json:"phase"`
	Image      string       `json:"image,omitempty"`
	Requires   string       `json:"requires,omitempty"`
	Inputs     []string     `json:"inputs,omitempty"`
	Outputs    []string     `json:"outputs,omitempty"`
	State      string       `json:"state"`
	ExitCode   int          `json:"exit_code,omitempty"`
	StartedAt  time.Time    `json:"started_at,omitzero"`
	FinishedAt time.Time    `json:"finished_at,omitzero"`
	Steps      []StepStatus `json:"steps,omitempty"`
}

// StepStatus reports one step's state (Draft 003 §3.2).
type StepStatus struct {
	Name          string    `json:"name"`
	Command       string    `json:"command,omitempty"`
	Args          []string  `json:"args,omitempty"`
	ExitOnFailure bool      `json:"exit_on_failure"`
	State         string    `json:"state"`
	ExitCode      int       `json:"exit_code,omitempty"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	FinishedAt    time.Time `json:"finished_at,omitzero"`
}

// LogChunk carries one span of a phase's log (Draft 003 §3.3). Offset is
// the monotonic byte offset within the phase's stream; Bytes ride the
// frame payload, not this JSON (a separate frame), so a chunk is never
// double-base64'd.
type LogChunk struct {
	RepoID string `json:"repo_id"`
	N      int    `json:"n"`
	Phase  string `json:"phase"`
	Offset int64  `json:"offset"`
	Len    int    `json:"len"`
}

// BuildStatus reports the build's overall terminal (or running) state
// (Draft 003 §8.1) so PCP can re-file the build index.
type BuildStatus struct {
	RepoID string `json:"repo_id"`
	N      int    `json:"n"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}

// ArtifactUpload is the metadata header the runner sends before pushing an
// artifact's bytes (Draft 003 §5.4). The bytes follow as a TypeArtifactData
// frame on the same stream.
type ArtifactUpload struct {
	RepoID string `json:"repo_id"`
	N      int    `json:"n"`
	Name   string `json:"name"`
	Phase  string `json:"phase,omitempty"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256,omitempty"`
}

// ArtifactRequest is the runner asking PCP for a declared input artifact
// (Draft 003 §5.4). PCP answers with a TypeArtifactData frame.
type ArtifactRequest struct {
	RepoID string `json:"repo_id"`
	N      int    `json:"n"`
	Name   string `json:"name"`
}

// --- framing ---------------------------------------------------------------

// frameHeader is the newline-delimited JSON header preceding every frame
// payload on a stream.
type frameHeader struct {
	Type string `json:"type"`
	Len  int    `json:"len"`
}

// maxFramePayload bounds a single frame's payload (16 MiB) — artifacts
// and logs chunk below this; a larger claim is a protocol error, not an
// allocation the reader honors.
const maxFramePayload = 16 << 20

// WriteFrame writes one typed frame: a JSON header line then payload.
func WriteFrame(w io.Writer, msgType string, payload []byte) error {
	if len(payload) > maxFramePayload {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	hdr, err := json.Marshal(frameHeader{Type: msgType, Len: len(payload)})
	if err != nil {
		return err
	}
	hdr = append(hdr, '\n')
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err = w.Write(payload)
	return err
}

// WriteMessage marshals v to JSON and writes it as a typed frame.
func WriteMessage(w io.Writer, msgType string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, msgType, payload)
}

// ReadFrame reads one typed frame's header and payload.
func ReadFrame(r *bufio.Reader) (msgType string, payload []byte, err error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return "", nil, err
	}
	var hdr frameHeader
	if err := json.Unmarshal(line, &hdr); err != nil {
		return "", nil, fmt.Errorf("bad frame header: %w", err)
	}
	if hdr.Len < 0 || hdr.Len > maxFramePayload {
		return "", nil, fmt.Errorf("bad frame length %d", hdr.Len)
	}
	if hdr.Len == 0 {
		return hdr.Type, nil, nil
	}
	payload = make([]byte, hdr.Len)
	if _, err := io.ReadFull(r, payload); err != nil {
		return "", nil, err
	}
	return hdr.Type, payload, nil
}

// ReadMessage reads one frame and unmarshals its JSON payload into v,
// returning the frame type (so a caller can assert it).
func ReadMessage(r *bufio.Reader, v any) (string, error) {
	msgType, payload, err := ReadFrame(r)
	if err != nil {
		return "", err
	}
	if v != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, v); err != nil {
			return msgType, err
		}
	}
	return msgType, nil
}

// blobChunk bounds one artifact/blob data frame (8 MiB, under
// maxFramePayload) so a large upload streams rather than buffering whole.
const blobChunk = 8 << 20

// WriteBlobStream copies r to w as a sequence of TypeArtifactData frames
// terminated by a zero-length frame (the EOF marker). Used for artifact
// up/download after a metadata frame.
func WriteBlobStream(w io.Writer, r io.Reader) error {
	buf := make([]byte, blobChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := WriteFrame(w, TypeArtifactData, buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return WriteFrame(w, TypeArtifactData, nil) // terminator
}

// ReadBlobStream reads TypeArtifactData frames from r into w until the
// zero-length terminator, returning the byte count copied.
func ReadBlobStream(r *bufio.Reader, w io.Writer) (int64, error) {
	var total int64
	for {
		_, payload, err := ReadFrame(r)
		if err != nil {
			return total, err
		}
		if len(payload) == 0 {
			return total, nil // terminator
		}
		n, err := w.Write(payload)
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
}
