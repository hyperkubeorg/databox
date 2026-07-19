**Disclosure**
This codebase contains AI generated code.
Testing is still in progress, code has lots of parts that need further work (looking at the docs).
Use the Github issues for any change requests/discussions or bug reporting.

# databox

Check out the [Meet the Cluster](https://meetthecluster.github.io/) to see visualizer and simulations of databox.

Distributed key-value and blob storage in a single Go binary. Raft-replicated
KV (linearizable reads/writes, snapshot-isolation transactions, watches,
distributed locks) plus a blob engine with its own replication/erasure-coding
data path — with an HTTPS API, a web GUI, an interactive console, and
prefix-based user grants. Stateless SQL (PostgreSQL wire, with
pgvector-style vector search) and S3-compatible layers run from the same
binary against the cluster, and point-in-time backups ship to S3 or SFTP.

Deployment is containerized: run the published images with podman or
docker, or install the Helm charts on Kubernetes. Images and charts ship
from `ghcr.io/hyperkubeorg` for amd64 and arm64, versioned by UTC date
stamp (`YYYYMMDD.HHMMSS`); all artifacts of one release share one stamp.
Load balancing across nodes is yours to provide (HAProxy works well —
[docs/admin/bare-metal.md](docs/admin/bare-metal.md#load-balancing)).

## 5-minute quickstart

Run a single-node cluster and store your first key and blob. You need
podman or docker (`docker` substitutes for `podman` 1:1 below).

```sh
# 1. Start a node. Zero config = a working single-node cluster.
podman run -d --name databox \
  -v databox:/var/lib/databox \
  -p 8443:8443 \
  ghcr.io/hyperkubeorg/databox:latest

# 2. Open the console. Accept the certificate fingerprint on first
#    connect; the fresh root user has no password (empty password logs
#    in until you set one).
podman exec -it databox databox console

databox> set /hello world
databox> get /hello
databox> list /
databox> exit
```

The web GUI is on the same port: <https://localhost:8443/>.

Set a root password before doing anything else that matters:

```sh
podman exec -it databox databox user passwd root
```

## Documentation

| Page | Contents |
|------|----------|
| [docs/getting-started.md](docs/getting-started.md) | Install, single node, KV/blob basics, 3-node cluster |
| [docs/architecture.md](docs/architecture.md) | Storage system vs. layers, Raft topology, blob data path |
| [docs/consistency.md](docs/consistency.md) | **Normative** consistency guarantees and their tests |
| [docs/layers/](docs/layers/README.md) | processing layers: [SQL](docs/layers/sql.md) (pg wire, full dialect walkthrough) and [S3](docs/layers/s3.md) |
| [docs/security.md](docs/security.md) | TLS/PKI, PSK rotation, users & grants, hardening |
| [docs/gui.md](docs/gui.md) | the web portal: explorers, watch, users, account |
| [docs/admin/local-dev-kind.md](docs/admin/local-dev-kind.md) | `make kind-up` development loop |
| [docs/admin/kindrelay.md](docs/admin/kindrelay.md) | the optional `make relay-*` targets: streaming-safe TCP relays |
| [sandbox/personalcloudplatform](sandbox/personalcloudplatform/README.md) | flagship application example built on databox: a self-hosted cloud platform (Drive/Email/Calendar/Video/Music) with blind mail + web gateways |
| [docs/admin/kubernetes.md](docs/admin/kubernetes.md) | Helm install from ghcr.io, secrets, scaling, upgrades |
| [docs/admin/kubernetes-cheatsheet.md](docs/admin/kubernetes-cheatsheet.md) | kubectl/helm onboarding: inspect, debug, kill — mapped onto this cluster |
| [docs/admin/bare-metal.md](docs/admin/bare-metal.md) | 1/3/5-node bootstrap with podman/docker, systemd, HAProxy, decommission |
| [docs/admin/backup-restore.md](docs/admin/backup-restore.md) | Backups to S3/SFTP, restore |

## Artifacts

Every push to the default branch runs
[.github/workflows/release.yml](.github/workflows/release.yml): one UTC
date stamp (`YYYYMMDD.HHMMSS`) versions everything the run publishes.
Images are multi-arch (linux/amd64 + linux/arm64), tagged `<stamp>` and
`latest`:

| Image | Runs |
|-------|------|
| `ghcr.io/hyperkubeorg/databox` | databox node; the S3/SQL gateways are subcommands of this binary and deploy from this image |
| `ghcr.io/hyperkubeorg/pcp` | Personal Cloud Platform app server |
| `ghcr.io/hyperkubeorg/postoffice` | PCP mail gateway (blind at rest) |
| `ghcr.io/hyperkubeorg/cloudferry` | PCP web gateway |
| `ghcr.io/hyperkubeorg/pcp-runner` | PCP Builds CI runner |
| `ghcr.io/hyperkubeorg/pcp-camd` | PCP Smart Home camera daemon |

Helm charts publish after all images of the stamp exist, with chart
version = appVersion = the stamp:

- `oci://ghcr.io/hyperkubeorg/charts/databox`
- `oci://ghcr.io/hyperkubeorg/charts/pcp`
- `oci://ghcr.io/hyperkubeorg/charts/pcp-runner`

## Building (development)

Deployments run the published containers; building from source is for
working on databox itself (Go 1.26+).

```sh
make            # help: all targets, grouped
make build      # ./bin/databox
make test       # unit + integration tests
make docker     # container image (podman or docker, auto-detected)
make kind-up    # 5-node kind cluster running databox (5 replicas) + the demo apps
```

Releases are built by the workflow described under [Artifacts](#artifacts).

`§N` markers in code comments are stable design-decision tags from the
original (retired) internal spec; they group related decisions so you can
grep every site of one design choice.

# License

[![GNU AGPLv3](AGPLv3_Logo.svg)](https://www.gnu.org/licenses/agpl-3.0.html)

[Third-party dependency license report](pkg/licenses/LICENSE-REVIEW.md) (also served in-app at `/licenses`)
