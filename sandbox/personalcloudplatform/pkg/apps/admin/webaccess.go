// webaccess.go — the Web access section, three task pages (§11.1):
// Gateways (list + the pairing WIZARD: code → verify → hostname → TLS
// mode → DNS check → first-request probe; per-gateway detail with
// status, tunnels, drift, sample sparklines, the error ring, and
// re-push), Hostnames & certificates (the routing table + custom cert
// upload + mode changes), and the Offline page editor. Replaces phase
// 7's single webaccess page.
package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/cloudferryclient"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferryproto"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// tlsModes is the hostname form's selector.
var tlsModes = []string{ferryproto.TLSModeACME, ferryproto.TLSModeSelfSigned, ferryproto.TLSModeCustom}

// GatewayRow is one gateway in the list.
type GatewayRow struct {
	dferry.Gateway
	Drift     bool
	LocalPool int
	Hosts     int
	Answering bool
}

// WAGatewaysPage is /admin/webaccess/gateways.
type WAGatewaysPage struct {
	shell
	Gateways []GatewayRow
}

func (h *handlers) waGateways(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := WAGatewaysPage{shell: h.shell(r, "Gateways", "wa-gateways", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	gws, err := h.k.Ferry.ListGateways(cctx)
	if err != nil {
		pg.Error = "couldn't load gateways"
	}
	pools := map[string]int{}
	if h.deps.PoolHealth != nil {
		pools = h.deps.PoolHealth()
	}
	for _, gw := range gws {
		row := GatewayRow{Gateway: gw, LocalPool: pools[gw.ID],
			Answering: time.Since(gw.LastSeen) < 2*time.Minute}
		row.Drift = gw.Status == dferry.GWActive && gw.LastPushedSerial != gw.LastConfigSerial
		if hosts, err := h.k.Ferry.HostsForGateway(cctx, gw.ID); err == nil {
			row.Hosts = len(hosts)
		}
		pg.Gateways = append(pg.Gateways, row)
	}
	h.render(w, "admin_wa_gateways", pg)
}

// WAGatewayPage is one gateway's detail: the pairing wizard while
// pending, the §11.3 dashboard once paired — including the wizard's
// later steps (hostname → DNS check → first-request probe) so the
// admin can walk the whole flow on one page.
type WAGatewayPage struct {
	shell
	GW        dferry.Gateway
	SetupCode string
	Hosts     []HostRow
	LocalPool int
	Drift     bool
	// Live is a fresh status poll (nil when unreachable).
	Live    *ferryproto.StatusResponse
	LiveErr string
	Sparks  []SparkSet
	// DNSChecked/DNSDegraded/DNSRecords: the wizard's A/AAAA step
	// (?check=1).
	DNSChecked  bool
	DNSDegraded bool
	DNSRecords  []DNSRecord
	TLSModes    []string
	// EdgeBodyMiB / EdgeGitBodyMiB render the body-cap overrides in MiB
	// (0 = default). Git wire POSTs ride their own knob (§6.4).
	EdgeBodyMiB    int64
	EdgeGitBodyMiB int64
	// EdgeDefaults labels the placeholders.
	EdgeDefaults struct {
		MaxConns, PerIPPerMin int
		BodyMiB, GitBodyMiB   int64
	}
	// Relays are the configured TCP relays, joined with the live status
	// poll's per-relay counters when the gateway answered.
	Relays []RelayRow
}

// RelayRow is one TCP relay's rendered state (config + live sample).
type RelayRow struct {
	ferryproto.TCPRelay
	HasLive bool
	Active  int
	Bytes   uint64
	Err     string
}

// HostRow is one hostname's rendered state.
type HostRow struct {
	dferry.Host
	GatewayName string
	CertExpiry  time.Time
	CertSource  string
	CertInRAM   bool
	HasRAMInfo  bool
}

func (h *handlers) waGatewayDetail(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	gw, found, err := h.k.Ferry.GetGateway(cctx, r.PathValue("id"))
	if err != nil || !found {
		http.NotFound(w, r)
		return
	}
	pg := WAGatewayPage{shell: h.shell(r, gw.Name, "wa-gateways", sess, user), GW: gw, TLSModes: tlsModes}
	pg.EdgeBodyMiB = gw.EdgeMaxBodyBytes >> 20
	pg.EdgeGitBodyMiB = gw.EdgeMaxGitBodyBytes >> 20
	pg.EdgeDefaults.MaxConns = dferry.DefaultMaxConns
	pg.EdgeDefaults.PerIPPerMin = dferry.DefaultPerIPPerMin
	pg.EdgeDefaults.BodyMiB = dferry.DefaultMaxBodyBytes >> 20
	pg.EdgeDefaults.GitBodyMiB = dferry.DefaultMaxGitBodyBytes >> 20
	if gw.Status == dferry.GWPending {
		pg.SetupCode = gw.SetupBlob()
	}
	if h.deps.PoolHealth != nil {
		pg.LocalPool = h.deps.PoolHealth()[gw.ID]
	}
	pg.Drift = gw.Status == dferry.GWActive && gw.LastPushedSerial != gw.LastConfigSerial
	hosts, _ := h.k.Ferry.HostsForGateway(cctx, gw.ID)

	var ramCerts map[string]ferryproto.HostCertStatus
	if gw.Status == dferry.GWActive {
		if fc, err := cloudferryclient.New(cloudferryclient.Pairing{
			ControlEndpoint: gw.ControlEndpoint, TunnelEndpoint: gw.TunnelEndpoint,
			TLSFingerprint: gw.TLSFingerprint, ControlPriv: gw.PCPControlPriv,
			FerrySealPub: gw.FerrySealPub,
		}); err != nil {
			pg.LiveErr = err.Error()
		} else {
			liveCtx, liveCancel := context.WithTimeout(r.Context(), 4*time.Second)
			defer liveCancel()
			if st, err := fc.Status(liveCtx); err != nil {
				pg.LiveErr = "the gateway didn't answer: " + err.Error()
			} else {
				pg.Live = &st
				h.k.Ferry.TouchGateway(cctx, gw.ID, st.Summary(), st.ConfigSerial, st.Tunnels)
				ramCerts = map[string]ferryproto.HostCertStatus{}
				for _, c := range st.Certs {
					ramCerts[c.Hostname] = c
				}
			}
		}
		if samples, err := h.k.System.Samples(cctx, gw.ID, 60); err == nil && len(samples) > 0 {
			n := len(samples)
			pg.Sparks = []SparkSet{
				sparkFrom("Tunnels", n, func(i int) int64 { return int64(samples[i].Tunnels) }),
				sparkFrom("Requests", n, func(i int) int64 { return int64(samples[i].Requests) }),
				sparkFrom("5xx responses", n, func(i int) int64 { return int64(samples[i].Err5xx) }),
				sparkFrom("Recent errors", n, func(i int) int64 { return int64(samples[i].Errors) }),
			}
		}
	}
	// Join configured relays with the live poll's per-relay counters.
	liveRelays := map[uint16]ferryproto.TCPRelayStatus{}
	if pg.Live != nil {
		for _, rs := range pg.Live.TCPRelays {
			liveRelays[rs.EdgePort] = rs
		}
	}
	for _, relay := range gw.TCPRelays {
		row := RelayRow{TCPRelay: relay}
		if rs, ok := liveRelays[relay.EdgePort]; ok {
			row.HasLive, row.Active, row.Bytes, row.Err = true, rs.ActiveConns, rs.Bytes, rs.Error
		}
		pg.Relays = append(pg.Relays, row)
	}
	for _, host := range hosts {
		row := HostRow{Host: host, GatewayName: gw.Name}
		if cert, found, _ := h.k.Ferry.GetCert(cctx, host.Hostname); found {
			row.CertExpiry, row.CertSource = cert.NotAfter, cert.Source
		}
		if ramCerts != nil {
			if st, ok := ramCerts[host.Hostname]; ok {
				row.CertInRAM, row.HasRAMInfo = st.CertInRAM, true
			} else {
				row.HasRAMInfo = true
			}
		}
		pg.Hosts = append(pg.Hosts, row)
		pg.DNSRecords = append(pg.DNSRecords, DNSRecord{
			Host: host.Hostname, Type: "A/AAAA",
			Note: "must point at the gateway's public address",
		})
	}
	if r.URL.Query().Get("check") == "1" && len(pg.DNSRecords) > 0 {
		pg.DNSChecked = true
		checkCtx, checkCancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer checkCancel()
		pg.DNSDegraded = checkDNSRecords(checkCtx, h.deps.Resolver, pg.DNSRecords)
	}
	h.render(w, "admin_wa_gateway", pg)
}

func (h *handlers) waGWCreate(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	gw, _, err := h.k.Ferry.CreateGateway(cctx, strings.TrimSpace(r.FormValue("name")), user.Username)
	if err != nil {
		h.k.Respond(w, r, "/admin/webaccess/gateways", err, nil)
		return
	}
	h.k.Audit(r, user, sess, "webaccess.gateway.create", gw.ID, gw.Name)
	h.k.Respond(w, r, "/admin/webaccess/gateways/"+gw.ID+"?ok=gateway+created+—+follow+the+pairing+steps", nil, nil)
}

// gwForm resolves the target gateway for a POST (form field "id").
func (h *handlers) gwForm(r *http.Request) (dferry.Gateway, string, error) {
	id := r.FormValue("id")
	back := "/admin/webaccess/gateways/" + id
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	gw, found, err := h.k.Ferry.GetGateway(cctx, id)
	if err != nil {
		return gw, back, err
	}
	if !found {
		return gw, "/admin/webaccess/gateways", users.ErrNotFound
	}
	return gw, back, nil
}

func (h *handlers) waGWPair(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "webaccess.gateway.pair", gw.ID, back, "paired+—+add+a+hostname+next", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Ferry.CompletePairing(cctx, gw.ID, r.FormValue("completion"))
		return err
	})
}

