// pairing.go — the pasted-blob halves of the pairing handshake. PCP
// encodes the setup blob and decodes the completion blob; `postoffice
// setup` does the reverse. One package defines both shapes so a
// mis-paste is impossible to build.
package mailproto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Blob prefixes make a mis-paste obvious instantly (and version the
// format). Setup travels PCP → operator terminal; completion comes back.
const (
	setupBlobPrefix      = "PCPPO1."
	completionBlobPrefix = "PCPPO2."
)

// SetupBlob is what the admin pastes into `postoffice setup`: PCP's
// public keys plus the one-time pairing token.
type SetupBlob struct {
	V            int    `json:"v"`
	Name         string `json:"name"`
	PCPControl   string `json:"pcp_control_pub"`
	PCPSeal      string `json:"pcp_seal_pub"`
	PairingToken string `json:"pairing_token"`
}

// CompletionBlob is what `postoffice setup` prints for the admin to
// paste back: the gateway's public identity and pinned TLS fingerprint.
type CompletionBlob struct {
	V            int    `json:"v"`
	POPub        string `json:"po_pub"`
	POSealPub    string `json:"po_seal_pub"`
	TLSFP        string `json:"tls_fingerprint"`
	Endpoint     string `json:"endpoint"`
	PairingToken string `json:"pairing_token"`
}

// EncodeSetupBlob renders the pairing code the admin console shows.
func EncodeSetupBlob(sb SetupBlob) string {
	sb.V = 1
	return encodeBlob(setupBlobPrefix, sb)
}

// DecodeSetupBlob parses a pasted setup code (postoffice side).
func DecodeSetupBlob(blob string) (SetupBlob, error) {
	var sb SetupBlob
	err := decodeBlob(setupBlobPrefix, blob, &sb)
	if err == nil && (sb.PCPControl == "" || sb.PCPSeal == "" || sb.PairingToken == "") {
		err = fmt.Errorf("setup code is missing fields")
	}
	return sb, err
}

// EncodeCompletionBlob renders the code `postoffice setup` prints.
func EncodeCompletionBlob(c CompletionBlob) string {
	c.V = 1
	return encodeBlob(completionBlobPrefix, c)
}

// DecodeCompletionBlob parses what the operator pasted back (PCP side).
func DecodeCompletionBlob(blob string) (CompletionBlob, error) {
	var c CompletionBlob
	err := decodeBlob(completionBlobPrefix, blob, &c)
	return c, err
}

func encodeBlob(prefix string, v any) string {
	raw, _ := json.Marshal(v)
	return prefix + base64.RawURLEncoding.EncodeToString(raw)
}

func decodeBlob(prefix, blob string, v any) error {
	blob = strings.TrimSpace(blob)
	if !strings.HasPrefix(blob, prefix) {
		return fmt.Errorf("that doesn't look like the right kind of code (expected %s…)", prefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(blob, prefix))
	if err != nil {
		return fmt.Errorf("that code didn't decode — paste it exactly")
	}
	return json.Unmarshal(raw, v)
}
