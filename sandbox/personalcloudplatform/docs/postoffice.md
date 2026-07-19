# postoffice — the PCP mail gateway

`postoffice` is Personal Cloud Platform's public face for email. PCP
runs on home hardware behind NAT and can't speak SMTP to the internet;
`postoffice` is a small container you run on a cloud host with
mail-capable networking. It receives mail, holds it encrypted until PCP
collects it, and relays PCP's outbound mail — all configured entirely
from the PCP admin console.

## The trust model in one paragraph

PCP is the authority. It mints every key, and after a one-time pairing
handshake it is the only side that ever opens the connection —
`postoffice` never dials PCP and never learns where PCP lives. Every
accepted message is sealed to a PCP-owned key **before it touches
disk**, so a stolen gateway disk yields only ciphertext and salted
address hashes; only PCP can decrypt mail at rest. DKIM signing keys and
the recipient list are pushed at runtime and held in RAM, so a restart
reveals nothing and simply waits for PCP to re-push.

## Install

The gateway ships as a container —
`ghcr.io/hyperkubeorg/postoffice` (multi-arch, amd64/arm64) — so the
cloud host needs only podman or docker (`docker` substitutes for
`podman` 1:1 below). Pin a date-stamp tag
(`YYYYMMDD.HHMMSS`) in production; the examples use `latest`. Run it
rootful: the host-side bind of port 25 is privileged.

Everything the gateway keeps lives in one data dir — a named volume
below, shared by the setup and run containers.

## Pair it

1. In PCP: **Admin → Mail → Post offices → Add**. Copy the setup code.
2. On the cloud host:
   ```
   podman run --rm -it -v postoffice:/var/lib/postoffice \
     ghcr.io/hyperkubeorg/postoffice:latest \
     setup --data-dir /var/lib/postoffice
   ```
   Paste the setup code, then enter the **public address** this gateway
   answers at — the hostname or public IP the outside world reaches it
   on, e.g. `mail.example.com:8443`. This is **not** a bind address:
   don't enter `0.0.0.0` (that's what `--https-listen` binds to at run
   time; it isn't something anything can dial). setup prints a
   completion code.
3. Paste the completion code back into the PCP console. The gateway is
   now paired and live.

Runtime authentication is mutual: PCP verifies the gateway by the exact
TLS certificate fingerprint captured at pairing (stronger than CA
validation), and the gateway verifies PCP because every request is
signed by the pairing's control key, with replay protection.

## Run it

```
podman run -d --name postoffice \
  -v postoffice:/var/lib/postoffice \
  -p 25:25 -p 8443:8443 \
  ghcr.io/hyperkubeorg/postoffice:latest
```

That's the entire local surface (the image's default command is
`run --data-dir /var/lib/postoffice`, serving SMTP on `:25` and the
PCP-facing HTTPS control port on `:8443`). Domains, recipients, DKIM
keys, spam policy, and every limit arrive from PCP's config push and are
cached on disk so the gateway keeps accepting and correctly rejecting
mail while PCP is offline. Open 25 and 8443 in the instance firewall.

### systemd (Quadlet)

To survive reboots, make the container a service —
`/etc/containers/systemd/postoffice.container` (with docker, add
`--restart unless-stopped` to the `docker run` instead):

```ini
[Unit]
Description=PCP postoffice mail gateway
After=network-online.target
Wants=network-online.target

[Container]
Image=ghcr.io/hyperkubeorg/postoffice:YYYYMMDD.HHMMSS
ContainerName=postoffice
Volume=postoffice:/var/lib/postoffice
PublishPort=25:25
PublishPort=8443:8443

[Service]
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## Observability

`GET /v1/status` (signed, PCP-only) is the gateway's self-report:
version, uptime, applied config serial, DKIM key freshness (in RAM
since the last restart?), spool depth (count + bytes), counters
(accepted / rejected-by-RBL / delivered / deferred / bounced), and a
RAM ring of the last 50 operational errors. PCP's sync loop polls it —
that's what the per-gateway page under **Admin → Mail → Post offices**
renders: pairing state, drift (a restarted gateway reports a stale
serial and lost keys, and gets an immediate re-push), queue and
throughput sparklines from the poll history, and the error ring.
Reachability, drift, key freshness, and queue growth also feed the
health worker: failures surface as plain-language problems on Admin
Home and notify every admin (see `docs/admin.md`).

## DNS

Each mail domain's record sheet is generated for you in **Admin → Mail
→ Domains → (domain)** — a wizard with copy buttons and live
"verified ✓" checks against real DNS, so you know each record landed.
You publish:

- **MX** — one per serving gateway, pointing at its host.
- **SPF** (`TXT`) — `v=spf1 a:<gateway-host> -all`, listing every
  gateway.
- **DKIM** (`TXT` at `pcp._domainkey.<domain>`) — the public key PCP
  generated.
- **DMARC** (`TXT` at `_dmarc.<domain>`) — a starter
  `p=quarantine` policy.
- **PTR** — set the reverse DNS of each gateway's IP to its hostname in
  your cloud provider's panel. Receivers reject mail from hosts whose
  PTR doesn't match; this one isn't in the zone file.

Deliverability lives or dies on this sheet. Without MX nothing arrives;
without SPF/DKIM/DMARC/PTR your outbound mail lands in spam.

## TLS

Self-signed by default everywhere — the PCP↔gateway channel is pinned
(so CA validation would add nothing), and inter-MTA SMTP uses
opportunistic STARTTLS without validation, which is the norm. Let's
Encrypt is not required; it is a possible future nicety, not a
dependency.

## Spam & policy

Built in, no setup: DNSBL/RBL checks at connect, SPF/DKIM/DMARC
verification with an `Authentication-Results` header, and DMARC
`p=reject` enforcement. Optional: point the **spamd endpoint** field at
a running SpamAssassin (`spamd`) — the status report health-checks it,
and messages are scored, tagged to the Spam folder at the tag
threshold, and refused at the reject threshold.

## Advanced: Postfix (or anything) in front

Because `postoffice` speaks ordinary SMTP on `:25`, you can put a
full-featured MTA in front of it for filtering or relaying. Point
Postfix's `relayhost` (or a transport map) at the gateway and it needs
nothing special from us — `postoffice` is just another SMTP hop.

## What the data dir holds

Only the gateway's own identity keys, its TLS keypair, PCP's *public*
keys, the salted recipient hashes, and sealed spool files. No message
plaintext, no plaintext addresses, no DKIM keys. That's the guarantee,
and it's covered by the trust-boundary test suite
(`pkg/postoffice/trustboundary_test.go`).
