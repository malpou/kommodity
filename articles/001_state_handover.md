# State Handover: How Kommodity Will Move a Management Plane Without Losing a Cluster

*Kommodity packs a whole Cluster API management plane into one binary backed by PostgreSQL. That
makes it easy to run — and, until now, impossible to move. This article walks through how state
handover is envisioned to be implemented in the actual codebase: what moves, what must never
move, the traps we found by reading our own code, and the guardrail that ships first. It is
written to be discussed, challenged, and reviewed.*

> Status: design article, written against the validated PRD in
> [`state-handover-prd.md`](state-handover-prd.md). The self-hosted deletion guardrail (FR17)
> and the azurearm pause fix described in §7 are implemented; everything else is roadmap.
> Cross-reference [`000_baseline.md`](000_baseline.md) for the narrative overview of Kommodity
> itself.

---

## 1. The Problem: A Management Plane That Cannot Let Go

Kommodity's pitch is "one port, one executable, PostgreSQL for state". A single binary serves
the Kubernetes API (via `k8s.io/apiserver` + Kine), runs the CAPI and Talos provider
controllers in-process, and hosts the KMS, attestation, and metadata services that Talos nodes
depend on to boot.

That single-binary design has a blind spot: there is no supported way to move the state — the
Clusters, Machines, `TalosControlPlane`/`TalosConfig` objects, infrastructure resources, and
the Secrets they depend on — from one Kommodity instance to another. Four operations are
blocked as a result:

1. **The pivot.** Every Cluster API deployment starts with a bootstrap management plane
   (a laptop, a CI job) that provisions the first real cluster, then hands control to a
   Kommodity instance running *on* that cluster. Upstream has `clusterctl move` for this;
   Kommodity has nothing.
2. **Migration.** Moving the management plane to new hardware, a new region, or a new
   jurisdiction — a hard requirement in the data-sovereignty world Kommodity targets.
