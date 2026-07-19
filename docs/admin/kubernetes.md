# Kubernetes (Helm)

You will install databox on Kubernetes with the published Helm chart,
retrieve the generated root password, scale safely, and upgrade without
rotating credentials. New to kubectl itself? Start with the
[cheat sheet](kubernetes-cheatsheet.md).

## Install

The chart is published to ghcr.io as an OCI artifact. Its version is the
release's UTC date stamp, and each chart pins the image built in the same
release — chart `20260717.143000` deploys image
`ghcr.io/hyperkubeorg/databox:20260717.143000` (multi-arch, amd64/arm64).
Pick a version from the
[chart package page](https://github.com/hyperkubeorg/databox/pkgs/container/charts%2Fdatabox):

```sh
helm install db oci://ghcr.io/hyperkubeorg/charts/databox \
  --version 20260717.143000 \
  --namespace databox --create-namespace
```

(Recent helm installs the newest version when `--version` is omitted.
From a git checkout, `helm install db ./charts/databox …` works too and
deploys the `latest` image.)

The chart deploys a StatefulSet (stable network identities for Raft peers,
`volumeClaimTemplates` for PebbleDB + chunk storage), a headless Service for
peer discovery, and a client-facing Service. Pod 0 bootstraps the cluster;
pods 1..N-1 fetch a join token and join automatically.

## Get the root password

On first install the chart generates a random root password and node PSK into
a Secret and — via Helm `lookup` — **never rotates them on upgrade**.

```sh
kubectl get secret db-databox-auth -n databox \
  -o jsonpath='{.data.root-password}' | base64 -d; echo
```

## Reach the API and GUI

```sh
kubectl port-forward svc/db-databox -n databox 8443:8443
# Fine for console/GUI use; it can wedge under sustained transfers
# (large blobs, video) — for real traffic expose the service (below).
# On the make kind-up cluster: `make port-forward[-pcp]`
# wraps this, and the optional `make relay-*` targets serve the same
# ports via a raw TCP relay that streaming can't wedge (kindrelay.md).
# https://localhost:8443/  — log in as root; accept the cert fingerprint
databox console --endpoint localhost:8443
```

To expose it outside the cluster, enable `ingress` or `httpRoute` in values
(both pass TLS through to databox, which terminates TLS itself), or set
`service.type: LoadBalancer` / `NodePort`. Load balancing in front of the
cluster is your infrastructure's job — on clusters without a
LoadBalancer implementation, an external HAProxy in TCP mode against the
NodePorts works well (same configuration as
[bare-metal.md § Load balancing](bare-metal.md#load-balancing)).

## Scaling

**Scaling up** is safe and automatic — new pods join and the placement
controller assigns them shards:

```sh
helm upgrade db oci://ghcr.io/hyperkubeorg/charts/databox \
  --version <installed-version> -n databox --set replicaCount=5
```

**Scaling down requires decommissioning first.** Never just lower
`replicaCount` — draining must move replicas off the node before it
disappears:

```sh
# Drain the highest-ordinal pod (going 5 → 4, that is db-databox-4; find its
# numeric node ID in `cluster status`)
databox cluster status --endpoint localhost:8443
databox cluster decommission <node-id> --endpoint localhost:8443
databox cluster status --endpoint localhost:8443   # wait for safe_to_proceed
helm upgrade db oci://ghcr.io/hyperkubeorg/charts/databox \
  --version <installed-version> -n databox --set replicaCount=4
```

Repeat one node at a time, waiting for `cluster status` to report all shards
fully replicated between steps (§16.3).

## Upgrades

Upgrading to a newer release is just upgrading the chart version — each
chart pins its matching image:

```sh
helm upgrade db oci://ghcr.io/hyperkubeorg/charts/databox \
  --version <newer-stamp> -n databox
```

The StatefulSet rolls pods one at a time. The credential Secret is reused, so
upgrades never change the root password or PSK. Watch the rollout:

```sh
kubectl rollout status statefulset/db-databox -n databox
```

## PVC sizing

Each node's volume holds PebbleDB (KV + Raft logs) and blob chunks. Shards
split at 16 GiB by default, so a node hosting multiple shards needs
substantially more than 16 GiB. PVCs cannot be shrunk later — size up front:

```sh
helm install db oci://ghcr.io/hyperkubeorg/charts/databox -n databox \
  --set storage.size=200Gi --set storage.className=fast-ssd
```

## Tuning and gateways

- **`extraEnv`** injects `DATABOX_*` configuration into the storage pods —
  the security and MVCC knobs live here (see the settings table in
  [../getting-started.md](../getting-started.md)):

  ```yaml
  extraEnv:
    - name: DATABOX_PSK_EXTRA_GRACE
      value: "720h"
    - name: DATABOX_MVCC_HISTORY_REVS   # MUST be identical on all nodes
      value: "4096"
  ```

- **`gateways.s3.enabled` / `gateways.sql.enabled`** deploy the stateless
  protocol gateways as separate Deployments against the cluster; scale
  their `replicas` independently of the storage nodes.
- Gateway listeners are secure-by-default and refuse to serve without
  TLS. Each gateway takes either **`tlsSecret`** (a `kubernetes.io/tls`
  Secret with `tls.crt`/`tls.key` to serve TLS directly) or
  **`allowCleartext: true`** (the chart default) for plaintext inside the
  cluster network — S3 requests are still SigV4-authenticated and pg
  sessions still password-authenticated; terminate TLS at your
  Ingress/Gateway when exposing them outside the cluster.

## Production notes

- Set real `resources` requests/limits.
- Add anti-affinity or a topology spread constraint so replicas land on
  distinct nodes/zones (a Raft quorum on one physical host tolerates nothing).
- Probes are wired over HTTPS out of the box: liveness to `/healthz`
  (process-alive — a catching-up node is not killed for being busy) and
  readiness to `/readyz` (node can actually serve — metadata members:
  leader known + bounded apply lag; other nodes: metadata members
  discovered). A bootstrapping or freshly-joined
  node answers 503 on `/readyz` and receives no Service traffic until
  it can serve — apps pointed at the cluster ride through bootstrap and
  maintenance without any operator action.
