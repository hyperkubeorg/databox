// editor.go — the in-service file editor (§16; supersedes the §5.2 v1
// read-only cut): GET /git/{ns}/{repo}/edit/{branch}/{path…} and
// /new/{branch}[/{dir…}] render a full-width Ace page (vendored,
// assets.go); the POSTs land real commits through the domain's
// WebCommit (webcommit.go) — write role required, BRANCH heads only
// (tags and bare shas 404, exactly like a bad URL), CSRF per house
// convention, bytes charged to the owning namespace like a push. A CAS
// conflict or quota rejection re-renders the editor with the user's
// content intact — nothing typed is ever lost to an error.
package git

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"

	dgit "github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/git"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/domain/users"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/kernel"
	"github.com/hyperkubeorg/databox/sandbox/personalcloudplatform/pkg/ui"
)

// EditorPage is the edit/new page's typed struct.
type EditorPage struct {
	repoShell
	IsNew  bool
	Branch string
	// BaseSHA is the branch head at render time — the CAS anchor the
	// POST carries back ("" only on the empty-repo first commit).
	BaseSHA string
	// OldPath is the file being edited ("" on new); Path is the editable
	// path field (rename = change it before committing).
	OldPath string
	Path    string
	Content string
	Message string
	// Action is the POST target; CancelHref goes back to the blob (edit)
	// or tree (new) view.
	Action     string
	CancelHref string
}

// editBranch resolves {rest…} into (branch, head, leftover path) and
// enforces the branches-only rule: the ref segment must name a LIVE
// branch. The empty repo's default branch counts (head zero) — that is
// the quick-setup "create a file in the browser" path.
func (h *handlers) editBranch(sto *dgit.RepoStorer, repo dgit.Repo, rest string) (branch, baseSHA, path string, ok bool) {
	ref, path, err := sto.SplitRefPath(rest)
	if err != nil || ref == "" {
		return "", "", "", false
	}
	for _, name := range branchNames(sto) {
		if name == ref {
			head, found, err := sto.ResolveRef(ref)
			if err != nil || !found {
				return "", "", "", false
			}
			return ref, head.String(), path, true
		}
	}
	// No live branch matched: only the empty repo's default branch may
	// proceed (first commit, no base).
	if empty, err := sto.IsEmpty(); err == nil && empty {
		if ref == repo.DefaultBranch || strings.HasPrefix(rest, repo.DefaultBranch+"/") {
			return repo.DefaultBranch, "", strings.Trim(strings.TrimPrefix(strings.Trim(rest, "/"), repo.DefaultBranch), "/"), true
		}
	}
	return "", "", "", false
}

// editPage renders the editor over an existing file.
func (h *handlers) editPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	branch, baseSHA, path, ok := h.editBranch(sto, repo, r.PathValue("rest"))
	if !ok || path == "" || baseSHA == "" {
		http.NotFound(w, r) // tags/shas/ghost branches are not editable (§16)
		return
	}
	f, found, err := sto.FileAt(plumbing.NewHash(baseSHA), path, dgit.MaxRenderFileBytes)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	repoPath := repo.OwnerNS + "/" + repo.Name
	if f.Binary || f.TooLarge {
		// Not editable in a browser — back to the blob view's fallbacks.
		http.Redirect(w, r, "/git/"+repoPath+"/blob/"+branch+"/"+path, http.StatusSeeOther)
		return
	}
	pg := EditorPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "code"),
		Branch:    branch, BaseSHA: baseSHA,
		OldPath: path, Path: path, Content: string(f.Content),
		Message:    "Update " + path[strings.LastIndex(path, "/")+1:],
		Action:     "/git/" + repoPath + "/edit/" + branch + "/" + path,
		CancelHref: "/git/" + repoPath + "/blob/" + branch + "/" + path,
	}
	ui.Render(w, h.views, "git_repo_edit", pg)
}

