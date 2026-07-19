// config_test.go covers resolution of the security/MVCC tunables added
// alongside the §6 hardening work: defaults, file/env merging, and the
// Finish() validation rules.
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// New fields have safe defaults and pass validation out of the box.
func TestDefaultsAndFinish(t *testing.T) {
	c := Default()
	if err := c.Finish(); err != nil {
		t.Fatalf("Finish on defaults: %v", err)
	}
	if c.PSKExtraGrace != 720*time.Hour {
		t.Fatalf("psk_extra_grace default = %v, want 720h", c.PSKExtraGrace)
	}
	if c.InternalClientCerts != "require" {
		t.Fatalf("internal_client_certs default = %q, want require", c.InternalClientCerts)
	}
	if c.MVCCHistoryRevs != 4096 || c.MVCCGCInterval != 512 {
		t.Fatalf("mvcc defaults = %d/%d, want 4096/512", c.MVCCHistoryRevs, c.MVCCGCInterval)
	}
}

// File values override defaults and are tracked as file-sourced.
func TestLoadFileNewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "psk_extra_grace: 48h\ninternal_client_certs: \"off\"\nmvcc_history_revs: 128\nmvcc_gc_interval: 16\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if err := c.LoadFile(path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if c.PSKExtraGrace != 48*time.Hour || c.InternalClientCerts != "off" ||
		c.MVCCHistoryRevs != 128 || c.MVCCGCInterval != 16 {
		t.Fatalf("file values not applied: %+v", c)
	}
	for _, name := range []string{"psk_extra_grace", "internal_client_certs", "mvcc_history_revs", "mvcc_gc_interval"} {
		if c.sources[name] != SourceFile {
			t.Fatalf("source of %s = %v, want file", name, c.sources[name])
		}
	}
}

// Environment beats the file for the new fields too.
func TestLoadEnvNewFields(t *testing.T) {
	t.Setenv("DATABOX_PSK_EXTRA_GRACE", "24h")
	t.Setenv("DATABOX_INTERNAL_CLIENT_CERTS", "off")
	t.Setenv("DATABOX_MVCC_HISTORY_REVS", "256")
	t.Setenv("DATABOX_MVCC_GC_INTERVAL", "32")
	c := Default()
	c.LoadEnv()
	if c.PSKExtraGrace != 24*time.Hour || c.InternalClientCerts != "off" ||
		c.MVCCHistoryRevs != 256 || c.MVCCGCInterval != 32 {
		t.Fatalf("env values not applied: %+v", c)
	}
}

// Finish rejects nonsense values for the new fields.
func TestFinishValidatesNewFields(t *testing.T) {
	bad := func(mutate func(*Config)) {
		t.Helper()
		c := Default()
		mutate(c)
		if err := c.Finish(); err == nil {
			t.Fatal("Finish accepted an invalid value")
		}
	}
	bad(func(c *Config) { c.InternalClientCerts = "maybe" })
	bad(func(c *Config) { c.MVCCHistoryRevs = 0 })
	bad(func(c *Config) { c.MVCCGCInterval = -1 })
}
