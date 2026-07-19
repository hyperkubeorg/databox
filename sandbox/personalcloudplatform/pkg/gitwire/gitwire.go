// Package gitwire is the transport-agnostic git wire-protocol core
// (Draft 002 §6.3–§6.5), factored out of the smart-HTTP handlers so the
// HTTP endpoints (pkg/apps/git/wire.go) and the SSH server (pkg/gitssh)
// drive ONE implementation:
//
//   - Advertise — the ref advertisement for either service (HTTP adds
//     its "# service=…" prefix, SSH doesn't);
//   - CommonHaves/ReadHaves/SendUploadPack — the protocol-v0 upload
//     negotiation pieces the stateless HTTP exchange composes, and
//     UploadPackInteractive — the full bidirectional loop SSH runs;
//   - ReceivePack — the whole push path: capability gate, the per-repo
//     push lock (GC never interleaves), §6.5 quota (pre-charge when the
//     transport knows an upper bound, incremental accrual otherwise,
//     reconcile after, refund on failure), §6.4 caps via the storer,
//     §6.2 atomic ref updates (which fire NoteRefUpdates → automatic
//     GC), the §9 MR head refresh, and the report-status encoding.
//
// Layering: this package sits with the other cross-app engines
// (gitmaint, mailer) — domain/git + site config only, no kernel, no
// sessions. Transports do auth/gating first and hand an authorized
// (repo, user) pair in.
package gitwire

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	gitstorer "github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gogitserver "github.com/go-git/go-git/v5/plumbing/transport/server"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
)

// Service names on the wire.
const (
	UploadPack  = "git-upload-pack"
	ReceivePack = "git-receive-pack"
)

// incrementalChargeStep batches the §6.5 accrual charges when the
// transport can't pre-charge an upper bound (chunked/gzip HTTP bodies,
// every SSH push).
const incrementalChargeStep = 8 << 20

// ErrRepoBusy is the push-lock rejection (HTTP: 503; SSH: stderr).
var ErrRepoBusy = errors.New("the repository is busy — try again")

// CapabilityError rejects a client capability the advertisement never
// offered. Returned before anything is written.
type CapabilityError struct{ Cap string }

func (e CapabilityError) Error() string { return "unsupported capability " + e.Cap }

// receiveCaps are the client capabilities ReceivePack honors — exactly
// what go-git's receive advertisement offers (§6.3).
var receiveCaps = map[capability.Capability]bool{
	capability.Agent:        true,
	capability.OFSDelta:     true,
	capability.DeleteRefs:   true,
	capability.ReportStatus: true,
}

// Core carries what the wire protocol needs — the git domain store, a
// logger, and the platform quota bootstrap. Both transports construct
// one from their own wiring.
type Core struct {
	Git          *dgit.Store
	Log          *slog.Logger
	DefaultQuota int64
}

// fixedLoader hands go-git's server the one storer this exchange is for.
type fixedLoader struct{ sto gitstorer.Storer }

func (l fixedLoader) Load(*transport.Endpoint) (gitstorer.Storer, error) { return l.sto, nil }

// Advertise writes the ref advertisement for service to w. httpPrefix
// adds smart HTTP's "# service=…" + flush preamble; SSH sends the bare
// advertisement.
func Advertise(ctx context.Context, sto *dgit.RepoStorer, service string, httpPrefix bool, w io.Writer) error {
	srv := gogitserver.NewServer(fixedLoader{sto})
	var ar *packp.AdvRefs
	var err error
	if service == UploadPack {
		var sess transport.UploadPackSession
		sess, err = srv.NewUploadPackSession(&transport.Endpoint{}, nil)
		if err == nil {
			ar, err = sess.AdvertisedReferencesContext(ctx)
		}
	} else {
		var sess transport.ReceivePackSession
		sess, err = srv.NewReceivePackSession(&transport.Endpoint{}, nil)
		if err == nil {
			ar, err = sess.AdvertisedReferencesContext(ctx)
		}
	}
	if err != nil {
		return err
	}
	if httpPrefix {
		ar.Prefix = [][]byte{[]byte("# service=" + service), pktline.Flush}
	}
	return ar.Encode(w)
}

