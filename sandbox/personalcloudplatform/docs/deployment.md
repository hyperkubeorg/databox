# Deployment & scaling

PCP is a stateless app: every user, session, file byte, mail message, and
calendar event is a record or blob in databox under `/pcp/`. Scaling PCP
is therefore two independent decisions — how many interchangeable PCP
replicas you run, and how durable the databox cluster underneath them is.
This guide takes you from one machine to a fault-tolerant cluster.

See also: [README](../README.md) (env vars, quickstart), `docs/admin.md`
(everything configured from the console), `docs/api.md` (REST/escape
hatches), [postoffice.md](postoffice.md) and [cloudferry.md](cloudferry.md)
(the gateways).

## Topology overview

Three moving parts, plus two optional gateways:

```
                        (internet SMTP)
                              │
                              ▼
   internet visitors    ┌───────────┐   sealed mail, PCP collects it
        │               │ postoffice│◄────────────────────────┐
        ▼               └───────────┘                          │
   ┌───────────┐  tunnel (PCP dials out)                       │
   │ cloudferry│◄──────────────────────┐                       │
   └───────────┘                       │                       │
                              ┌────────┴───────────────────────┴──┐
                              │  PCP replicas (stateless, N of)    │
                              └──────────────┬─────────────────────┘
                                             │ databox client (user auth)
                              ┌──────────────▼─────────────────────┐
                              │  databox cluster (1..N nodes)      │
                              │  all state under /pcp/             │
                              └────────────────────────────────────┘
```

- **databox** — the only database and blob store. The source of truth; the
  one stateful thing. Durability lives or dies here.
- **PCP** — the app. Holds no state; any replica serves any request. Dials
  *out* to the gateways, never accepts a connection from them.
- **postoffice** — public mail face on a cloud host (SMTP in/out), sealed
  at rest. Optional; only if you want email. [postoffice.md](postoffice.md)
- **cloudferry** — public web face on a cloud host (real hostnames + TLS
  over a tunnel), so your PCP network accepts **zero** inbound connections.
  Optional; only if you want to be reachable from the internet.
  [cloudferry.md](cloudferry.md)

Both gateways are blind at rest and pair from the admin console — PCP mints
the keys, dials out, and neither gateway learns where PCP lives.

## Single host (start here)

Everything on one machine: one databox node and one PCP replica, as two
containers on a shared network. The prerequisite is podman or docker
(`docker` substitutes for `podman` 1:1 below); images live at
`ghcr.io/hyperkubeorg/{databox,pcp}` (multi-arch, amd64/arm64) — pin a
date-stamp tag in production. Gateways can wait — add them once you want
internet mail/web.

**1. Run the databox node.** One node is a complete cluster; the shared
network lets PCP reach it by container name:

```sh
podman network create pcp
podman run -d --name databox --network pcp \
  -v databox:/var/lib/databox -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest
podman exec -it databox databox user passwd root   # set a root password immediately
```

