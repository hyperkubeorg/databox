// Command smoke is the phase-3 live-smoke harness (throwaway; the
// admin UI takes over these setup steps in later phases). Against a
// REAL single-node databox it:
//
//  1. provisions ada + the example.test mail domain + mailbox through
//     the domain functions,
//  2. pairs a REAL postoffice binary (driving `postoffice setup` over
//     its stdin), starts it on loopback SMTP, and starts the REAL pcp
//     binary whose mailer loops do the work,
//  3. injects a 3-message References chain over raw SMTP and asserts
//     ONE thread, ascending messages, index position, unread rollup,
//  4. replays a delivery (no dupes), routes a spam-scored message (via
//     a fake spamd) to the Spam folder, exercises the quota-full DSN,
//  5. sends with the undo-send hold: cancels one, releases one to the
//     gateway and follows the submission → defer/bounce transitions,
//  6. checks the §11.3 loop records and the gateway's /v1/status
//     self-report.
//
// Usage:
//
//	smoke --databox 127.0.0.1:28443 \
//	      --postoffice-bin ./bin/postoffice --pcp-bin ./bin/pcp \
//	      --work /tmp/pcp-smoke
package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hyperkubeorg/databox/pkg/client"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/kvx"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/mail"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/notify"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/system"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/poclient"
)

var (
	failures int
	log      = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// children are killed on EVERY exit path (must/fatal included), so
	// a failed run never leaves a pcp/postoffice squatting on the ports.
	children []*exec.Cmd
)

var shuttingDown atomic.Bool

// pcpRestarting marks an INTENTIONAL pcp kill (phase 7's offline test)
// so the early-exit watchdog stays quiet.
var pcpRestarting atomic.Bool

func killChildren() {
	shuttingDown.Store(true)
	for _, c := range children {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	}
}

func exit(code int) {
	killChildren()
	os.Exit(code)
}

func pass(name string) { log.Info("PASS", "check", name) }
func fail(name string, args ...any) {
	failures++
	log.Error("FAIL: "+name, args...)
}