// CommonHaves filters a client's haves to the ones this repo actually
// has — revlist would error on unknown hashes, and the pack must
// include everything else.
func CommonHaves(sto *dgit.RepoStorer, haves []plumbing.Hash) []plumbing.Hash {
	var common []plumbing.Hash
	for _, have := range haves {
		if sto.HasEncodedObject(have) == nil {
			common = append(common, have)
		}
	}
	return common
}

// ReadHaves reads the "have <hash>" pkt-lines and the "done" marker
// that follow an upload request's want-list flush — the STATELESS
// (HTTP) variant, which consumes the whole body.
func ReadHaves(r io.Reader) (haves []plumbing.Hash, done bool, err error) {
	s := pktline.NewScanner(r)
	for s.Scan() {
		line := strings.TrimSpace(string(s.Bytes()))
		switch {
		case line == "": // flush between have batches
		case line == "done":
			return haves, true, nil
		case strings.HasPrefix(line, "have "):
			h := plumbing.NewHash(strings.TrimPrefix(line, "have "))
			if h.IsZero() {
				return nil, false, fmt.Errorf("bad have line")
			}
			haves = append(haves, h)
		default:
			return nil, false, fmt.Errorf("unexpected pkt-line %q", line)
		}
	}
	return haves, false, s.Err()
}

// SendUploadPack runs go-git's upload-pack session for upreq — whose
// Haves the caller already filtered to common — and streams the
// response (final ACK/NAK + pack) to w.
func SendUploadPack(ctx context.Context, sto *dgit.RepoStorer, upreq *packp.UploadPackRequest, common []plumbing.Hash, w io.Writer) error {
	upreq.Haves = common
	srv := gogitserver.NewServer(fixedLoader{sto})
	sess, err := srv.NewUploadPackSession(&transport.Endpoint{}, nil)
	if err != nil {
		return err
	}
	resp, err := sess.UploadPack(ctx, upreq)
	if err != nil {
		return err
	}
	if len(common) > 0 {
		resp.ServerResponse.ACKs = common[len(common)-1:]
	}
	return resp.Encode(w)
}

// UploadPackInteractive drives one full bidirectional upload-pack
// exchange (the SSH shape — the caller already sent the advertisement):
// wants from r, then the protocol-v0 single-ack negotiation loop (per
// have-batch flush: the first common object ACKs once, otherwise NAK),
// and on "done" the final ACK/NAK + pack to w. A client that hangs up
// after the advertisement (ls-remote) returns nil.
func UploadPackInteractive(ctx context.Context, sto *dgit.RepoStorer, r io.Reader, w io.Writer) error {
	peeked, r2, err := peekFlush(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil // hung up after the advertisement
		}
		return err
	}
	if peeked {
		return nil // lone flush-pkt: ls-remote said goodbye
	}
	upreq := packp.NewUploadPackRequest()
	if err := upreq.UploadRequest.Decode(r2); err != nil {
		return fmt.Errorf("bad upload-pack request: %w", err)
	}
	var common []plumbing.Hash
	acked := false
	s := pktline.NewScanner(r2)
	var batch []plumbing.Hash
	for s.Scan() {
		line := strings.TrimSpace(string(s.Bytes()))
		switch {
		case strings.HasPrefix(line, "have "):
			h := plumbing.NewHash(strings.TrimPrefix(line, "have "))
			if h.IsZero() {
				return fmt.Errorf("bad have line")
			}
			batch = append(batch, h)
		case line == "": // flush: answer this batch
			common = append(common, CommonHaves(sto, batch)...)
			batch = nil
			resp := packp.ServerResponse{}
			if !acked && len(common) > 0 {
				resp.ACKs = common[:1] // single-ack: the FIRST common, once
				acked = true
			}
			if err := resp.Encode(w, false); err != nil {
				return err
			}
		case line == "done":
			common = append(common, CommonHaves(sto, batch)...)
			return SendUploadPack(ctx, sto, upreq, common, w)
		default:
			return fmt.Errorf("unexpected pkt-line %q", line)
		}
	}
	if err := s.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil // client hung up mid-negotiation
}