func (h *handlers) waGWRepair(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "webaccess.gateway.repair", gw.ID, back, "re-pair+started+—+wipe+the+gateway's+data+dir+and+run+setup+again", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		_, err := h.k.Ferry.RepairGateway(cctx, gw.ID)
		return err
	})
}

func (h *handlers) waGWStatus(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	disable := r.FormValue("action") == "disable"
	flash := "gateway+enabled"
	if disable {
		flash = "gateway+disabled"
	}
	h.mutate(w, r, sess, user, "webaccess.gateway.status", gw.ID, back, flash, h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.SetGatewayStatus(cctx, gw.ID, disable)
	})
}

func (h *handlers) waGWACMEDir(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "webaccess.gateway.acmedir", gw.ID, back, "ACME+directory+saved", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.SetACMEDirectory(cctx, gw.ID, r.FormValue("url"))
	})
}

// waGWLimits stores the gateway's edge-limit overrides (blank/0 =
// default) and kicks a push — the wire already carries them.
func (h *handlers) waGWLimits(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	maxConns, err1 := formInt(r, "max_conns")
	perIP, err2 := formInt(r, "per_ip_per_min")
	maxBodyMiB, err3 := formInt(r, "max_body_mib")
	maxGitBodyMiB, err4 := formInt(r, "max_git_body_mib")
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		h.k.Respond(w, r, back, fmt.Errorf("limits must be whole numbers (blank or 0 = default)"), nil)
		return
	}
	h.mutate(w, r, sess, user, "webaccess.gateway.limits", gw.ID, back, "edge+limits+saved+—+they+push+within+seconds", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.SetEdgeLimits(cctx, gw.ID, maxConns, perIP, int64(maxBodyMiB)<<20, int64(maxGitBodyMiB)<<20)
	})
}

