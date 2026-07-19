# Local Development with kind

You will bring up a local multi-node databox cluster in Kubernetes with
`make kind-up`, iterate on the binary, and run the chaos tests locally.

## Prerequisites

- Docker or Podman (auto-detected by the Makefile).
- [`kind`](https://kind.sigs.k8s.io/) and `kubectl` on your PATH.
- Go 1.26+.

## Bring the cluster up

```sh
make kind-up
```

This builds the container image, creates the kind cluster defined in
`kind.yaml` (5 nodes, with `extraPortMappings` for host access — plus a
kube-proxy config that serves NodePorts on loopback, which rootless
podman's port publishing depends on), loads the image, and installs the
Helm chart. Tear it down with:

```sh
make kind-down
```

## The hacking loop

1. Edit code.
2. Re-run:

   ```sh
   make kind-up
   ```

   That is the whole loop: it rebuilds the images from your tree,
   side-loads them under a fresh per-run tag, upgrades both Helm
   releases to that tag — which makes Kubernetes roll every workload
   onto the new build — and blocks until the rollout is done. When it
   returns, the cluster is serving the code in your tree; there is no
   kubectl step to remember. (`kind load docker-image` is deliberately
   not used: under podman the image lookup misses; the saved-archive
   path works with both runtimes.)

3. Watch logs:

   ```sh
   kubectl logs -f statefulset/databox
   ```

New to Kubernetes? [kubernetes-cheatsheet.md](kubernetes-cheatsheet.md)
maps every inspect/debug/kill command onto exactly what this cluster
runs.

## Reach the cluster

The mapped host ports (see `kind.yaml`; all bound to 127.0.0.1 — the
dev cluster is never reachable from the LAN): databox on
<https://localhost:30443> (alias 8443), the pcp demo on
<http://localhost:30081>, and the S3/SQL gateways on 30900/30432.
The `make port-forward-*` targets are plain `kubectl port-forward` on
the familiar ports (8080/4001/9000/5432). If a tunnel wedges under
sustained transfer (video streaming), the OPTIONAL `make relay-*`
targets serve the same ports via a raw TCP relay to those NodePorts —
[kindrelay.md](kindrelay.md) explains the trade-off.
Retrieve the generated root password and connect:

```sh
kubectl get secret databox-auth -o jsonpath='{.data.root-password}' | base64 -d; echo
databox console --endpoint localhost:30443
```

Each node serves its own self-signed certificate; the browser-facing
`databox-gui` Service (NodePort 30443, pinned to pod 0 by selector)
keeps your browser/CLI on one node so accepting the certificate sticks.
The main `databox` Service is deliberately UNPINNED — in-cluster
clients rotate away from slow nodes. On a cluster deployed before this
split, run `make kind-up` again to converge.
Accept the certificate fingerprint on first connect (§6.3), or pin it with
`--ca-fingerprint`.

## Run the tests locally

```sh
make test    # unit + integration
make e2e     # in-process multi-node chaos/consistency suite
make cover   # coverage summary
```

`make e2e` spins real multi-node clusters inside the test process (bootstrap,
join tokens, PSK-authenticated RPC, raft) and validates every guarantee in
[../consistency.md](../consistency.md). It needs no kind cluster — it is pure
Go — so it is the fastest way to validate a change to the storage core.
