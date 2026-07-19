# kindrelay — the optional streaming-safe alternative to port-forward

You will understand what `cmd/kindrelay` is, when to reach for the
Makefile's opt-in `relay-*` targets instead of the default
`port-forward-*` (plain `kubectl port-forward`), what the relay fixes,
and where it does not apply.

## The problem

`kubectl port-forward` is a debugging tunnel, not a data path. Every
forwarded connection becomes a stream multiplexed inside **one** SPDY
session that runs client → API server → kubelet → pod. Under sustained
transfer — streaming a video from the pcp demo, pulling a
large blob — that shared session wedges. The symptom in the
port-forward output:

```
E... portforward.go:489] ... error copying from remote stream to local connection: ... connection reset by peer
E... portforward.go:455] ... error creating error stream for port 4001 -> 8080: Timeout occurred
```

The first line is unremarkable on its own — browsers abort and reopen
Range requests constantly while seeking and buffering video. The
tunnel's reaction is the bug: once the session degrades, **every**
subsequent connection fails with the second line until the
port-forward process is restarted. Meanwhile the pods are healthy and
log nothing; the break is entirely in the tunnel. This hit us in
practice (2026-07) streaming video from the pcp demo's predecessor,
and it also silently dropped that app's watch-progress heartbeats,
losing the viewer's resume position.

## The fix

kind can publish NodePorts directly on the developer's host
(`extraPortMappings` in `kind.yaml`) — plain TCP into the node, no API
server in the path. Every service the Makefile forwards has a fixed
NodePort published this way.

`cmd/kindrelay` is the last mile: a raw TCP relay (~80 lines, stdlib
only) that listens on the same friendly port as its `port-forward-*`
sibling — both loopbacks, like kubectl — and pipes bytes to the
published NodePort:

| target                   | serves                    | relays to NodePort |
| ------------------------ | ------------------------- | ------------------ |
| `make relay`             | `https://localhost:8080`  | 30443 (databox GUI)|
| `make relay-pcp`         | `http://localhost:4001`   | 30081              |
| `make relay-ssh`         | `ssh://git@localhost:4222`| 30422 (pcp git SSH)|
| `make relay-s3`          | `http://localhost:9000`   | 30900              |
| `make relay-sql`         | `localhost:5432`          | 30432              |

Reach for a `relay-*` target when a `port-forward-*` tunnel wedges
under load (video streaming from the pcp demo is the
canonical trigger); for everything else the kubectl targets are the
default and work everywhere kubectl does.

Why this holds up where the tunnel didn't:

- **Per-connection isolation.** One inbound TCP connection maps to one
  outbound connection. An aborted transfer tears down its own pair and
  affects nothing else — the exact event that wedges the shared SPDY
  session.
- **Wire speed, no re-encoding.** `io.Copy` in both directions; TLS
  (databox, pg `sslmode=require`) passes through untouched.
- **Half-close correct.** Each direction shuts down independently
  (`CloseWrite`), so protocols that close one side first terminate
  cleanly instead of hanging.
- **Probe-first.** It dials the NodePort once before listening and
  exits with instructions if the cluster is down or predates the
  mapping — it fails at startup, not on your first browser request.
- **IPv4-explicit.** The targets dial `127.0.0.1:<nodeport>`, never
  `localhost`: kind's `extraPortMappings` bind the IPv4 wildcard, and a
  container runtime's stray `::1` proxy socket can ACCEPT connections
  whose inner leg then dies — an accept-then-close black hole that
  looks like an empty response in the browser. If a relayed backend
  ever sends zero bytes, the relay logs it with a curl command to test
  the published port directly.

## Shortcomings

- **kind-only.** It depends entirely on `kind.yaml`'s host port
  mappings. Against a remote or production cluster it relays to
  nothing — there, use `kubectl port-forward` (fine for light console
  use) or expose the service properly (Ingress/LoadBalancer,
  [kubernetes.md](kubernetes.md)).
- **Mappings freeze at cluster creation.** A port added to `kind.yaml`
  after the cluster was made is dead until one
  `make kind-down && make kind-up`. The gateway ports (30900/30432)
  are newer than the app ports; clusters created before them need the
  recreate for `relay-s3`/`relay-sql`.
- **Rootless podman needs loopback NodePorts.** `rootlessport`
  publishes the mappings by dialing the NodePort on the node's
  LOOPBACK from inside — and recent kube-proxy defaults don't serve
  loopback (nftables/ipvs modes never do). kind.yaml pins
  `kubeProxyMode: iptables` with `nodePortAddresses: []` +
  `localhostNodePorts: true` for exactly this; a cluster created
  before that config resets every published-port connection
  (accept → RST) and needs one recreate.
- **Startup probe only.** If the backend dies mid-run, individual
  connections fail; the relay does not health-check, reconnect, retry,
  or buffer.
- **A dumb pipe.** No added auth, no TLS termination, no HTTP
  awareness, no logging beyond connection errors. It binds localhost
  only — and the kind mappings it dials are themselves loopback-bound
  (`listenAddress: 127.0.0.1`), so nothing is exposed to the network —
  but whatever the service speaks is exactly what you get.
- **The friendly port is vanity.** The NodePorts are already on
  localhost (the table's third column); the relay only preserves the
  documented ports. Hitting a NodePort directly is always fine.
- **Needs the Go toolchain** (`go run ./cmd/kindrelay`) — already a
  prerequisite for hacking on this repo.