// newPage renders the editor over a fresh file ({rest…} = branch or
// branch/dir — the directory prefills the path field).
func (h *handlers) newPage(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	branch, baseSHA, dir, ok := h.editBranch(sto, repo, r.PathValue("rest"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	repoPath := repo.OwnerNS + "/" + repo.Name
	pg := EditorPage{
		repoShell: h.repoShell(r, sess, user, repo, role, "code"),
		IsNew:     true,
		Branch:    branch, BaseSHA: baseSHA,
		Action:     "/git/" + repoPath + "/new/" + branch,
		CancelHref: "/git/" + repoPath,
	}
	if dir != "" {
		pg.Path = dir + "/"
		pg.CancelHref = "/git/" + repoPath + "/tree/" + branch + "/" + dir
	}
	// A ?template= starts the new file from a canned example: filename
	// prefilled, content ready to edit. Templates live at the repo root, so
	// they override any directory prefix.
	if name, body, ok := fileTemplate(r.URL.Query().Get("template")); ok {
		pg.Path = name
		pg.Content = body
		pg.Message = "Add " + name
		pg.CancelHref = "/git/" + repoPath
	}
	ui.Render(w, h.views, "git_repo_edit", pg)
}

// fileTemplate maps a ?template= key to a starter filename + body. Unknown
// keys return ok=false (a plain blank new-file page).
func fileTemplate(key string) (name, body string, ok bool) {
	switch key {
	case "pcp-builder":
		return ".pcp-builder.yaml", pcpBuilderTemplate, true
	}
	return "", "", false
}

// pcpBuilderTemplate is a full, commented .pcp-builder.yaml starter (Builds
// Draft 003 §5) the user edits before committing.
const pcpBuilderTemplate = `# .pcp-builder.yaml — a PCP Builds pipeline.
# Phases run in containers on a paired runner; the pipeline wires them into a
# DAG. See the repo's Builds tab (or PROJECT-DRAFT-003 §5) for the full schema.

env:
  # Literal values, plus ${{SECRET}} references to secrets set in repo settings.
  GREETING: "hello from PCP Builds"
  # DB_PASSWORD: "${{DB_PASSWORD}}"

phases:
  build:
    image: golang:1.22
    # user: 1000                 # optional container run-as UID
    run:
      - step: compile
        command: go
        args: [build, -o, out/app, ./...]
        exitOnFailure: true       # default true; false keeps the phase going
    artifacts:
      - name: binary              # captured after this phase succeeds
        path: out/app

  test:
    image: golang:1.22
    inputs: [binary]             # the 'binary' artifact is mounted first
    run:
      - step: unit
        command: go
        args: [test, ./...]

pipeline:
  - phase: build
  - phase: test
    requiresPhase: build          # boolean over phase names: && || ! ( )

# Optional: promote artifacts to a release when a tag build succeeds.
# release:
#   when: tag
#   artifacts: [binary]
#   notesFrom: CHANGELOG.md
`

// editorCommit is the shared POST body for editPost/newPost: CSRF,
// build the WebCommitInput, land it, and route the outcome — success
// redirects (or answers JSON), a friendly domain rejection re-renders
// the editor with everything the user typed.
func (h *handlers) editorCommit(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User, isNew bool) {
	repo, role, sto, ok := h.openRepo(w, r, user, dgit.RoleWrite)
	if !ok {
		return
	}
	if !kernel.CheckCSRF(r, sess) {
		http.Error(w, "bad csrf token", http.StatusForbidden)
		return
	}
	branch, _, restPath, ok := h.editBranch(sto, repo, r.PathValue("rest"))
	if !ok {
		http.NotFound(w, r) // tag/sha/ghost-branch POSTs are bad URLs (§16)
		return
	}
	repoPath := repo.OwnerNS + "/" + repo.Name
	op := r.FormValue("op")
	in := dgit.WebCommitInput{
		Branch:  branch,
		BaseSHA: strings.TrimSpace(r.FormValue("base")),
		Message: r.FormValue("message"),
		Author:  user.Username,
		// Textareas post CRLF; commits store LF (the editor itself always
		// yields LF — this only levels the no-JS fallback).
		Content: []byte(strings.ReplaceAll(r.FormValue("content"), "\r\n", "\n")),
	}
	if !isNew {
		in.OldPath = restPath
	}
	if op == "delete" {
		in.NewPath = ""
	} else {
		in.NewPath = strings.Trim(strings.TrimSpace(r.FormValue("path")), "/")
	}

	cctx, cancel := kernel.Ctx(r)
	defer cancel()
	sc, err := h.k.Site.Get(cctx)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	limit, err := h.k.Git.NSQuotaLimit(cctx, sc, repo.OwnerNS, h.k.DefaultQuota)
	if err != nil {
		http.Error(w, "temporary failure — try again", http.StatusInternalServerError)
		return
	}
	in.QuotaLimit = limit

	commit, err := h.k.Git.WebCommit(cctx, sc, repo, in)
	if errors.Is(err, dgit.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if kernel.WantsJSON(r) || op == "delete" {
			// Deletes come from the blob page's confirm modal — bounce
			// back there with the error banner.
			h.k.Respond(w, r, "/git/"+repoPath+"/blob/"+branch+"/"+in.OldPath, err, nil)
			return
		}
		// Re-render the editor with the user's content intact (§16) —
		// conflict, quota, bad path, … are all recoverable in place.
		pg := EditorPage{
			repoShell: h.repoShell(r, sess, user, repo, role, "code"),
			IsNew:     isNew,
			Branch:    branch, BaseSHA: in.BaseSHA,
			OldPath: in.OldPath, Path: r.FormValue("path"),
			Content: r.FormValue("content"), Message: in.Message,
			Action:     r.URL.Path,
			CancelHref: "/git/" + repoPath,
		}
		if in.OldPath != "" {
			pg.CancelHref = "/git/" + repoPath + "/blob/" + branch + "/" + in.OldPath
		}
		pg.Error = kernel.UserErr(err)
		ui.Render(w, h.views, "git_repo_edit", pg)
		return
	}

	action, target, dest := "gitrepo.webcommit", in.NewPath, ""
	switch {
	case op == "delete":
		action, target = "gitrepo.webdelete", in.OldPath
		dir := ""
		if i := strings.LastIndex(in.OldPath, "/"); i >= 0 {
			dir = "/" + in.OldPath[:i]
		}
		dest = "/git/" + repoPath + "/tree/" + branch + dir + "?ok=" + url.QueryEscape(in.OldPath+" deleted")
	default:
		dest = "/git/" + repoPath + "/blob/" + branch + "/" + in.NewPath + "?ok=" + url.QueryEscape("committed to "+branch)
	}
	h.k.Audit(r, user, sess, action, repoPath+" "+target, "branch="+branch+" commit="+commit.String()[:8])
	h.k.Respond(w, r, dest, nil, map[string]any{
		"commit": commit.String(), "branch": branch, "path": in.NewPath,
	})
}

func (h *handlers) editPost(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.editorCommit(w, r, sess, user, false)
}

func (h *handlers) newPost(w http.ResponseWriter, r *http.Request, sess users.Session, user users.User) {
	h.editorCommit(w, r, sess, user, true)
}