// until polls fn every 500ms until it returns true or the deadline
// passes.
func until(d time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func main() {
	databoxEP := flag.String("databox", "127.0.0.1:28443", "running databox endpoint (root, empty password)")
	poBin := flag.String("postoffice-bin", "./bin/postoffice", "postoffice binary")
	pcpBin := flag.String("pcp-bin", "./bin/pcp", "pcp binary")
	cfBin := flag.String("cloudferry-bin", "./bin/cloudferry", "cloudferry binary")
	work := flag.String("work", "/tmp/pcp-smoke", "scratch directory")
	gitOnly := flag.Bool("git-only", false, "run only the Git Services phase (no postoffice/cloudferry)")
	flag.Parse()
	ctx := context.Background()

	const (
		poCtl    = "127.0.0.1:18443"
		poSMTP   = "127.0.0.1:11587"
		spamdEP  = "127.0.0.1:17833"
		mailDom  = "example.test"
		extRcpt  = "friend@pcp-smoke-nonexistent.invalid"
		spamWord = "GAMBLE"
	)

	// --- connect as root -------------------------------------------------------
	db, err := client.New(client.Options{
		Endpoint:      *databoxEP,
		OnUnknownCert: func(string, *x509.Certificate) bool { return true },
	})
	must(err, "client")
	must(db.Login(ctx, "root", ""), "login")
	must(kvx.DeletePrefix(ctx, db, "/pcp/"), "wipe /pcp/")

	userStore := &users.Store{DB: db, SessionTTL: time.Hour}
	siteStore := &site.Store{DB: db}
	notifyStore := &notify.Store{DB: db}
	systemStore := &system.Store{DB: db}
	mailStore := &mail.Store{DB: db, Users: userStore, Notify: notifyStore, DefaultQuota: 10 << 30}

	// --git-only: seed ada, start pcp, run the Git Services phase, done.
	if *gitOnly {
		defer killChildren()
		runGitOnly(ctx, *pcpBin, *databoxEP, *work, db, userStore, siteStore)
		return // unreachable — runGitOnly exits
	}

	// --- fake spamd: scores GAMBLE bodies 7.0, everything else 0 ---------------
	go runFakeSpamd(spamdEP, spamWord)

	// --- provision site + user + domain + mailbox ------------------------------
	must(siteStore.Update(ctx, func(c *site.Config) error {
		// Every feature is off by default (Draft 004 §1); the smoke
		// enables the set it exercises. Git joins in phase 11.
		c.Drive.Enabled = true
		c.Calendar.Enabled = true
		c.Contacts.Enabled = true
		c.Video.Enabled = true
		c.Music.Enabled = true
		c.Messenger.Enabled = true
		c.Mail.Enabled = true
		c.Mail.SpamdAddr = spamdEP
		return nil
	}), "enable features")
	ada, err := userStore.CreateUser(ctx, "ada", "Ada", "password123")
	must(err, "create ada")
	_, err = mailStore.AddDomain(ctx, mailDom, "ada")
	must(err, "add domain")
	box, err := mailStore.CreateMailbox(ctx, "ada", mailDom, "ada", 5)
	must(err, "create mailbox")
	pass("provisioning: domain + DKIM + ada@" + mailDom)

	// --- pair the REAL postoffice binary ---------------------------------------
	po, setupBlob, err := mail.NewPendingPostOffice("smoke-po", "ada")
	must(err, "mint postoffice")
	must(kvx.SetJSON(ctx, db, "/pcp/mail/postoffices/"+po.ID, po), "store postoffice")
	poDir := filepath.Join(*work, "po-data")
	_ = os.RemoveAll(poDir)
	setup := exec.Command(*poBin, "setup", "--data-dir", poDir)
	setup.Stdin = strings.NewReader(setupBlob + "\n" + poCtl + "\n")
	out, err := setup.CombinedOutput()
	must(err, "postoffice setup: "+string(out))
	completion := ""
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "PCPPO2.") {
			completion = strings.TrimSpace(line)
		}
	}
	if completion == "" {
		must(fmt.Errorf("no completion blob in output:\n%s", out), "pairing")
	}
	po, err = mailStore.CompletePairing(ctx, po.ID, completion)
	must(err, "complete pairing")
	must(mailStore.SetPODomains(ctx, po.ID, map[string]int{mailDom: 10}), "po domains")
	pass("pairing: real `postoffice setup` handshake round-tripped")

	// --- start postoffice run + pcp --------------------------------------------
	// startPO/killPO are reusable — phase 8 restarts the gateway to
	// prove the DKIM re-push flow on the PO detail page.
	var poRun *exec.Cmd
	startPO := func() {
		poRun = exec.Command(*poBin, "run", "--data-dir", poDir, "--https-listen", poCtl, "--smtp-listen", poSMTP)
		poRun.Stdout, poRun.Stderr = os.Stderr, os.Stderr
		must(poRun.Start(), "postoffice run")
		children = append(children, poRun)
	}
	killPO := func() {
		if poRun != nil && poRun.Process != nil {
			_ = poRun.Process.Kill()
			_, _ = poRun.Process.Wait()
		}
	}
	startPO()
	defer killChildren()

	// startPCP is reusable — phase 7 kills and restarts the process to
	// prove offline-page + drift-recovery behavior. pcpRestarting
	// silences the early-exit watchdog for intentional kills.
	var pcpCmd *exec.Cmd
	startPCP := func() {
		pcp := exec.Command(*pcpBin)
		pcp.Env = append(os.Environ(),
			"LISTEN=127.0.0.1:18089",
			"DATABOX_ENDPOINT="+*databoxEP,
			"DATABOX_USER=root", "DATABOX_PASSWORD=",
			"INSECURE_COOKIES=1",
			"PCP_ADMIN=ada",          // phase 7 drives the admin Web Access page
			"PCP_GIT_GC_DEBOUNCE=3s", // phase 11 observes the automatic GC quickly
			"PCP_GIT_SSH_ADDR="+smokeSSHAddr,
		)
		pcp.Stdout, pcp.Stderr = os.Stderr, os.Stderr
		must(pcp.Start(), "pcp start")
		children = append(children, pcp)
		pcpCmd = pcp
		// A crashed child (port squat, bad env) must fail the smoke
		// loudly instead of timing out on every later assertion.
		go func() {
			_ = pcp.Wait()
			if !shuttingDown.Load() && !pcpRestarting.Load() {
				log.Error("fatal: pcp exited early")
				exit(1)
			}
		}()
	}
	killPCP := func() {
		pcpRestarting.Store(true)
		if pcpCmd != nil && pcpCmd.Process != nil {
			_ = pcpCmd.Process.Kill()
			_, _ = pcpCmd.Process.Wait()
		}
	}
	restartPCP := func() {
		startPCP()
		pcpRestarting.Store(false)
	}
	startPCP()

	// Sync loop reaches the gateway and pushes config (DKIM into RAM).
	if until(60*time.Second, func() bool {
		p, found, _ := mailStore.GetPostOffice(ctx, po.ID)
		return found && p.LastPushedSerial > 0 && !p.LastSeen.IsZero()
	}) {
		pass("sync loop paired + pushed config (serial recorded)")
	} else {
		fail("config push never recorded")
	}

	// --- 3-message References chain over raw SMTP ------------------------------
	send := func(from, subject, msgID string, refs []string, body string) {
		c, err := smtp.Dial(poSMTP)
		must(err, "smtp dial")
		must(c.Mail(from), "MAIL")
		must(c.Rcpt("ada@"+mailDom), "RCPT")
		w, err := c.Data()
		must(err, "DATA")
		var b strings.Builder
		fmt.Fprintf(&b, "From: %s\r\nTo: ada@%s\r\nSubject: %s\r\n", from, mailDom, subject)
		fmt.Fprintf(&b, "Date: %s\r\nMessage-ID: %s\r\n", time.Now().Format(time.RFC1123Z), msgID)
		if len(refs) > 0 {
			fmt.Fprintf(&b, "References: %s\r\nIn-Reply-To: %s\r\n", strings.Join(refs, " "), refs[len(refs)-1])
		}
		fmt.Fprintf(&b, "MIME-Version: 1.0\r\nContent-Type: text/plain\r\n\r\n%s\r\n", body)
		_, _ = w.Write([]byte(b.String()))
		must(w.Close(), "DATA close")
		_ = c.Quit()
		time.Sleep(1100 * time.Millisecond) // distinct Date seconds keep message order observable
	}
	send("bob@remote.example", "Trip plans", "<t1@remote.example>", nil, "first")
	send("bob@remote.example", "Re: Trip plans", "<t2@remote.example>", []string{"<t1@remote.example>"}, "second")
	send("carol@else.example", "Re: Trip plans", "<t3@else.example>", []string{"<t1@remote.example>", "<t2@remote.example>"}, "third")

	var tripThread mail.ThreadMeta
	if until(45*time.Second, func() bool {
		rows, _, _ := mailStore.ListThreads(ctx, "ada", box.ID, mail.FolderInbox, "", 10, 0)
		if len(rows) == 1 && rows[0].MsgCount == 3 {
			tripThread = rows[0]
			return true
		}
		return false
	}) {
		pass("intake: 3-message chain → ONE inbox thread with 3 messages")
	} else {
		rows, _, _ := mailStore.ListThreads(ctx, "ada", box.ID, mail.FolderInbox, "", 10, 0)
		fail("chain did not converge to one thread", "rows", fmt.Sprintf("%+v", rows))
	}
	if tripThread.UnreadCount == 3 && tripThread.Snippet == "third" {
		pass("thread meta: unread rollup = 3, snippet from latest")
	} else {
		fail("thread meta wrong", "unread", tripThread.UnreadCount, "snippet", tripThread.Snippet)
	}
	msgs, _ := mailStore.ListThreadMessages(ctx, "ada", box.ID, tripThread.ThreadID)
	if len(msgs) == 3 && msgs[0].Snippet == "first" && msgs[1].Snippet == "second" && msgs[2].Snippet == "third" {
		pass("messages ascend oldest→newest inside the thread")
	} else {
		got := make([]string, 0, len(msgs))
		for _, m := range msgs {
			got = append(got, m.Snippet)
		}
		fail("message order wrong", "got", got)
	}

	// --- duplicate redelivery: same deterministic id, no dupes ------------------
	if len(msgs) == 3 {
		raw, err := mailStore.MessageBlob(ctx, "ada", msgs[0].BlobID)
		must(err, "blob read")
		dupMeta := msgs[0]
		if err := mailStore.Deliver(ctx, mail.Delivery{
			User: "ada", BoxID: box.ID, Folder: mail.FolderInbox,
			Meta: dupMeta, Raw: raw,
		}); err != nil {
			fail("redelivery errored", "err", err)
		}
		th, _, _ := mailStore.GetThread(ctx, "ada", box.ID, tripThread.ThreadID)
		if th.MsgCount == 3 {
			pass("idempotent redelivery: duplicate spool replay wrote nothing")
		} else {
			fail("redelivery duplicated", "msgs", th.MsgCount)
		}
	}

	// --- spam-scored message lands in Spam --------------------------------------
	send("spammer@remote.example", "You won", "<sp1@remote.example>", nil, "click here to "+spamWord+" now")
	if until(45*time.Second, func() bool {
		rows, _, _ := mailStore.ListThreads(ctx, "ada", box.ID, mail.FolderSpam, "", 10, 0)
		return len(rows) == 1 && strings.Contains(rows[0].Subject, "You won")
	}) {
		pass("spam routing: scored message landed in the Spam folder")
	} else {
		fail("spam message never reached the spam folder")
	}

	// --- undo-send: hold honored, cancel restores --------------------------------
	sc, _ := siteStore.Get(ctx)
	ada, _, _ = userStore.Get(ctx, "ada")
	res1, err := mailStore.SendMessage(ctx, sc, ada, box, mail.ComposeInput{
		From: "ada@" + mailDom, To: []string{extRcpt},
		Subject: "Cancel me", Text: "never leaves",
	})
	must(err, "send #1")
	if _, om, found := mailStore.FindOutbound(ctx, res1.OutID); found && om.State == mail.OutHeld && time.Until(res1.HoldUntil) > 5*time.Second {
		pass("undo-send: row HELD with a ~10s window")
	} else {
		fail("hold not honored", "state", om.State)
	}
	if rows, _, _ := mailStore.ListSent(ctx, "ada", box.ID, "", 10); len(rows) != 0 {
		fail("held send already visible in Sent")
	}
	if in, err := mailStore.CancelOutbound(ctx, "ada", res1.OutID); err == nil && in.Subject == "Cancel me" {
		pass("undo-send: cancel returned the composed draft data")
	} else {
		fail("cancel failed", "err", err)
	}
	if _, _, found := mailStore.FindOutbound(ctx, res1.OutID); found {
		fail("cancelled row survived")
	}

	// --- send #2: release → submit to gateway → defer/bounce transitions ---------
	res2, err := mailStore.SendMessage(ctx, sc, ada, box, mail.ComposeInput{
		From: "ada@" + mailDom, To: []string{extRcpt},
		Subject: "Bounce me", Text: "off to a dead MX",
	})
	must(err, "send #2")
	submitted := until(60*time.Second, func() bool {
		_, om, found := mailStore.FindOutbound(ctx, res2.OutID)
		return found && om.State == mail.OutSubmitted
	})
	if submitted {
		pass("outbound: hold released and row SUBMITTED to the gateway")
	} else {
		_, om, _ := mailStore.FindOutbound(ctx, res2.OutID)
		fail("row never reached submitted", "state", om.State)
	}
	if rows, _, _ := mailStore.ListSent(ctx, "ada", box.ID, "", 10); len(rows) == 1 && rows[0].HasOutbound {
		pass("sent facet: released send appears in Sent (folder=" + rows[0].Folder + ")")
	} else {
		fail("sent facet missing after release")
	}
	// The gateway attempts MX delivery on its 30s tick; a .invalid
	// domain yields a permanent DNS failure (bounce) or, with no
	// resolver at all, a deferral. Accept either transition, assert it
	// was observed.
	pc, err := poclient.New(poclient.Pairing{
		Endpoint: po.Endpoint, TLSFingerprint: po.TLSFingerprint,
		ControlPriv: po.PCPControlPriv, POSealPub: po.POSealPub,
	})
	must(err, "poclient")
	outcome := ""
	until(120*time.Second, func() bool {
		if _, _, found := mailStore.FindOutbound(ctx, res2.OutID); !found {
			outcome = "bounced (row cleared by event)"
			return true
		}
		st, err := pc.Status(ctx)
		if err == nil && (st.Counters.Bounced > 0 || st.Counters.Deferred > 0) {
			outcome = fmt.Sprintf("gateway counters: bounced=%d deferred=%d", st.Counters.Bounced, st.Counters.Deferred)
			return true
		}
		return false
	})
	if outcome != "" {
		pass("delivery state transition observed: " + outcome)
	} else {
		fail("no defer/bounce transition within 120s")
	}
	if strings.HasPrefix(outcome, "bounced") {
		// The DSN materialized into ada's inbox and the Sent copy is flagged.
		if until(30*time.Second, func() bool {
			rows, _, _ := mailStore.ListThreads(ctx, "ada", box.ID, mail.FolderInbox, "", 10, 0)
			for _, r := range rows {
				if strings.Contains(r.Subject, "Undeliverable") {
					return true
				}
			}
			return false
		}) {
			pass("bounce DSN materialized in the inbox")
		} else {
			fail("bounce DSN never arrived")
		}
	}

	// --- quota-full bounce path ---------------------------------------------------
	must(db.RunTx(ctx, func(tx *client.Tx) error {
		return userStore.UpdateInTx(ctx, tx, "ada", func(u *users.User) error {
			u.QuotaOverride = 1 // one byte: everything is over quota
			return nil
		})
	}), "shrink quota")
	send("bob@remote.example", "Too big", "<big1@remote.example>", nil, "this will not fit")
	if until(45*time.Second, func() bool {
		foundDSN := false
		_ = mailStore.ScanOutbound(ctx, func(_ string, om mail.OutMsg) error {
			if om.BlobOf == mail.SystemMailAccount && len(om.RcptTo) == 1 && om.RcptTo[0] == "bob@remote.example" {
				foundDSN = true
			}
			return nil
		})
		return foundDSN
	}) {
		pass("quota-full: over-quota delivery bounced as a system DSN to the sender")
	} else {
		fail("quota DSN never queued")
	}
	must(db.RunTx(ctx, func(tx *client.Tx) error {
		return userStore.UpdateInTx(ctx, tx, "ada", func(u *users.User) error {
			u.QuotaOverride = 0
			return nil
		})
	}), "restore quota")

	// --- loop records + gateway self-report ---------------------------------------
	loops, err := systemStore.Loops(ctx)
	must(err, "loops read")
	for _, name := range []string{"mailsync", "mailintake", "mailoutbound"} {
		if rec, ok := loops[name]; ok && !rec.LastSuccess.IsZero() {
			pass("loop record: /pcp/system/loops/" + name)
		} else {
			fail("loop record missing", "loop", name)
		}
	}
	st, err := pc.Status(ctx)
	if err == nil && st.Version != "" && st.DKIMInRAM && st.Counters.Accepted >= 4 && st.SMTPListening {
		pass(fmt.Sprintf("gateway self-report: v%s dkim-in-ram accepted=%d spool=%d errors=%d",
			st.Version, st.Counters.Accepted, st.SpoolCount, len(st.LastErrors)))
	} else {
		fail("self-report incomplete", "err", err, "status", fmt.Sprintf("%+v", st))
	}

	// --- phase 4: the Email app + Mail API over HTTP -----------------------------
	phase4(ctx, "http://127.0.0.1:18089", poSMTP, mailDom,
		mailStore, userStore, &apikeys.Store{DB: db}, box)

	// --- phase 5: Calendar + Contacts + the ICS round-trip ------------------------
	phase5(ctx, "http://127.0.0.1:18089", poSMTP, mailDom, db,
		mailStore, userStore, &apikeys.Store{DB: db}, box)

	// --- phase 6: Video & Music --------------------------------------------------
	phase6(ctx, "http://127.0.0.1:18089", db, userStore, &apikeys.Store{DB: db})

	// --- phase 7: cloudferry web gateway ------------------------------------------
	killFerry, restartFerry := phase7(ctx, "http://127.0.0.1:18089", db, userStore, *cfBin, *work, box.ID, killPCP, restartPCP)

	// --- phase 8: admin console + observability ------------------------------------
	phase8(ctx, "http://127.0.0.1:18089", db, userStore, &apikeys.Store{DB: db},
		mailDom, po.ID, killFerry, restartFerry, killPO, startPO)

	// --- phase 9: Messenger (servers, channels, DMs, mentions, API) -----------------
	phase9messenger(ctx, "http://127.0.0.1:18089", userStore, &apikeys.Store{DB: db})

	// --- phase 10: security — TOTP 2FA login + API scope granularity ---------------
	phase10security(ctx, "http://127.0.0.1:18089", userStore, &apikeys.Store{DB: db})

	// --- phase 11: Git Services — clone/push, quota, fork, anonymous ---------------
	phase11git(ctx, "http://127.0.0.1:18089", db, userStore, siteStore, &apikeys.Store{DB: db}, *work)

	if failures > 0 {
		log.Error("SMOKE FAILED", "failures", failures)
		exit(1)
	}
	log.Info("SMOKE PASSED — all checks green")
	killChildren()
}

func must(err error, what string) {
	if err != nil {
		log.Error("fatal: "+what, "err", err)
		exit(1)
	}
}

// runFakeSpamd answers the SPAMC protocol: PING → PONG, CHECK → a
// score of 7.0 when the trigger word appears, else 0.1.
func runFakeSpamd(addr, trigger string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("fake spamd listen", "err", err)
		return
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			r := bufio.NewReader(c)
			line, _ := r.ReadString('\n')
			if strings.HasPrefix(line, "PING") {
				fmt.Fprint(c, "SPAMD/1.5 0 PONG\r\n")
				return
			}
			// Read the rest (headers + body).
			var body strings.Builder
			buf := make([]byte, 4096)
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			for {
				n, err := r.Read(buf)
				body.Write(buf[:n])
				if err != nil {
					break
				}
			}
			score := "0.1"
			if strings.Contains(body.String(), trigger) {
				score = "7.0"
			}
			fmt.Fprintf(c, "SPAMD/1.1 0 EX_OK\r\nSpam: True ; %s / 5.0\r\n\r\n", score)
		}(conn)
	}
}