// formInt parses an optional numeric field ("" = 0).
func formInt(r *http.Request, name string) (int, error) {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return 0, nil
	}
	return strconv.Atoi(v)
}

// waGWRelayAdd stores one TCP relay (public edge port → target port on
// the PCP host) and kicks a push. The pushed list doubles as the tunnel
// worker's dial allowlist, so the mutation is the whole authorization.
func (h *handlers) waGWRelayAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	edge, err1 := formInt(r, "edge_port")
	target, err2 := formInt(r, "target_port")
	if err1 != nil || err2 != nil || edge < 1 || edge > 65535 || target < 1 || target > 65535 {
		h.k.Respond(w, r, back, fmt.Errorf("relay ports must be whole numbers 1–65535"), nil)
		return
	}
	detail := fmt.Sprintf("%s :%d → 127.0.0.1:%d", gw.ID, edge, target)
	h.mutate(w, r, sess, user, "webaccess.gateway.tcprelay.add", detail, back, "relay+added+—+it+pushes+within+seconds", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.AddTCPRelay(cctx, gw.ID, ferryproto.TCPRelay{
			EdgePort: uint16(edge), TargetPort: uint16(target),
			Label: strings.TrimSpace(r.FormValue("label")),
		})
	})
}

// waGWRelayRemove drops one relay by edge port; the next push closes
// the gateway listener and shrinks the allowlist.
func (h *handlers) waGWRelayRemove(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	edge, err := formInt(r, "edge_port")
	if err != nil || edge < 1 || edge > 65535 {
		h.k.Respond(w, r, back, fmt.Errorf("bad edge port"), nil)
		return
	}
	detail := fmt.Sprintf("%s :%d", gw.ID, edge)
	h.mutate(w, r, sess, user, "webaccess.gateway.tcprelay.remove", detail, back, "relay+removed", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.RemoveTCPRelay(cctx, gw.ID, uint16(edge))
	})
}

