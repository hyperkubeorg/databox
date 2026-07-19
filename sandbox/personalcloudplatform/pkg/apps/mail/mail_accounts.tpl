{{/* mail_accounts.tpl — the account chooser (data: mail.AccountsPage):
     every mailbox with unread count and last activity, plus the
     self-service create form while the allowance lasts. Also the
     landing state for members with no mailbox yet. */}}
{{define "mail_accounts"}}{{template "top" .}}
<div class="msettings">
  <h1 class="dtitle">Email</h1>
  {{if .Accounts}}<p class="sub">Pick an address to open, or create another.</p>{{end}}
  {{if .Error}}<div class="banner error">{{.Error}}</div>{{end}}
  {{if .Flash}}<div class="banner flash">{{.Flash}}</div>{{end}}

  {{if .Accounts}}
  <section class="panel">
    <h3>Your addresses</h3>
    <table class="dtable">
      {{range .Accounts}}
      <tr>
        <td><a href="/mail?box={{.BoxID}}"><b>{{.Addr}}</b></a></td>
        <td>{{if .Unread}}<span class="unread-pill">{{.Unread}} unread</span>{{end}}</td>
        <td class="sub">{{if .LastActive}}last mail {{.LastActive}}{{else}}no mail yet{{end}}</td>
        <td class="rowacts"><a class="btn btn--ghost" href="/mail?box={{.BoxID}}">Open</a></td>
      </tr>
      {{end}}
    </table>
  </section>
  {{end}}

  {{if .CanCreate}}
  <section class="panel">
    <h3>New address</h3>
    <p class="sub">You can create {{.Remaining}} more address{{if ne .Remaining 1}}es{{end}} ({{.Used}} of {{.Allowance}} used).</p>
    {{template "mail_addr_form" (dict "CSRF" .Session.CSRF "Domains" .Domains "Back" "/mail?box=new")}}
  </section>
  {{else if not .Accounts}}
  <section class="panel">
    <h3>No email address yet</h3>
    {{if .Allowance}}
    <p class="sub">No mail domains are set up yet — ask your admin.</p>
    {{else}}
    <p class="sub">Your administrator hasn't granted you any email addresses yet. Ask them for an email allowance.</p>
    {{end}}
  </section>
  {{else if not .Remaining}}
  <p class="sub">You've used {{.Used}} of {{.Allowance}} address{{if ne .Allowance 1}}es{{end}} — ask your admin for more.</p>
  {{end}}
</div>
{{template "bottom" .}}{{end}}

{{/* mail_addr_form — the shared create form (chooser + settings).
     Args (dict): CSRF, Domains ([]mail.Domain, enabled), Back. */}}
{{define "mail_addr_form"}}
<form method="post" action="/mail/accounts/create" class="addrform">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <input type="hidden" name="back" value="{{.Back}}">
  <div class="addrrow">
    <input type="text" name="local" id="addrLocal" placeholder="you" maxlength="64" required autocomplete="off"
           pattern="[a-z0-9][a-z0-9._-]{0,63}" title="a-z, 0-9, dots, dashes, and underscores">
    <span class="addrat">@</span>
    <select name="domain" id="addrDomain" title="Mail domain" aria-label="Mail domain">
      {{range .Domains}}<option value="{{.Domain}}">{{.Domain}}</option>{{end}}
    </select>
    <button class="btn btn--primary" type="submit">Create address</button>
  </div>
  <p class="sub" id="addrAvail" aria-live="polite"></p>
</form>
<script>
(function(){
  var local=document.getElementById("addrLocal"),dom=document.getElementById("addrDomain"),out=document.getElementById("addrAvail");
  if(!local||!dom||!out)return;
  var t;
  function check(){
    var v=local.value.trim().toLowerCase();
    if(!v){out.textContent="";return;}
    fetch("/mail/api/addrcheck?local="+encodeURIComponent(v)+"&domain="+encodeURIComponent(dom.value),{headers:{"X-Requested-With":"fetch"}})
      .then(function(r){return r.json();})
      .then(function(j){
        if(!j.ok){out.textContent="";return;}
        out.textContent=j.available?v+"@"+dom.value+" is available":j.reason;
      })
      .catch(function(){out.textContent="";});
  }
  local.addEventListener("input",function(){clearTimeout(t);t=setTimeout(check,300);});
  dom.addEventListener("change",check);
})();
</script>
{{end}}
