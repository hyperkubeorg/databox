package health

import (
	"context"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/clusterview"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx/kvxtest"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
)

// fixture wires every store onto one fake databox. The first signup is
// the admin (first-admin marker), so notifications land on "root".
func fixture(t *testing.T) (*Worker, *client.Client) {
	t.Helper()
	db := kvxtest.New(t)
	userStore := &users.Store{DB: db}
	w := New()
	w.System = &system.Store{DB: db}
	w.Mail = &mail.Store{DB: db, Users: userStore}
	w.Ferry = &dferry.Store{DB: db}
	w.Site = &site.Store{DB: db}
	w.Users = userStore
	w.Media = &media.Store{DB: db}
	w.Notify = &notify.Store{DB: db}
	w.Cluster = &clusterview.Store{DB: db}
	w.DefaultQuota = 10 << 30
	ctx := context.Background()
	if _, err := userStore.CreateUser(ctx, "root", "Root", "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := userStore.CreateUser(ctx, "ada", "Ada", "password123"); err != nil {
		t.Fatal(err)
	}
	return w, db
}

// problemIDs collects the open problem set.
func problemIDs(t *testing.T, w *Worker) map[string]system.Problem {
	t.Helper()
	open, err := w.System.OpenProblems(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]system.Problem{}
	for _, p := range open {
		out[p.ID] = p
	}
	return out
}

// An unreachable ACTIVE post office raises critical, notifies every
// admin exactly once per window, and auto-resolves when polls resume.
func TestGatewayUnreachableRaisesAndResolves(t *testing.T) {
	ctx := context.Background()
	w, db := fixture(t)
	po := mail.PostOffice{ID: "poabc123def4", Name: "fra-1", Status: mail.POActive,
		LastSeen: time.Now().Add(-10 * time.Minute)}
	must(t, kvx.SetJSON(ctx, db, "/pcp/mail/postoffices/"+po.ID, po))

	must(t, w.RunOnce(ctx))
	open := problemIDs(t, w)
	p, ok := open["po-unreachable.poabc123def4"]
	if !ok || p.Severity != system.SevCritical || p.Area != "mail" {
		t.Fatalf("unreachable problem missing/wrong: %+v", open)
	}
	if p.Summary == "" || p.Action == "" || p.Source == "" {
		t.Fatalf("problem must carry summary+action+source: %+v", p)
	}

	// Admins were notified; members were not; dedup holds on re-run.
	rootNotifs, _ := w.Notify.List(ctx, "root", 50)
	adaNotifs, _ := w.Notify.List(ctx, "ada", 50)
	if len(rootNotifs) != 1 || len(adaNotifs) != 0 {
		t.Fatalf("notify fanout wrong: root=%d ada=%d", len(rootNotifs), len(adaNotifs))
	}
	must(t, w.RunOnce(ctx))
	rootNotifs, _ = w.Notify.List(ctx, "root", 50)
	if len(rootNotifs) != 1 {
		t.Fatalf("dedup failed: root has %d notifications", len(rootNotifs))
	}

	// The gateway answers again: the problem resolves into a tombstone.
	po.LastSeen = time.Now()
	must(t, kvx.SetJSON(ctx, db, "/pcp/mail/postoffices/"+po.ID, po))
	must(t, w.RunOnce(ctx))
	if open := problemIDs(t, w); len(open) != 0 {
		t.Fatalf("problem should have resolved: %+v", open)
	}
	all, _ := w.System.Problems(ctx)
	if len(all) != 1 || !all[0].Resolved() {
		t.Fatalf("tombstone missing: %+v", all)
	}
}

// Key freshness + growing queues, evaluated from stored samples.
func TestSampleChecks(t *testing.T) {
	ctx := context.Background()
	w, db := fixture(t)
	po := mail.PostOffice{ID: "pofresh12345", Name: "fra-2", Status: mail.POActive,
		LastSeen: time.Now(), LastPushedSerial: 7}
	must(t, kvx.SetJSON(ctx, db, "/pcp/mail/postoffices/"+po.ID, po))
	// Oldest → newest: queues growing, keys missing on the newest poll.
	for i, spool := range []int{1, 3, 5} {
		w.System.RecordSample(ctx, po.ID, system.Sample{
			Kind: system.SamplePostoffice, Serial: 7,
			KeysInRAM:  i != 2,
			SpoolCount: spool,
		})
		time.Sleep(2 * time.Millisecond) // distinct inverted-timestamp ids
	}
	must(t, w.RunOnce(ctx))
	open := problemIDs(t, w)
	if _, ok := open["po-keys.pofresh12345"]; !ok {
		t.Fatalf("keys-awaiting-re-push problem missing: %+v", open)
	}
	if _, ok := open["po-queue.pofresh12345"]; !ok {
		t.Fatalf("growing-queue problem missing: %+v", open)
	}
	if _, ok := open["po-unreachable.pofresh12345"]; ok {
		t.Fatal("fresh gateway must not read unreachable")
	}
}

