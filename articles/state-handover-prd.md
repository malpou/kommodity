# PRD: State Handover for Kommodity

> Status: draft v2, validated against the codebase (August 2026). This revision corrects the
> original draft with code-level findings; every correction is marked **[validated]** with file
> references. Companion design article: [`articles/001_state_handover.md`](001_state_handover.md).

---

## 1. Summary

Kommodity is a single Go binary that acts as an all-in-one Kubernetes/Cluster API management
plane: a combined API server, in-process CAPI controllers, Talos Linux providers, and security
services (network KMS, TPM attestation), with all state persisted in PostgreSQL via Kine (no
etcd).

Today there is no supported way to move that state between kommodity instances. This blocks the
standard bootstrap→management pivot, management-plane migration, fleet rebalancing, and —
critically — deprovisioning the last cluster in a self-managed deployment, which cannot delete
itself from within.

This PRD proposes **state handover**: a native, CAPI-contract-aware operation
(`kommodity handover`) that pauses reconciliation on a source instance, exports the
owner-reference graph of Cluster API objects plus dependent Secrets/KMS keys, imports them into a
target instance, and transfers sole reconciliation ownership — modeled on `clusterctl move`/pivot,
adapted to kommodity's Kine/PostgreSQL storage and Talos architecture.

## 2. Problem Statement

Operators cannot safely transfer kommodity's Cluster API state (Clusters, Machines,
TalosControlPlane/TalosConfig objects, infrastructure resources, kubeconfig/talosconfig Secrets,
KMS-key Secrets) between instances. Consequences:

1. **No pivot.** Standing up a production, self-hosted management plane from a bootstrap instance
   requires manual, error-prone steps.
2. **No migration.** Moving the management plane to new hardware, region, or jurisdiction (a
   data-sovereignty requirement) has no supported path.
3. **No per-cluster moves.** Individual workload clusters cannot be rebalanced or reassigned
   between management planes.
4. **The reverse chicken-and-egg.** A self-managed kommodity cannot delete its own hosting
   cluster — the management plane would destroy its own host mid-deletion, orphaning
   infrastructure.
5. **Split-brain risk.** Any ad-hoc approach (e.g., `pg_dump` copy) risks two instances
   reconciling the same live Talos clusters, and provides no atomicity for KMS keys that nodes
   require at boot.

## 3. Goals and Non-Goals

**Goals**

- G1: Safe, idempotent handover of the CAPI object graph and dependent Secrets/KMS keys between
  two kommodity instances.
- G2: Exactly one active reconciler per workload cluster at all times (no split-brain).
- G3: Support five topologies: bootstrap→management pivot; management→management migration;
  promotion of a workload cluster to management; per-cluster move between management planes;
  reverse pivot to an ephemeral instance for last-cluster deprovisioning.
- G4: Zero downtime for workload clusters and their applications; only management-plane control
  operations pause briefly.
- G5: Dry-run, pre-flight validation, rollback, progress observability, and full auditability.

**Non-Goals**