// peekFlush reports whether the stream begins with a lone flush-pkt
// ("0000") and returns a reader with nothing consumed otherwise.
func peekFlush(r io.Reader) (bool, io.Reader, error) {
	head := make([]byte, 4)
	n, err := io.ReadFull(r, head)
	if err != nil {
		if n == 0 {
			return false, r, io.EOF
		}
		return false, r, err
	}
	if string(head) == "0000" {
		return true, r, nil
	}
	return false, io.MultiReader(strings.NewReader(string(head)), r), nil
}

// ReceiveOptions parameterize one push.
type ReceiveOptions struct {
	SC   site.Config
	Repo dgit.Repo
	// User is the authenticated pusher (transports never allow anonymous
	// receive-pack).
	User string
	// PreCharge, when > 0, charges this §6.5 upper bound up front (HTTP
	// with a plain Content-Length); otherwise unpacked bytes accrue
	// incrementally.
	PreCharge int64
}

// Receive applies one already-decoded reference-update request: the
// §6.4/§6.5 push path shared by every transport. Contract: an error
// return means NOTHING was written to out (the transport renders it —
// HTTP status, SSH stderr); once the unpack starts, failures land in
// the report-status stream and Receive returns nil. A request with no
// commands writes nothing and returns nil.
func (c *Core) Receive(ctx context.Context, opts ReceiveOptions, req *packp.ReferenceUpdateRequest, out io.Writer) error {
	for _, cap := range req.Capabilities.All() {
		if !receiveCaps[cap] {
			return CapabilityError{Cap: string(cap)}
		}
	}
	if len(req.Commands) == 0 {
		return nil
	}
	repo := opts.Repo

	// §6.5 concurrent-push safety: the same lock GC holds, so a push and
	// a collection never interleave on one repo (domain gc.go).
	unlockPush, err := c.Git.LockRepoPush(ctx, repo.ID)
	if err != nil {
		return ErrRepoBusy
	}
	defer unlockPush()

	report := func(unpackErr error, cmdErr error) {
		writeReport(out, req, unpackErr, cmdErr)
	}

	// --- quota (§6.5): resolve the owning namespace's limit ------------------
	limit, err := c.Git.NSQuotaLimit(ctx, opts.SC, repo.OwnerNS, c.DefaultQuota)
	if err != nil {
		return fmt.Errorf("quota resolve: %w", err)
	}
	var charged int64
	charge := func(delta int64, enforce bool) error {
		lim := int64(0)
		if enforce {
			lim = limit
		}
		if err := c.Git.ChargeNSQuota(ctx, repo.OwnerNS, delta, lim); err != nil {
			return err
		}
		charged += delta
		return nil
	}
	refundAll := func() {
		if charged != 0 {
			// A refund can't fail on quota; storage errors here only cost
			// accounting accuracy, never data.
			if err := c.Git.ChargeNSQuota(ctx, repo.OwnerNS, -charged, 0); err != nil {
				c.Log.Warn("git push refund failed", "repo", repo.ID, "bytes", charged, "err", err)
			}
			charged = 0
		}
	}

	sto, err := c.Git.Storer(ctx, repo)
	if err != nil {
		return fmt.Errorf("storer: %w", err)
	}
	abort := func() {
		if err := sto.Abort(); err != nil {
			c.Log.Warn("git push abort sweep failed", "repo", repo.ID, "err", err)
		}
		refundAll()
	}

	// Pre-charge the transport's upper bound when it knows one (§6.5);
	// otherwise charge incrementally as unpacked bytes accrue.
	if opts.PreCharge > 0 {
		if err := charge(opts.PreCharge, true); err != nil {
			report(err, err)
			return nil
		}
	} else {
		var pending int64
		sto.OnStored = func(delta int64) error {
			pending += delta
			if pending >= incrementalChargeStep {
				step := pending
				pending = 0
				return charge(step, true)
			}
			return nil
		}
	}

	// --- unpack (§6.2: expand to loose through the storer) -------------------
	if req.Packfile != nil {
		if err := packfile.UpdateObjectStorage(sto, req.Packfile); err != nil {
			abort()
			report(err, err)
			return nil
		}
	}
	if err := sto.Flush(); err != nil {
		abort()
		report(err, err)
		return nil
	}

	// --- reconcile to actual stored bytes (§6.5) ------------------------------
	actual := sto.StoredBytes()
	if delta := actual - charged; delta > 0 {
		if err := charge(delta, true); err != nil {
			abort()
			report(err, err)
			return nil
		}
	} else if delta < 0 {
		if err := charge(delta, false); err != nil {
			c.Log.Warn("git push reconcile refund failed", "repo", repo.ID, "err", err)
		}
	}

	// --- atomic ref updates (§6.2) --------------------------------------------
	updates := make([]dgit.RefUpdate, 0, len(req.Commands))
	for _, cmd := range req.Commands {
		if !cmd.New.IsZero() {
			if err := sto.HasEncodedObject(cmd.New); err != nil {
				abort()
				report(nil, fmt.Errorf("missing objects for %s", cmd.Name))
				return nil
			}
		}
		updates = append(updates, dgit.RefUpdate{Name: string(cmd.Name), Old: cmd.Old, New: cmd.New})
	}
	if err := c.Git.ApplyRefUpdates(ctx, repo.ID, updates, actual); err != nil {
		abort()
		report(nil, err)
		return nil
	}
	// §9 head refresh: a branch move re-snapshots every open MR sourced
	// from it (head + activity re-file + author notification) — best
	// effort, after the push already succeeded.
	for _, u := range updates {
		if branch, isBranch := strings.CutPrefix(u.Name, "refs/heads/"); isBranch && !u.New.IsZero() && u.New != u.Old {
			c.Git.RefreshMRHeads(ctx, opts.SC, repo, branch, u.New.String(), opts.User)
		}
	}
	c.Log.Info("git push", "repo", repo.OwnerNS+"/"+repo.Name, "user", opts.User,
		"refs", len(updates), "bytes", actual)
	report(nil, nil)
	return nil
}

