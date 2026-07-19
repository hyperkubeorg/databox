// templates_test.go renders the newer pages with REALISTIC data payloads.
// pkg/renderer's tests prove every template parses and renders with nil
// Data; this test goes one step further for the pages added later — a
// template referencing a misspelled field of its handler's data struct
// only fails at execution time WITH that struct, so we execute exactly
// that pairing here.
package frontend

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/blob"
	"github.com/hyperkubeorg/databox/pkg/renderer"
)

// renderPage executes one template with the given handler data and
// returns the HTML, failing the test on any render error.
func renderPage(t *testing.T, name string, data any) string {
	t.Helper()
	rd := renderer.MustNew()
	rec := httptest.NewRecorder()
	p := &renderer.Page{Title: "t", User: "root", Admin: true, CSRF: "tok", Path: "/" + strings.TrimSuffix(name, ".tpl"), Data: data}
	if err := rd.Render(rec, 200, name, p); err != nil {
		t.Fatalf("Render(%s) = %v", name, err)
	}
	return rec.Body.String()
}

// TestClusterTemplate renders the map shell and asserts the scaffolding
// cluster.js drives is present (SVG scene, legend, controls, script) —
// node info windows are created dynamically, so none appear here. The
// page is full-bleed and deliberately carries no heading.
func TestClusterTemplate(t *testing.T) {
	body := renderPage(t, "cluster.tpl", nil)
	for _, want := range []string{
		`id="cmap-svg"`, `class="cmap-legend"`, `id="cmap-status"`,
		`src="/assets/cluster.js"`, "metadata voter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cluster.tpl output missing %q", want)
		}
	}
	if strings.Contains(body, "<h1") {
		t.Error("cluster.tpl must not render a heading — the map is full-bleed")
	}
}

// TestPoliciesTemplate exercises policies.tpl with rules in both
// families, the defaults, and a resolved sample.
func TestPoliciesTemplate(t *testing.T) {
	body := renderPage(t, "policies.tpl", &policiesData{
		Replication: []policyRow{{Path: "/important", JSON: `{"replicas":3}`}},
		EC:          []policyRow{{Path: "/logs", JSON: `{"data":6,"parity":3,"enabled":true}`}},
		Defaults:    blob.DefaultPolicy(0),
		Sample: &policySample{
			Key:    "/logs/app.log",
			Policy: blob.Policy{Replicas: 2, DataShards: 6, ParityShards: 3, ECEnabled: true},
			ECRule: "/logs", ECEnabled: true,
		},
		Notice: "saved",
	})
	for _, want := range []string{"/important", "rs-6-3", "built-in default", "saved"} {
		if !strings.Contains(body, want) {
			t.Errorf("policies.tpl output missing %q", want)
		}
	}
}

// TestLocksTemplate exercises locks.tpl with a TTL'd holder, an expired
// one, and a no-TTL exclusive lock.
func TestLocksTemplate(t *testing.T) {
	body := renderPage(t, "locks.tpl", &locksData{
		Rows: []lockRow{
			{Resource: "jobs/nightly", Holder: "worker-1", Mode: "exclusive", Fencing: 7,
				Expires: time.Now().Add(time.Minute)},
			{Resource: "jobs/hourly", Holder: "worker-2", Mode: "shared", Fencing: 3,
				Expires: time.Now().Add(-time.Minute), Expired: true},
			{Resource: "deploy", Holder: "ci", Mode: "exclusive", Fencing: 12},
		},
	})
	for _, want := range []string{"jobs/nightly", "worker-2", "expired", "force unlock"} {
		if !strings.Contains(body, want) {
			t.Errorf("locks.tpl output missing %q", want)
		}
	}
}

// TestAuditTemplate exercises audit.tpl with filters, rows, and the
// truncation banner.
func TestAuditTemplate(t *testing.T) {
	body := renderPage(t, "audit.tpl", &auditData{
		Actor: "root", Action: "force",
		Rows: []auditRow{{
			Time: time.Now().UTC(), Actor: "root",
			Action: "force-unlock", Detail: "resource=deploy reason=stuck",
		}},
		Truncated: true,
	})
	for _, want := range []string{"force-unlock", "resource=deploy", "Scan budget"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit.tpl output missing %q", want)
		}
	}
}

// TestQueryTemplate exercises query.tpl in its three result shapes:
// a text get, a list page with a continuation, and an inline error.
func TestQueryTemplate(t *testing.T) {
	get := renderPage(t, "query.tpl", &queryData{
		Op: "get", Key: "/app/config", Limit: 100,
		Result: &queryResult{Op: "get", Key: "/app/config", Found: true, Rev: 42,
			Size: 5, Printable: true, Text: "hello", Notice: "found at rev 42, 5 bytes"},
	})
	for _, want := range []string{"hello", "found at rev 42", "Watch preview"} {
		if !strings.Contains(get, want) {
			t.Errorf("query.tpl get output missing %q", want)
		}
	}

	list := renderPage(t, "query.tpl", &queryData{
		Op: "list", Key: "/app/", Limit: 2,
		Result: &queryResult{Op: "list", Key: "/app/", Notice: "2 keys under /app/",
			Rows: []kvRow{{Key: "/app/a", Rev: 1, Size: 3}, {Key: "/app/b", Rev: 2, Size: 4, Blob: true}},
			Next: "/app/b"},
	})
	for _, want := range []string{"/app/a", "Next page", "blob"} {
		if !strings.Contains(list, want) {
			t.Errorf("query.tpl list output missing %q", want)
		}
	}

	denied := renderPage(t, "query.tpl", &queryData{
		Op: "set", Key: "/secret", Limit: 100,
		Result: &queryResult{Op: "set", Key: "/secret", Err: "Unauthorized: no grant allows write on /secret"},
	})
	if !strings.Contains(denied, "no grant allows write") {
		t.Error("query.tpl error output missing the inline denial")
	}
}