3. **Per-cluster rebalancing.** Moving one workload cluster between two management planes.
4. **The reverse chicken-and-egg.** A self-managed Kommodity cannot delete the cluster it runs
   on. Mid-deletion, the management plane would destroy its own API server and database while
   CAPI finalizers still need both — orphaning cloud infrastructure. This is a known,
   painful failure mode across the ecosystem (see CAPI issues #9954/#10544 and Sidero Omni
   #2465), and every serious product solves it the same way: move the state *out* first.

And hovering over all four: **split-brain**. Copy the database naively (`pg_dump` to a second
instance) and two sets of controllers now reconcile the same live Talos clusters — both
holding valid credentials, both issuing machine config, both talking to the same cloud APIs.

## 2. What We Actually Have (a Code-Level Inventory)

Before designing anything we audited the codebase. The findings drive every design decision
below, so here they are up front.

### 2.1 Everything lives in one place — almost

All Kubernetes objects, Secrets and ConfigMaps included, are persisted through Kine into
PostgreSQL (`pkg/kine`). Kommodity defines **no CRDs of its own** — every CRD under
`pkg/provider/crds/` is vendored from CAPI, the Talos providers, CAPZ, KubeVirt, or Scaleway,
and is applied at startup by `provider.Cache.ApplyCRDProviders` (`pkg/provider/provider.go`).

The in-memory state we found is all reconstructible:

| In-memory state | Where | Rebuilt from |
|---|---|---|
| Talos proxy CIDR registry | `pkg/talosproxy/cidr_registry.go` | the `kommodity.io/node-cidr` Cluster annotation, via `pkg/talosproxy/reconciler.go` |
| Talos proxy tunnel pool | `pkg/talosproxy/tunnel_pool.go` | re-established on demand from `<cluster>-kubeconfig` |
| Attestation nonce store | `pkg/attestation/rest/store.go` | nodes simply request a new nonce (TTL is 5 minutes) |
| Rate limiters, CRD cache, cluster caches, GC discovery cache | various | rebuilt at startup |

This resolves one of the PRD's open questions: **the annotation is the real state**; nothing
non-reconstructible lives outside PostgreSQL.

### 2.2 The must-move list is longer than the CAPI graph

The CAPI owner-reference graph (Cluster → Machines → TalosConfigs → infra resources → bootstrap
data Secrets) is the backbone, but a working target also needs:

- **KMS key Secrets** — one per node, `talos-kms-<nodeUUID>` in `kommodity-system`
  (`pkg/kms/server.go`), holding per-volume LUKS key material.
- **Attestation reports and policies** — `attestation-report-<machine>` ConfigMap+Secret,
  `attestation-policy-<cluster>` ConfigMap (`pkg/attestation/rest/`).
- **Reconciler-created per-cluster objects** — autoscaler config/token Secrets, CCM credential
  Secrets, Azure `-aso-secret`s (`pkg/controller/reconciler/`).
- **The service-account signing key** (`pkg/libkapi/auth/key.go`). This one is subtle: it is
  generated at startup and persisted create-only. If it does not move, the target generates a
  fresh key and `pkg/controller/reconciler/signingkey.go` rotates every workload cluster's SA
  tokens — surprise churn across the fleet.
- **Not** the `webhook-serving-cert` Secret (`pkg/server/webhookcert.go`): each instance keeps
  its own, because the CRD conversion-webhook caBundles persisted in the database are pinned to
  it. Likewise CRDs themselves never move — Kommodity mutates them on apply (conversion webhook
  rewritten to `https://localhost:<port>/convert`, per-instance caBundle), so an exported CRD
  is instance-specific by construction. The target applies its own CRDs; handover moves
  objects only.

### 2.3 The KMS keys are bound to more than the endpoint

This is the finding that most changed the design. The obvious worry is "nodes must reach a KMS
endpoint at boot". The actual constraint is stronger: `pkg/kms/server.go` seals each volume key
with AES-GCM whose AAD is `nodeUUID || nonce || peerIP`, records the sealing IP in the Secret
(`sealedFromIP`), and `Unseal` refuses any request whose client IP (resolved from
`X-Forwarded-For` → `X-Real-Ip` → `X-Envoy-External-Address` → TCP peer,
`pkg/kms/client_ip.go`) differs from it.

**Consequence:** a handover that changes the source IP nodes appear from — a different ingress
path, a NAT change, a new load balancer — makes every existing key un-unsealable *even though
the Secret moved perfectly*. Handover preflight must verify IP-path equivalence, not endpoint
reachability; where the path must change, volumes have to be re-sealed against the new path
before cutover. Encrypted machines that cannot unseal do not boot. There is no recovering from
getting this wrong mid-teardown.

### 2.4 Pause compliance: good news, two gaps

`clusterctl move`'s safety model is simple: set `Cluster.spec.paused` on the source, wait for
quiesce, move, unpause on the target. That only works if *every* in-process reconciler honors
pause. We audited all of them:

- **CAPI core** (Cluster, Machine, MachineSet, MachineDeployment — `pkg/controller/reconciler/core.go`):
  full pause support, predicates and `paused.EnsurePausedCondition`. ✅
- **Talos bootstrap provider (CABPT v0.6.12)**: pause predicate + in-reconcile check. ✅
- **Talos control-plane provider (CACPPT v0.5.13)**: honors pause *semantically* but returns
  `ctrl.Result{Requeue: true}` from its pause check and has no watch predicate — a paused
  TalosControlPlane hot-loops the workqueue forever. Safe, but "the queue is empty" can never
  be the quiesce signal. ⚠️ (Both Talos providers are **upstream releases, not forks** —
  `go.mod` carries no replace directives for them — so fixing this means an upstream PR, not a
  local patch.)
- **KubeVirt v0.1.10, Scaleway v0.1.6, CAPZ v1.21.0**: all honor pause. CAPZ even annotates its
  ASO resources with `clusterctl.cluster.x-k8s.io/block-move`. ✅
- **Kommodity's own reconcilers** (CCM-CRS, autoscaler, Azure credential materializer, signing
  key, talosproxy): all filter on `predicates.ResourceNotPaused`. ✅
- **The embedded Azure ARM reconciler (`pkg/controller/reconciler/azurearm/`)**: had **no pause
  handling at all** — it would have kept issuing ARM PUTs and DELETEs on its 10-minute drift
  cadence from both instances throughout a handover. ❌ → fixed as part of this work (§7.2).

### 2.5 The move labels don't exist

`clusterctl move` discovers what to move by listing CRDs labeled
`clusterctl.cluster.x-k8s.io` and reading `move`/`move-hierarchy` labels. Those labels are
injected by `clusterctl init` — which Kommodity never runs, because it applies CRDs itself.
Result: against a Kommodity API server, upstream discovery finds **zero types**. Reusing
upstream's `ObjectMover` (importable from `sigs.k8s.io/cluster-api/cmd/clusterctl/client/cluster`,
already pinned at v1.10.10 in `go.mod`) therefore requires stamping the move labels onto CRDs
during `ApplyCRDProviders` — a small, deliberate change that unlocks a well-tested engine
instead of a bespoke reimplementation.

### 2.6 Two more sharp edges

- **No leader election.** `pkg/controller/controller.go` configures none. Nothing in the
  runtime prevents two instances from reconciling the same objects; the pause flag is the
  *only* fence, and the handover engine must treat it as such.
- **The embedded garbage collector** (`pkg/controller/garbagecollector.go`) deletes orphans by
  owner reference. Import must create owners before dependents (or quiesce the GC on the
  target during import), or freshly imported dependents get collected before their owners
  arrive.
