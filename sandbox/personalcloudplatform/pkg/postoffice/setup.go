// setup.go — the interactive pairing flow (`postoffice setup`): paste
// the setup code from the PCP admin console, confirm the public
// endpoint, and paste the printed completion code back. After this the
// box is never configured locally again — everything arrives from PCP.
package postoffice

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
)

// RunSetup drives the pairing conversation on in/out (the operator's
// terminal over ssh).
func RunSetup(dir string, in io.Reader, out io.Writer) error {
	r := bufio.NewReader(in)
	fmt.Fprintln(out, "postoffice setup — pair this gateway with your Personal Cloud Platform.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "In the PCP admin console: Admin → Mail → Post offices → your gateway.")
	fmt.Fprint(out, "Paste the setup code (PCPPO1.…): ")
	blobLine, err := readLine(r)
	if err != nil {
		return err
	}
	sb, err := mailproto.DecodeSetupBlob(blobLine)
	if err != nil {
		return err
	}

	defHost, _ := os.Hostname()
	defEndpoint := defHost + ":8443"
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "What PUBLIC address does this gateway answer at? This is the hostname or")
	fmt.Fprintln(out, "public IP the outside world (and your PCP) reaches it on — NOT a local bind")
	fmt.Fprintln(out, "address. Do not enter 0.0.0.0 (that's what --https-listen binds to; it isn't")
	fmt.Fprintln(out, "an address anything can dial). Example: mail.example.com:8443")
	fmt.Fprintf(out, "Public address [%s]: ", defEndpoint)
	endpoint, err := readLine(r)
	if err != nil {
		return err
	}
	if endpoint == "" {
		endpoint = defEndpoint
	}
	host, err := validatePublicEndpoint(endpoint)
	if err != nil {
		return err
	}

	if _, err := os.Stat(filepath.Join(dir, identityFile)); err == nil {
		fmt.Fprint(out, "This data dir is already initialized — re-pair and mint a NEW identity? [y/N]: ")
		answer, _ := readLine(r)
		if !strings.EqualFold(answer, "y") {
			return fmt.Errorf("setup cancelled")
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, pcpFile), PCPPeer{
		ControlPub: sb.PCPControl, SealPub: sb.PCPSeal, PairedAt: time.Now(),
	}); err != nil {
		return err
	}
	st, err := initIdentity(dir, host, endpoint)
	if err != nil {
		return err
	}

	completion := mailproto.EncodeCompletionBlob(mailproto.CompletionBlob{
		POPub: st.Identity.SignPub, POSealPub: st.Identity.SealPub,
		TLSFP: st.TLSFingerprint(), Endpoint: endpoint,
		PairingToken: sb.PairingToken,
	})
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Paired with %q. Paste this completion code back into the admin console:\n", sb.Name)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, completion)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Then start the gateway:  postoffice run --data-dir %s\n", dir)
	return nil
}

// validatePublicEndpoint checks that the operator entered a reachable
// host:port, not a bind wildcard — the most common setup mistake. It
// returns the host on success.
func validatePublicEndpoint(ep string) (string, error) {
	host, port, found := strings.Cut(ep, ":")
	if !found || host == "" || port == "" {
		return "", fmt.Errorf("that doesn't look like host:port (e.g. mail.example.com:8443)")
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("the port must be a number, e.g. mail.example.com:8443")
		}
	}
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "0.0.0.0", "::", "":
		return "", fmt.Errorf("%q is a bind address, not one anything can dial — enter this server's public hostname or IP (e.g. mail.example.com)", host)
	}
	return host, nil
}

// readLine trims one line of operator input.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("input closed")
	}
	return strings.TrimSpace(line), nil
}
