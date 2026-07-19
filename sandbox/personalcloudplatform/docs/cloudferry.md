# cloudferry — the PCP web gateway

*postoffice carries the mail; cloudferry carries the visitors.* PCP runs
on home hardware behind NAT and can't accept inbound connections;
`cloudferry` is a small container you run on any public cloud
instance to make your PCP reachable at real hostnames with real TLS —
while your PCP network accepts **zero** inbound connections. All of it
is configured from the PCP admin console (**Admin → Web access**).

## The trust model in one paragraph

PCP is the authority. It mints every key, and after a one-time pairing
handshake it is the only side that ever opens a connection — cloudferry
never dials PCP and never learns where PCP lives. **One cloudferry, one
PCP, for life**: a paired gateway rejects any other control identity,
and re-pairing requires wiping its data dir. Config is pushed sealed;
serving-certificate private keys are pushed sealed and held **in RAM
only** — the gateway's disk keeps just its own identity, PCP's public
keys, the keyless config cache (hostnames, TLS modes, limits), and the
offline page.

## Honesty about the blindness boundary

At rest, blind; in RAM, transient plaintext. cloudferry terminates
public TLS, so request/response bytes exist in its memory while they
transit — exactly as sealed mail transits postoffice RAM before
sealing. A stolen disk yields no cert keys and no traffic. Sessions,
CSRF, and all authorization stay on the PCP; the gateway only adds
`X-Forwarded-For/Proto`, which the PCP kernel trusts solely on
tunnel-served requests.

The corollary trade-off: after a gateway **restart while the PCP is
offline**, there are no cert keys to serve HTTPS with — port 443 can
complete a TLS handshake only under a fallback self-signed cert (browser
warning) and answers the offline page; port 80 serves the offline page
cleanly. The moment the PCP reconnects, the drift check re-pushes
config and certificates and everything heals without operator action.

## How traffic flows

- **PCP dials out**: a pool (default 4 per replica) of persistent TLS
  connections to the gateway's tunnel port, each authenticated by a
  signed hello and multiplexed with yamux.
- Each public request becomes **one stream** over the pool
  (round-robin), carrying one HTTP exchange. SSE streams flush through
  as they happen; WebSocket 101 upgrades become a raw byte relay.
- Port 80: ACME challenge paths always tunnel through; force-HTTPS
  hostnames get a 301; everything else tunnels as-is.
- Port 443: SNI picks the hostname's pushed certificate from RAM;
  unknown hostnames get 421 under the fallback cert.
- Configured **TCP relay** ports: each accepted connection becomes one
  stream carrying raw bytes to a fixed local port on the PCP host (see
  below).
- No tunnel connected → the offline page, `503` + `Retry-After` (raw
  TCP relay ports just accept-and-close — there is no offline page in
  a byte stream).

## Install

The gateway ships as a container —
`ghcr.io/hyperkubeorg/cloudferry` (multi-arch, amd64/arm64) — so the
cloud host needs only podman or docker (`docker` substitutes for
`podman` 1:1 below). Pin a date-stamp tag (`YYYYMMDD.HHMMSS`) in
production; the examples use `latest`. Run it rootful: the host-side
binds of ports 80/443 are privileged.

Everything the gateway keeps lives in one data dir — a named volume
below, shared by the setup and run containers.

## Pair it

1. In PCP: **Admin → Web access → Add a gateway**. Copy the setup code.
2. On the cloud host:
   ```
   podman run --rm -it -v cloudferry:/var/lib/cloudferry \
     ghcr.io/hyperkubeorg/cloudferry:latest \
     setup --data-dir /var/lib/cloudferry
   ```
   Paste the setup code, confirm the **public host** (never a bind
   address like 0.0.0.0) and the control/tunnel ports. setup prints a
   completion code.
3. Paste the completion code back into the admin console. Done — the
   sync loop pushes config within seconds and the tunnel pool connects.

The gateway's admin page is a wizard for the rest: add a hostname and
its TLS mode, run the live DNS A/AAAA check, and watch the
first-request probe confirm visitors get through.

Runtime authentication is mutual: PCP verifies the gateway by the exact
TLS fingerprint captured at pairing; the gateway verifies PCP because
every control request AND every tunnel hello is signed by the pairing's
control key, with replay protection. To re-pair (or move the gateway to
another PCP): stop it, delete the data dir, run setup again.

## Run it

```
podman run -d --name cloudferry \
  -v cloudferry:/var/lib/cloudferry \
  -p 80:80 -p 443:443 -p 7443:7443 -p 7444:7444 \
  ghcr.io/hyperkubeorg/cloudferry:latest
```

That's the entire local surface (the image's default command is
`run --data-dir /var/lib/cloudferry`, listening on `:80`/`:443` public,
`:7443` tunnel, `:7444` control). Hostnames, TLS modes, certificates,
edge limits, TCP relays, the offline page — all arrive from PCP's
config push and
(minus keys) are cached on disk so routing and the offline page survive
restarts. Edge limits (max concurrent connections, per-IP requests/min,
max request body, max git push/fetch body — the latter applies to
`/git/…/git-upload-pack` and `…/git-receive-pack` POSTs instead of the
general cap) are tuned per gateway on its admin page; changes apply
live except header/idle timeouts, which are read at listener start
(restart to apply).