- **The talosproxy pause predicate bites on the target.** An imported Cluster arrives paused,
  so the talosproxy reconciler ignores it and registers no CIDR — while CACPPT (no pause
  predicate, §2.4) starts reconciling immediately and dials private node IPs directly. Unpause
  ordering must register the proxy CIDR before controllers dial out.

## 3. The Design: A Native Engine, Through the Front Door

The proposal is a `pkg/handover` engine embedded in the binary, exposed as
`kommodity handover` (Kommodity currently has *no* CLI layer at all — `main()` is implicitly
`serve` — so this brings cobra in, with bare invocation staying `serve` for compatibility with
the container image and Makefile).

The engine talks to **running Kommodity API servers on both ends, over HTTPS**. It never
touches PostgreSQL directly: the Kine listen socket is process-local, the API server is where
admission, audit, and RBAC live, and going through the front door makes the storage layer
irrelevant to correctness. It reuses upstream clusterctl discovery and move (§2.5) rather than
re-implementing the object-graph walk.

Every topology shares one spine:

```
connect → preflight → discover → pause source → export → import to target
       → verify (proxy tunnel up, KMS reachable at the sealed peer IP, control planes healthy)
       → cutover (unpause target; delete-from-source or keep-paused per topology)
       → report        (rollback on any pre-cutover failure)
```

The five topologies are the PRD's five user stories: bootstrap pivot, full migration,
promotion of a workload cluster, scoped per-cluster move (`--cluster=<name>`), and the reverse
pivot for last-cluster teardown (`--reverse --teardown-mode`, with capability verification as
a hard gate — an ephemeral instance that cannot reach the nodes or serve KMS at the right IP
must never take ownership).