- Backup/restore or disaster recovery of a dead source (upstream explicitly warns move "has not
  been designed for being used as a backup/restore solution").
- PostgreSQL HA/replication for kommodity's own store.
- Application/PV migration (Velero's domain) or changes to workload-cluster etcd.
- Active-active / multi-writer management planes.

## 4. Definitions

| Term | Meaning in kommodity |
|---|---|
| **Bootstrap kommodity** | Temporary instance (laptop/CI, ephemeral PostgreSQL) used only to provision the first permanent cluster. |
| **Management kommodity** | Durable instance owning and reconciling a fleet of Talos workload clusters; state in durable PostgreSQL. |
| **Workload cluster** | Talos Kubernetes cluster managed via CAPI; accessed through `<cluster>-kubeconfig` / `<cluster>-talosconfig` Secrets. |
| **Self-managed kommodity** | Management plane whose binary and PostgreSQL run *on* a cluster it manages (the **self-hosting cluster**). |
| **State handover (pivot)** | Transferring ownership and active reconciliation of a CAPI object graph from source to target, source relinquishing control. |
| **Reverse pivot** | Full-state handover from a self-managed instance back to an **ephemeral teardown instance** so the last cluster can be deleted from outside itself, after which the ephemeral instance is discarded. |
| **Scoped handover** | Handover whose discovery root is one named `Cluster` object graph (per `clusterctl move --filter-cluster`), not the whole namespace. |

## 5. Users

- **Platform engineer** — performs initial pivot, migrations, per-cluster moves.
- **SRE / on-call** — migrates off failing hardware; needs dry-run and rollback.
- **Compliance officer** — requires audit trails, secret hygiene, data-residency controls.
- **Kommodity maintainer** — needs implementation to fit existing package structure and CAPI
  provider contracts.

## 6. User Stories

1. **Pivot:** Run a local bootstrap kommodity, provision the first Talos cluster, install
   kommodity there, hand over all state, discard the bootstrap.
2. **Migration:** Stand up a new management kommodity and hand over the whole fleet with zero
   workload downtime; decommission the old instance.
3. **Promotion:** Install kommodity into an existing workload cluster and hand over the fleet,
   converting it to a management plane.
4. **Per-cluster move:** Move one workload cluster from plane A to plane B for rebalancing or
   tenant reassignment, leaving the rest of A's fleet untouched and A fully operational.
5. **Last-cluster teardown:** Hand state back from a self-managed instance to a local ephemeral
   kommodity, delete the last cluster from outside, confirm infrastructure deprovisioned, discard
   the ephemeral instance and its database.
6. **Safety:** Get a dry-run showing exactly what would move and which preflight checks fail;
   roll back cleanly on partial failure; be *blocked* from deleting the self-hosting cluster from
   within.

## 7. Functional Requirements

### 7.1 Core handover engine

- **FR1 — Discovery.** Build the object set via the CAPI move contract: owner-reference graph
  rooted at `Cluster` objects, plus objects labeled `clusterctl.cluster.x-k8s.io/move` /
  `move-hierarchy`, including Talos control-plane/bootstrap configs,
  Machines/MachineDeployments/MachineSets, infrastructure resources, and dependent Secrets
  (kubeconfig, talosconfig, KMS keys, provider credentials).
  **[validated]** No CRD in a running kommodity carries any `clusterctl.cluster.x-k8s.io/*`
  label today: kommodity applies CRDs itself via `provider.Cache.ApplyCRDProviders`
  (`pkg/provider/provider.go`), and upstream release manifests do not carry the labels either —
  `clusterctl init` injects them at install time. clusterctl's `objectGraph.getCRDList` filters
  on the `clusterctl.cluster.x-k8s.io` label and would therefore discover **zero** types against
  a kommodity API server. Discovery requires either (a) stamping the move/move-hierarchy labels
  on CRDs during `ApplyCRDProviders`, or (b) kommodity-owned discovery seeded from
  `provider.Cache.GetProviderGroupResources()`. Decision: **(a) label at apply time**, so the
  upstream `ObjectMover` (importable from the already-pinned `sigs.k8s.io/cluster-api v1.10.10`
  module with no new module requirement) can be reused rather than re-implemented.
- **FR2 — Export.** Export spec + metadata (never Status — per the CAPI contract, Status is never
  restored on move), either directly to a target API or to an encrypted directory for air-gapped
  transfer. **[validated]** CRDs themselves must never be exported: kommodity mutates CRDs on
  apply (deprecated-version stripping, conversion webhook rewritten to
  `https://localhost:<WebhookPort>/convert`, per-instance caBundle from the
  `webhook-serving-cert` Secret), so an exported CRD set is instance-specific. The target always
  applies its own CRDs; handover moves objects only.
- **FR3 — Import.** Create-if-absent / update-if-present for namespaced objects;
  create-if-absent only for cluster-scoped objects; reconstruct owner references.
  **[validated]** Import order must create owners before dependents (or the embedded
  owner-reference garbage collector, `pkg/controller/garbagecollector.go`, must be quiesced on
  the target during import) — otherwise dependents imported before their owners are collected as
  orphans.
- **FR4 — Pause/resume.** Set `Cluster.Spec.Paused=true` on source; wait until no targeted
  object carries `clusterctl.cluster.x-k8s.io/block-move`; unpause on target after import;
  source never reconciles moved clusters again.
  **[validated — corrected]** The Talos providers are **upstream siderolabs releases, not
  forks** (`cluster-api-bootstrap-provider-talos v0.6.12`,
  `cluster-api-control-plane-provider-talos v0.5.13`; `go.mod` has no replace directives for
  them). Pause compliance audit of every in-process reconciler:
  - CAPI core (Cluster/Machine/MachineSet/MachineDeployment, `pkg/controller/reconciler/core.go`):
    full pause support (`paused.EnsurePausedCondition` + pause predicates).
  - CABPT `TalosConfigReconciler`: honors pause (predicate + in-reconcile `annotations.IsPaused`).
  - CACPPT `TalosControlPlaneReconciler`: **semantically paused but hot-requeues** — no watch
    predicate, and the in-reconcile check returns `ctrl.Result{Requeue: true}`, spinning the
    workqueue for as long as the object is paused. Correct for safety, unusable as a quiesce
    signal; "source is quiesced" must be defined as "no non-paused reconcile in progress", not
    "empty workqueue". An upstream fix should be pursued but is not a blocker.
  - KubeVirt v0.1.10, Scaleway v0.1.6, CAPZ v1.21.0, Docker: all honor pause. CAPZ additionally
    sets `clusterctl.cluster.x-k8s.io/block-move` on ASO resources.
  - Kommodity-owned reconcilers (CCM-CRS, Autoscaler, AzureCredentialMaterializer, SigningKey,
    talosproxy): all use `predicates.ResourceNotPaused`.
  - **`azurearm.Reconciler` (`pkg/controller/reconciler/azurearm/`): no pause handling at all**
    — it would keep issuing ARM PUTs on its 10-minute drift cadence from both instances during a
    move. Fixing this is a Phase 0 prerequisite (shipped with this PRD revision).
- **FR5 — Single-writer guarantee.** Exactly one instance owns each cluster after handover, with
  a defined commit/fence point spanning the two independent PostgreSQL/Kine stores.
  **[validated]** Note that kommodity configures **no leader election** in its controller
  manager (`pkg/controller/controller.go`) — nothing in the runtime prevents two instances from
  reconciling the same objects. The pause flag on the source is the *only* fence; the handover
  engine must treat it as such (verify paused-and-observed before unpausing the target).
- **FR6 — Idempotency.** Interrupted handovers converge correctly on re-run without duplicating
  objects.
- **FR7 — Preflight.** Validate target reachability/auth, CAPI core + provider version parity
  (target ≥ source), no in-flight rollouts, and operator-acknowledged data-residency for
  cross-jurisdiction moves.
- **FR8 — Rollback.** On failure before source relinquishes ownership: unpause source, remove
  partial target objects.
- **FR9 — Dry-run.** Print full object set and preflight results with zero mutation.
- **FR10 — KMS atomicity.** KMS-key Secrets move atomically with their owning cluster.
  **[validated — strengthened]** The KMS binding is stronger than "nodes must reach a live KMS
  endpoint". Keys live in one Secret per node, `talos-kms-<nodeUUID>` in `kommodity-system`
  (`pkg/kms/server.go`), with per-volume entries `<prefix>.key/.nonce/.luksKey` and a
  `sealedFromIP` field. The AES-GCM AAD is `nodeUUID || nonce || peerIP`, and `Unseal` refuses
  requests whose client IP (resolved from `X-Forwarded-For` → `X-Real-Ip` →
  `X-Envoy-External-Address` → TCP peer, `pkg/kms/client_ip.go`) differs from `sealedFromIP`.
  **A handover that changes the source IP nodes appear from makes every key un-unsealable even
  though the Secret moved perfectly.** Preflight must therefore verify IP-path equivalence (the
  target sees nodes at the same peer IPs), or the flow must re-seal volumes against the new path
  before cutover. The source KMS stays reachable until cutover is confirmed, and machine config
  is updated first if the endpoint changes.
- **FR11 — Talos proxy re-establishment.** Preserve the `kommodity.io/node-cidr` annotation and
  confirm the target can establish the `talos-cluster-proxy` tunnel before unpausing; abort if
  the target has no network path to the nodes.
  **[validated]** The proxy's CIDR registry is in-memory only
  (`pkg/talosproxy/cidr_registry.go`) and is rebuilt by the talosproxy reconciler from the
  `kommodity.io/node-cidr` Cluster annotation — the annotation is the real state (open question
  resolved). Ordering hazard: the talosproxy reconciler carries a `ResourceNotPaused` predicate,
  so an imported-still-paused Cluster gets **no CIDR registration on the target**; meanwhile
  CACPPT (no pause predicate) begins reconciling immediately and would dial private node IPs
  directly. Unpause ordering must ensure CIDR registration precedes controller dial-out.
  Also: the proxy reads `<cluster>-kubeconfig` from the hardcoded `default` namespace
  (`pkg/talosproxy/tunnel.go`) — a `--target-namespace` remap must account for it.
- **FR12 — Audit.** Every handover action flows through kommodity's Kubernetes audit-logging
  pipeline. **[validated]** The pipeline exists end-to-end (`pkg/server/audit.go`, enabled via
  `KOMMODITY_AUDIT_POLICY_FILE_PATH`); an engine operating through the API server on both ends
  inherits it with no extra work.
- **FR23 — Non-CAPI must-move state (new).** **[validated]** The following objects are not part
  of the CAPI owner graph but must move (or be consciously excluded) for a functioning target:
  - `talos-kms-<nodeUUID>` Secrets (per node, `kommodity-system`) — see FR10.
  - `attestation-report-<machineSuffix>` ConfigMap + Secret and
    `attestation-policy-<cluster>` ConfigMap (`pkg/attestation/rest/`).
  - `<cluster>-cluster-autoscaler-config` ConfigMap, `<cluster>-cluster-autoscaler`
    service-account-token Secret (namespace `default`), `<cluster>-ccm-secret`,
    `<cluster>-aso-secret`, `<release>-kommodity-extra-secrets`.
  - The **service-account signing-key Secret** (labeled `kommodity.io/managed-by=libkapi`,
    `pkg/libkapi/auth/key.go`): generated at startup and create-only; if it does not move, every
    workload cluster's SA tokens are rotated by `pkg/controller/reconciler/signingkey.go`.
  - The **`webhook-serving-cert` Secret** (`pkg/server/webhookcert.go`): CRD conversion
    caBundles persisted in the DB pin to it. On the target this must *not* be overwritten — the
    target keeps its own; the engine must instead let the target re-reconcile caBundles.
  - In-memory-only state that is deliberately *not* moved because it is reconstructible:
    attestation nonce store (TTL 5 min; in-flight attestations at cutover fail and retry),
    per-IP rate limiters, talosproxy tunnel pool, provider CRD cache, CAPI ClusterCache, GC
    discovery cache. Auto-bootstrap state is node-local/extension-owned and not in this repo
    (open question resolved).

### 7.2 Per-cluster move (Scenario 4)

- **FR13 — Scoping.** `--cluster=<name> --namespace=<ns>` roots discovery at a single Cluster;
  sibling clusters are excluded. Note: single-cluster move is *not* an upstream E2E-verified use
  case — kommodity owns correctness here.
- **FR14 — Shared-object handling.** Objects shared across clusters (provider identities, shared
  credential Secrets, ClusterClasses) are copied-not-moved with an explicit warning, never
  deleted from source; dry-run enumerates each one's disposition.
- **FR15 — Conflict detection.** Preflight checks the target for name/namespace collisions
  across the entire graph; abort with a diff and offer `--target-namespace` / `--rename`
  remapping (the move contract would otherwise silently update same-named target objects).
- **FR16 — Target prerequisites.** Verify the target has the specific infrastructure provider
  enabled at version ≥ source, required credentials, and node-CIDR reachability for this
  cluster.

### 7.3 Reverse pivot and teardown (Scenario 5)

- **FR17 — Self-host guardrail (P0).** Kommodity detects when a `Cluster` object corresponds to
  the cluster hosting its own binary/PostgreSQL and **blocks** deletion of that Cluster from
  within, with an error pointing to the reverse-pivot flow. This ships independently of, and
  before, the full flow. **[validated — design constrained]** CEL admission policies are
  explicitly disabled in kommodity (`pkg/server/admission.go` disables the
  ValidatingAdmissionPolicy plugins), so the guardrail is a **validating admission webhook**
  served by the existing in-process webhook server. The contract (binding for the future
  reverse-pivot flow): the marker is the **`kommodity.io/self-hosted` annotation** on the
  Cluster (presence-based — any value counts, so typos fail closed; stamped by the chart via
  `kommodity.selfHosted: true`, later automated by the pivot), OR'd with the
  **`KOMMODITY_SELF_HOSTED_CLUSTER=<namespace>/<name>`** environment backstop, which cannot be
  stripped through the Kubernetes API. Deletion — and removal of the marker annotation itself —
  is allowed only when the **`kommodity.io/allow-self-hosted-delete: "true"`** override
  annotation is present; setting it is an explicit, auditable act, surfaced as an admission
  warning. The webhook fails closed (`failurePolicy: Fail`); since webhook server and API
  server are one binary, the only "webhook down" window is startup. Shipped with this PRD
  revision.
- **FR18 — Handover to ephemeral instance.** Full-state reverse handover from the self-managed
  instance to a local/CI kommodity in `--teardown-mode` backed by ephemeral PostgreSQL.
- **FR19 — Capability verification (hard gate).** Before the ephemeral instance takes over: all
  providers at version ≥ source, valid infra credentials, reachability to the infra-provider API
  *and* to the Talos node CIDR, and a KMS endpoint nodes can reach **at the peer IP their keys
  are sealed to (FR10)**. Any failure aborts, leaving the self-managed instance authoritative.
  These are hard gates, not warnings — unreachable nodes/KMS are the primary cause of teardown
  deadlock (cf. CAPI #9954/#10544, Omni #2465).
- **FR20 — Safe deletion ordering.** Rely on CAPI's native ordering (workers → control-plane
  Machines → control plane object → infrastructure *last*); kommodity's job is to keep the
  ephemeral instance, its KMS, and the Talos proxy alive until the `cluster.x-k8s.io` finalizer
  clears and infrastructure is confirmed gone. KMS keys are not deleted until nodes are wiped.
- **FR21 — Discard gate.** `kommodity teardown-status` confirms zero clusters/machines remain
  before signaling that the ephemeral instance and its database are safe to discard.
- **FR22 — Deadlock escape hatch.** Stuck deletions surface with actionable diagnostics; a
  documented, flag-gated force-cleanup path (finalizer removal + manual infra cleanup) exists so
  operators are never stranded.

## 8. Non-Functional Requirements

- **Safety:** Atomic at the granularity of a cluster's object graph; abort per-cluster if pause
  cannot be confirmed.
- **Downtime:** Zero for workload clusters; brief, bounded unavailability of management control
  operations during cutover.
- **Consistency:** Export reads a quiesced view (controllers paused; optional read-only mode on
  source during final cutover).
- **Security:** OIDC-authenticated, admin-only; secrets transferred over TLS only, never logged;
  offline export directories treated as secret-bearing and encrypted.
- **Observability:** Structured logs, progress reporting, metrics (objects discovered/moved,
  duration, failures).
- **Conventions:** Follows CLAUDE.md standards (per-package `errors.go`, wrapped errors,
  testify, `pkg/config`); this PRD precedes code per the project's own process.
- **CLI:** **[validated]** kommodity currently has **no CLI layer** — `main()` is implicitly
  `serve` and configuration is env-var-only (`pkg/config`). `kommodity handover` implies
  introducing a command framework (cobra is already a transitive dependency); bare invocation
  must remain equivalent to `serve` to keep the container image, Makefile, and integration
  tests working.

## 9. Design Overview

**Recommended:** a native `pkg/handover` engine embedded in the binary, exposed as
`kommodity handover` (and later a declarative `StateHandover` CRD for GitOps). It operates
through the API server on both ends, making the Kine/PostgreSQL store transparent, and reuses
upstream clusterctl move discovery (`sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster`,
same module already at v1.10.10) enabled by stamping move labels on CRDs at apply time (FR1).
Because the Kine listen socket is process-local, the engine always talks to running kommodity
API servers over HTTPS — never to the database directly.

**Flow (all scenarios share the spine):** connect → preflight → discover → pause source →
export → import to target → verify (Talos proxy tunnel up, KMS reachable at the sealed peer IP,
control planes healthy) → cutover (unpause target; delete-from-source or keep-paused per
topology) → report; rollback on any pre-cutover failure.

**Alternatives rejected:** raw `pg_dump` copy (no split-brain protection, no scoping, carries
stale Status; kept only as an optional fast-path for full 1:1 migration); Talos etcd snapshots
(kommodity has no etcd); Velero (not CAPI-ownership-aware); Rancher-Turtles-style
import-by-label (doesn't transfer ownership); Gardener ShootState (assumes seed/shoot
architecture, too heavy).

**Prior art for the reverse pivot:** Oracle OCNE spins up an ephemeral cluster to delete a
self-managed cluster; D2iQ Konvoy requires moving lifecycle services to a bootstrap cluster
before self-managed deletion; EKS Anywhere's `delete cluster` creates a management cluster,
moves state, then deletes. The proposed flow follows this proven pattern.

## 10. CLI Sketches

```bash
# Pivot / migration (full state)
kommodity handover --to-kubeconfig=mgmt.kubeconfig --namespace=clusters --dry-run
kommodity handover --to-kubeconfig=mgmt.kubeconfig --namespace=clusters

# Air-gapped
kommodity handover --to-directory=./state        # source
kommodity handover --from-directory=./state      # target

# Per-cluster move
kommodity handover --cluster=tenant-x --namespace=fleet \
  --from-kubeconfig=A.kubeconfig --to-kubeconfig=B.kubeconfig \
  --target-namespace=fleet-b --on-conflict=abort

# Reverse pivot + last-cluster teardown
kommodity serve --db-uri=postgres://localhost/ephemeral --teardown-mode
kommodity handover --from-kubeconfig=self-managed.kubeconfig \
  --to-kubeconfig=ephemeral.kubeconfig --reverse --verify-capability
kubectl --kubeconfig=ephemeral.kubeconfig delete cluster last-cluster
kommodity teardown-status --kubeconfig=ephemeral.kubeconfig   # blocks until zero objects

# Rollback
kommodity handover --rollback --to-kubeconfig=target.kubeconfig
```

## 11. Success Metrics

- 100% of discovered objects and dependent Secrets/KMS keys correctly owned on target; **zero
  split-brain incidents**.
- Zero workload-cluster downtime across cutover (synthetic checks).
- ≥99% handover success in CI E2E (KubeVirt + Scaleway); every failure rolls back cleanly.
- Last-cluster teardown completes end-to-end with no orphaned infrastructure.
- Bounded, documented cutover window for a reference fleet size; dry-run used before ≥90% of
  production handovers; all handovers audited.

## 12. Risks and Open Questions

**Risks:** CACPPT pause hot-requeue makes quiesce detection noisy (upstream fix desirable);
mid-teardown loss of KMS leaving nodes unable to boot/unseal; **KMS peer-IP binding breaking
silently after an ingress-path change (FR10)**; finalizer deadlocks on unreachable nodes/APIs;
ephemeral host behind NAT reaching the cloud API but not private node CIDRs; version skew;
shared-credential duplication across planes after per-cluster moves; Status loss for any
non-reconstructible state; no leader election means the pause fence is the only split-brain
protection (FR5).

**Open questions (remaining):** reliable self-host identification *corroboration* across
providers (node UUID vs. CIDR — the annotation marker ships first, corroboration hardens it);
merge/conflict policy for populated targets; default deadlock timeout and force-cleanup policy;
source read-only mode during cutover.

**Resolved by validation:** the Talos-proxy CIDR registry is reconstructible from the
`kommodity.io/node-cidr` annotation; auto-bootstrap extension state is node-local and not in
this repo; the complete catalog of non-CAPI must-move state is FR23; providers are upstream
releases, not forks.

**Dependencies:** CAPI v1.10.x move contract (`sigs.k8s.io/cluster-api v1.10.10` in go.mod);
upstream Talos providers honoring `Spec.Paused` (verified, with the CACPPT requeue caveat);
OIDC auth, audit logging, Talos proxy, KMS/attestation services; PostgreSQL + Kine on both
ends.

## 13. Milestones

| Phase | Scope | Exit criteria |
|---|---|---|
| **0 — Design** | PRD validated against code (this revision); provider pause/label compliance audited; state cataloged (FR23); **self-host delete guardrail (FR17) shipped standalone as P0**; azurearm pause gap fixed | PRD merged, guardrail merged, azurearm paused-aware |
| **1 — Offline MVP** | `pkg/handover`; directory export/import; discovery (CRD move-labels at apply time); dry-run; preflight; audit | Fleet state exported and imported into empty target; KubeVirt E2E green |
| **2 — Live pivot/migration** | Direct source→target with pause/resume, single-writer fence, KMS atomicity + IP-path verification, Talos-proxy re-establishment, idempotent retry | Zero-downtime bootstrap→management pivot on Scaleway + KubeVirt |
| **3 — Scoped moves** | `--cluster` scoping, conflict detection/remap, shared-object classification, per-cluster rollback | One cluster moved between two live planes, rest of fleet untouched |
| **4 — Reverse pivot & teardown** | `--reverse`, `--teardown-mode`, capability hard gates, teardown-status, force-cleanup path, promotion flow, `StateHandover` CRD | Last self-managed cluster decommissioned end-to-end, no orphaned infra |
| **5 — Hardening/GA** | Chaos tests (mid-teardown network loss, KMS outage), cross-jurisdiction runbooks, security review, docs | GA criteria per semver stability commitments |

## 14. Caveats

Kommodity is experimental and pre-1.0; package names, CRD groups, and CLI flags here are
proposals. The design leans on the upstream move contract, which is only E2E-validated for the
bootstrap use case — kommodity must maintain its own E2E suites for migration, per-cluster, and
reverse-pivot topologies. The state catalog (FR23) is the code-level confirmation the original
draft called for; it should be re-audited whenever a new reconciler or service adds persisted or
in-memory state.
