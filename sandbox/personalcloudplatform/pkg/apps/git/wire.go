// wire.go — the git smart-HTTP endpoints (§6.3), protocol v0:
//
//	GET  /git/{ns}/{repo}/info/refs?service=…   advertisement
//	POST /git/{ns}/{repo}/git-upload-pack       fetch/clone   (read)
//	POST /git/{ns}/{repo}/git-receive-pack      push          (write)
//
// Auth is git's Basic: username + an API-key token as the password,
// mapped onto apikeys.Verify — scope git:read for upload, git:write for
// receive — then RoleFor gates the repo (§4.3). Anonymous upload-pack
// is allowed only for public repos while the site allows public (§10's
// check ships now). Unauthorized anonymous requests answer 401 with a
// Basic challenge; authenticated-but-forbidden and nonexistent answer
// 404 — never 403, never confirming a private repo (§4.3).
//
// This file is the HTTP TRANSPORT only: auth mapping, the stateless
// request/response shape (probe flush-pkts, per-POST negotiation
// rounds, gzip bodies, Content-Length pre-charge), and status-code
// mapping. The protocol core — advertisement, upload sessions, and the
// whole receive-pack path (quota, caps, push lock, atomic ref updates,
// GC + MR-head hooks, report-status) — lives in pkg/gitwire, shared
// verbatim with the SSH transport (pkg/gitssh).
package git

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"

	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/apikeys"
	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/site"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/gitwire"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
)

// uploadReqMax bounds an upload-pack negotiation body — wants + haves
// lists, never pack data.
const uploadReqMax = 16 << 20

// core builds the shared wire-protocol core (pkg/gitwire) from the
// kernel wiring.
func (h *handlers) core() *gitwire.Core {
	return &gitwire.Core{Git: h.k.Git, Log: h.k.Log, DefaultQuota: h.k.DefaultQuota}
}

// wireErr routes protocol failures: status + a terse plain-text body
// (git prints it after "fatal: unable to access").
func wireErr(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}