**Alternatives we rejected:** raw `pg_dump` copy (no split-brain fence, no scoping, carries
stale Status); Velero (not ownership-aware); Rancher-Turtles-style import-by-label (imports,
doesn't transfer ownership); Gardener ShootState (assumes a seed/shoot architecture Kommodity
doesn't have). The reverse-pivot pattern follows proven prior art: Oracle OCNE, D2iQ Konvoy,
and EKS Anywhere all delete self-managed clusters by first moving state to an ephemeral
external management plane.

## 4. Why the Guardrail Ships First

Everything in §3 is roadmap. One piece could not wait: **nothing stops a self-managed
Kommodity from deleting its own hosting cluster today.** One `kubectl delete cluster` — a typo,
a mis-targeted kubeconfig, an over-eager GitOps prune — and the management plane starts
tearing down its own host, with orphaned infrastructure as the best-case outcome.

Upstream deliberately leaves this unsolved (the CAPI self-hosted docs essentially say "don't do
that"), which is exactly why a product that *supports* self-managed deployment has to solve it.
So FR17 — block deletion of the self-hosting cluster, point the operator at the reverse-pivot
flow — ships standalone, before any handover code. §7 describes what was built.

## 5. What "Done" Looks Like

The success metrics from the PRD, unchanged: every discovered object and dependent Secret
correctly owned by the target with zero split-brain incidents; zero workload-cluster downtime
across cutover; ≥99% handover success in CI E2E (KubeVirt + Scaleway) with clean rollback on
every failure; last-cluster teardown that leaves nothing orphaned; dry-run adopted as the norm
before production handovers, every handover audited through the existing pipeline
(`pkg/server/audit.go` — the engine inherits it for free by operating through the API server).

## 6. The Milestones

| Phase | Ships | Exit criteria |
|---|---|---|
| **0 — Design** *(this work)* | Validated PRD; pause/label audit; state catalog; **FR17 guardrail**; azurearm pause fix | PRD merged, guardrail merged |
| **1 — Offline MVP** | `pkg/handover`, directory export/import, discovery via CRD move-labels, dry-run, preflight | fleet exported → imported into empty target; KubeVirt E2E green |
| **2 — Live pivot/migration** | pause/resume spine, single-writer fence, KMS IP-path verification, proxy re-establishment | zero-downtime bootstrap→management pivot on Scaleway + KubeVirt |
| **3 — Scoped moves** | `--cluster` scoping, conflict detection, shared-object classification | one cluster moved between live planes, fleet untouched |
| **4 — Reverse pivot & teardown** | `--reverse`, `--teardown-mode`, capability hard gates, teardown-status, force-cleanup escape hatch | last self-managed cluster decommissioned, nothing orphaned |
| **5 — Hardening/GA** | chaos tests (mid-teardown KMS loss, network partition), runbooks, security review | GA per semver commitments |

## 7. What Ships Now: The Guardrail, In Detail

### 7.1 Blocking self-hosted cluster deletion (FR17)

**Mechanism.** A validating admission webhook. Kommodity explicitly disables the CEL
`ValidatingAdmissionPolicy` plugins (`pkg/server/admission.go`), so a webhook is the only
admission path — and the plumbing already exists: the controller-runtime webhook server that
serves the CAPI/Talos/CAPZ webhooks in-process, with its serving cert persisted and its
manifests applied (and rewritten to `https://localhost:<WebhookPort>`) by
`Cache.ApplyWebhookProviders`.

The new pieces:

- `pkg/controller/webhook/selfhosted.go` — `SelfHostedClusterValidator`, registered on the
  shared webhook server at `/validate-kommodity-io-self-hosted-cluster`, DELETE-only, on
  `cluster.x-k8s.io/v1beta1` Clusters.
- `pkg/provider/kommodity/kommodity-validating-webhook-configuration.yaml` — the webhook
  manifest, in a **new embed directory**. It cannot live in `pkg/provider/webhooks/` because
  `scripts/fetch-providers.sh` wipes that directory on every `make generate`; the new
  directory is loaded by the same cache (`loadWebhookDir` in `pkg/provider/provider.go`) and
  survives regeneration. `failurePolicy: Fail` — the webhook and the API server are the same
  binary, so "webhook down, API server up" only exists in the startup window, and failing
  closed there is exactly what a guardrail should do.

**The marker.** The PRD imagined a marker "written at pivot time" — but no pivot flow exists
yet, so the marker has to work today. Two markers, OR'd:

1. The **`kommodity.io/self-hosted: "true"` annotation** on the Cluster, stamped by the Helm
   chart when `kommodity.selfHosted: true` is set. Annotations travel with the object through
   a move, so when the pivot flow exists this *becomes* the pivot-time marker with no schema
   change.
2. The **`KOMMODITY_SELF_HOSTED_CLUSTER=<namespace>/<name>` environment variable**
   (`pkg/config`). An operator-level backstop that cannot be stripped through the Kubernetes
   API, and that protects deployments created before the chart change.

**The override.** Deletion is allowed despite the markers when the Cluster carries
`kommodity.io/allow-self-hosted-delete: "true"` — an explicit, auditable act (surfaced as an
admission warning). Today a human sets it for a deliberate teardown; the reverse-pivot flow
will set it in-process only after verifying the management plane no longer runs on the
cluster. The override deliberately bypasses the env-var marker too, so a reverse pivot never
needs an env change and restart; the trade-off (someone with Cluster-update RBAC can annotate
then delete) is accepted and documented — the same RBAC could remove the annotation marker
anyway, and the admission check is atomic against the stored object, so there is no TOCTOU
window beyond RBAC itself.

### 7.2 The azurearm pause fix

The pause audit (§2.4) found the embedded Azure ARM reconciler reconciling straight through
pause. The fix follows the upstream idiom exactly: a `predicates.ResourceNotPaused` event
filter at watch level, plus an in-reconcile check that consults the
`cluster.x-k8s.io/paused` annotation on the ASO object itself and `spec.paused` on the owning
CAPI Cluster — resolved via the `cluster.x-k8s.io/cluster-name` label CAPZ stamps on every ASO
resource it creates. The check runs before the deletion branch, so a paused handover stops ARM
DELETEs too, and a paused resource simply returns without requeueing (unpausing the Cluster
generates the event that wakes everything up again — the upstream convention).

Without this, "pause the source" would have been a lie on Azure: both instances would have
kept writing to ARM through the entire handover window.

## 8. Open Questions (Bring Opinions)

These are the things this article is *for* — the decisions we want challenged before Phase 1:

1. **Self-host corroboration.** The annotation + env var mark the cluster; should the guardrail
   also corroborate at admission time (node UUID? node CIDR overlap with the instance's own
   address?) to catch a mislabeled fleet, at the cost of complexity and failure modes?
2. **KMS IP-path changes.** When a migration *must* change the ingress path, is re-sealing
   volumes against the new path (nodes online, staged re-seal) acceptable, or do we need
   multi-IP sealing in the KMS itself?
3. **Populated targets.** The move contract silently updates same-named objects on the target.
   Abort-on-conflict is the proposed default — is opt-in merge ever needed?
4. **Quiesce definition.** Given CACPPT's hot-requeue-while-paused (§2.4), quiesce must be
   "paused observed and no non-paused reconcile in flight". Is an upstream fix worth pursuing
   first?
5. **Deadlock policy.** Default timeout before a stuck teardown surfaces the force-cleanup
   path, and how loud that path should be.

If you have war stories about `clusterctl move`, self-managed teardowns, or KMS migrations —
this is the review to bring them to.

---

*The validated PRD with the full functional requirements lives at
[`articles/state-handover-prd.md`](state-handover-prd.md). The guardrail implementation is in
`pkg/controller/webhook/selfhosted.go`, `pkg/provider/kommodity/`, and the
`kommodity.selfHosted` chart value; the pause fix is in
`pkg/controller/reconciler/azurearm/reconciler.go`.*
