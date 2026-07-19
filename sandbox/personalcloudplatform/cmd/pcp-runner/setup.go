// setup.go — the interactive pairing flow (`pcp-runner setup`): paste the
// setup code from the PCP admin console, confirm the buildwire endpoint,
// pick the executor kind, and paste the printed completion code back.
// After this the runner dials PCP and serves jobs; it is never configured
// locally again (config arrives over the tunnel, §7).
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/buildproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/wire"
)

// RunSetup drives the pairing conversation on in/out. kindFlag overrides
// the auto-detected executor kind ("" = auto-detect).
func RunSetup(dir, kindFlag string, in io.Reader, out io.Writer) error {
	for _, f := range []string{identityFile, pcpFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return fmt.Errorf("this data dir is already paired — a runner binds to ONE PCP for its lifetime; to re-pair, stop it and wipe %s first", dir)
		}
	}

	r := bufio.NewReader(in)
	fmt.Fprintln(out, "pcp-runner setup — pair this build runner with your Personal Cloud Platform.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "In the PCP admin console: Admin → Builds → Runners → your runner.")
	fmt.Fprint(out, "Paste the setup code (PCPBR1.…): ")
	blobLine, err := readLine(r)
	if err != nil {
		return err
	}
	sb, err := buildproto.DecodeSetupBlob(blobLine)
	if err != nil {
		return err
	}
	if sb.RunnerID == "" {
		return fmt.Errorf("that setup code carries no runner id — regenerate it in the admin console")
	}

	endpoint := sb.Endpoint
	if endpoint == "" {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "What buildwire endpoint does this runner dial PCP at? (host:port; PCP's")
		fmt.Fprintln(out, "default buildwire port is 4223, or a cloudferry TCP relay's public port.)")
		fmt.Fprint(out, "PCP buildwire endpoint: ")
		endpoint, err = readLine(r)
		if err != nil {
			return err
		}
	}
	if endpoint == "" {
		return fmt.Errorf("a buildwire endpoint is required")
	}

	kind := kindFlag
	if kind == "" {
		kind = detectKind()
	}
	if !buildproto.ValidKind(kind) {
		return fmt.Errorf("unknown executor kind %q (want k8s or baremetal)", kind)
	}

	ctlPriv, ctlPub, err := wire.NewSignPair()
	if err != nil {
		return err
	}
	sealPriv, sealPub, err := wire.NewSealPair()
	if err != nil {
		return err
	}
	id := Identity{
		ControlPriv: ctlPriv, ControlPub: ctlPub,
		SealPriv: sealPriv, SealPub: sealPub, Kind: kind,
	}
	peer := PCPPeer{
		RunnerID: sb.RunnerID, ControlPub: sb.PCPControl, SealPub: sb.PCPSeal,
		Endpoint: endpoint, PairedAt: time.Now(),
	}
	st, err := writeIdentity(dir, id, peer)
	if err != nil {
		return err
	}

	completion := buildproto.EncodeCompletionBlob(buildproto.CompletionBlob{
		RunnerPub: st.Identity.ControlPub, RunnerSealPub: st.Identity.SealPub,
		TLSFP: st.TLSFingerprint(), Kind: kind, PairingToken: sb.PairingToken,
	})
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Paired with %q as a %s runner. Paste this completion code back into the admin console:\n", sb.Name, kind)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, completion)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Then start the runner:  pcp-runner run --data-dir %s\n", dir)
	return nil
}

// detectKind guesses the executor kind: in-cluster (a ServiceAccount
// token mounted, or KUBERNETES_SERVICE_HOST set) → k8s, else bare metal.
func detectKind() string {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return buildproto.KindK8s
	}
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		return buildproto.KindK8s
	}
	return buildproto.KindBareMetal
}

// readLine trims one line of operator input.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("input closed")
	}
	return strings.TrimSpace(line), nil
}
