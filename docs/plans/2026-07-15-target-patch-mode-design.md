<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# Target-Cluster Patch Mode (GRM-free direct webhook/CRD caBundle stamping)

**Date:** 2026-07-15
**Status:** Proposed
**Upstream issue:** [SAP-cloud-infrastructure/webhook-injector#13](https://github.com/SAP-cloud-infrastructure/webhook-injector/issues/13)
**CC tracking:** [cc/unified-kubernetes#1335](https://github.wdf.sap.corp/cc/unified-kubernetes/issues/1335) (dep of `dual-deployment-operator` #1294, epic #831)
**History:** revives the target-cluster label-watch reconciler from [PR #10](https://github.com/SAP-cloud-infrastructure/webhook-injector/pull/10), which was removed in [PR #11](https://github.com/SAP-cloud-infrastructure/webhook-injector/pull/11) when the model collapsed to ConfigMap + ManagedResource.

## Problem

`dual-deployment-operator` (the operator successor to the `-remote` wrapper charts) is going **GRM-free**: it renders each source twice (host + remote) and applies both halves **directly** via server-side apply. On the shoot, the operator and webhook-injector keep **disjoint** writes:

- **Operator** (SSA, field manager `dual-deployment-operator`): CRDs, ClusterRoles/Bindings, Roles/Bindings, ServiceAccounts.
- **Injector**: webhook `caBundle` + cert lifecycle.

webhook-injector today has exactly two paths, and **neither can keep `caBundle` in sync on objects that live directly on the target cluster**:

| Path | Reconciler | Reads | Stamps | Direct-apply target objects? |
|---|---|---|---|---|
| `--webhook-config-name` | `WebhookReconciler` | host ConfigMap (`webhooks.yaml`) | MWC/VWC on target | Authors MWC/VWC **from a ConfigMap**, not from labeled target objects; never CRDs |
| `--managed-resource-label` | `ManagedResourceReconciler` | labeled `ManagedResource` Secrets on seed | CRDs **inside MR Secrets** | ❌ only CRDs wrapped in a `ManagedResource` |

A GRM-free consumer that applies MWC/VWC **and** CRDs directly to the shoot fits neither path:
- Its CRDs are not inside a `ManagedResource` → the MR reconciler never fires.
- It may not want to hand-author MWC/VWC into the injector's source ConfigMap, or may apply them directly too.

**Net:** conversion-webhook `caBundle` on directly-applied CRDs is unmanaged, and there is no way to keep `caBundle` in sync on directly-applied MWC/VWC without routing them through the source ConfigMap.

### Why this was removed, and why the removal doesn't cover this case

The capability existed. Issue #9 asked for a target-cluster patch mode; PR #10 implemented a target-cluster **Watch/patch** reconciler (labeled MWC/VWC/CRD) + an admission-webhook bootstrap; PR #11 **deleted** all of it, collapsing to ConfigMap + ManagedResource — [because stamping a shoot-side object directly thrashes against GRM](https://github.com/SAP-cloud-infrastructure/webhook-injector#gardener-seed-deployment): *"GRM would overwrite the stamp on its next sync."*

That rationale is sound **when GRM owns the object**. It does **not** apply when there is no GRM — exactly the `dual-deployment-operator` topology. PR #11 optimized for the GRM case and, as a side effect, removed the only viable path for the GRM-free case. This design brings the **Watch/patch reconciler** back (not the admission mutator), scoped by a label so it never touches GRM-owned objects.

## Solution

Add an **opt-in patch mode**, gated by a new `--target-crd-label` flag, implemented as a new leader-only `TargetWebhookReconciler` that **watches labeled objects on the target cluster** and keeps their `caBundle` in sync:

- **CustomResourceDefinition** with `spec.conversion.strategy=Webhook` → patch `spec.conversion.webhook.clientConfig.caBundle`.
- **MutatingWebhookConfiguration** → patch each `webhooks[*].clientConfig.caBundle`.
- **ValidatingWebhookConfiguration** → patch each `webhooks[*].clientConfig.caBundle`.

**Architecture:**
```
--target-crd-label=key=value set →
  1. Build a second, label-scoped cluster.Cluster handle on the TARGET cluster.
  2. Register TargetWebhookReconciler (leader-only): three controllers
     (target-crd, target-mwc, target-vwc), each watching the labeled kind.
  3. Each reconcile: live-Get the object, stamp caBundle from the hosting
     cert Secret (via APIReader), Patch (MergeFrom) ONLY caBundle; skip when
     already converged.
  4. On cert rotation: fan out a signal → re-list every labeled object and
     enqueue, so the new caBundle propagates in seconds.
  5. CRDs additionally publish to the shared rotation Gate so staged→active
     promotion waits for target propagation.
--target-crd-label unset → reconciler + target cache not registered →
  behavior byte-for-byte unchanged.
```

**Scope of writes (deliberately minimal):**
- Patches **only** `caBundle` (CRD conversion clientConfig; MWC/VWC per-webhook clientConfig).
- **No** create / delete / replace of any object. Only pre-existing labeled objects are stamped.
- **No** Service→URL rewrite. *(Decision — see below.)*
- **No** admission-webhook bootstrap mutator. *(Decision — see below.)*

### Design decisions (locked)

1. **Watch-based, with a target-cluster cache** — recommended and chosen. Earlier, for a CRD-only path, poll-on-sync was acceptable because a CRD's conversion webhook is not consulted until a non-storage-version custom resource is read/written, so a `--sync-period` (default 5m) delay before first stamp was tolerable. **MWC/VWC change this:** an admission webhook with `failurePolicy: Fail` and a stale/empty `caBundle` blocks *every* matching create/update the instant the object exists — a 5-minute first-stamp gap is a 5-minute outage window. The apiserver also self-exempts admission webhooks on `admissionregistration.k8s.io/{mutating,validating}webhookconfigurations`, so an admission hook cannot cover MWC/VWC — a **Watch is the only correct mechanism**. This is exactly what PR #10 built and validated on real clusters (`a-qa-de-200`/`m-qa-de-200`). The cost is one additional label-scoped `cluster.Cluster` cache on the target; it is registered **only** when the flag is set.

2. **caBundle-only, no Service→URL rewrite.** The MR path optionally rewrites Service-form `clientConfig` to URL-form via `webhookhost.BuildExternalURL` + `--external-host`/`--external-port`. The direct-apply consumer already emits correct `clientConfig`, so patch mode keeps **only** `caBundle` in sync and never touches `clientConfig.service`/`clientConfig.url`. This narrows the write surface and removes any interaction with `--external-host`/`--external-port`. (Rewrite can be added later if a consumer needs it.)

3. **No admission-webhook bootstrap.** PR #10 also shipped an in-process admission mutator + a bootstrap `MutatingWebhookConfiguration` (`webhook-injector-mutator`) that stamped CRDs synchronously at admission time (`pkg/admission/*`). It is **out of scope** here: issue #13 explicitly excludes it, it was the heaviest and most failure-prone piece (`failurePolicy`, self-exemption edge cases, extra `--admission-*` flags), and the Watch reconciler is a complete safety net on its own. The rotation Gate + Watch give the same propagation guarantees without it.

4. **`--webhook-config-name` becomes optional.** Today it is required (PR #11). It becomes optional so a pure direct-apply consumer can run patch mode alone. **Startup validation:** at least one of `--webhook-config-name` or `--target-crd-label` MUST be set; if neither is set, startup fails (a controller that manages nothing is a misconfiguration). This mirrors PR #10's "at least one mode" rule.

5. **Additive, backward-compatible.** `--managed-resource-label` and its `ManagedResourceReconciler` are **untouched**. The existing ConfigMap path (`WebhookReconciler`) is unchanged except that its required-flag check moves into the new "at least one mode" validation. All existing flags keep their names, defaults, and semantics. Unset `--target-crd-label` → no new reconciler, no target cache, no behavior change.

6. **Reuses the shared rotation gate.** `checkPropagation` in `reconciler.go` already polls target CRDs against the shared `rotationgate.Gate` snapshot. `TargetWebhookReconciler` publishes stamped **CRD** names to the gate (MWC/VWC are not gated — they are not part of the conversion-caBundle propagation the gate models), so staged→active promotion waits for the union bundle on target CRDs too. `checkPropagation` needs **zero changes**.

## Benefits

1. **Unblocks the GRM-free topology** — `dual-deployment-operator` can keep `caBundle` in sync on directly-applied CRDs **and** MWC/VWC, with no `ManagedResource` and without the operator taking on cert-lifecycle work.
2. **Backward-compatible** — unset flag → reconciler + target cache not registered → zero behavior change; every existing flag and path unchanged.
3. **Event-driven** — labeled target objects are stamped within seconds of creation/change, not up to one `--sync-period` later. Critical for `failurePolicy: Fail` admission webhooks.
4. **Narrow blast radius** — patches only `caBundle`; no `create`/`delete`; label-scoped so it never touches GRM-owned objects; no admission mutator.
5. **Proven shape** — restores the PR #10 Watch reconciler that was validated on real QA clusters, minus the parts issue #13 excludes.

## Technical Details

### New flag

**File:** `cmd/webhook-injector/main.go`

```go
flag.StringVar(&targetCRDLabel, "target-crd-label", "",
    "Label selector (key=value) enabling target-cluster patch mode. When set, "+
    "webhook-injector watches CRDs, MutatingWebhookConfigurations, and "+
    "ValidatingWebhookConfigurations on the TARGET cluster carrying this label "+
    "and keeps their caBundle in sync as certs rotate — for GRM-free / "+
    "direct-apply topologies where these objects are applied directly to the "+
    "shoot (no ManagedResource). Only pre-existing labeled objects are stamped; "+
    "nothing is created or deleted, and only caBundle is patched. Independent of "+
    "--managed-resource-label; an object should be covered by at most one path. "+
    "When unset, patch mode is not registered.")
```

Parsed with the existing `parseLabel` helper.

### Optional `--webhook-config-name` + "at least one mode" validation

**File:** `cmd/webhook-injector/main.go` (currently lines 122-133)

Replace the hard requirement:

```go
	// --webhook-config-name is required: ConfigMap mode is the only mode.
	if webhookConfigName == "" {
		setupLog.Error(nil, "--webhook-config-name is required")
		flag.Usage()
		os.Exit(1)
	}
```

with a "at least one mode" check evaluated after both labels are parsed:

```go
	tcLabelKey, tcLabelValue, tcLabelOK := parseLabel(targetCRDLabel)
	if targetCRDLabel != "" && !tcLabelOK { /* fatal: key=value form */ }

	// At least one mode must be active. ConfigMap mode (--webhook-config-name)
	// and target patch mode (--target-crd-label) are independent; --managed-
	// resource-label augments ConfigMap mode but is not itself a mode.
	if webhookConfigName == "" && !tcLabelOK {
		setupLog.Error(nil, "at least one of --webhook-config-name or --target-crd-label must be set")
		flag.Usage()
		os.Exit(1)
	}
```

`WebhookReconciler` is registered **only** when `webhookConfigName != ""`. Its `SetupWithManager` `For(&ConfigMap{})` and Secret watch already key on `WebhookConfigName`; when config mode is off, the reconciler is simply not constructed. (Cert generation/rotation lives inside `WebhookReconciler.Reconcile`, driven by the ConfigMap's service refs — see "Cert lifecycle when config mode is off" below.)

### Cert lifecycle when config mode is off

Today, cert generation/rotation is driven entirely by `WebhookReconciler.Reconcile`, which derives SANs from the ConfigMap's webhook service refs + `--cert-sans`. In pure patch mode (no ConfigMap), there is no ConfigMap to derive SANs from, so:

- **SAN source in pure patch mode is `--cert-sans`** (mirrors PR #10, which made `--cert-sans` required in patch mode). The target objects' `clientConfig` is not parsed for SANs — patch mode only *reads* their caBundle slot to write it; the reachable host/IP is operator-supplied via `--cert-sans`.
- `WebhookReconciler` must still own the cert Secret + rotation state machine even when config mode is off, because the whole rotation/gate/reloader design lives there. **Options:**
  - **A. Always register `WebhookReconciler`; make the ConfigMap optional inside it.** When `WebhookConfigName == ""`, the reconciler skips ConfigMap parsing and MWC/VWC apply, and drives cert lifecycle from `--cert-sans` alone. It still needs a trigger to reconcile (no ConfigMap to watch) — a startup `GenericEvent` runnable (the same bootstrap-runnable pattern PR #10 used for the Secret-anchored patch mode) plus `RequeueAfter` self-driving. **Preferred** — keeps one cert authority.
  - B. Split cert lifecycle into its own reconciler. Larger refactor; rejected as over-engineering for this change.

  Choose **A**. `WebhookReconciler` gains: (i) skip ConfigMap Get/parse/apply when `WebhookConfigName == ""`; (ii) build services from `--cert-sans` only in that case; (iii) a leader-only startup `GenericEvent` runnable so the first reconcile fires without a ConfigMap event. This is additive — when `WebhookConfigName != ""` the existing ConfigMap-driven path is byte-for-byte unchanged.

- **Validation:** in pure patch mode (`--target-crd-label` set, `--webhook-config-name` unset), `--cert-sans` MUST be non-empty (else the cert has no SANs and TLS fails). Fatal at startup otherwise.

### Target-cluster cache + reconciler

**File:** `pkg/controllers/target_webhook_reconciler.go` (new)

Mirrors PR #10's `TargetWebhookReconciler`, CRD+MWC/VWC only (no admission):

```go
type TargetWebhookReconciler struct {
    // TargetClient patches labeled objects on the target cluster.
    TargetClient client.Client

    // CertSecretReader reads the hosting cert Secret (mgr.GetAPIReader()).
    CertSecretReader client.Reader
    Namespace        string
    CertSecretName   string

    // Label scoping which target objects this reconciler owns.
    LabelKey   string
    LabelValue string

    // Gate publishes stamped CRD names for the staged-rotation gate. Nil
    // disables publishing. Only CRDs are published (MWC/VWC aren't gated).
    Gate *rotationgate.Gate
}
```

- Built on a **second `cluster.Cluster`** created against the target rest.Config, with a label-scoped cache via `cache.ByObject` for `CustomResourceDefinition`, `MutatingWebhookConfiguration`, `ValidatingWebhookConfiguration`. Added to the manager via `mgr.Add(targetCluster)`.
- **Three controllers** (`target-crd`, `target-mwc`, `target-vwc`) via `ctrl.NewControllerManagedBy(mgr).For(kind, builder.WithPredicates(labelPredicate)).WatchesRawSource(source.Kind(targetCluster.GetCache(), kind, ...))` — each reconcile lives on the target cache.
- Each reconcile: live-Get from target, stamp `caBundle` (reuse the shared `stampCRDConversionCABundle` for CRDs; a parallel `stampAdmissionCABundle` for MWC/VWC that walks `webhooks[*].clientConfig`), `Patch` MergeFrom only when changed. CRDs publish to the Gate; MWC/VWC do not.
- Rotation fan-out: a leader-only resync runnable listing all labeled objects of all three kinds and enqueuing them, fed by the shared rotation trigger.
- Bootstrap: because the target cache may be empty at startup with no events pending, a leader-only startup `GenericEvent` runnable emits an initial resync (same pattern as the cert bootstrap runnable).

### Shared stamp helpers

- `stampCRDConversionCABundle(crd, caCert) (changed bool)` — already extracted (from the CRD-only design); reused as-is.
- `stampAdmissionCABundle(webhooks []admissionv1.MutatingWebhook / ValidatingWebhook, caCert) (changed bool)` — new pure helper walking `webhooks[*].clientConfig.CABundle`, returns whether any changed. Two thin typed wrappers (MWC/VWC) since the webhook slice element types differ.

### Rotation fan-out wiring

**File:** `pkg/controllers/reconciler.go`, `cmd/webhook-injector/main.go`

`WebhookReconciler.RotationTrigger chan<- struct{}` → `RotationTriggers []chan<- struct{}` (as in the CRD-only design). `signalRotation` loops non-blocking sends. `main.go` appends one channel per registered consumer (MR path, patch mode).

### RBAC

**File:** `config/rbac.yaml`

The target ClusterRole `webhook-injector-target` currently has `get,list,watch,create,update,patch` on MWC/VWC and `get,list,watch` on CRDs. Patch mode needs **`watch`** on all three (for the target cache) and **`patch`** on CRDs:

```yaml
  - apiGroups: ["admissionregistration.k8s.io"]
    resources: ["mutatingwebhookconfigurations", "validatingwebhookconfigurations"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]   # unchanged
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["get", "list", "watch", "patch"]   # +patch for patch mode
```

Still **no** `create`/`delete` on CRDs.

### Metrics

**File:** `pkg/metrics/metrics.go`

- `webhook_injector_target_crd_stamps_total{result}` — CRD conversion caBundle stamp outcomes (`stamped|converged|error`).
- Reuse the existing `TargetStampsTotal{kind,result}` for MWC/VWC stamped by patch mode (same kinds, same `stamped|converged|error` — the counter already models exactly this).

### main.go wiring (shape)

```go
tcLabelKey, tcLabelValue, tcLabelOK := parseLabel(targetCRDLabel)
// ... at-least-one-mode + pure-patch-mode --cert-sans validation ...

var triggers []chan<- struct{}
// mrTrigger appended when mrLabelOK (unchanged) ...

if webhookConfigName != "" {
    // register WebhookReconciler (ConfigMap mode) — existing wiring
}
// else: register WebhookReconciler in cert-only mode (decision A) with a
//       startup GenericEvent runnable and --cert-sans-derived SANs.

if tcLabelOK {
    targetCluster, err := cluster.New(targetRestConfig, func(o *cluster.Options){
        o.Scheme = scheme
        o.Cache.ByObject = /* label-scoped CRD/MWC/VWC */
    })
    // mgr.Add(targetCluster)
    tcTrigger := make(chan struct{}, 1); triggers = append(triggers, tcTrigger)
    tc := &controllers.TargetWebhookReconciler{ TargetClient: targetCluster.GetClient(), CertSecretReader: mgr.GetAPIReader(), Namespace: namespace, CertSecretName: certSecretName, LabelKey: tcLabelKey, LabelValue: tcLabelValue, Gate: gate }
    tc.SetupWithManager(mgr, targetCluster, tcTrigger)
}
webhookReconciler.RotationTriggers = triggers
```

## Rotation Behavior

No change to the phase machine. Patch mode plugs in as a gate **producer** (CRDs only) and a rotation-fan-out **consumer** (all three kinds):

1. `WebhookReconciler` stages a new cert, signals fan-out.
2. Fan-out reaches `TargetWebhookReconciler`, which re-lists labeled target CRD/MWC/VWC and stamps the union `caBundle` onto each.
3. `checkPropagation` (staged phase) reads the gate snapshot — now including patch-mode CRDs — and polls each for both fingerprints before promoting. Empty snapshot / `--rotation-gate-timeout` fallback unchanged.
4. On promote/prune, fan-out fires again; objects re-stamped with the reordered/pruned bundle.

Pure patch-mode deployments now have a **non-empty** gate (from target CRDs), closing the same zero-downtime guarantee the MR path enjoys.

## Backward Compatibility

- Unset `--target-crd-label` → `TargetWebhookReconciler` and the target cache are not constructed; no new watches, no new RBAC exercised, no behavior change.
- `--managed-resource-label` path (`ManagedResourceReconciler`) is untouched.
- `--webhook-config-name` set → ConfigMap mode identical to today (its only change is that the required-check became an "at least one mode" check; when it's set, that check passes exactly as before).
- All existing flag names, defaults, and semantics preserved. New flag defaults to unset.
- `RotationTrigger` → `RotationTriggers` is an internal struct-field change with no external surface.

## Testing Strategy

**Unit tests:**
- `pkg/controllers/target_webhook_reconciler_test.go` (new): CRD stamp/converge/notfound/label-loss (as in the CRD-only design); MWC stamp/converge; VWC stamp/converge; multi-webhook MWC (all entries stamped); gate publish for CRD, no gate publish for MWC/VWC; only `caBundle` changes (MergeFrom).
- `pkg/controllers/reconciler_test.go`: `RotationTriggers` fan-out to multiple consumers; cert-only mode (no ConfigMap) generates a cert from `--cert-sans`; startup bootstrap runnable fires one event.
- `cmd/webhook-injector` (or a small `main_test.go` if the package has one; else document manual `--help` + startup checks): at-least-one-mode validation; pure-patch-mode requires `--cert-sans`.

**E2E tests** (`test/e2e/`): apply labeled CRD + MWC + VWC **directly** to the target (no ManagedResource), start injector with `--target-crd-label` and no `--webhook-config-name`, assert all three get `caBundle` within seconds (Watch), force-rotate, assert union then promoted single cert land, and the gate held promotion until CRD propagation. Match the existing e2e harness.

**Manual testing:** kind hosting+target, directly-applied labeled objects, verify `kubectl get {crd,mwc,vwc} -o jsonpath=...caBundle` tracks rotation; verify a `failurePolicy: Fail` MWC is stamped before it can block traffic.

## Files to Change

**New:**
- `pkg/controllers/target_webhook_reconciler.go`
- `pkg/controllers/target_webhook_reconciler_test.go`

**Modify:**
- `cmd/webhook-injector/main.go` — `--target-crd-label` flag; at-least-one-mode + pure-patch `--cert-sans` validation; optional `WebhookReconciler` registration / cert-only mode; target `cluster.Cluster` construction; `RotationTriggers` slice.
- `pkg/controllers/reconciler.go` — `RotationTriggers`; `signalRotation` loop; ConfigMap-optional cert-only path + startup bootstrap runnable.
- `pkg/controllers/managed_resource_reconciler.go` — extract `stampCRDConversionCABundle` (shared); untouched otherwise.
- `pkg/metrics/metrics.go` — `target_crd_stamps_total{result}`.
- `config/rbac.yaml` — `watch` on MWC/VWC/CRD (cache) + `patch` on CRD.
- `README.md`, `CLAUDE.md` — patch mode, optional config-name, flag/metrics tables, history note.

## Out of Scope

- Admission-webhook bootstrap mutator (`pkg/admission/*`, `--admission-*` flags) — decision 3.
- Service→URL rewrite in patch mode — decision 2.
- cert-manager issuance (self-signed lifecycle stays).
- Any change to the `--managed-resource-label` MR-Secret path.

## Completion Criteria

- [ ] `stampCRDConversionCABundle` + `stampAdmissionCABundle` extracted/added; existing MR tests unchanged and green.
- [ ] `--target-crd-label` registers `TargetWebhookReconciler` (target-crd/mwc/vwc) on a label-scoped target cache; unset → nothing registered, behavior byte-for-byte unchanged.
- [ ] Patch mode patches **only** `caBundle` (CRD conversion + MWC/VWC per-webhook); no create/delete; no other field mutated; no admission mutator.
- [ ] `--webhook-config-name` optional; startup fails only when neither it nor `--target-crd-label` is set; pure patch mode requires `--cert-sans`.
- [ ] `WebhookReconciler` runs cert lifecycle in cert-only mode from `--cert-sans`; ConfigMap mode unchanged when the flag is set.
- [ ] Stamped CRDs published to the rotation gate; MWC/VWC not gated; staged promotion waits for target CRD propagation.
- [ ] `RotationTriggers` fan-out delivers to both MR and patch-mode consumers.
- [ ] Target RBAC adds `watch` (MWC/VWC/CRD) + `patch` (CRD); no `create`/`delete` on CRDs.
- [ ] `target_crd_stamps_total{result}` added; MWC/VWC reuse `target_stamps_total`.
- [ ] Unit + e2e passing; no regression to cert rotation/pruning, reloader, leader-election, ConfigMap mode, or MR mode.
- [ ] README + CLAUDE.md updated.