// waGWRepush clears the recorded push fingerprint so the next sync
// sweep re-pushes config AND serving certificates.
func (h *handlers) waGWRepush(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "webaccess.gateway.repush", gw.ID, back, "re-push+queued+—+lands+within+seconds", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.RecordPush(cctx, gw.ID, "", 0)
	})
}

func (h *handlers) waGWDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	gw, back, err := h.gwForm(r)
	if err != nil {
		h.k.Respond(w, r, back, err, nil)
		return
	}
	h.mutate(w, r, sess, user, "webaccess.gateway.delete", gw.ID+" "+gw.Name, "/admin/webaccess/gateways", "gateway+removed", nil, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		if err := h.k.Ferry.DeleteGateway(cctx, gw.ID); err != nil {
			return err
		}
		return h.k.System.DeleteSamples(cctx, gw.ID)
	})
}

// WAHostnamesPage is /admin/webaccess/hostnames.
type WAHostnamesPage struct {
	shell
	Hosts    []HostRow
	Gateways []dferry.Gateway
	TLSModes []string
}

func (h *handlers) waHostnames(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := WAHostnamesPage{shell: h.shell(r, "Hostnames & certificates", "wa-hostnames", sess, user), TLSModes: tlsModes}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	gws, err := h.k.Ferry.ListGateways(cctx)
	if err != nil {
		pg.Error = "couldn't load gateways"
	}
	pg.Gateways = gws
	names := map[string]string{}
	for _, gw := range gws {
		names[gw.ID] = gw.Name
	}
	hosts, err := h.k.Ferry.ListHosts(cctx)
	if err != nil {
		pg.Error = "couldn't load hostnames"
	}
	for _, host := range hosts {
		row := HostRow{Host: host, GatewayName: names[host.GatewayID]}
		if cert, found, _ := h.k.Ferry.GetCert(cctx, host.Hostname); found {
			row.CertExpiry, row.CertSource = cert.NotAfter, cert.Source
		}
		pg.Hosts = append(pg.Hosts, row)
	}
	h.render(w, "admin_wa_hostnames", pg)
}

