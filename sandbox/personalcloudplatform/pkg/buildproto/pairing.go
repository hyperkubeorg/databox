// pairing.go — the pasted-blob halves of the build-runner pairing
// handshake (Draft 003 §6.1). PCP encodes the setup blob and decodes the
// completion blob; `pcp-runner setup` does the reverse. One package defines
// both shapes so a mis-paste is impossible to build. Mirror of
// ferryproto/pairing.go with the runner's own prefixes — pasting a
// cloudferry or postoffice code here fails instantly.
//
// Unlike cloudferry, the RUNNER dials PCP (Draft 003 §6.2: runners sit
// behind firewalls), so the completion blob carries no endpoints for PCP to
// dial — it carries the runner's public identity, pinned TLS fingerprint,
// and the executor kind it reports. The setup blob instead carries the PCP
// buildwire endpoint the runner should dial back to.
package buildproto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Blob prefixes make a mis-paste obvious instantly (and version the format).
// Setup travels PCP → runner terminal; completion comes back.
const (
	setupBlobPrefix      = "PCPBR1."
	completionBlobPrefix = "PCPBR2."
)

// Executor kinds a runner reports at pairing.
const (
	KindK8s       = "k8s"
	KindBareMetal = "baremetal"
)

// SetupBlob is what the admin pastes into `pcp-runner setup`: PCP's public
// keys, the one-time pairing token, and the buildwire endpoint the runner
// dials back to (host:port).
type SetupBlob struct {
	V    int    `json:"v"`
	Name string `json:"name"`
	// RunnerID is the runner's PCP-side record id; the runner persists it
	// and sends it in every buildwire hello so PCP knows which record to
	// verify the signature (and pin the TLS fingerprint) against (§6.2).
	RunnerID     string `json:"runner_id"`
	PCPControl   string `json:"pcp_control_pub"`
	PCPSeal      string `json:"pcp_seal_pub"`
	PairingToken string `json:"pairing_token"`
	// Endpoint is the PCP buildwire control endpoint the runner dials
	// (host:port). Empty = the runner is told out-of-band.
	Endpoint string `json:"endpoint,omitempty"`
}

// CompletionBlob is what `pcp-runner setup` prints for the admin to paste
// back: the runner's public identity, pinned TLS fingerprint, and the
// executor kind (k8s|baremetal) it will run as.
type CompletionBlob struct {
	V             int    `json:"v"`
	RunnerPub     string `json:"runner_pub"`
	RunnerSealPub string `json:"runner_seal_pub"`
	TLSFP         string `json:"tls_fingerprint"`
	Kind          string `json:"kind"`
	PairingToken  string `json:"pairing_token"`
}

// EncodeSetupBlob renders the pairing code the admin console shows.
func EncodeSetupBlob(sb SetupBlob) string {
	sb.V = 1
	return encodeBlob(setupBlobPrefix, sb)
}

// DecodeSetupBlob parses a pasted setup code (runner side).
func DecodeSetupBlob(blob string) (SetupBlob, error) {
	var sb SetupBlob
	err := decodeBlob(setupBlobPrefix, blob, &sb)
	if err == nil && (sb.PCPControl == "" || sb.PCPSeal == "" || sb.PairingToken == "") {
		err = fmt.Errorf("setup code is missing fields")
	}
	return sb, err
}

// EncodeCompletionBlob renders the code `pcp-runner setup` prints.
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

// ValidKind reports whether k is a known executor kind.
func ValidKind(k string) bool { return k == KindK8s || k == KindBareMetal }

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