### systemd (Quadlet)

To survive reboots, make the container a service —
`/etc/containers/systemd/cloudferry.container` (with docker, add
`--restart unless-stopped` to the `docker run` instead):

```ini
[Unit]
Description=PCP cloudferry web gateway
After=network-online.target
Wants=network-online.target

[Container]
Image=ghcr.io/hyperkubeorg/cloudferry:YYYYMMDD.HHMMSS
ContainerName=cloudferry
Volume=cloudferry:/var/lib/cloudferry
PublishPort=80:80
PublishPort=443:443
PublishPort=7443:7443
PublishPort=7444:7444

[Service]
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Open 80, 443, 7443, and 7444 in the instance firewall. DNS: point each
hostname's A/AAAA record at this instance.

## TCP relays

Beyond HTTP, a gateway can relay whole TCP ports: each relay maps a
**public edge port** to a **target port on the PCP host** — the gateway
listens, and every accepted connection becomes one tunnel stream that
the PCP side splices to `127.0.0.1:<target>`. Configured on the
gateway's admin page (**Web access → gateway → TCP relays**): edge
port, target port, optional label; changes push within seconds, and
removal closes the public listener. The motivating case is SSH for git:
edge port `22` relayed to PCP's own git-over-SSH endpoint on `4222`
(Git Services ships one — `PCP_GIT_SSH_ADDR`, docs/gitservices.md →
"Git over SSH"). Nothing is SSH-specific — it's bytes in, bytes out.

- **Blindness**: the gateway never parses relayed bytes. For a protocol
  that encrypts end-to-end (SSH), the relay is blind **in flight as
  well as at rest** — unlike HTTPS, where the gateway terminates TLS
  and plaintext transits its RAM. The relay config on disk names ports,
  never keys or payload.
- **The push is the allowlist**: the PCP-side worker refuses to dial
  any target port not in the configured relays, so a compromised
  gateway cannot steer streams to arbitrary local ports.
- **Edge limits**: relayed connections count against the gateway's
  max-concurrent-connections cap, plus relay-specific budgets — at most
  8 concurrent relay connections and 60 new connections/min per client
  IP, and a 10-minute both-ways idle timeout.
- **Self-report**: `/v1/status` lists each relay's listener state,
  active connections, and total relayed bytes; listener failures (a
  port already in use, a privileged port it may not bind) surface there
  and in the error ring, and the bind retries every few seconds.
- **Publish each edge port on the container**: the gateway binds relay
  listeners inside its own network namespace, so a relay's edge port
  reaches the internet only if it is published — recreate the container
  (or extend the Quadlet) with `-p 22:22` when you add an SSH relay.
  Inside the container, ports below 1024 are bindable as-is. If sshd
  already owns port 22 on the gateway host, move it or pick another
  edge port.
- A relay may not use the gateway's own listener ports (80/443/
  tunnel/control) — refused at the console and again at the gateway.
- **What answers on the target port is your business**: PCP relays to
  whatever listens on `127.0.0.1:<target>` on the PCP host. It applies
  no authentication of its own there — pick daemons that do (sshd).

## TLS for your hostnames

Per hostname, chosen in the admin console:

- **acme** — automatic Let's Encrypt. The **PCP runs the ACME client**;
  HTTP-01 challenges arrive at the gateway on port 80 and tunnel down
  like any request, so issuance and renewal need nothing on the
  gateway. Renewal starts 30 days before expiry. (A per-gateway
  directory URL override exists for staging/test CAs.)
- **selfsigned** — PCP mints a 1-year cert and rotates it (browser
  warning; fine for personal use).
- **custom** — paste your own PEM chain + key in the console.

In every mode the key material lives in PCP's databox and reaches the
gateway sealed, RAM-only.

## Observability

`GET /v1/status` (signed, PCP-only) is the gateway's self-report:
version, uptime, applied config serial, per-hostname cert freshness (in
RAM since the last restart?), live tunnel/stream counts, counters
(requests, 4xx/5xx, offline serves, forced redirects), per-relay TCP
relay state (listening/error, active connections, relayed bytes), and a
RAM ring of the last 50 operational errors. PCP's sync loop polls it every 20s —
that's what the per-gateway page under **Admin → Web access** renders:
pairing/tunnel state, drift detection (a restarted gateway reports a
stale serial and missing cert keys and gets an immediate re-push),
tunnel/request/error sparklines from the poll history, and the error
ring. Reachability, drift, missing cert keys, dead tunnels, and cert
expiry also feed the health worker: failures surface as plain-language
problems on Admin Home and notify every admin (see `docs/admin.md`).

## What the data dir holds

Only the gateway's own identity keys, its control-plane TLS keypair,
PCP's *public* keys, the keyless config cache, and the offline page. No
serving-certificate private keys, ever. That's the guarantee, and it's
covered by the trust-boundary test suite
(`pkg/cloudferry/trustboundary_test.go`).