With a single node, `replicas` is effectively 1: metadata and data live on
the one node, and blobs are stored in **replica mode with a single copy** —
**no erasure coding and no cross-node replication are possible with one
node. Durability is only as good as that one disk.** See
[what single-host does and doesn't give you](#what-single-host-does-and-doesnt-give-you).

**2. Create the scoped `pcp` user and grants.** PCP should not run as
`root` (it would hold full-cluster credentials, and the app warns loudly if
it does):

```sh
podman exec -it databox databox user create pcp
podman exec -it databox databox grant add pcp allow /pcp    list,read,write,delete
podman exec -it databox databox grant add pcp allow /.databox list,read   # admin Databox panel
```

**3. Run PCP** pointed at the node, as that user:

```sh
podman run -d --name pcp --network pcp -p 8080:8080 \
  -e DATABOX_ENDPOINT=databox:8443 \
  -e DATABOX_USER=pcp -e DATABOX_PASSWORD=… \
  -e DATABOX_CA_FINGERPRINT=<sha256 from first connect> \
  ghcr.io/hyperkubeorg/pcp:latest
```

For plain-HTTP local dev add `-e INSECURE_COOKIES=1` and drop the
fingerprint; in any real deploy pin the cert (see
[production checklist](#production-checklist)).

**4. First admin.** Open `http://localhost:8080` and sign up — the **first
account becomes the admin** (`PCP_ADMIN=<name>` promotes a named account
instead). Everything else — mail domains, gateway pairing, signup mode,
tiers, branding — is configured from **Admin** (`docs/admin.md`).

### systemd sketch (Quadlet)

```ini
# /etc/containers/systemd/pcp.network
[Network]

# /etc/containers/systemd/databox.container
[Container]
Image=ghcr.io/hyperkubeorg/databox:YYYYMMDD.HHMMSS
ContainerName=databox
Network=pcp.network
Volume=databox:/var/lib/databox
PublishPort=8443:8443
[Service]
Restart=always
[Install]
WantedBy=multi-user.target

# /etc/containers/systemd/pcp.container
[Unit]
After=databox.service
[Container]
Image=ghcr.io/hyperkubeorg/pcp:YYYYMMDD.HHMMSS
ContainerName=pcp
Network=pcp.network
PublishPort=8080:8080
Environment=DATABOX_ENDPOINT=databox:8443 DATABOX_USER=pcp
EnvironmentFile=/etc/pcp/env          # DATABOX_PASSWORD, DATABOX_CA_FINGERPRINT, …
[Service]
Restart=always
[Install]
WantedBy=multi-user.target
```

(With docker, use `--restart unless-stopped` on both `docker run`s
instead.) The gateways have their own container runbooks — see
[postoffice.md](postoffice.md) and [cloudferry.md](cloudferry.md). They run
on a cloud host, not this box.

## What single-host does and doesn't give you

One node is a real, complete databox cluster — but not a durable one. Read
this row before you trust production data to it:

| Property | 1 node | 3+ nodes |
|---|---|---|
| Metadata / KV copies | 1 | metadata on 3 voters (5 at 8+ nodes); other nodes route to them |
| Data shard copies | 1 | `replicas` (default 3) |
| Blob storage | replica mode, **1 copy** | replica (small) / `rs-4-2` EC (large) |
| Erasure coding | **none** (needs ≥ 3 nodes) | yes, for blobs > 1 chunk |
| Tolerates a node down | **no** | yes (1 of 3) |
| A disk loss means | **data loss** without backups | repaired from survivors |

On a single host **backups are not optional** — they are your only
durability. Take them regularly: [admin/backup-restore.md](../../../docs/admin/backup-restore.md).

## Migrating to a 3+ node cluster

The key point: **PCP itself does not change.** It just points at a databox
endpoint; whether that endpoint is one node or thirty is a databox concern.
Scaling durability is a databox operation, done underneath a running PCP.

### 1. Grow databox to 3 nodes

Mint a join token on the existing node and start two more with it. The
mechanics (token TTL, `--advertise`, `cluster status`) are in databox's
[getting-started.md § Grow to 3 nodes](../../../docs/getting-started.md#grow-to-3-nodes)
— summarized here:

```sh
# on the existing node
podman exec -it databox databox cluster join-token --ttl 1h

# on node2 (and node3, with node3:8443)
podman run -d --name databox \
  -v databox:/var/lib/databox -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest \
  server --advertise node2:8443 --join 'PASTE_TOKEN'

# back on node1: expect 3 active, safe_to_proceed
podman exec -it databox databox cluster status
```

### 2. What happens automatically once ≥ 3 nodes exist

Driven by databox's placement controller and blob-repair loop (no PCP
action, see [architecture.md](../../../docs/architecture.md#automatic-management)):

- **The metadata group seats 3 voters** (5 once the fleet reaches 8) —
  metadata lives on those nodes and nowhere else; every other node routes
  lookups to them, so any node can authenticate and serve.
- **Data shards reach replication factor `replicas`** (default 3): the
  placement controller copies shards onto the new nodes.
- **New large blobs use erasure coding.** A blob larger than one chunk
  (chunk size 8 MiB default, so > 8 MiB) written from now on, on a cluster
  of ≥ 3 nodes, is stored as Reed-Solomon `rs-4-2` — 4 data + 2 parity
  shards per stripe, any 4 of 6 reconstruct it, tolerating 2 failures at
  1.5× overhead. Smaller blobs stay in replica mode (default 2 copies).
- **Blob repair re-replicates and reconstructs**: it copies under-replicated
  chunks onto new nodes and rebuilds EC stripes from survivors, IO-capped by
  `repair_bytes_per_sec` (64 MiB/s default).

**Automatic vs. instantaneous.** Raising durability is not instantaneous.
The blobs and shards you wrote while single-node exist as one copy until
placement and repair have re-replicated them across the new nodes — that
runs in the background, rate-limited, and takes time proportional to how
much data you have. `databox cluster status` shows when groups reach their
replication factor. New writes get the target durability immediately; the
backlog catches up.

### 3. Run multiple PCP replicas

Because sessions, CSRF tokens, and everything else are databox records, PCP
replicas are fully interchangeable — run as many as you like behind the
cluster; any instance serves any request. The background loops (mail intake/
outbound, media scan, gateway sync, health, git maintenance) elect one sweeper per pass with
databox locks, so extra replicas add capacity without doubling work. Point
each replica's `DATABOX_ENDPOINT` at the cluster (any node, or a load
balancer / client Service in front of them). In Kubernetes this is the
chart's `replicaCount` ([below](#kubernetes)).

You can also pair **several gateways**: multiple postoffices (one MX per
serving gateway) for mail, and multiple cloudferries for web. Note
cloudferry is **one-PCP-owned for life** — a gateway pairs to exactly one
PCP and rejects any other — but a single PCP may own many gateways.

### Durability & the `replicas` knob

`replicas` (default 3) is the databox KV replication factor for new data
shards; effective replication is `min(replicas, nodes)`. (Metadata
placement is databox's own 1/3/5-voter rule, not this knob.)
A 3-node cluster at `replicas: 3` tolerates **1 node down** and keeps
serving. Want more headroom? More nodes and/or a higher `replicas`. Blob EC
`rs-4-2` independently tolerates 2 lost shards on ≥ 3 nodes; both replica
count and EC geometry are overridable per key subtree with databox policies
([architecture.md § Blob engine](../../../docs/architecture.md#blob-engine-the-second-data-path)).

## Production checklist

- **Pin the databox cert.** Set `DATABOX_CA_FINGERPRINT` to the cluster CA
  fingerprint and `DATABOX_REQUIRE_FINGERPRINT=1` so PCP refuses to start
  unpinned. Unpinned, PCP logs a loud MITM warning and trusts on first use —
  fine for dev, unsuitable for production.
- **Scoped databox user, not root.** `DATABOX_USER=pcp` with grants on
  `/pcp` (and read on `/.databox`); never run PCP as `root`.
- **Real TLS via cloudferry ACME.** Publish through a paired cloudferry with
  per-hostname `acme` (PCP runs the ACME client; the gateway needs nothing).
  Your PCP network then accepts zero inbound connections.
- **Backups.** Even on a replicated cluster, keep off-cluster backups —
  replication is not backup. [admin/backup-restore.md](../../../docs/admin/backup-restore.md).
- **Gateways pair from the console**, never by editing config on the box:
  Admin → Mail → Post offices / Web access → Gateways, then run
  `postoffice setup` / `cloudferry setup` on the cloud host and paste codes
  each way. Runbooks: [postoffice.md](postoffice.md), [cloudferry.md](cloudferry.md).
- **Size quotas and uploads.** `PCP_DEFAULT_QUOTA` (per-user bytes, default
  10 GiB; 0 = unlimited) is the fallback when no tier or override applies —
  set tiers in Admin for real plans. `PCP_MAX_UPLOAD` caps a single request
  body (default 5 GiB). Size databox disks for your users × quota plus EC/
  replication overhead.

## Kubernetes

Deploy PCP with the published `pcp` chart against a **databox chart
release** — the app chart is just a Deployment + Service (stateless), so
scale it with `replicaCount` (default 2). Install databox first with its
own chart; it runs the StatefulSet, join tokens, and replication for you —
see [databox admin/kubernetes.md](../../../docs/admin/kubernetes.md). Both
charts are OCI artifacts versioned by release date stamp, each pinning the
images built alongside it:

```sh
helm install db  oci://ghcr.io/hyperkubeorg/charts/databox \
  --version <stamp> --namespace databox --create-namespace
helm install pcp oci://ghcr.io/hyperkubeorg/charts/pcp \
  --version <stamp> \
  --set databox.endpoint=databox:8443 \
  --set replicaCount=3
```

The demo values log in as `root` off the databox chart's generated secret;
a real install creates a scoped `pcp` user (grants on `/pcp` + read on
`/.databox`) and points `databox.existingSecret` at that credential.
`adminUser`, `signupMode`, `defaultQuota`, `maxUpload`, and `extraEnv` map
to the `PCP_*` / `TRUST_PROXY_HEADERS` env vars. Most installs skip the
Ingress and publish through a paired cloudferry instead (zero inbound
connections). The gateways never run in-cluster — they live on public cloud
hosts. Specifics live in the chart's `values.yaml` and `NOTES.txt`.
</content>
</invoke>
