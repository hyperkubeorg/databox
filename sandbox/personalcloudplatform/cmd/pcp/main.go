// Command pcp is the Personal Cloud Platform: a self-hosted consumer
// ecosystem — Drive, Email, Calendar, Video, and Music behind one
// launcher — built on databox as its only database and blob store. It is
// the flagship example application: apps live in pkg/apps, domain storage
// in pkg/domain, and the shared core (auth, sessions, routing, chrome,
// audit) in pkg/kernel.
//
// The app is completely stateless — every user, session, file byte, and
// audit entry lives in databox — so it scales by replica count and any
// instance can serve any request.
//
// Configuration is environment-only (12-factor, container-friendly):
//
//	LISTEN                        listen address            (default :8080)
//	DATABOX_ENDPOINT              cluster host:port         (default localhost:8443)
//	DATABOX_USER                  databox user for the app  (default pcp — a scoped
//	                              user; root works but logs a loud warning)
//	DATABOX_PASSWORD              that user's password
//	DATABOX_CA_FINGERPRINT        pin the cluster cert (recommended in prod)
//	DATABOX_REQUIRE_FINGERPRINT=1 refuse to start without a fingerprint
//	TRUST_PROXY_HEADERS=1         rate-limit by X-Forwarded-For's first IP
//	                              (ONLY behind a proxy that overwrites it)
//	INSECURE_COOKIES=1            allow non-Secure cookies (local plain-HTTP dev)
//	PCP_ADMIN                     username to promote to admin once it exists
//	                              (bootstrap poll — no chicken-and-egg admin)
//	PCP_SIGNUP_MODE               initial signup mode on a FRESH deploy
//	                              (open|invite|trusted-invite|admin-invite;
//	                              an admin's saved choice always wins)
//	PCP_DEFAULT_QUOTA             per-user quota in bytes when no tier or
//	                              override applies (default 10GiB; 0 = unlimited)
//	PCP_MAX_UPLOAD                max single request body in bytes (default 5GiB)
//	PCP_GIT_GC_DEBOUNCE           delay before the automatic repo GC runs after
//	                              a force-push/ref delete (Go duration,
//	                              default 30s; the smoke shortens it)
//	PCP_GIT_SSH_ADDR              git-over-SSH listen address (default ":4222";
//	                              set empty to disable the SSH transport)
//	PCP_BUILDWIRE_ADDR            build-runner control listen address
//	                              (default ":4223"; set empty to disable —
//	                              runners then cannot connect)
package main

import (
	"context"
	"crypto/x509"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"

	appadmin "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/admin"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/api"
	appbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/build"
	appcal "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/calendar"
	appcontacts "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/contacts"
	appdrive "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/drive"
	appgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/invitespage"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/launcher"
	appmail "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/mail"
	appmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/messenger"
	appmusic "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/music"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/notifications"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/settings"
	appsmarthome "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/smarthome"
	appvideo "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/apps/video"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dbuild "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/build"
	dcalendar "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/calendar"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/clusterview"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/collab"
	dcontacts "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/contacts"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/drives"
	dferry "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/ferry"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/invites"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/media"
	dmessenger "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/messenger"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/nodes"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/shares"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	dsmarthome "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/smarthome"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ferry"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/gitmaint"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/gitssh"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/health"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/mailer"
)

