package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"
)

func TestSealRoundTrip(t *testing.T) {
	priv, pub, err := NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("From: a@x.io\r\n\r\nhello")
	sealed, err := Seal(pub, msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unseal(priv, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Errorf("round trip: %q", got)
	}

	// The wrong key never opens it.
	otherPriv, _, err := NewSealPair()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unseal(otherPriv, sealed); err == nil {
		t.Error("wrong key opened the envelope")
	}
	// A flipped ciphertext byte never opens it.
	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := Unseal(priv, tampered); err == nil {
		t.Error("tampered envelope opened")
	}
	// Sealing is non-deterministic (fresh ephemeral key per message).
	sealed2, _ := Seal(pub, msg)
	if string(sealed) == string(sealed2) {
		t.Error("two seals of the same message are identical")
	}
	// The envelope is versioned with the PCP magic.
	if string(sealed[:5]) != "PCPS1" {
		t.Errorf("seal magic = %q, want PCPS1", sealed[:5])
	}
}

func TestSignVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privB64 := base64.StdEncoding.EncodeToString(priv)
	v, err := NewVerifier(base64.StdEncoding.EncodeToString(pub))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"x":1}`)
	hdr, err := SignRequest(privB64, "PUT", "/v1/config", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify("PUT", "/v1/config", hdr, body); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	// Replay: the same header a second time is refused.
	if err := v.Verify("PUT", "/v1/config", hdr, body); err == nil {
		t.Error("replayed request accepted")
	}
	// Tampered body, path, and method are refused.
	hdr2, _ := SignRequest(privB64, "PUT", "/v1/config", body)
	if err := v.Verify("PUT", "/v1/config", hdr2, []byte(`{"x":2}`)); err == nil {
		t.Error("tampered body accepted")
	}
	hdr3, _ := SignRequest(privB64, "PUT", "/v1/config", body)
	if err := v.Verify("PUT", "/v1/inbound/ack", hdr3, body); err == nil {
		t.Error("cross-path signature accepted")
	}
	// The wrong key's signature is refused.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	hdr4, _ := SignRequest(base64.StdEncoding.EncodeToString(otherPriv), "PUT", "/v1/config", body)
	if err := v.Verify("PUT", "/v1/config", hdr4, body); err == nil {
		t.Error("foreign signature accepted")
	}
	// Stale timestamps are refused even with an unseen nonce.
	v.now = func() time.Time { return time.Now().Add(MaxSkew + time.Minute) }
	hdr5, _ := SignRequest(privB64, "GET", "/v1/status", nil)
	if err := v.Verify("GET", "/v1/status", hdr5, nil); err == nil {
		t.Error("stale request accepted")
	}
}

func TestKeyPairHelpers(t *testing.T) {
	sPriv, sPub, err := NewSignPair()
	if err != nil || sPriv == "" || !ValidKeyB64(sPub) {
		t.Fatalf("sign pair: %v", err)
	}
	xPriv, xPub, err := NewSealPair()
	if err != nil || !ValidKeyB64(xPriv) || !ValidKeyB64(xPub) {
		t.Fatalf("seal pair: %v", err)
	}
	if ValidKeyB64("not-base64!!") || ValidKeyB64(base64.StdEncoding.EncodeToString([]byte("short"))) {
		t.Error("ValidKeyB64 accepted junk")
	}
}
