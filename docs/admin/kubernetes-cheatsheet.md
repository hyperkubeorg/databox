# Kubernetes Cheat Sheet

Everything you need to poke at the `make kind-up` cluster, written for
someone strong in Go and distributed systems but new to Kubernetes. Every
command works unchanged on a live cluster — only the [context and
namespace](#from-kind-to-a-live-cluster) differ.

## The mental model in four lines

- A **pod** is a running container (plus its identity and probes). Pods
  are cattle: you never repair one, you delete it and a controller makes
  a replacement.
- A **deployment** runs N interchangeable pods (pcp, the gateways). A
  **statefulset** runs N pods with stable names and their own disks
  (`databox-0` … `databox-4`).
- A **service** is a stable virtual address in front of pods that come
  and go.
- `kubectl` is a REST client for the cluster's API; `helm` is a package
  manager that renders and applies bundles of these objects (a "chart",
  installed as a "release").

## Which cluster am I talking to?

`kubectl` targets whatever your kubeconfig's *current context* points at
— worth checking before destructive commands:

```sh
kubectl config current-context     # kind-databox for the dev cluster
kubectl config get-contexts        # everything your kubeconfig knows
kubectl config use-context kind-databox
```

## What's running

```sh
kubectl get pods                   # the workhorse
kubectl get pods -o wide           # + which node, pod IP
kubectl get pods -w                # watch changes live (Ctrl-C stops)
kubectl get all                    # pods + services + deployments + statefulsets
```

In the kind cluster you should see (READY means "passing its readiness
probe", i.e. actually able to serve):

```
NAME                                   READY   STATUS    RESTARTS
databox-0 … databox-4                  1/1     Running   0          ← the storage nodes (statefulset)
pcp-<hash>-<hash>        (x2)          1/1     Running   0          ← the app (deployment, 2 replicas)
databox-gateway-s3-<hash>-<hash>       1/1     Running   0          ← S3 gateway (deployment)
databox-gateway-sql-<hash>-<hash>      1/1     Running   0          ← SQL gateway (deployment)
```

A freshly joined or restarted databox pod showing `0/1 Running` is not
broken — it answers 503 on `/readyz` until it can serve, and Kubernetes
keeps traffic off it. Watch it come ready with `-w`.

## Debugging a pod

The two commands that answer 90% of "why is it broken":

```sh
kubectl describe pod databox-0     # spec, probe config, and — at the bottom — EVENTS
kubectl logs databox-0             # the process's stdout/stderr
```

`describe`'s Events section is where Kubernetes tells you what *it* did:
scheduling failures, image pull errors, failed probes, restarts. `logs`
is what the *process* said. Between them:

```sh
kubectl logs -f databox-0            # follow live
kubectl logs --previous databox-0    # the PREVIOUS crash's output — the
                                     # first thing to read on a crash loop
kubectl logs -f deployment/pcp       # one pod of a deployment, picked for you
kubectl get events --sort-by=.lastTimestamp   # cluster-wide event stream
```

Status decoder:

| STATUS | Meaning | First move |
|---|---|---|
| `Pending` | not scheduled yet | `describe` — no node fits, or a PVC can't bind |
| `ImagePullBackOff` | can't fetch the image | `describe` — typo'd tag, or the image wasn't side-loaded/pushed |
| `CrashLoopBackOff` | process keeps exiting | `logs --previous` |
| `Running 0/1` | up but failing readiness | `logs`; for databox, normal while bootstrapping/joining |
| `Terminating` (stuck) | shutdown hanging | see [killing pods](#killing-and-restarting-pods) |

## Killing and restarting pods

Deleting a pod is safe and is *the* way to restart one — its controller
immediately builds a replacement:

```sh
kubectl delete pod pcp-<hash>-<hash>   # deployment spawns a fresh one
kubectl delete pod databox-2           # statefulset recreates databox-2 —
                                       # same name, same volume, so it rejoins
                                       # the raft cluster as itself
```

Killing any single databox pod is a fine chaos drill: raft rides through
it, and the pod rejoins on return. Do **not** use pod deletion to
permanently shrink the cluster — that's a
[decommission](kubernetes.md#scaling).

```sh
kubectl rollout restart deployment/pcp          # rolling restart, all replicas
kubectl rollout restart statefulset/databox     # one pod at a time, raft-safe
kubectl rollout status  statefulset/databox     # watch it finish
kubectl delete pod databox-2 --force --grace-period=0   # last resort for a
                                       # pod stuck Terminating (e.g. its node died)
```

`make kind-up` already does the restart dance for you: each run deploys a
fresh image tag and waits for the rollout — you never need these just to
pick up a rebuild.

## Getting a shell inside

```sh
kubectl exec -it databox-0 -- sh       # the images are alpine: sh, not bash
kubectl exec -it databox-0 -- databox cluster status   # run the CLI in-place
kubectl exec -it deploy/pcp -- env     # inspect the env PCP actually got
```

The databox CLI through pod 0 is the standard way to run admin commands
in-cluster (`cluster status`, `user create`, `grant add`, `decommission`).

## Services and reaching things

```sh
kubectl get svc
```

| Service | Type | Where it lands |
|---|---|---|
| `databox` | ClusterIP | in-cluster clients; rotates across ready nodes |
| `databox-headless` | Headless | raft peer discovery (`databox-0.databox-headless…` DNS) — not for clients |
| `databox-gui` | NodePort 30443 | your browser → <https://localhost:30443>; pinned to pod 0 so the self-signed cert sticks |
| `pcp` | NodePort 30081 | <http://localhost:30081> |
| `databox-gateway-s3` | NodePort 30900 | S3 API on localhost:30900 |
| `databox-gateway-sql` | NodePort 30432 | pg wire on localhost:30432 |

On kind, those NodePorts reach localhost because `kind.yaml` maps them to
127.0.0.1 at cluster creation. On a live cluster you front NodePorts with
your own load balancer instead
([bare-metal.md § Load balancing](bare-metal.md#load-balancing)). For a
quick ad-hoc tunnel to anything:

```sh
kubectl port-forward svc/databox 8443:8443    # https://localhost:8443 until Ctrl-C
```

(Fine for a console; sustained transfers can wedge it — the `make
relay-*` targets exist for that, [kindrelay.md](kindrelay.md).)

## State: volumes

Each databox pod owns a PersistentVolumeClaim; that is the node's disk.

```sh
kubectl get pvc                        # data-databox-0 … data-databox-4
```

Deleting a **pod** never touches its PVC — `databox-2` comes back with
its data. Deleting a **PVC** is wiping that node's disk; the only routine
reason is re-bootstrapping from scratch (`make kind-down && make kind-up`
does it wholesale).

## Secrets and config

```sh
kubectl get secret databox-auth -o jsonpath='{.data.root-password}' | base64 -d; echo
```

That's the generated root password (Secrets are base64-wrapped, not
encrypted — access to secrets ≈ access to the cluster). Databox
configuration arrives as `DATABOX_*` env vars on the pods:
`kubectl describe pod databox-0` shows them.

## The Helm layer

```sh
helm list                              # releases: databox, pcp
helm get values databox                # what the install overrode
helm get values databox --all          # + every default
helm upgrade databox charts/databox --reuse-values --set replicaCount=5
helm uninstall pcp                     # remove a release (pods, services, all of it)
```

A release is just "chart + values, applied". `make kind-up` runs
`helm upgrade --install` for both releases on every invocation — check
`helm get values` when you're unsure what the cluster was told.

## When it's really stuck: the escalation path

1. `kubectl get pods` — what state, how many restarts?
2. `kubectl describe pod <p>` — read Events, bottom-up.
3. `kubectl logs <p>` (add `--previous` after a crash).
4. `kubectl get events --sort-by=.lastTimestamp` — the cluster-wide story.
5. `kubectl exec -it databox-0 -- databox cluster status` — what does
   databox itself think?
6. Nuclear, dev-only: `make kind-down && make kind-up` — fresh cluster,
   fresh volumes, ~2 minutes. Never the answer on a live cluster.

## From kind to a live cluster

Everything above translates 1:1. What changes:

- **Context**: `kubectl config use-context <prod>` instead of
  `kind-databox`. Check before you delete anything.
- **Namespace**: kind-up deploys into `default`; live installs usually
  use one (`helm install … -n databox`), so append `-n databox` to every
  kubectl command — or set it once:
  `kubectl config set-context --current --namespace=databox`.
- **Images**: side-loaded `localhost/...` dev tags become published
  `ghcr.io/hyperkubeorg/...` date-stamp tags, pinned by the chart
  ([kubernetes.md](kubernetes.md)).
- **Ports**: kind's localhost NodePort mappings become your own load
  balancer / Service exposure.
- **Storage**: kind's default StorageClass becomes a real one you choose
  (`--set storage.className=…`), and PVC deletion is data loss with no
  `make kind-up` to save you.
- **Scaling down** is never just lowering `replicaCount` — decommission
  first ([kubernetes.md § Scaling](kubernetes.md#scaling)).