// Certificate expiry: <14d warns, <3d goes critical.
func TestCertExpiry(t *testing.T) {
	ctx := context.Background()
	w, db := fixture(t)
	must(t, kvx.SetJSON(ctx, db, "/pcp/cloudferry/hosts/warn.example.com", dferry.Host{
		Hostname: "warn.example.com", GatewayID: "gwabc123def4", TLSMode: "acme"}))
	must(t, kvx.SetJSON(ctx, db, "/pcp/cloudferry/certs/warn.example.com", dferry.HostCert{
		Hostname: "warn.example.com", Source: "acme", NotAfter: time.Now().Add(10 * 24 * time.Hour)}))
	must(t, kvx.SetJSON(ctx, db, "/pcp/cloudferry/hosts/crit.example.com", dferry.Host{
		Hostname: "crit.example.com", GatewayID: "gwabc123def4", TLSMode: "custom"}))
	must(t, kvx.SetJSON(ctx, db, "/pcp/cloudferry/certs/crit.example.com", dferry.HostCert{
		Hostname: "crit.example.com", Source: "custom", NotAfter: time.Now().Add(24 * time.Hour)}))

	must(t, w.RunOnce(ctx))
	open := problemIDs(t, w)
	warn := open["cert-expiry.warn-example-com"]
	crit := open["cert-expiry.crit-example-com"]
	if warn.Severity != system.SevWarn {
		t.Fatalf("10-day cert = %+v, want warn", warn)
	}
	if crit.Severity != system.SevCritical {
		t.Fatalf("1-day cert = %+v, want critical", crit)
	}
}

// A failing loop record raises; success resolves.
func TestLoopFailureRaises(t *testing.T) {
	ctx := context.Background()
	w, _ := fixture(t)
	w.System.RecordLoop(ctx, "mailsync", context.DeadlineExceeded)
	must(t, w.RunOnce(ctx))
	if _, ok := problemIDs(t, w)["loop.mailsync"]; !ok {
		t.Fatal("failing loop must raise")
	}
	w.System.RecordLoop(ctx, "mailsync", nil)
	must(t, w.RunOnce(ctx))
	if _, ok := problemIDs(t, w)["loop.mailsync"]; ok {
		t.Fatal("healthy loop must resolve")
	}
}

// The §11.4 databox checks read fixture metadata through the
// `.databox/` view: a node databox's liveness VERDICT reports dead
// raises critical — while a live node with an old LastSeen raises
// nothing (databox stamps LastSeen only on verdict flips, so on a
// healthy cluster the stamp just ages; judging by it once declared
// every node dead after an hour of clean uptime). A splitting shard
// starts info and escalates past 30 minutes (problem age, not wall
// clock); pause flags surface as info.
func TestDataboxChecks(t *testing.T) {
	ctx := context.Background()
	w, db := fixture(t)
	must(t, kvx.SetJSON(ctx, db, "/.databox/"+cluster.KeyNodes+"1", cluster.Node{
		ID: 1, Name: "n1", Addr: "10.0.0.1:8443", State: "active",
		Live: false, LastSeen: time.Now().Add(-10 * time.Minute)}))
	must(t, kvx.SetJSON(ctx, db, "/.databox/"+cluster.KeyNodes+"2", cluster.Node{
		ID: 2, Name: "n2", Addr: "10.0.0.2:8443", State: "active",
		Live: true, LastSeen: time.Now().Add(-2 * time.Hour)}))
	must(t, kvx.SetJSON(ctx, db, "/.databox/"+cluster.KeyGroups+"2", cluster.GroupInfo{
		GID: 2, Members: []uint64{1, 2}, Kind: "data"}))
	must(t, kvx.SetJSON(ctx, db, "/.databox/"+cluster.KeyShards+"1", cluster.Shard{
		ID: 1, Start: "", End: "", GID: 2, State: "splitting", NewGID: 3}))
	must(t, kvx.SetJSON(ctx, db, "/.databox/"+cluster.KeyAdminPause+"rebalance", cluster.PauseFlag{
		Paused: true, Actor: "ops", Since: time.Now()}))

	must(t, w.RunOnce(ctx))
	open := problemIDs(t, w)
	if p := open["databox-node.1"]; p.Severity != system.SevCritical {
		t.Fatalf("dead node = %+v, want critical", p)
	}
	if p, ok := open["databox-node.2"]; ok {
		t.Fatalf("live node with an aged LastSeen must not alert, got %+v", p)
	}
	if p := open["databox-split.1"]; p.Severity != system.SevInfo {
		t.Fatalf("young split = %+v, want info", p)
	}
	if p := open["databox-paused.rebalance"]; p.Severity != system.SevInfo {
		t.Fatalf("pause flag = %+v, want info", p)
	}

	// Age the split problem past the threshold: it escalates to warn.
	splitProblem := open["databox-split.1"]
	splitProblem.Since = time.Now().Add(-45 * time.Minute)
	must(t, kvx.SetJSON(ctx, db, "/pcp/system/problems/databox-split.1", splitProblem))
	must(t, w.RunOnce(ctx))
	if p := problemIDs(t, w)["databox-split.1"]; p.Severity != system.SevWarn {
		t.Fatalf("aged split = %+v, want warn", p)
	}
}

// When the databox user can't read the system view (the fake answers
// 404/permission style errors are covered by clusterview; here: an
// EMPTY metadata view yields no databox problems and no crash).
func TestDataboxUnreadableDegrades(t *testing.T) {
	ctx := context.Background()
	w, _ := fixture(t)
	w.Cluster = nil // unwired = degrade silently
	must(t, w.RunOnce(ctx))
	for id := range problemIDs(t, w) {
		t.Fatalf("no problems expected, got %s", id)
	}
}

// Site storage near the promised total raises the warn.
func TestStorageNearCap(t *testing.T) {
	ctx := context.Background()
	w, _ := fixture(t)
	w.DefaultQuota = 500
	must(t, w.Users.ChargeQuota(ctx, "root", 950, 0))
	must(t, w.RunOnce(ctx))
	if _, ok := problemIDs(t, w)["storage-near-cap"]; !ok {
		t.Fatal("storage warning missing at 95% of promised quota")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
