# Bare-Metal Deployment (podman/docker)

You will bootstrap a 1-, 3-, or 5-node databox cluster serving traffic in
under five minutes, wire it into systemd, put a load balancer in front, and
learn the safe decommission procedure.

Databox deploys as a container — the only prerequisite on each host is
podman or docker (`docker` substitutes for `podman` 1:1 below). Production
pins a date-stamp tag (`ghcr.io/hyperkubeorg/databox:YYYYMMDD.HHMMSS`, from
the [package page](https://github.com/hyperkubeorg/databox/pkgs/container/databox))
so every node runs the same build; the examples use `latest` for brevity.

The image's entrypoint is the `databox` binary, so the CLI runs through the
node's own container. The commands below assume:

```sh
alias databox='podman exec -it databox databox'
```

## Single node (30 seconds)

```sh
# Zero config = a working single-node cluster. The named volume owns
# /var/lib/databox, so the container is disposable and the data is not.
podman run -d --name databox \
  -v databox:/var/lib/databox \
  -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest
```

Set a root password before anything else that matters:

```sh
databox user passwd root
```

## Three or five nodes (< 5 minutes)

Odd counts only (1, 3, 5) — a Raft group needs a majority to make progress.
The metadata group sizes itself: 1 voter below 3 nodes, 3 voters from 3–7
nodes, 5 voters once the fleet reaches 8 (lowest ordinals preferred).
Metadata exists on those nodes and nowhere else — every other node
routes metadata lookups to them through a bounded, seconds-TTL cache
(databox never replicates a piece of data to all nodes). Quorum never
grows with fleet size and tolerates `⌊voters/2⌋` simultaneous voter
failures; non-member failures never cost metadata availability.

**Node 1 — bootstrap.** `--advertise` must be an address the other nodes
can reach through the published port:

```sh
podman run -d --name databox \
  -v databox:/var/lib/databox -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest \
  server --advertise node1.example.com:8443
```

**Generate a join token** (on node 1, or any existing node):

```sh
databox cluster join-token --ttl 1h
# prints one line: <base64 token embedding endpoint, CA fingerprint, secret, PSK>
```

**Nodes 2..N — join** with that token:

```sh
podman run -d --name databox \
  -v databox:/var/lib/databox -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest \
  server --advertise node2.example.com:8443 --join <token>
```

Each joiner authenticates, receives an auto-issued certificate and PSK
material, registers, and the placement controller begins assigning shards.
Confirm convergence:

```sh
databox cluster status
```

## systemd

With podman, a [Quadlet](https://docs.podman.io/en/latest/markdown/podman-systemd.unit.5.html)
unit makes the container a native service —
`/etc/containers/systemd/databox.container`:

```ini
[Unit]
Description=databox storage node
After=network-online.target
Wants=network-online.target

[Container]
Image=ghcr.io/hyperkubeorg/databox:YYYYMMDD.HHMMSS
ContainerName=databox
Volume=databox:/var/lib/databox
PublishPort=8443:8443
Exec=server --advertise node1.example.com:8443

[Service]
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload           # generates databox.service from the Quadlet
systemctl enable --now databox
journalctl -u databox -f
```

With docker, add `--restart unless-stopped` to the `docker run` instead —
the docker daemon (itself under systemd) then supervises the container
across crashes and reboots.

Configuration beyond flags travels as environment (`-e DATABOX_*` /
`Environment=` in the Quadlet) — every setting in the
[getting-started table](../getting-started.md#all-settings) maps to a
`DATABOX_*` variable.

## Load balancing

Databox does not ship a load balancer: clients can be pointed at any node,
and spreading them across nodes is your infrastructure's job. HAProxy in
TCP mode works well — databox terminates its own TLS, so the balancer just
passes bytes:

```
# /etc/haproxy/haproxy.cfg (the relevant parts)
frontend databox
    bind :8443
    mode tcp
    default_backend databox_nodes

backend databox_nodes
    mode tcp
    # `source` pins each client IP to one node. Browsers need this: every
    # node serves its OWN self-signed certificate, and a rotating backend
    # re-prompts the cert warning on every other request. API clients
    # retry across nodes and would be fine with roundrobin.
    balance source
    option httpchk GET /readyz
    server node1 node1.example.com:8443 check check-ssl verify none
    server node2 node2.example.com:8443 check check-ssl verify none
    server node3 node3.example.com:8443 check check-ssl verify none
```

`/readyz` answers 503 while a node bootstraps, joins, or falls behind —
the check keeps traffic off nodes that cannot serve yet. The S3 and SQL
gateway listeners (see [layers/](../layers/README.md)) balance the same way:
TCP mode, one backend per gateway instance.

## Decommissioning (safe node removal)

Remove nodes **one at a time**, waiting for full re-replication between
each. `cluster status` shows each node's numeric ID:

```sh
databox cluster status        # note the node's ID
databox cluster decommission 3
databox cluster status        # wait until safe_to_proceed is true and node 3
                              # no longer appears in any group
# only then move on to the next node
```

The migrator moves shard replicas off the draining node. Blob chunks are
not proactively migrated: a draining-but-alive node still counts as a valid
chunk holder, and the repair loop re-replicates its chunks only after the
node stops answering. After stopping a drained node, wait for the repair
pass to clear any under-replication alerts before wiping its disk or
removing the next node. `cluster status` reports per-node `safe-to-remove`
plus cluster-wide under-replication and quorum-at-risk indicators; the CLI
prints the check explicitly when a decommission completes.

**Dead hardware** that cannot drain:

```sh
databox cluster remove 3 --force   # audited; repair loops rebuild
```

Removing a node without a majority still alive risks losing quorum — always
consult `cluster status` first.
