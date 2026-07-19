{{/* admin_mail_sending.tpl — Mail → Sending policy (data:
     admin.MailSendingPage). */}}
{{define "admin_mail_sending"}}{{template "top" .}}{{template "admtop" .}}
{{$m := .SC.Mail}}
<h1>Sending policy</h1>
<p class="pagesub">Every limit the site applies to mail. Blank fields fall back to the defaults shown beside them. To turn Email on or off, use <a href="/admin/services">Services</a>.</p>
<div class="panel">
  <form method="post" action="/admin/mail/sending/config">
    <input type="hidden" name="csrf" value="{{.Session.CSRF}}">
    <div class="ffield"><label>Email accounts per member (now: {{.EffMailboxes}})</label>
      <input type="number" name="default_mailboxes" value="{{if $m.DefaultMailboxes}}{{$m.DefaultMailboxes}}{{end}}" min="0" max="100" placeholder="blank = 1">
      <div class="hint">Blank gives every member one account. Raise it for more; a specific member can be zeroed on the user page.</div></div>
    <div class="ffield"><label>Aliases per member (now: {{.EffAliases}})</label>
      <input type="number" name="max_aliases" value="{{if $m.MaxAliases}}{{$m.MaxAliases}}{{end}}" min="0" max="100"></div>
    <div class="ffield"><label>Max message size (now: {{bytes .EffMsgBytes}})</label>
      <input type="text" name="max_msg_bytes" value="{{if $m.MaxMsgBytes}}{{$m.MaxMsgBytes}}{{end}}" placeholder="bytes"></div>
    <div class="ffield"><label>Per-member send rate (now: {{.EffPerDay}}/day, burst {{.EffBurst}})</label>
      <div class="adminline">
        <input type="number" name="send_per_day" value="{{if $m.SendPerDay}}{{$m.SendPerDay}}{{end}}" placeholder="per day" style="max-width:130px">
        <input type="number" name="send_burst" value="{{if $m.SendBurst}}{{$m.SendBurst}}{{end}}" placeholder="burst" style="max-width:130px">
      </div>
      <div class="hint">Caps outbound mail per member — the brake on a compromised account.</div></div>
    <div class="ffield"><label>Trash auto-purge (now: {{.EffTrashDays}} days)</label>
      <input type="number" name="trash_days" value="{{if $m.TrashDays}}{{$m.TrashDays}}{{end}}" min="0" max="3650"></div>
    <div class="ffield"><label>Spam thresholds (now: tag ≥ {{.EffSpamTag}}, refuse ≥ {{.EffSpamReject}})</label>
      <div class="adminline">
        <input type="text" name="spam_tag" value="{{if $m.SpamTag}}{{$m.SpamTag}}{{end}}" placeholder="tag score" style="max-width:130px">
        <input type="text" name="spam_reject" value="{{if $m.SpamReject}}{{$m.SpamReject}}{{end}}" placeholder="refuse score" style="max-width:130px">
      </div></div>
    <div class="ffield"><label>DNSBL zones (space-separated)</label>
      <input type="text" name="rbl_zones" value="{{range $i, $z := $m.RBLZones}}{{if $i}} {{end}}{{$z}}{{end}}" placeholder="zen.spamhaus.org"></div>
    <div class="ffield"><label>SpamAssassin (spamd) address</label>
      <input type="text" name="spamd_addr" value="{{$m.SpamdAddr}}" placeholder="host:783 — blank = no scoring"></div>
    <button class="btn btn--primary" type="submit">Save policy</button>
  </form>
</div>
{{template "admbottom" .}}{{template "bottom" .}}{{end}}
