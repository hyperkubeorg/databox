{{/* admin_shell.tpl — the console scaffolding every page shares: the
     one-time style block and the left rail (§11.1 IA). Pages render as

       {{template "top" .}}{{template "admtop" .}}
         …page content…
       {{template "admbottom" .}}{{template "bottom" .}}

     Data contract: the page struct embeds admin.shell (Active +
     OpenProblems + Chrome). */}}

{{define "admtop"}}
<style>
.adm{display:flex;gap:26px;width:100%;max-width:1180px;margin:0 auto;padding:26px 20px 60px;align-items:flex-start}
.admrail{width:212px;flex:none;position:sticky;top:20px;display:flex;flex-direction:column;gap:2px}
.admrail h4{font-size:10.5px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:var(--text-faint);margin:14px 10px 4px}
.admrail a{display:flex;align-items:center;gap:8px;padding:6px 10px;border-radius:var(--r-sm);font-size:13px;color:var(--text-dim);text-decoration:none}
.admrail a:hover{background:var(--surface-2);color:var(--text);text-decoration:none}
.admrail a.is-on{background:var(--accent-tint);color:var(--text);font-weight:600}
.admmain{flex:1;min-width:0}
.admmain h1{font-family:var(--display);font-size:21px;font-weight:600;letter-spacing:-.01em;margin-bottom:6px}
.admmain .pagesub{margin:0 0 18px}
.tbl{width:100%;border-collapse:collapse;font-size:13px}
.tbl th{text-align:left;font-size:11px;font-weight:600;letter-spacing:.04em;text-transform:uppercase;color:var(--text-faint);padding:7px 10px;border-bottom:1px solid var(--border)}
.tbl td{padding:8px 10px;border-bottom:1px solid var(--border-soft);vertical-align:top}
.tbl tr:last-child td{border-bottom:none}
.tbl code{font-size:12px}
.dot{width:10px;height:10px;border-radius:50%;display:inline-block;flex:none}
.dot.ok{background:var(--good,#67c99a)}
.dot.warn{background:#e8b34b}
.dot.crit{background:var(--danger,#e8746b)}
.sev{font-size:11px;font-weight:700;letter-spacing:.04em;text-transform:uppercase;padding:2px 8px;border-radius:12px;flex:none}
.sev.info{background:var(--accent-tint);color:var(--text)}
.sev.warn{background:rgba(232,179,75,.16);color:#e8b34b}
.sev.critical{background:rgba(232,116,107,.16);color:var(--danger,#e8746b)}
.arearow{display:flex;align-items:center;gap:12px;padding:11px 4px;border-top:1px solid var(--border-soft)}
.arearow:first-child{border-top:none}
.arearow .name{width:110px;font-weight:600;font-size:13.5px}
.arearow .line{flex:1;color:var(--text-dim);font-size:13px}
.problem{display:flex;gap:12px;align-items:flex-start;padding:12px 4px;border-top:1px solid var(--border-soft)}
.problem:first-child{border-top:none}
.problem .body{flex:1;min-width:0}
.problem .summary{font-size:13.5px}
.problem .action{font-size:12.5px;color:var(--text-dim);margin-top:3px}
.problem .when{font-size:11.5px;color:var(--text-faint);white-space:nowrap}
.steps{display:flex;flex-direction:column;gap:0;counter-reset:step}
.step{display:flex;gap:14px;padding:14px 0;border-top:1px solid var(--border-soft)}
.step:first-child{border-top:none;padding-top:0}
.step__n{width:26px;height:26px;border-radius:50%;flex:none;display:grid;place-items:center;font-size:12.5px;font-weight:700;border:1px solid var(--border);color:var(--text-dim)}
.step.is-done .step__n{background:var(--good,#67c99a);border-color:transparent;color:var(--bg)}
.step.is-now .step__n{background:var(--accent);border-color:transparent;color:var(--on-accent)}
.step__body{flex:1;min-width:0}
.step__body h4{font-size:13.5px;font-weight:600;margin-bottom:4px}
.copyrow{display:flex;gap:8px;align-items:flex-start;margin:6px 0}
.copyrow code{flex:1;min-width:0;overflow-wrap:anywhere;font-size:12px;background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);padding:7px 9px;user-select:all}
.spark{color:var(--accent)}
.adminline{display:flex;gap:10px;align-items:baseline;flex-wrap:wrap}
@media(max-width:860px){.adm{flex-direction:column}.admrail{width:100%;position:static;flex-direction:row;flex-wrap:wrap}.admrail h4{width:100%}}
</style>
<div class="adm">
  <nav class="admrail" aria-label="Administration">
    <a href="/admin"{{if eq .Active "home"}} class="is-on"{{end}}>Home{{if gt .OpenProblems 0}} <span class="badge">{{.OpenProblems}}</span>{{end}}</a>
    <h4>Services</h4>
    <a href="/admin/services"{{if eq .Active "services"}} class="is-on"{{end}}>Features</a>
    <a href="/admin/build"{{if eq .Active "build"}} class="is-on"{{end}}>Builds</a>
    <a href="/admin/smarthome"{{if eq .Active "smarthome"}} class="is-on"{{end}}>Smart Home</a>
    <h4>People</h4>
    <a href="/admin/users"{{if eq .Active "users"}} class="is-on"{{end}}>Users</a>
    <a href="/admin/invites"{{if eq .Active "invites"}} class="is-on"{{end}}>Invites</a>
    <h4>Storage</h4>
    <a href="/admin/tiers"{{if eq .Active "tiers"}} class="is-on"{{end}}>Tiers &amp; quotas</a>
    <a href="/admin/usage"{{if eq .Active "usage"}} class="is-on"{{end}}>Usage</a>
    <a href="/admin/gitorgs"{{if eq .Active "gitorgs"}} class="is-on"{{end}}>Git organizations</a>
    <h4>Site</h4>
    <a href="/admin/site"{{if eq .Active "site"}} class="is-on"{{end}}>Branding &amp; signup</a>
    <a href="/admin/site/git"{{if eq .Active "site-git"}} class="is-on"{{end}}>Git Services</a>
    <h4>Mail</h4>
    <a href="/admin/mail/domains"{{if eq .Active "mail-domains"}} class="is-on"{{end}}>Domains</a>
    <a href="/admin/mail/postoffices"{{if eq .Active "mail-postoffices"}} class="is-on"{{end}}>Post offices</a>
    <a href="/admin/mail/addresses"{{if eq .Active "mail-addresses"}} class="is-on"{{end}}>Addresses</a>
    <a href="/admin/mail/aliases"{{if eq .Active "mail-aliases"}} class="is-on"{{end}}>Aliases</a>
    <a href="/admin/mail/distros"{{if eq .Active "mail-distros"}} class="is-on"{{end}}>Distribution lists</a>
    <a href="/admin/mail/welcome"{{if eq .Active "mail-welcome"}} class="is-on"{{end}}>Welcome messages</a>
    <a href="/admin/mail/sending"{{if eq .Active "mail-sending"}} class="is-on"{{end}}>Sending policy</a>
    <h4>Web access</h4>
    <a href="/admin/webaccess/gateways"{{if eq .Active "wa-gateways"}} class="is-on"{{end}}>Gateways</a>
    <a href="/admin/webaccess/hostnames"{{if eq .Active "wa-hostnames"}} class="is-on"{{end}}>Hostnames &amp; certificates</a>
    <a href="/admin/webaccess/offline"{{if eq .Active "wa-offline"}} class="is-on"{{end}}>Offline page</a>
    <h4>Security</h4>
    <a href="/admin/audit"{{if eq .Active "audit"}} class="is-on"{{end}}>Audit log</a>
    <a href="/admin/ipbans"{{if eq .Active "ipbans"}} class="is-on"{{end}}>IP bans</a>
    <h4>System</h4>
    <a href="/admin/system/workers"{{if eq .Active "sys-workers"}} class="is-on"{{end}}>Workers</a>
    <a href="/admin/system/databox"{{if eq .Active "sys-databox"}} class="is-on"{{end}}>Databox</a>
    <a href="/admin/system/problems"{{if eq .Active "sys-problems"}} class="is-on"{{end}}>Problems</a>
  </nav>
  <div class="admmain">
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}
{{end}}

{{define "admbottom"}}
  </div>
</div>
{{end}}

{{/* sevchip renders one severity chip (arg: severity string). */}}
{{define "sevchip"}}<span class="sev {{.}}">{{.}}</span>{{end}}
