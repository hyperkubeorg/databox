package admin

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hyperkubeorg/databox/pkg/cluster"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/clusterview"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/invites"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// fixtureShell is the admin scaffolding every page test embeds.
func fixtureShell(title, active string) shell {
	return shell{
		Chrome: kernel.Chrome{
			Title: title, SiteName: "Test Cloud", Theme: "dark",
			CurrentApp: "admin", AppName: "Admin",
			User:    users.User{Username: "root", DisplayName: "Root", IsAdmin: true},
			Session: &users.Session{Username: "root", CSRF: "tok"},
			Admin:   true,
		},
		Active:       active,
		OpenProblems: 2,
	}
}

// TestWebaccessRelayRoutesMounted pins the TCP-relay admin surface:
// both mutations exist as admin-only POSTs.
func TestWebaccessRelayRoutesMounted(t *testing.T) {
	mounted := map[string]bool{}
	for _, rt := range Mount(&kernel.App{}, Deps{}).Routes {
		mounted[rt.Pattern] = true
	}
	for _, want := range []string{
		"POST /admin/webaccess/gateways/tcprelays/add",
		"POST /admin/webaccess/gateways/tcprelays/remove",
	} {
		if !mounted[want] {
			t.Errorf("route %q not mounted", want)
		}
	}
}

