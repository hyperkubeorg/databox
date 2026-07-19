// cli_client.go builds authenticated API clients for the operator-facing
// commands (cluster/user/grant/backup/…) and implements the console's
// trust-on-first-use certificate store (§6.3):
//
//	~/.databox/known_certs/<sha256-hex>   one empty file per trusted cert
//
// A connection to a server whose certificate is neither CA-verified nor
// already pinned pauses and shows the fingerprint; accepting stores it.
package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"github.com/hyperkubeorg/databox/pkg/client"
)

// connFlags are shared by every command that talks to a cluster.
func connFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "endpoint", Value: "localhost:8443", Usage: "cluster endpoint (host:port)"},
		&cli.StringFlag{Name: "user", Value: "root", Usage: "username to authenticate as"},
		&cli.StringFlag{Name: "password", Usage: "password (prompted if required and not given)", Sources: cli.EnvVars("DATABOX_PASSWORD")},
		&cli.StringFlag{Name: "token", Usage: "pre-authenticated session token", Sources: cli.EnvVars("DATABOX_TOKEN")},
		&cli.StringFlag{Name: "ca-fingerprint", Usage: "pin the server certificate by SHA-256 fingerprint (skips the trust prompt — for automation)", Sources: cli.EnvVars("DATABOX_CA_FINGERPRINT")},
		&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Value: "text", Usage: "output format: text|json|yaml"},
	}
}

// knownCertsDir is the console trust store location.
func knownCertsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".databox", "known_certs")
}

// loadKnownFingerprints reads all pinned fingerprints from the store.
func loadKnownFingerprints() []string {
	entries, err := os.ReadDir(knownCertsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		// File names are the raw hex; convert to the AA:BB display form
		// the client compares against.
		out = append(out, hexToColons(e.Name()))
	}
	return out
}

// hexToColons converts "aabbcc…" to "AA:BB:CC:…".
func hexToColons(h string) string {
	h = strings.ToUpper(h)
	var parts []string
	for i := 0; i+1 < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// promptTrust implements the §6.3 interactive workflow.
func promptTrust(fp string, cert *x509.Certificate) bool {
	fmt.Fprintf(os.Stderr, "\nThe server presented an unknown certificate:\n")
	fmt.Fprintf(os.Stderr, "  Subject:      %s\n", cert.Subject)
	fmt.Fprintf(os.Stderr, "  Issuer:       %s\n", cert.Issuer)
	fmt.Fprintf(os.Stderr, "  Valid:        %s → %s\n", cert.NotBefore.Format("2006-01-02"), cert.NotAfter.Format("2006-01-02"))
	fmt.Fprintf(os.Stderr, "  SHA-256:      %s\n", fp)
	fmt.Fprintf(os.Stderr, "Trust this certificate and remember it? [y/N]: ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	if strings.TrimSpace(strings.ToLower(line)) != "y" {
		return false
	}
	// Persist: one file named by the bare hex fingerprint.
	dir := knownCertsDir()
	_ = os.MkdirAll(dir, 0o700)
	name := strings.ToLower(strings.ReplaceAll(fp, ":", ""))
	_ = os.WriteFile(filepath.Join(dir, name), nil, 0o600)
	return true
}

// dial builds a client from command flags and authenticates it — with the
// provided token, or by logging in (prompting for a password when needed).
func dial(ctx context.Context, cmd *cli.Command) (*client.Client, error) {
	// An explicitly pinned fingerprint joins the persisted trust store,
	// so automation (`--ca-fingerprint`, or the env var) never hits the
	// interactive prompt.
	pins := loadKnownFingerprints()
	if fp := cmd.String("ca-fingerprint"); fp != "" {
		pins = append(pins, fp)
	}
	c, err := client.New(client.Options{
		Endpoint:          cmd.String("endpoint"),
		TrustFingerprints: pins,
		OnUnknownCert:     promptTrust,
		Token:             cmd.String("token"),
	})
	if err != nil {
		return nil, err
	}
	if cmd.String("token") != "" {
		return c, nil
	}
	user := cmd.String("user")
	pass := cmd.String("password")
	err = c.Login(ctx, user, pass)
	if err != nil && pass == "" && term.IsTerminal(int(os.Stdin.Fd())) {
		// Interactive fallback: the account has a password; ask for it.
		fmt.Fprintf(os.Stderr, "Password for %s: ", user)
		raw, terr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if terr != nil {
			return nil, err
		}
		err = c.Login(ctx, user, string(raw))
	}
	if err != nil {
		return nil, fmt.Errorf("login as %q failed: %w", user, err)
	}
	return c, nil
}
