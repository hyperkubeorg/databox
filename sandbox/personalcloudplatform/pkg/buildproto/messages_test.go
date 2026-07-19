package buildproto

import (
	"bufio"
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	job := DispatchJob{
		RepoID: "repo1", N: 7, Commit: "abc", SpecYAML: []byte("env: {}\n"),
		Secrets: []SealedSecret{{Name: "DB", Sealed: "c2VhbGVk"}},
		Profile: ExecutionProfile{ID: "gpu", ContainerFlags: []string{"--gpus", "all"}},
	}
	if err := WriteMessage(&buf, TypeDispatch, job); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(&buf)
	var got DispatchJob
	msgType, err := ReadMessage(br, &got)
	if err != nil {
		t.Fatal(err)
	}
	if msgType != TypeDispatch {
		t.Fatalf("type = %q, want %q", msgType, TypeDispatch)
	}
	if got.RepoID != "repo1" || got.N != 7 || string(got.SpecYAML) != "env: {}\n" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if len(got.Secrets) != 1 || got.Secrets[0].Name != "DB" {
		t.Fatalf("secrets round trip: %+v", got.Secrets)
	}
	if got.Profile.ID != "gpu" || len(got.Profile.ContainerFlags) != 2 {
		t.Fatalf("profile round trip: %+v", got.Profile)
	}
}

func TestFrameRawPayload(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("some raw log bytes\nwith a newline")
	if err := WriteFrame(&buf, TypeLog, payload); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(&buf)
	msgType, got, err := ReadFrame(br)
	if err != nil {
		t.Fatal(err)
	}
	if msgType != TypeLog {
		t.Fatalf("type = %q", msgType)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestEmptyFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, TypeArtifactData, nil); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(&buf)
	msgType, got, err := ReadFrame(br)
	if err != nil {
		t.Fatal(err)
	}
	if msgType != TypeArtifactData || len(got) != 0 {
		t.Fatalf("empty frame = %q / %d bytes", msgType, len(got))
	}
}

func TestBlobStreamRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	data := bytes.Repeat([]byte("A"), (8<<20)+123) // spans multiple chunks
	if err := WriteBlobStream(&wire, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	n, err := ReadBlobStream(bufio.NewReader(&wire), &out)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(data)) || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("blob stream length %d, want %d", n, len(data))
	}
}

func TestSetupBlobCarriesRunnerID(t *testing.T) {
	blob := EncodeSetupBlob(SetupBlob{
		Name: "runner-a", RunnerID: "rid123",
		PCPControl: "cpub", PCPSeal: "spub", PairingToken: "tok",
	})
	got, err := DecodeSetupBlob(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerID != "rid123" {
		t.Fatalf("runner id = %q", got.RunnerID)
	}
}