func (h *handlers) waHostAdd(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	hostname := strings.ToLower(strings.TrimSpace(r.FormValue("hostname")))
	h.mutate(w, r, sess, user, "webaccess.host.add", hostname, "/admin/webaccess/hostnames", "hostname+added+—+point+its+DNS+at+the+gateway", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.PutHost(cctx, dferry.Host{
			Hostname:   hostname,
			GatewayID:  r.FormValue("gateway_id"),
			TLSMode:    r.FormValue("tls_mode"),
			ForceHTTPS: r.FormValue("force_https") == "on",
			By:         user.Username,
		})
	})
}

// waHostMode changes an existing hostname's TLS mode / force-HTTPS /
// gateway without re-typing it.
func (h *handlers) waHostMode(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	hostname := strings.ToLower(strings.TrimSpace(r.FormValue("hostname")))
	h.mutate(w, r, sess, user, "webaccess.host.mode", hostname+" → "+r.FormValue("tls_mode"), "/admin/webaccess/hostnames", "hostname+updated", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		host, found, err := h.k.Ferry.GetHost(cctx, hostname)
		if err != nil {
			return err
		}
		if !found {
			return users.ErrNotFound
		}
		if gw := r.FormValue("gateway_id"); gw != "" {
			host.GatewayID = gw
		}
		if mode := r.FormValue("tls_mode"); mode != "" {
			host.TLSMode = mode
		}
		host.ForceHTTPS = r.FormValue("force_https") == "on"
		return h.k.Ferry.PutHost(cctx, host)
	})
}

func (h *handlers) waHostDelete(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	hostname := r.FormValue("hostname")
	h.mutate(w, r, sess, user, "webaccess.host.delete", hostname, "/admin/webaccess/hostnames", "hostname+removed", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.DeleteHost(cctx, hostname)
	})
}

func (h *handlers) waCertUpload(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	hostname := strings.ToLower(strings.TrimSpace(r.FormValue("hostname")))
	h.mutate(w, r, sess, user, "webaccess.cert.upload", hostname, "/admin/webaccess/hostnames", "certificate+stored+—+it+pushes+on+the+next+sync", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		host, found, err := h.k.Ferry.GetHost(cctx, hostname)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("add the hostname first, then upload its certificate")
		}
		if host.TLSMode != ferryproto.TLSModeCustom {
			return fmt.Errorf("this hostname is in %q mode — switch it to custom first", host.TLSMode)
		}
		return h.k.Ferry.SetCert(cctx, dferry.HostCert{
			Hostname: hostname, Source: ferryproto.TLSModeCustom,
			CertPEM: r.FormValue("cert_pem"), KeyPEM: r.FormValue("key_pem"),
		})
	})
}

// WAOfflinePage is /admin/webaccess/offline: the editor plus a live
// preview iframe (srcdoc).
type WAOfflinePage struct {
	shell
	HTML string
}

func (h *handlers) waOffline(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	pg := WAOfflinePage{shell: h.shell(r, "Offline page", "wa-offline", sess, user)}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	var err error
	if pg.HTML, err = h.k.Ferry.OfflinePage(cctx); err != nil {
		pg.Error = "couldn't load the offline page"
	}
	h.render(w, "admin_wa_offline", pg)
}

func (h *handlers) waOfflineSave(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.mutate(w, r, sess, user, "webaccess.offline.save", "offlinepage", "/admin/webaccess/offline", "offline+page+saved", h.deps.KickFerry, func() error {
		cctx, cancel := kernel.Ctx(r)
		defer cancel()
		return h.k.Ferry.SetOfflinePage(cctx, r.FormValue("html"))
	})
}