// Every console page parses with the base shell and renders its
// fixture data — one task category per page, rail highlighted.
func TestAdminPagesRender(t *testing.T) {
	views := ui.MustParse(tplFS)
	now := time.Now()

	problem := system.Problem{ID: "po-unreachable.abc", Severity: system.SevCritical, Area: "mail",
		Summary: "Post office fra-1 hasn't answered for 10m.", Action: "Check the machine.",
		Source: "/admin/mail/postoffices/abc", Since: now.Add(-10 * time.Minute)}

	cases := []struct {
		page string
		data any
		want []string
	}{
		{"admin_home", HomePage{shell: fixtureShell("Admin", "home"),
			Problems: []system.Problem{problem},
			Areas: []AreaRow{
				{Name: "Mail", Href: "/admin/mail/domains", Light: "crit", Status: "1 domain"},
				{Name: "People", Href: "/admin/users", Light: "ok", Status: "3 members"},
			}},
			[]string{"answered for 10m", "Check the machine", `class="sev critical"`, `class="dot crit"`, "3 members"}},

		{"admin_users", UsersPage{shell: fixtureShell("Users", "users"),
			Users: []users.User{{Username: "ada", DisplayName: "Ada", Banned: true, Tier: "family", InvitedBy: "root"}}},
			[]string{"@ada", "banned", "family", "invited by @root", `href="/admin/users/ada"`}},

		{"admin_user_detail", UserDetailPage{shell: fixtureShell("@ada", "users"),
			U:     users.User{Username: "ada", DisplayName: "Ada", UsedBytes: 5 << 20, MailboxCount: 1},
			Quota: 10 << 30, Mailboxes: 3,
			Tiers:     []site.Tier{{Name: "family", Bytes: 42 << 30}},
			Caps:      users.KnownCaps,
			Sessions:  []users.UserSession{{Session: users.Session{Username: "ada", IP: "10.0.0.1", ExpiresAt: now.Add(time.Hour)}, TokenHint: "abcd1234…"}},
			IPs:       []users.UserIP{{IP: "10.0.0.1", Logins: 3, FirstSeen: now, LastSeen: now}},
			Keys:      []apikeys.Key{{KeyID: "k123", Name: "phone", Scopes: []string{"drive:read"}, CreatedAt: now}},
			Addresses: []mail.Address{{Domain: "example.test", Local: "ada", Type: mail.AddrMailbox}}},
			[]string{"@ada", "phone", "drive:read", "Revoke", "View as @ada", "Delete account forever", "abcd1234…", "ada@example.test"}},

		{"admin_invites", InvitesAdminPage{shell: fixtureShell("Invites", "invites"), SignupMode: "invite",
			Invites: []InviteVM{{Invite: invites.Invite{Code: "codecodecode", CreatedBy: "root", Kind: "quantity", MaxUses: 5, Uses: 2, Description: "family"},
				Status: "active", Uses: []invites.InviteUse{{Username: "ada", At: now}}}}},
			[]string{"/signup?invite=codecodecode", "(2/5)", "@ada", "Revoke", "family"}},

		{"admin_tiers", TiersPage{shell: fixtureShell("Tiers", "tiers"),
			SC:      site.Config{Tiers: []site.Tier{{Name: "family", Bytes: 42 << 30}}, DefaultQuota: 10 << 30},
			PerTier: map[string]int{"family": 2}},
			[]string{"family", "42.0 GiB", "Add / resize tier", "Max single upload"}},

		{"admin_usage", UsagePage{shell: fixtureShell("Usage", "usage"), Members: 2, TotalUsed: 1 << 30, TotalQuota: 20 << 30,
			Top: []UsageRow{{Username: "ada", Used: 1 << 30, Quota: 10 << 30, Pct: 10}}},
			[]string{"2 account(s)", "@ada", "10%"}},

		{"admin_site", SitePage{shell: fixtureShell("Site", "site"), Modes: SignupModes,
			SC: site.Config{Name: "Test Cloud", SignupMode: site.SignupInvite}},
			[]string{"Site name", "Trusted invite", "only admins can mint", `value="invite" checked`}},

		{"admin_services", ServicesPage{shell: fixtureShell("Services", "services"),
			Rows: []ServiceRow{
				{ID: "drive", Name: "Drive", Enabled: true, CanDisable: true},
				{ID: "calendar", Name: "Calendar", Requires: []ReqChip{{Name: "Drive", Enabled: true}}, CanEnable: true},
				{ID: "git", Name: "Git", CanEnable: true, PolicyHref: "/admin/site/git", PolicyLabel: "Git Services policy"},
			}},
			[]string{"Services", "Drive", "Requires", "Enable", "Disable", "Git Services policy"}},

		{"admin_service", ServiceDetailPage{shell: fixtureShell("Drive — Services", "services"),
			Row:        ServiceRow{ID: "drive", Name: "Drive", Enabled: true, CanDisable: false, DisableReason: "disable Calendar first", Dependents: []string{"Calendar"}},
			PurgeParts: []string{"All drives, files, and folders"},
			Orphans:    []string{"Calendar"}},
			[]string{"Danger zone", "ten times", "Permanently delete all Drive data", "confirm_name", `action="/admin/services/drive/purge"`}},

		{"admin_mail_domains", MailDomainsPage{shell: fixtureShell("Domains", "mail-domains"), Enabled: true,
			Domains: []DomainRow{{Domain: "example.test", Enabled: true, CreatedAt: now, POCount: 1}}},
			[]string{"example.test", "Setup wizard", "Add domain", `href="/admin/mail/domains/example.test"`}},

		{"admin_mail_domain", MailDomainPage{shell: fixtureShell("example.test", "mail-domains"),
			D:   mail.Domain{Domain: "example.test", DKIMSelector: "pcp", DKIMPublicKey: "AAAA", Enabled: false},
			POs: []mail.PostOffice{{ID: "poabc123def4", Name: "fra-1", Endpoint: "mx.example.test:8443"}},
			Records: []DNSRecord{
				{Host: "example.test", Type: "MX", Value: "10 mx.example.test.", Status: DNSOK},
				{Host: "example.test", Type: "TXT", Value: "v=spf1 -all", Status: DNSUnknown},
			},
			Checked: true, Degraded: true},
			[]string{"verified ✓", "unchecked from here", "Check DNS now", "Enable domain", "couldn't run from this server", "data-copy"}},

		{"admin_mail_postoffices", MailPOsPage{shell: fixtureShell("Post offices", "mail-postoffices"),
			POs: []PORow{{PostOffice: mail.PostOffice{ID: "poabc123def4", Name: "fra-1", Status: mail.POActive, Endpoint: "mx:8443", LastSeen: now}, Answering: true}}},
			[]string{"fra-1", "answering", `href="/admin/mail/postoffices/poabc123def4"`}},

		{"admin_mail_po", MailPOPage{shell: fixtureShell("fra-1", "mail-postoffices"),
			PO: mail.PostOffice{ID: "poabc123def4", Name: "fra-1", Status: mail.POPending, PairingToken: "tok"}, SetupBlob: "PCPPO1.SETUPBLOB"},
			[]string{"PCPPO1.SETUPBLOB", "postoffice setup", "Verify + complete pairing"}},

		{"admin_mail_po", MailPOPage{shell: fixtureShell("fra-1", "mail-postoffices"),
			PO:      mail.PostOffice{ID: "poabc123def4", Name: "fra-1", Status: mail.POActive, Endpoint: "mx:8443", LastPushedSerial: 4, ManifestSerial: 3},
			Drift:   true,
			Live:    &mailproto.StatusResponse{Version: "1", DKIMInRAM: false, SpoolCount: 2, SMTPListening: true, StartedAt: now},
			Sparks:  []SparkSet{sparkFrom("Spool (messages)", 3, func(i int) int64 { return int64(i) })},
			Served:  map[string]int{"example.test": 10},
			Domains: []mail.Domain{{Domain: "example.test", Enabled: true}}},
			[]string{"config drift", "awaiting re-push", "Re-push config", "Spool (messages)", "<svg", "checked"}},

		{"admin_mail_addresses", MailAddressesPage{shell: fixtureShell("Addresses", "mail-addresses"),
			Mailboxes: []AddrVM{{Address: mail.Address{Domain: "example.test", Local: "ada", Type: mail.AddrMailbox, Owner: "ada", CreatedAt: now}, Full: "ada@example.test"}},
			Domains:   []mail.Domain{{Domain: "example.test", Enabled: true}}},
			[]string{"ada@example.test", "Delete account", "permanently erased", "Create email account"}},

		{"admin_mail_aliases", MailAliasesPage{shell: fixtureShell("Aliases", "mail-aliases"),
			Aliases: []AddrVM{{Address: mail.Address{Domain: "example.test", Local: "postmaster", Type: mail.AddrAlias, Owner: "root"}, Full: "postmaster@example.test"}},
			Domains: []mail.Domain{{Domain: "example.test", Enabled: true}}},
			[]string{"postmaster@example.test", "Retarget", "Create alias"}},

		{"admin_mail_distros", MailDistrosPage{shell: fixtureShell("Lists", "mail-distros"),
			Distros: []AddrVM{{Address: mail.Address{Domain: "example.test", Local: "everyone", Type: mail.AddrDistro, Members: []string{"ada@example.test"}}, Full: "everyone@example.test"}},
			Domains: []mail.Domain{{Domain: "example.test", Enabled: true}}},
			[]string{"everyone@example.test", "ada@example.test", "Create a list"}},

		{"admin_mail_welcome", MailWelcomePage{shell: fixtureShell("Welcome", "mail-welcome"),
			Welcomes: []mail.Welcome{{ID: "w1", Subject: "Hello!", Scope: "all", Enabled: true, Order: 1}},
			Edit:     mail.Welcome{Scope: "all", Enabled: true},
			Domains:  []mail.Domain{{Domain: "example.test"}}},
			[]string{"Hello!", "every new mailbox", "Save message"}},

		{"admin_mail_sending", MailSendingPage{shell: fixtureShell("Sending", "mail-sending"),
			SC:        site.Config{Mail: site.MailConfig{Enabled: true, SendPerDay: 100}},
			EffPerDay: 100, EffBurst: 20, EffMsgBytes: 25 << 20, EffMailboxes: 3, EffAliases: 10, EffTrashDays: 30,
			EffSpamTag: 5, EffSpamReject: 15},
			[]string{"Services", "100/day", "Save policy", "DNSBL"}},

		{"admin_build", BuildPage{shell: fixtureShell("Builds", "build"), Enabled: true,
			RetentionDays: 30, DefaultRetention: site.DefaultBuildRetentionDays, DefaultMax: build.DefaultMaxConcurrent,
			AccessMode: site.BuildAccessAllowlist, AccessModes: buildAccessModes,
			Access: []build.AccessEntry{{Subject: "o:acme", By: "root", CreatedAt: now}},
			Runners: []BuildRunnerRow{
				{Runner: build.Runner{ID: "rnabc123def4", Name: "k8s-1", Status: build.RunnerActive, Scope: "system", Kind: "k8s", MaxConcurrent: 2, LastSeen: now}, Answering: true},
				{Runner: build.Runner{ID: "rnpend123def", Name: "pending-1", Status: build.RunnerPending, Scope: "org:acme"}}}},
			[]string{"Builds", "o:acme", "Add to allowlist", "k8s-1", "pending-1", "waiting to pair",
				"Compute access", "Retention", `href="/admin/build/runners/rnabc123def4"`, "/admin/services"}},

		{"admin_build_runner", BuildRunnerPage{shell: fixtureShell("k8s-1", "build"),
			R:         build.Runner{ID: "rnpend123def", Name: "k8s-1", Status: build.RunnerPending, Scope: "system"},
			SetupBlob: "PCPBLD1.SETUP", DefaultMax: build.DefaultMaxConcurrent},
			[]string{"PCPBLD1.SETUP", "pcp-runner setup", "Verify + complete pairing"}},

		{"admin_build_runner", BuildRunnerPage{shell: fixtureShell("k8s-1", "build"),
			R:         build.Runner{ID: "rnabc123def4", Name: "k8s-1", Status: build.RunnerActive, Scope: "system", Kind: "k8s", MaxConcurrent: 4, TLSFingerprint: "abc123", LastSeen: now},
			Answering: true, DefaultMax: build.DefaultMaxConcurrent},
			[]string{"Throttle", "Save throttle", "Re-pair (new identity)", "Remove", "abc123", "executor"}},

		{"admin_wa_gateways", WAGatewaysPage{shell: fixtureShell("Gateways", "wa-gateways"),
			Gateways: []GatewayRow{{Gateway: dferry.Gateway{ID: "gwabc123def4", Name: "edge-1", Status: dferry.GWActive, LastSeen: now}, Answering: true, Drift: true, Hosts: 2, LocalPool: 4}}},
			[]string{"edge-1", "answering", "drift", `href="/admin/webaccess/gateways/gwabc123def4"`}},

		{"admin_wa_gateway", WAGatewayPage{shell: fixtureShell("edge-1", "wa-gateways"),
			GW: dferry.Gateway{ID: "gwabc123def4", Name: "edge-1", Status: dferry.GWPending}, SetupCode: "PCPCF1.SETUP", TLSModes: tlsModes},
			[]string{"PCPCF1.SETUP", "cloudferry setup", "Verify + complete pairing"}},

		{"admin_wa_gateway", WAGatewayPage{shell: fixtureShell("edge-1", "wa-gateways"),
			GW:        dferry.Gateway{ID: "gwabc123def4", Name: "edge-1", Status: dferry.GWActive, ControlEndpoint: "e:1", TunnelEndpoint: "e:2"},
			LocalPool: 4, TLSModes: tlsModes,
			Hosts:      []HostRow{{Host: dferry.Host{Hostname: "pcp.example.test", TLSMode: "acme"}, HasRAMInfo: true, CertInRAM: true, CertExpiry: now.Add(60 * 24 * time.Hour), CertSource: "acme"}},
			Live:       &ferryproto.StatusResponse{Version: "1", Tunnels: 4, Counters: ferryproto.Counters{Requests: 12}},
			DNSChecked: true,
			Relays: []RelayRow{
				{TCPRelay: ferryproto.TCPRelay{EdgePort: 22, TargetPort: 4222, Label: "ssh"}, HasLive: true, Active: 1, Bytes: 2048},
				{TCPRelay: ferryproto.TCPRelay{EdgePort: 2525, TargetPort: 2525}, HasLive: true, Err: "listen tcp :2525: bind: address already in use"}},
			DNSRecords: []DNSRecord{{Host: "pcp.example.test", Type: "A/AAAA", Status: DNSOK, Found: "198.51.100.4"}}},
			[]string{"4 live tunnel(s)", "resolves ✓", "in memory ✓", "Re-push config + certs", "First-request probe",
				"TCP relays", "127.0.0.1:4222", "listening ✓", "address already in use", "Add relay"}},

		{"admin_wa_hostnames", WAHostnamesPage{shell: fixtureShell("Hostnames", "wa-hostnames"), TLSModes: tlsModes,
			Gateways: []dferry.Gateway{{ID: "gwabc123def4", Name: "edge-1"}},
			Hosts:    []HostRow{{Host: dferry.Host{Hostname: "pcp.example.test", GatewayID: "gwabc123def4", TLSMode: "acme", ForceHTTPS: true}, GatewayName: "edge-1", CertExpiry: now, CertSource: "acme"}}},
			[]string{"pcp.example.test", "force https", "Custom certificate", "Add hostname"}},

		{"admin_wa_offline", WAOfflinePage{shell: fixtureShell("Offline", "wa-offline"), HTML: "<h1>brb</h1>"},
			[]string{"Offline page", "srcdoc", "Save offline page"}},

		{"admin_audit", AuditPage{shell: fixtureShell("Audit", "audit"),
			Rows: []kernel.AuditRow{{ID: "id1", AuditEntry: kernel.AuditEntry{At: now, Actor: "root", Action: "user.ban", Target: "ada", Impersonating: "ada", IP: "10.0.0.1"}}},
			SC:   site.Config{AuditDays: 30}},
			[]string{"user.ban", "as @ada", "Export CSV", "Save + apply now", `value="30"`}},

		{"admin_ipbans", IPBansPage{shell: fixtureShell("IP bans", "ipbans"),
			Bans: []users.IPBan{{IP: "203.0.113.7", User: "ada", By: "root", At: now}}},
			[]string{"203.0.113.7", "@ada", "Unban", "Ban"}},

		{"admin_sys_workers", WorkersPage{shell: fixtureShell("Workers", "sys-workers"), Now: now,
			Workers:  []WorkerRow{{ID: "poabc123def4", Name: "fra-1", Kind: "post office", Status: "active", Answering: true, LastSeen: now, Href: "/admin/mail/postoffices/poabc123def4"}},
			Loops:    []LoopRow{{Name: "mailsync", LoopRecord: system.LoopRecord{LastRun: now, LastSuccess: now}}, {Name: "health", LoopRecord: system.LoopRecord{LastRun: now, LastError: "boom", LastErrorAt: now}, Failing: true}},
			Replicas: []system.Replica{{ID: "rep1", Host: "box", PID: 42, StartedAt: now, SeenAt: now}}},
			[]string{"fra-1", "mailsync", "failing", "boom", "box", "beating"}},

		{"admin_sys_databox", DataboxPage{shell: fixtureShell("Databox", "sys-databox"), Now: now,
			Snap: clusterview.Snapshot{Readable: true,
				Nodes:  []cluster.Node{{ID: 1, Name: "n1", Addr: "10.0.0.1:8443", State: "active", LastSeen: now}},
				Groups: []clusterview.GroupHealth{{GroupInfo: cluster.GroupInfo{GID: 1, Members: []uint64{1}, Kind: "meta"}, Stats: cluster.GroupStats{GID: 1, Bytes: 1 << 20, Leader: 1, Reported: now}, HasStats: true}},
				Shards: []cluster.Shard{{ID: 1, GID: 2, State: "splitting", NewGID: 3, SplitKey: "/m"}},
				Paused: map[string]cluster.PauseFlag{"rebalance": {Paused: true, Actor: "ops", Since: now}},
				Alerts: []cluster.Alert{{Name: "underrep", Severity: "warning", Message: "group 2 has 1/3 replicas", Since: now}}},
			Nodes: 1, Groups: 1, Shards: 1, Bytes: 1 << 20},
			[]string{"n1", "splitting", "databox cluster resume rebalance", "databox cluster decommission", "group 2 has 1/3 replicas", "View-only"}},

		{"admin_sys_databox", DataboxPage{shell: fixtureShell("Databox", "sys-databox"), Now: now,
			Snap: clusterview.Snapshot{Readable: false, Notice: "Cluster metadata isn't readable with this databox user."}},
			[]string{"readable with this databox user"}},

		{"admin_sys_problems", ProblemsPage{shell: fixtureShell("Problems", "sys-problems"),
			Open:     []system.Problem{problem},
			Resolved: []system.Problem{{ID: "old", Severity: system.SevWarn, Summary: "was broken", Since: now.Add(-2 * time.Hour), ResolvedAt: now.Add(-time.Hour)}}},
			[]string{"answered for 10m", "Re-check now", "was broken", "resolved"}},
	}

	for _, tc := range cases {
		var buf bytes.Buffer
		if err := views.ExecuteTemplate(&buf, tc.page, tc.data); err != nil {
			t.Fatalf("render %s: %v", tc.page, err)
		}
		out := buf.String()
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s missing %q", tc.page, want)
			}
		}
		// The §11.1 shell invariants: the rail renders on every page and
		// the Home badge shows the open count.
		for _, want := range []string{`class="admrail"`, `<span class="badge">2</span>`} {
			if !strings.Contains(out, want) {
				t.Errorf("%s missing shell element %q", tc.page, want)
			}
		}
	}
}

// The sparkline helper emits pure-SVG polylines.
func TestSparkline(t *testing.T) {
	svg := string(sparkline([]int64{0, 2, 5, 3}))
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "polyline") {
		t.Fatalf("sparkline = %q", svg)
	}
	if sparkline(nil) != "" {
		t.Fatal("empty series renders nothing")
	}
}
