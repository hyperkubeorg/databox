# Builds — CI/CD

Builds runs repository pipelines in containers on a paired **runner** and produces
**releases**. It layers onto Git Services: builds are repo tabs, triggering and
viewing route through git roles, and logs/artifacts store in databox. Real compute
is gated — Builds is off until an admin enables it and names who may spend compute.

## Enabling

1. **Services → Features**: enable **Git**, then enable **Builds** (it requires
   Git). Both are off by default.
2. **Admin → Builds**: set the compute **access policy** (allowlist by default —
   name the users/orgs/teams/repos that may trigger builds; or switch to
   *everyone*), the **retention** window (default 15 days), and pair at least one
   **runner**.

## Runners

A runner is a separate process (`pcp-runner`), never the `pcp` binary. It **dials
PCP** and holds a persistent session over which PCP pushes jobs — so a runner
behind a firewall (a Kubernetes cluster, a bare-metal box) needs no inbound ports.
Two executors, one binary:

- **Kubernetes** — runs in-cluster and creates one Pod per pipeline phase.
- **Bare metal** — requires `podman` or `docker`; runs each phase as a container.

### Pairing a runner

1. **Admin → Builds → pair a runner**: name it; PCP shows a `PCPBR1.…` setup code.
2. Give the code to the runner:
   - **Kubernetes**: `helm install pcp-runner oci://ghcr.io/hyperkubeorg/charts/pcp-runner --set pairing.setupCode=PCPBR1.…`
     (image: `ghcr.io/hyperkubeorg/pcp-runner`, pinned by the chart)
   - **Bare metal**: `pcp-runner setup` (paste the code), then `pcp-runner run`.
3. The runner prints a `PCPBR2.…` completion code; paste it back into the wizard.
   The runner reports active with its capacity.

Each runner has a **concurrency cap** (max simultaneous containers/Pods) set on its
detail page; PCP never dispatches beyond it. A repo or org can use a system runner
or pair its own.

## The pipeline file — `.pcp-builder.yaml`

At the repo root. Declares `env` (with `${{SECRET}}` references to repo/org
secrets), named `phases` (each an image + ordered steps, optional declared
`artifacts` outputs and `inputs`), and a `pipeline` that wires phases into a DAG
via boolean `requiresPhase` expressions. See PROJECT-DRAFT-003.md §5 for the full
schema. Parse or validation errors (unknown phase, dependency cycle, an input not
produced by a dependency) fail the build with the error as its log.

## Secrets

Set in the repo (or org) Builds settings. PCP seals each secret to the assigned
runner's key and stores only ciphertext — PCP cannot read it; only the paired
runner opens it at dispatch and injects it as container env (masked in logs).
Changing a scope's runner invalidates its secrets (re-enter them).

## Builds and releases

In-progress builds stream per-phase, per-step status and logs. A build can be
**cancelled** or **retried**; a *cancelled* build can be **deleted**, purging its
logs and artifacts (the remedy if a secret leaked into a log). A successful
build's artifacts promote to an immutable, git-tagged **release** with notes —
the durable output. Non-release build logs and artifacts are swept after the
retention window; releases are exempt.

## Cleanup and purge

The retention worker reaps terminal builds' logs and artifacts past
`RetentionDays`. To wipe all Builds data, use **Services → Builds → purge**.

---

Runtime note: the runner, its executors, the buildwire transport, and the
dispatch/cleanup loops are validated by build and unit tests; end-to-end execution
against a real cluster or container engine is exercised during deployment testing.
The buildwire listener binds `PCP_BUILDWIRE_ADDR` (default `:4223`); remote runners
reach it through a cloudferry TCP relay (dev loop: `make relay-build`).