// challenge is the anonymous-unauthorized answer (§6.3).
func challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="pcp-git"`)
	wireErr(w, http.StatusUnauthorized, "authentication required")
}

// wireGate re-checks the master switch (§2) — a disabled Git Services
// is indistinguishable from unbuilt on the wire too.
func (h *handlers) wireGate(w http.ResponseWriter, r *http.Request) (site.Config, bool) {
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		wireErr(w, http.StatusInternalServerError, "temporary failure")
		return site.Config{}, false
	}
	if !sc.GitEnabled() {
		http.NotFound(w, r)
		return site.Config{}, false
	}
	return sc, true
}

// wireAuth resolves git's Basic credential onto the apikeys system
// (§6.3). Returns the authenticated username, or "" for anonymous
// (no credential at all). ok=false means the response was written
// (401 challenge for any bad credential — a prober learns nothing).
func (h *handlers) wireAuth(ctx context.Context, w http.ResponseWriter, r *http.Request, scope string) (string, bool) {
	username, password, present := r.BasicAuth()
	if !present {
		return "", true // anonymous — the caller decides what that may do
	}
	key, valid, err := h.k.APIKeys.Verify(ctx, password)
	if err != nil {
		wireErr(w, http.StatusInternalServerError, "temporary failure")
		return "", false
	}
	if !valid || !strings.EqualFold(username, key.Owner) {
		challenge(w)
		return "", false
	}
	owner, found, err := h.k.Users.Get(ctx, key.Owner)
	if err != nil {
		wireErr(w, http.StatusInternalServerError, "temporary failure")
		return "", false
	}
	if !found || owner.Banned {
		challenge(w)
		return "", false
	}
	if !key.HasScope(scope) {
		// Authenticated but forbidden: 404, never 403 (§4.3/§6.3).
		http.NotFound(w, r)
		return "", false
	}
	return key.Owner, true
}

// wireRepo resolves {ns}/{repo} (trailing .git stripped) and gates it
// with RoleFor. When the site disallows public repos, public visibility
// counts for nothing (§2 forces all repos private). ok=false means the
// response was written: 401 for anonymous, 404 for authenticated.
func (h *handlers) wireRepo(ctx context.Context, w http.ResponseWriter, r *http.Request,
	sc site.Config, user string, need dgit.Role) (dgit.Repo, bool) {
	ns := r.PathValue("ns")
	name := strings.TrimSuffix(r.PathValue("repo"), ".git")
	deny := func() (dgit.Repo, bool) {
		if user == "" {
			challenge(w) // anonymous can't confirm anything (§6.3)
		} else {
			http.NotFound(w, r)
		}
		return dgit.Repo{}, false
	}
	repo, found, err := h.k.Git.GetRepoByPath(ctx, ns, name)
	if err != nil {
		wireErr(w, http.StatusInternalServerError, "temporary failure")
		return dgit.Repo{}, false
	}
	if !found {
		return deny()
	}
	gated := repo
	if !sc.GitPublicReposAllowed() {
		gated.Visibility = dgit.VisPrivate
	}
	role, err := h.k.Git.RoleFor(ctx, user, &gated)
	if err != nil {
		wireErr(w, http.StatusInternalServerError, "temporary failure")
		return dgit.Repo{}, false
	}
	if role < need {
		return deny()
	}
	return repo, true
}

// wireBody unwraps a gzip request body (git compresses large POSTs) and
// applies the §6.4 body cap.
func wireBody(w http.ResponseWriter, r *http.Request, limit int64) (io.Reader, error) {
	var body io.Reader = http.MaxBytesReader(w, r.Body, limit)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, err
		}
		return zr, nil
	}
	return body, nil
}

// flushWriter flushes the HTTP response after every write so pack data
// streams (§6.4 — smart HTTP needs flush-aware streaming).
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// --- GET /git/{ns}/{repo}/info/refs ---------------------------------------------

func (h *handlers) infoRefs(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.wireGate(w, r)
	if !ok {
		return
	}
	service := r.URL.Query().Get("service")
	var scope string
	var need dgit.Role
	switch service {
	case gitwire.UploadPack:
		scope, need = apikeys.ScopeGitRead, dgit.RoleRead
	case gitwire.ReceivePack:
		scope, need = apikeys.ScopeGitWrite, dgit.RoleWrite
	default:
		http.NotFound(w, r) // dumb HTTP is not served (§1)
		return
	}
	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	user, ok := h.wireAuth(cctx, w, r, scope)
	if !ok {
		return
	}
	if service == gitwire.ReceivePack && user == "" {
		challenge(w) // anonymous push can never be a thing
		return
	}
	if user == "" && !h.k.AllowAnon(r) {
		// The anonymous rate tier (§10) covers the wire too — keyed on
		// the ferry-forwarded real client IP (kernel.ClientIP).
		wireErr(w, http.StatusTooManyRequests, "too many requests — slow down")
		return
	}
	repo, ok := h.wireRepo(cctx, w, r, sc, user, need)
	if !ok {
		return
	}
	sto, err := h.k.Git.Storer(cctx, repo)
	if err != nil {
		wireErr(w, http.StatusInternalServerError, "temporary failure")
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	if err := gitwire.Advertise(cctx, sto, service, true, flushWriter{w}); err != nil {
		h.k.Log.Warn("git advertisement failed", "repo", repo.ID, "err", err)
	}
}

// --- POST /git/{ns}/{repo}/git-upload-pack --------------------------------------

func (h *handlers) uploadPack(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.wireGate(w, r)
	if !ok {
		return
	}
	// Clones stream for a while on big repos; use the request's own
	// context rather than the 15s app budget.
	cctx := r.Context()
	user, ok := h.wireAuth(cctx, w, r, apikeys.ScopeGitRead)
	if !ok {
		return
	}
	if user == "" && !h.k.AllowAnon(r) {
		wireErr(w, http.StatusTooManyRequests, "too many requests — slow down")
		return
	}
	repo, ok := h.wireRepo(cctx, w, r, sc, user, dgit.RoleRead)
	if !ok {
		return
	}
	body, err := wireBody(w, r, uploadReqMax)
	if err != nil {
		wireErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	// The same probe git sends before an over-postBuffer receive-pack
	// body applies to huge fetch negotiations: a lone flush-pkt asks
	// "may I?" — answer 200, say nothing.
	bufferedUp := bufio.NewReader(body)
	if head, err := bufferedUp.Peek(4); err == nil && string(head) == "0000" {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(http.StatusOK)
		return
	}
	upreq := packp.NewUploadPackRequest()
	if err := upreq.UploadRequest.Decode(bufferedUp); err != nil {
		wireErr(w, http.StatusBadRequest, "bad upload-pack request")
		return
	}
	// go-git's embedded decode stops at the want-list flush; the haves
	// and the terminating "done" are read here (protocol v0, one
	// stateless round per POST).
	haves, done, err := gitwire.ReadHaves(bufferedUp)
	if err != nil {
		wireErr(w, http.StatusBadRequest, "bad upload-pack request")
		return
	}
	sto, err := h.k.Git.Storer(cctx, repo)
	if err != nil {
		wireErr(w, http.StatusInternalServerError, "temporary failure")
		return
	}
	common := gitwire.CommonHaves(sto, haves)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	out := flushWriter{w}
	if !done {
		// Negotiation continues client-side: answer ACK/NAK, no pack.
		resp := packp.ServerResponse{}
		if len(common) > 0 {
			resp.ACKs = common[:1]
		}
		_ = resp.Encode(out, false)
		return
	}
	if err := gitwire.SendUploadPack(cctx, sto, upreq, common, out); err != nil {
		h.k.Log.Warn("git upload-pack stream failed", "repo", repo.ID, "err", err)
	}
}

// --- POST /git/{ns}/{repo}/git-receive-pack -------------------------------------

func (h *handlers) receivePack(w http.ResponseWriter, r *http.Request) {
	sc, ok := h.wireGate(w, r)
	if !ok {
		return
	}
	cctx := r.Context() // pushes may exceed the 15s app budget
	user, ok := h.wireAuth(cctx, w, r, apikeys.ScopeGitWrite)
	if !ok {
		return
	}
	if user == "" {
		challenge(w)
		return
	}
	repo, ok := h.wireRepo(cctx, w, r, sc, user, dgit.RoleWrite)
	if !ok {
		return
	}
	body, err := wireBody(w, r, sc.GitMaxBodyBytes())
	if err != nil {
		wireErr(w, http.StatusBadRequest, "bad request body")
		return
	}
	// git's PROBE request: a push larger than http.postBuffer first
	// sends a lone flush-pkt ("0000") to confirm auth before streaming
	// the real body chunked. A real update-request never begins with a
	// flush — answer 200 and say nothing.
	buffered := bufio.NewReader(body)
	if head, err := buffered.Peek(4); err == nil && string(head) == "0000" {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(http.StatusOK)
		return
	}
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(buffered); err != nil {
		wireErr(w, http.StatusBadRequest, "bad receive-pack request")
		return
	}
	// Pre-charge Content-Length when it's a plain body (an upper bound
	// of the wire bytes; the core reconciles). Gzip/chunked bodies make
	// the core charge incrementally as unpacked bytes accrue (§6.5).
	var preCharge int64
	if r.ContentLength > 0 && !strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		preCharge = r.ContentLength
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	err = h.core().Receive(cctx, gitwire.ReceiveOptions{
		SC: sc, Repo: repo, User: user, PreCharge: preCharge,
	}, req, flushWriter{w})
	if err == nil {
		return // report-status written (or nothing to do) — implicit 200
	}
	var capErr gitwire.CapabilityError
	switch {
	case errors.Is(err, gitwire.ErrRepoBusy):
		wireErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.As(err, &capErr):
		wireErr(w, http.StatusBadRequest, err.Error())
	default:
		wireErr(w, http.StatusInternalServerError, "temporary failure")
	}
}
