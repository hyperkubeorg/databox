{{/* git_repo_blob.tpl — one file (data: git.BlobPage): monospace with
     line numbers in a copy-safe gutter, binary and too-large fallbacks,
     rendered markdown with a plain toggle. Syntax highlighting (§16,
     supersedes the §5.2 cut) is client-side over data-lang — the
     escaped text is the no-JS truth (repobottom wires it). Writers on a
     BRANCH head get Edit + Delete (the editor, §16); tag/sha views stay
     read-only with a hint. The file header sticks while the code
     scrolls. */}}
{{define "git_repo_blob"}}{{template "top" .}}
{{template "repotop" .}}

  <div class="metarow">
    <nav class="crumbs" aria-label="Path">
      <a href="/git/{{.RepoPath}}/tree/{{.Ref}}">{{.Repo.Name}}</a>{{range .Crumbs}}<span class="sep">/</span><a href="{{.Href}}">{{.Name}}</a>{{end}}
    </nav>
    <span class="chip">{{.Ref}}</span>
    {{if .HeadShort}}<a class="shachip" href="/git/{{.RepoPath}}/commit/{{.HeadSha}}" title="Browsing commit {{.HeadSha}}">{{template "gicon" "commit"}}{{.HeadShort}}</a>{{end}}
  </div>

  {{if .Binary}}
  <div class="gcard gcard--flush">
    <div class="filehead">
      <span class="crumbs"><span class="cur">{{.FileName}}</span></span>
      <span class="fsize">{{bytes .Size}}</span>
      <span class="spacer"></span>
      <a class="btn btn--ghost" href="{{.HistoryHref}}">{{template "gicon" "clock"}}History</a>
      <a class="btn btn--ghost" href="{{.RawHref}}">{{template "gicon" "download"}}Download</a>
      {{template "blobeditacts" .}}
    </div>
    <p class="gempty">Binary file, {{bytes .Size}} — <a href="{{.RawHref}}">download</a>.</p>
  </div>
  {{else if .TooLarge}}
  <div class="gcard gcard--flush">
    <div class="filehead">
      <span class="crumbs"><span class="cur">{{.FileName}}</span></span>
      <span class="fsize">{{bytes .Size}}</span>
      <span class="spacer"></span>
      <a class="btn btn--ghost" href="{{.HistoryHref}}">{{template "gicon" "clock"}}History</a>
      <a class="btn btn--ghost" href="{{.RawHref}}">{{template "gicon" "download"}}Raw</a>
      {{template "blobeditacts" .}}
    </div>
    <p class="gempty">This file is too large to display ({{bytes .Size}}) — <a href="{{.RawHref}}">view raw</a>.</p>
  </div>
  {{else}}
  <div class="gcard gcard--flush">
    <div class="filehead">
      <span class="crumbs"><span class="cur">{{.FileName}}</span></span>
      <span class="fsize">{{bytes .Size}}{{if .Lines}} · {{len .Lines}} lines{{end}}</span>
      <span class="spacer"></span>
      {{if .IsMD}}{{if .Plain}}<a class="btn btn--ghost" href="{{.BlobHref}}">Rendered</a>{{else}}<a class="btn btn--ghost" href="{{.BlobHref}}?plain=1">Plain</a>{{end}}{{end}}
      <a class="btn btn--ghost" href="{{.HistoryHref}}">History</a>
      {{if .BlameHref}}<a class="btn btn--ghost" href="{{.BlameHref}}">Blame</a>{{end}}
      <a class="btn btn--ghost" href="{{.RawHref}}">Raw</a>
      <a class="btn btn--ghost" href="{{.RawHref}}" download title="Download file" aria-label="Download file">{{template "gicon" "download"}}</a>
      {{if .CanEdit}}
      <a class="btn btn--ghost" href="{{.EditHref}}" title="Edit this file in the browser">{{template "gicon" "pencil"}}Edit</a>
      {{else if and .CanWrite (not .IsBranch)}}
      <span class="btn btn--ghost" style="opacity:.45;cursor:not-allowed" title="Switch to a branch to edit — tags and commits are read-only" aria-disabled="true">{{template "gicon" "pencil"}}Edit</span>
      {{end}}
      {{template "blobeditacts" .}}
    </div>
    {{if .MDHTML}}
    <div class="gitmd" style="padding:18px 22px">{{.MDHTML}}</div>
    {{else}}
    <div class="gitcode" style="padding:8px 0"{{if .Lang}} data-lang="{{.Lang}}"{{end}}><table>
      {{range .Lines}}<tr><td class="ln">{{.N}}</td><td class="c">{{.Text}}</td></tr>
      {{end}}
    </table></div>
    {{end}}
  </div>
  {{end}}

  {{if .CanEdit}}
  <dialog class="eddialog" id="delDialog">
    <h3>{{template "gicon" "trash"}} Delete {{.FileName}}?</h3>
    <p>This commits the deletion of <code>{{.Path}}</code> to <b>{{.Ref}}</b>. The file stays in history.</p>
    <form method="post" action="{{.DeleteAction}}">
      <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
      <input type="hidden" name="op" value="delete">
      <input type="hidden" name="base" value="{{.BaseSHA}}">
      <div class="ffield" style="margin-bottom:0">
        <label for="delmsg">Commit message</label>
        <input id="delmsg" name="message" value="Delete {{.FileName}}">
      </div>
      <div class="formacts">
        <button class="btn btn--ghost" type="button" onclick="document.getElementById('delDialog').close()">Cancel</button>
        <button class="btn btn--danger" type="submit">{{template "gicon" "trash"}}Delete file</button>
      </div>
    </form>
  </dialog>
  {{end}}

{{template "repobottom" .}}
{{template "bottom" .}}{{end}}

{{/* blobeditacts renders the Delete button (opens the confirm dialog)
     when the viewer may edit this blob (write + branch head). */}}
{{define "blobeditacts"}}{{if .CanEdit}}<button class="btn btn--danger" type="button" onclick="document.getElementById('delDialog').showModal()" title="Delete this file">{{template "gicon" "trash"}}Delete</button>{{end}}{{end}}