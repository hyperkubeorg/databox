// setup.go — the interactive pairing flow (`cloudferry setup`): paste
// the setup code from the PCP admin console, confirm the public
// endpoints, and paste the printed completion code back. After this the
// gateway is never configured locally again — everything arrives from
// PCP.
//
// One cloudferry, one PCP (§10.1): setup REFUSES an already-initialized
// data dir. Re-pairing — with the same PCP or any other — requires
// wiping the directory first, so a compromised admin console can't
// silently re-key a running gateway.
package cloudferry

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// RunSetup drives the pairing conversation on in/out (the operator's
// terminal over ssh).
func RunSetup(dir string, in io.Reader, out io.Writer) error {
	for _, f := range []string{identityFile, pcpFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return fmt.Errorf("this data dir is already paired — a cloudferry binds to ONE PCP for its lifetime; to re-pair, stop the gateway and wipe %s first", dir)
		}
	}

	r := bufio.NewReader(in)
	fmt.Fprintln(out, "cloudferry setup — pair this gateway with your Personal Cloud Platform.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "In the PCP admin console: Admin → Web access → your gateway.")
	fmt.Fprint(out, "Paste the setup code (PCPCF1.…): ")
	blobLine, err := readLine(r)
	if err != nil {
		return err
	}
	sb, err := ferryproto.DecodeSetupBlob(blobLine)
	if err != nil {
		return err
	}

	defHost, _ := os.Hostname()
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "What PUBLIC hostname or IP does this gateway answer at? This is what the")
	fmt.Fprintln(out, "outside world (and your PCP) reaches it on — NOT a local bind address.")
	fmt.Fprintln(out, "Do not enter 0.0.0.0. Example: ferry.example.com")
	fmt.Fprintf(out, "Public host [%s]: ", defHost)
	host, err := readLine(r)
	if err != nil {
		return err
	}
	if host == "" {
		host = defHost
	}
	if err := validatePublicHost(host); err != nil {
		return err
	}
	control, err := askEndpoint(r, out, "Control port (PCP dials it for config/status)", host, "7444")
	if err != nil {
		return err
	}
	tunnel, err := askEndpoint(r, out, "Tunnel port (PCP's stream pool dials it)", host, "7443")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, pcpFile), PCPPeer{
		ControlPub: sb.PCPControl, SealPub: sb.PCPSeal, PairedAt: time.Now(),
	}); err != nil {
		return err
	}
	signPriv, signPub, err := wire.NewSignPair()
	if err != nil {
		return err
	}
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		return err
	}
	st, err := initIdentity(dir, host, control, tunnel, Identity{
		SignPriv: signPriv, SignPub: signPub,
		SealPriv: sealPriv, SealPub: sealPub,
	})
	if err != nil {
		return err
	}

	completion := ferryproto.EncodeCompletionBlob(ferryproto.CompletionBlob{
		FerryPub: st.Identity.SignPub, FerrySealPub: st.Identity.SealPub,
		TLSFP: st.TLSFingerprint(), Control: control, Tunnel: tunnel,
		PairingToken: sb.PairingToken,
	})
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Paired with %q. Paste this completion code back into the admin console:\n", sb.Name)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, completion)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Then start the gateway:  cloudferry run --data-dir %s\n", dir)
	return nil
}

// askEndpoint prompts for one public port and returns host:port.
func askEndpoint(r *bufio.Reader, out io.Writer, what, host, def string) (string, error) {
	fmt.Fprintf(out, "%s [%s]: ", what, def)
	port, err := readLine(r)
	if err != nil {
		return "", err
	}
	if port == "" {
		port = def
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("the port must be a number, e.g. %s", def)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// validatePublicHost rejects bind wildcards — the most common setup
// mistake.
func validatePublicHost(host string) error {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "0.0.0.0", "::", "":
		return fmt.Errorf("%q is a bind address, not one anything can dial — enter this server's public hostname or IP (e.g. ferry.example.com)", host)
	}
	return nil
}

// readLine trims one line of operator input.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("input closed")
	}
	return strings.TrimSpace(line), nil
}