// writeReport encodes the report-status response when the client asked
// for it (stock git always does). unpackErr poisons the unpack line;
// cmdErr marks every command — the push is atomic (§6.2), so per-ref
// partial status can't happen.
func writeReport(w io.Writer, req *packp.ReferenceUpdateRequest, unpackErr, cmdErr error) {
	if !req.Capabilities.Supports(capability.ReportStatus) {
		return
	}
	rs := packp.NewReportStatus()
	rs.UnpackStatus = "ok"
	if unpackErr != nil {
		rs.UnpackStatus = Terse(unpackErr)
	}
	for _, cmd := range req.Commands {
		status := "ok"
		if cmdErr != nil {
			status = Terse(cmdErr)
		}
		rs.CommandStatuses = append(rs.CommandStatuses, &packp.CommandStatus{
			ReferenceName: cmd.Name, Status: status,
		})
	}
	_ = rs.Encode(w)
}

// Terse renders an error for the wire: quota and cap rejections keep
// their message; anything else collapses so internals never leak.
func Terse(err error) string {
	switch {
	case errors.Is(err, dgit.ErrQuotaExceeded):
		return "quota exceeded: not enough storage space left"
	case errors.Is(err, dgit.ErrObjectTooLarge), errors.Is(err, dgit.ErrTooManyObjects),
		errors.Is(err, dgit.ErrStale):
		return err.Error()
	default:
		return "push rejected"
	}
}