// env reads a variable with a default.
func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// envBytes reads an int64 env var with a default; junk keeps the default.
func envBytes(log *slog.Logger, name string, def int64) int64 {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		log.Warn("bad byte-count env value, using default", "name", name, "value", v, "default", def)
		return def
	}
	return n
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Connect to the databox cluster the way any external app would:
	// through pkg/client, with the cert pinned when a fingerprint is
	// configured, trust-on-first-use (logged loudly) otherwise.
	opts := client.Options{Endpoint: env("DATABOX_ENDPOINT", "localhost:8443")}
	if fp := os.Getenv("DATABOX_CA_FINGERPRINT"); fp != "" {
		opts.TrustFingerprints = []string{fp}
	} else {
		if os.Getenv("DATABOX_REQUIRE_FINGERPRINT") == "1" {
			log.Error("DATABOX_REQUIRE_FINGERPRINT=1 but DATABOX_CA_FINGERPRINT is unset — refusing to start without a pinned cluster certificate")
			os.Exit(1)
		}
		opts.OnUnknownCert = func(fp string, _ *x509.Certificate) bool {
			log.Warn("SECURITY: trusting UNVERIFIED databox certificate — any MITM is silently accepted; unsuitable for production. Set DATABOX_CA_FINGERPRINT (and DATABOX_REQUIRE_FINGERPRINT=1) to pin the cluster cert", "fingerprint", fp)
			return true
		}
	}
	db, err := client.New(opts)
	if err != nil {
		log.Error("databox client", "err", err)
		os.Exit(1)
	}
	// Retry the login until the cluster is reachable: in Kubernetes this
	// app often starts before the databox StatefulSet finishes electing.
	user, pass := env("DATABOX_USER", "pcp"), os.Getenv("DATABOX_PASSWORD")
	if user == "root" {
		log.Warn("SECURITY: running as databox root — the app holds full-cluster credentials. Create a scoped user with a grant on /pcp and set DATABOX_USER; see README")
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = db.Login(ctx, user, pass)
		cancel()
		if err == nil {
			break
		}
		log.Warn("databox not ready, retrying in 3s", "endpoint", opts.Endpoint, "err", err)
		time.Sleep(3 * time.Second)
	}
	log.Info("connected to databox", "endpoint", opts.Endpoint, "as", user)

	userStore := &users.Store{DB: db, SessionTTL: 24 * time.Hour}
	siteStore := &site.Store{DB: db}
	driveStore := &drives.Store{DB: db, Users: userStore}
	nodeStore := &nodes.Store{DB: db, Users: userStore}
	shareStore := &shares.Store{DB: db, Nodes: nodeStore, Drives: driveStore, Users: userStore}
	mediaStore := &media.Store{DB: db, Nodes: nodeStore, Drives: driveStore}
	collabStore := &collab.Store{DB: db, Nodes: nodeStore}
	notifyStore := &notify.Store{DB: db}
	systemStore := &system.Store{DB: db}
	defaultQuota := envBytes(log, "PCP_DEFAULT_QUOTA", 10<<30)
	mailStore := &mail.Store{DB: db, Users: userStore, Notify: notifyStore, DefaultQuota: defaultQuota}
	contactsStore := &dcontacts.Store{DB: db, Nodes: nodeStore, Drives: driveStore}
	messengerStore := &dmessenger.Store{DB: db, Users: userStore, Notify: notifyStore, Log: log}
	ferryStore := &dferry.Store{DB: db}
	buildStore := &dbuild.Store{DB: db, Log: log}
	gitStore := &dgit.Store{DB: db, Users: userStore, Notify: notifyStore, Mail: mailStore, Log: log}
	// The automatic repo-GC debounce (Git Services §6.5). Junk keeps the
	// domain's 30s default.
	if v := os.Getenv("PCP_GIT_GC_DEBOUNCE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			gitStore.GCDebounce = d
		} else {
			log.Warn("bad PCP_GIT_GC_DEBOUNCE, using default", "value", v)
		}
	}
	inviteStore := &invites.Store{DB: db}
	clusterStore := &clusterview.Store{DB: db}
	smarthomeStore := &dsmarthome.Store{DB: db, Notify: notifyStore, Users: userStore}
	// Contacts feed the compose typeahead through the SuggestRecipients
	// seam (mail sits below contacts in the domain layering).
	mailStore.Contacts = contactsStore.Match
	calendarStore := &dcalendar.Store{
		DB: db, Users: userStore, Drives: driveStore, Nodes: nodeStore,
		Collab: collabStore, Notify: notifyStore, Mail: mailStore, Log: log,
	}

	// Every signup births the member's personal drive in the SAME
	// transaction as the account (PCD parity). Wired here — the users
	// domain can't import drives without inverting the domain layering.
	userStore.OnSignup = func(tx *client.Tx, u *users.User) {
		id := kvx.NewID()
		drives.StagePersonalDrive(tx, id, u.Username)
		u.PersonalDrive = id
	}
	// Invite redemption joins the same signup transaction (phase 8) —
	// the invites domain sits above users, so the hook keeps the
	// boundary while the redemption still commits atomically.
	userStore.RedeemInvite = inviteStore.RedeemInTx
	// Signup checks the git namespace registry + reserved names in the
	// same uniqueness transaction (Git Services §3.1) — works while Git
	// Services is disabled, so an org name is never shadowed later.
	userStore.ReserveName = gitStore.CheckUsernameInTx

	// PCP_SIGNUP_MODE seeds the gate on a FRESH deploy only (an admin's
	// saved config always wins). Best-effort.
	if mode := os.Getenv("PCP_SIGNUP_MODE"); mode != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := siteStore.BootstrapSignupMode(ctx, mode); err != nil {
			log.Warn("PCP_SIGNUP_MODE bootstrap failed", "mode", mode, "err", err)
		}
		cancel()
	}

	workCtx, workCancel := context.WithCancel(context.Background())
	defer workCancel()

	// PCP_ADMIN bootstrap: the named member is promoted to admin as soon
	// as their account exists (users.BootstrapAdmin polls, so the env can
	// be set before the account signs up).
	if admin := os.Getenv("PCP_ADMIN"); admin != "" {
		go userStore.BootstrapAdmin(workCtx, log, admin)
	}

	k := &kernel.App{
		Users:             userStore,
		Site:              siteStore,
		APIKeys:           &apikeys.Store{DB: db},
		Nodes:             nodeStore,
		Drives:            driveStore,
		Shares:            shareStore,
		Media:             mediaStore,
		Collab:            collabStore,
		Mail:              mailStore,
		Calendar:          calendarStore,
		Contacts:          contactsStore,
		Msg:               messengerStore,
		Git:               gitStore,
		Ferry:             ferryStore,
		Build:             buildStore,
		SmartHome:         smarthomeStore,
		Notifs:            notifyStore,
		System:            systemStore,
		Invites:           inviteStore,
		Cluster:           clusterStore,
		Log:               log,
		SecureCookies:     os.Getenv("INSECURE_COOKIES") == "",
		TrustProxyHeaders: os.Getenv("TRUST_PROXY_HEADERS") == "1",
		MaxUpload:         envBytes(log, "PCP_MAX_UPLOAD", 5<<30),
		DefaultQuota:      defaultQuota,
		// The web layer advertises SSH clone URLs from this (clone box,
		// Git settings); the server itself starts below.
		GitSSHAddr: env("PCP_GIT_SSH_ADDR", ":4222"),
	}

	// Mail worker loops (intake/outbound/sync). Started unconditionally;
	// each sweep is gated on site.Config.Mail.Enabled, so flipping the
	// admin switch takes effect within one poll period with no restart.
	// Databox locks make each loop a cluster-wide singleton; every pass
	// records /pcp/system/loops/<name>. Built before the router so the
	// Email app and the Mail API can kick the outbound loop after sends.
	ml := mailer.New(mailStore, siteStore, systemStore, log)

	// The cloudferry worker (spec §10): config sync, per-gateway tunnel
	// dialer pools, and ACME/selfsigned certificate issuance. Built
	// before the router so the admin console can kick it and mount its
	// ACME challenge handler.
	fw := ferry.New(ferryStore, systemStore, log)

	// The health worker (spec §11.2): databox-lock singleton, 60s
	// cadence; reads gateway records + stored samples, raises/resolves
	// problems, notifies admins. Built before the router so the admin
	// console's "re-check now" can kick it.
	hw := health.New()
	hw.System, hw.Mail, hw.Ferry = systemStore, mailStore, ferryStore
	hw.Site, hw.Users, hw.Media = siteStore, userStore, mediaStore
	hw.Notify, hw.Cluster, hw.Log = notifyStore, clusterStore, log
	hw.DefaultQuota = defaultQuota

	// Every app registers EXPLICITLY here — no init() side effects, so
	// mount order is deterministic and a duplicate route pattern fails
	// the process at startup instead of silently winning last.
	router, err := k.Router(
		launcher.Mount(k),
		appdrive.Mount(k),
		appmail.Mount(k, ml.KickOutbound, ml.Kick),
		appcal.Mount(k, ml.KickOutbound),
		appcontacts.Mount(k),
		appvideo.Mount(k),
		appmusic.Mount(k),
		appmessenger.Mount(k),
		appgit.Mount(k),
		appbuild.Mount(k),
		appsmarthome.Mount(k),
		settings.Mount(k),
		notifications.Mount(k),
		invitespage.Mount(k),
		appadmin.Mount(k, appadmin.Deps{
			KickMail:   ml.Kick,
			KickFerry:  fw.Kick,
			PoolHealth: fw.PoolHealth,
			Recheck:    hw.Kick,
			Challenge:  fw.ChallengeHandler(),
		}),
		api.Mount(k, ml.KickOutbound),
	)
	if err != nil {
		log.Error("route registration", "err", err)
		os.Exit(1)
	}

	// Requests arriving through a cloudferry tunnel serve the SAME
	// router, tunnel-marked so the kernel trusts the gateway's
	// X-Forwarded-For/Proto for exactly those requests (tunnel.go).
	fw.TunnelHandler = kernel.MarkTunnel(router)

	// Audit retention runs as a worker, not a request-path piggyback.
	go k.RunAuditRetention(workCtx)

	// Smart Home maintenance (Draft 005 §6.3/§8): the retention sweep +
	// agent offline/online transitions every 30s, the orphan-footage
	// walk hourly. Gated on the feature flag each tick — flipping the
	// Services switch takes effect within one period, no restart.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		tick := 0
		for {
			select {
			case <-workCtx.Done():
				return
			case <-t.C:
			}
			cctx, cancel := context.WithTimeout(workCtx, 5*time.Minute)
			sc, err := siteStore.Get(cctx)
			if err == nil && sc.SmartHomeEnabled() {
				tick++
				err = smarthomeStore.RunSweep(cctx, tick%120 == 0)
				if err != nil {
					log.Warn("smarthome sweep failed", "err", err)
				}
				systemStore.RecordLoop(cctx, "smarthomesweep", err)
			}
			cancel()
		}
	}()

	go ml.RunSync(workCtx)
	go ml.RunIntake(workCtx)
	go ml.RunOutbound(workCtx)

	go fw.RunSync(workCtx)
	go fw.RunTunnels(workCtx)
	go fw.RunACME(workCtx)

	// Health checks (§11.2) and this replica's heartbeat (§11.3).
	go hw.Run(workCtx)
	go systemStore.RunReplicaHeartbeat(workCtx)

	// The media scan worker (spec §9): registry-watch + periodic sweep;
	// per-folder databox locks keep replicas from doubling the work;
	// records /pcp/system/loops/mediascan.
	scanner := &media.Scanner{Media: mediaStore, System: systemStore, Log: log}
	go scanner.Run(workCtx)

	// Git Services nightly maintenance (Draft 002 §6.5): the straggler
	// GC sweep behind the debounced post-push passes — databox-lock
	// singleton, gated on the Git toggle, records /pcp/system/loops/gitgc.
	go gitmaint.New(gitStore, siteStore, systemStore, log).Run(workCtx)

	// The Builds runtime (Draft 003 §6.2): the buildwire listener runners
	// dial, the dispatch/ingest loop, and the nightly cleanup worker. The
	// loops self-gate on the Builds master switch, so they no-op until an
	// admin enables Builds and a runner pairs. Empty PCP_BUILDWIRE_ADDR
	// disables the listener (dispatch still runs, harmlessly idle).
	buildwireAddr := env("PCP_BUILDWIRE_ADDR", ":4223")
	if br, err := newBuildRuntime(buildwireAddr, buildStore, gitStore, siteStore, log); err != nil {
		log.Error("build runtime", "err", err)
		os.Exit(1)
	} else {
		if br.Listener != nil {
			go func() {
				if err := br.Listener.Run(workCtx); err != nil {
					log.Error("buildwire listener", "err", err)
				}
			}()
		}
		go br.Dispatcher.RunDispatch(workCtx)
		go br.Cleaner.Run(workCtx)
	}

	// The git-over-SSH endpoint (pkg/gitssh): publickey-only auth against
	// the SSH-key registry, driving the same wire core as smart HTTP.
	// Empty PCP_GIT_SSH_ADDR disables it; the Git master switch (§2)
	// still gates every connection while it listens. Ties into the same
	// graceful shutdown as the HTTP server via workCtx.
	if k.GitSSHAddr != "" {
		gs := &gitssh.Server{
			Addr: k.GitSSHAddr, Git: gitStore, Site: siteStore,
			Users: userStore, Log: log, DefaultQuota: defaultQuota,
		}
		go func() {
			if err := gs.Run(workCtx); err != nil {
				log.Error("git ssh server", "err", err)
			}
		}()
	}

	// Timeouts bound how long one connection can hold a goroutine
	// (slowloris defense). Read/WriteTimeout are generous ON PURPOSE:
	// multi-gigabyte uploads and media streaming (phase 2+) legitimately
	// take long; request SIZE is capped by MaxBytesReader in handlers.
	// SSE streams clear both deadlines per connection (kernel.StartSSE).
	srv := &http.Server{
		Addr:              env("LISTEN", ":8080"),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Minute,
		WriteTimeout:      60 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	// Graceful shutdown on SIGTERM (what Kubernetes sends first).
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Info("pcp serving", "listen", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
