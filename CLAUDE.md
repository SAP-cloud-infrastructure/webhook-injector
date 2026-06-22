<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

webhook-injector is a Kubernetes controller that runs as a **native sidecar** (Kubernetes 1.29+) to webhook servers, managing webhook configurations and TLS certificates. The key innovation is enabling webhook servers to serve webhooks for remote clusters (clusters they don't run in) while providing a language-agnostic solution that works with any webhook server that can watch and reload certificate files.

**Webhook Server Requirement**: The webhook server must watch certificate files (`tls.crt`, `tls.key`) and reload them when changed. Controller-runtime webhook servers (v0.17+) provide this automatically via an internal `certwatcher`. Other frameworks need custom implementation using inotify/fsnotify or periodic polling.

The image ships two binaries:
- `webhook-injector` — the long-running controller described in this document.
- `crd-injector` — a small "apply once and exit" companion that reads CRD YAML from a ConfigMap in the hosting cluster and server-side applies each CRD to the target cluster. Intended as a Kubernetes Job or a regular initContainer that blocks the webhook server until CRDs exist. See the `cmd/crd-injector` section below.

## Build System

This project uses **go-makefile-maker** for build automation. The Makefile is generated from `Makefile.maker.yaml`. On macOS, use `gmake` instead of `make` because the built-in make is too old for go-makefile-maker.

### Essential Commands

```bash
# Build binary
make build                     # Builds to build/webhook-injector

# Run all checks (lint, REUSE, tests)
make check                     # MUST pass before committing

# Run specific checks
make static-check              # golangci-lint, shellcheck, go-licence-detector, REUSE
make test                      # Unit tests only (excludes test/integration)

# E2E Integration tests
make test-e2e                  # E2E tests with kind clusters (replaces test-integration)

# Manual E2E development
make e2e-setup                 # Create kind clusters
make e2e-dev                   # Skaffold dev mode
make e2e-teardown              # Cleanup clusters

# Linting
golangci-lint run              # Run linter directly
golangci-lint run --fix        # Auto-fix issues

# Run single test
go test -v -run TestName ./pkg/controllers/
go test -v ./pkg/certificates/    # Run all tests in package

# REUSE compliance
reuse lint                     # Check license compliance
reuse download Apache-2.0      # Download license file
```

### Pre-commit Requirements

- `make check` MUST pass before committing
- All Go files MUST have Apache-2.0 license headers (auto-added by addlicense)
- REUSE compliance MUST be satisfied (configured in REUSE.toml)

## Architecture

### Controllers

The application registers up to four independent controllers (the second and third are patch-mode only; the third is also opt-in via `--managed-resource-mode`) with different leader election behavior:

1. **WebhookReconciler** (runs on **leader only**)
   - Location: `pkg/controllers/reconciler.go`
   - Watches (depend on mode — see Configuration):
     - ConfigMap mode: webhook-definitions ConfigMap + certificate Secret (hosting cluster)
     - Patch mode: certificate Secret (hosting cluster) — no ConfigMap. Target-cluster MWC/VWC/CRD convergence is handled by `TargetWebhookReconciler` (see below).
   - Generates/rotates self-signed certificates with dynamic SANs
   - Stores certificates in Secret (in hosting cluster)
   - Injects CA bundle into webhook configurations
   - ConfigMap mode: applies webhook configurations to target cluster (creates if missing). When `--webhook-label` is also supplied, additionally refreshes `caBundle` on labeled CRDs (labeled admission webhooks are intentionally ignored — ConfigMap mode owns admission-webhook creation).
   - Patch mode: does **not** patch target-cluster resources directly. `TargetWebhookReconciler` (see below) Watches labeled MWC/VWC/CRD on the target cluster and converges them. `WebhookReconciler` in patch mode only manages the certificate Secret and bootstraps the admission MWC.
   - Leader election: DEFAULT behavior (requires leader)

2. **TargetWebhookReconciler** (patch mode only; runs on **leader only**)
   - Location: `pkg/controllers/target_webhook_reconciler.go`
   - Watches: labeled `MutatingWebhookConfiguration`, `ValidatingWebhookConfiguration`, and `CustomResourceDefinition` resources on the **target** cluster, via a separate `cluster.Cluster` handle whose cache is label-scoped
   - Stamps `clientConfig.CABundle` from the hosting-cluster certificate Secret and (when `--external-host` is set) rewrites Service-form `clientConfig` to URL-form
   - For CRDs: short-circuits the Update when nothing has changed (caBundle already current and no Service→URL rewrite needed) to avoid bouncing `resourceVersion` on every admission-stamped CRD write
   - **Rotation fan-out**: receives a signal from `WebhookReconciler` whenever the cert rotates. On signal, lists every labeled MWC/VWC/CRD currently in the target cache and enqueues each for reconcile, so the new caBundle propagates immediately rather than waiting for the target cluster cache `--sync-period` tick. Uses a buffered single-slot channel + non-blocking send: a missed signal is harmless because a single signal is enough to trigger a full re-stamp pass.
   - Why this exists for MWC/VWC: kube-apiserver self-protects against admission webhooks on `admissionregistration.k8s.io/{mutating,validating}webhookconfigurations` (`rules.IsExemptAdmissionConfigurationResource`), so an admission-time hook on those kinds is silently bypassed. A target-cluster Watch is the only way to converge on creates/updates.
   - Why this exists for CRDs: the bootstrap MWC stamps CRDs synchronously at CREATE/UPDATE, but out-of-band edits (kubectl, GitOps, other controllers) bypass admission. The Watch is the drift-correction safety net.
   - Leader election: DEFAULT behavior (requires leader)

3. **ManagedResourceReconciler** (patch mode + `--managed-resource-mode` only; runs on **leader only**)
   - Location: `pkg/controllers/managed_resource_reconciler.go`
   - Watches: labeled `ManagedResource` (`resources.gardener.cloud/v1alpha1`) objects on the **hosting** cluster (not the target cluster). Cache is label-scoped, cluster-wide.
   - Why this exists: on a Gardener seed, CRDs are not applied directly to the shoot — they live inside `ManagedResource` Secrets, and gardener-resource-manager (GRM) reconciles them into the shoot. `TargetWebhookReconciler` would stamp the shoot-side CRD, but GRM immediately overwrites it from the Secret. Stamping the *source* (this controller) makes the stamp survive GRM's next sync.
   - For each labeled MR: walks `spec.secretRefs[*]`, fetches the referenced Secret, decodes each data key (with brotli `.br` support to match GRM's compression), finds CRDs with `conversion.strategy=Webhook`, stamps `clientConfig.caBundle`, and — when `--external-host` is set and the entry is Service-form — rewrites to URL-form via `webhookhost.BuildExternalURL`. Documents the controller doesn't touch survive byte-for-byte (no YAML round-trip), preserving user-authored formatting.
   - Idempotency: a no-op reconcile must produce zero Secret updates. Both per-doc (`maybeStampCRDDoc` returns the original bytes when already converged) and per-Secret (skip `Update` when no doc in that Secret changed).
   - **Force-reconcile annotation** (`--managed-resource-force-reconcile`, default `true`): after a successful Secret stamp, patches `gardener.cloud/operation=reconcile` on the MR so GRM picks up the change immediately even if it is in backoff. Set only on reconciles that actually wrote a Secret, and skipped when the annotation is already present — no-op reconciles do not churn GRM. GRM clears the annotation itself after consuming it.
   - **keepObjects=true invariant**: when an MR ships at least one CRD, the reconciler verifies that `spec.keepObjects` is explicitly `true`. If unset or `false`, the reconcile still stamps the CRD (we'd rather have a working webhook than refuse to write), but logs an error and increments `webhook_injector_managed_resource_keep_objects_violations_total`. The destructive shape: without `keepObjects=true`, deleting the MR cascades into the shoot's CRDs and every custom resource of those kinds. CRDs with `conversion.strategy=None` still count for this check.
   - **GRM health observation**: every reconcile reads `mr.Status.Conditions` and checks `ResourcesApplied` and `ResourcesHealthy`. If either is present and not `Status=True`, the reconcile logs an error (including `Reason`/`Message`) and increments `webhook_injector_managed_resource_unhealthy_total{condition,status}`. Missing conditions (freshly created MRs before GRM evaluates) are deliberately ignored to avoid noise during normal MR creation. The check is non-blocking — the stamp still happens — and runs on every reconcile pass (including periodic `--sync-period` ticks), so dashboards reflect current GRM state.
   - **Rotation fan-out**: subscribes to the same rotation signal as `TargetWebhookReconciler` via a one-to-two splitter goroutine in `main.go`. On rotation, lists every labeled MR on the hosting cluster and enqueues each for reconcile so the new caBundle propagates immediately rather than waiting for the next `--sync-period` tick.
   - Leader election: DEFAULT behavior (requires leader)

4. **CertificateReloader** (runs on **all replicas**)
   - Location: `pkg/controllers/reloader.go`
   - Watches: Certificate Secret (in hosting cluster)
   - Writes certificates to local filesystem (`/tmp/webhook-certs` by default)
   - Optimization: Only writes when certificate content changes (prevents unnecessary webhook server reloads)
   - Leader election: Uses `WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)})`

### Key Architectural Decisions

**Two-cluster model:**
- **Hosting cluster**: Where webhook-injector and webhook server run
- **Target cluster**: Where webhooks are enforced (may be same or remote)
- This enables serving webhooks for clusters you don't run in

**Why this controller split:**
- WebhookReconciler must run on leader only to prevent multiple replicas from generating conflicting certificates or racing to update target cluster
- TargetWebhookReconciler must run on leader only to avoid concurrent writes to the same target-cluster MWC/VWC/CRD
- CertificateReloader must run on all replicas so each webhook server pod gets certificates on its local filesystem

**Native sidecar pattern (Kubernetes 1.29+):**
- webhook-injector is deployed as a native sidecar: an init container with `restartPolicy: Always`
- Starts before the webhook server, ensuring certificates are available at startup
- Terminates after the webhook server for clean shutdown ordering
- Shares emptyDir volume for certificate files
- Works with any webhook server that can watch certificate files and reload on changes
- Controller-runtime v0.17+ webhook servers handle this automatically via fsnotify
- For clusters < 1.29, can fall back to a regular sidecar container (no startup ordering guarantee)

**ConfigMap-driven:**
- Users provide full `MutatingWebhookConfiguration` or `ValidatingWebhookConfiguration` YAML in a ConfigMap
- webhook-injector parses, injects CA bundle, and applies to target cluster
- Supports service references (in-cluster) and URLs (remote)

**Gardener seed support:**
- On a Gardener seed, CRDs are wrapped inside `ManagedResource` Secrets and applied to the shoot by `gardener-resource-manager`. Stamping the shoot-side CRD alone produces a thrash loop because GRM re-applies the original Secret on every sync.
- The opt-in `--managed-resource-mode` flag enables `ManagedResourceReconciler`, which stamps the *source* (the MR's referenced Secret on the seed). The shoot-side `TargetWebhookReconciler` still runs as a belt-and-suspenders convergence path for the brief window between Secret update and GRM sync, but the authoritative stamp lives in the Secret.
- Brotli (`.br` suffix) compression is supported because GRM transparently compresses larger payloads.

## Code Structure

```
cmd/webhook-injector/           Main entrypoint
  main.go                       Creates manager, sets up both controllers, handles target cluster client

cmd/crd-injector/               One-shot binary (apply CRDs from ConfigMap, then exit)
  main.go                       parseCRDs() + applyCRD() — server-side apply via RawPatch+ApplyPatchType
  main_test.go                  Table-driven tests for parseCRDs

pkg/certificates/               Certificate generation using crypto/x509
  generator.go                  GenerateSelfSignedCert() with dynamic SANs
  types.go                      ServiceReference struct

pkg/config/                     Environment variable configuration
  config.go                     LoadFromEnv(), Validate()

pkg/controllers/                Two independent controllers
  reconciler.go                 WebhookReconciler (leader-only, manages certificates and target cluster)
  reloader.go                   CertificateReloader (all replicas, syncs certs to filesystem)
  managed_resource_reconciler.go ManagedResourceReconciler (patch mode + --managed-resource-mode, stamps CRDs inside Gardener ManagedResource Secrets)
  cabundle.go                    Shared loadCABundle helper used by TargetWebhookReconciler and ManagedResourceReconciler

pkg/managedresource/            Helpers for decoding/re-encoding Gardener ManagedResource Secret data
  codec.go                      Brotli-aware decode/encode + multi-doc YAML split/join (preserves untouched docs byte-for-byte)

pkg/metrics/                    Prometheus metric definitions
  metrics.go                    Cert rotations / expiry / files-written, target-cluster stamps, admission decisions, rotation fan-out, ManagedResource stamps + force-reconcile + brotli encoding (see Metrics section)

pkg/webhooks/                   Webhook configuration parsing
  parser.go                     ParseWebhookConfigurations(), ExtractServiceReferences()
  injector.go                   InjectCABundle()

test/e2e/                       E2E integration tests with kind clusters
```

### cmd/crd-injector

A separate "apply once and exit" binary that ships in the same image. It reads
a ConfigMap from the hosting cluster, parses every key as multi-document YAML,
requires every document to be `apiextensions.k8s.io/v1 CustomResourceDefinition`
(anything else is an error so it can't be used as a generic applier), and
server-side applies each CRD to the target cluster with field manager
`crd-injector`. Exits 0 on success, non-zero on any parse or apply error. No
watches, no controller-runtime manager, no caBundle handling — `webhook-injector`'s
labeled-CRD path is responsible for stamping `caBundle` (and the Service→URL
rewrite) on these CRDs after they exist.

To avoid fighting `webhook-injector` across re-applies, `applyCRD` does a
`Get` first; if the CRD already exists in the target cluster it strips the
three fields `webhook-injector` owns at runtime — `spec.conversion.webhook.clientConfig.caBundle`,
`spec.conversion.webhook.clientConfig.url`, and `spec.conversion.webhook.clientConfig.service` —
from the apply body before SSA. With server-side apply, fields absent from the
body are not owned by the applier, so re-applies are a true no-op for those
three fields. On *first* creation the user-authored `clientConfig` is left
intact (the apiserver requires `url` or `service` when `strategy=Webhook`),
and `webhook-injector`'s bootstrap admission MWC rewrites the entry during the
same admission cycle — admission-time mutations are attributed to the apiserver
rather than to `crd-injector`, so our managed-fields entry still does not list
those three keys after the create completes. `client.ForceOwnership` is
deliberately *not* used: with the contested fields out of our body there is
nothing to force, and keeping it on would let `crd-injector` steal future
`webhook-injector`-owned fields.

Implementation notes worth knowing before changing this file:
- Uses a `serializer.NewCodecFactory(scheme)` deserializer rather than
  `clientgoscheme.Codecs` because the latter does not know
  `apiextensions.k8s.io/v1` and would silently fail to decode CRDs.
- Server-side apply is performed via `client.RawPatch(types.ApplyPatchType, body)`
  rather than the deprecated `client.Apply` constant — the new
  `client.Client.Apply()` API in controller-runtime v0.23 requires generated
  `runtime.ApplyConfiguration` types we don't otherwise need here.
- The strip helper `stripWebhookInjectorFields` zeroes only the three
  `clientConfig` sub-fields, not the surrounding `clientConfig` object: a
  CRD with `strategy=Webhook` is required by the apiserver to carry one.
  All three sub-fields are `omitempty` in their API types, so the JSON
  marshal omits the keys entirely once they're nil.
- Namespace auto-detection uses the same priority order as `webhook-injector`:
  `--namespace` flag → `NAMESPACE` env var → serviceaccount file → fail.
- Target cluster connection mirrors `webhook-injector`: `--target-kubeconfig`
  for explicit kubeconfig, in-cluster config otherwise.

Flags:
- `--configmap-name` — required; ConfigMap (in `--namespace`, hosting cluster) whose values contain CRD YAML.
- `--namespace` — optional; auto-detects from `NAMESPACE` env or serviceaccount file.
- `--target-kubeconfig` — optional; defaults to in-cluster config.
- `--field-manager` — defaults to `crd-injector`; set if you want a different field-owner identity.

Typical usage as a Kubernetes Job or initContainer that blocks the webhook
server until CRDs exist:

```yaml
- name: install-crds
  image: ghcr.io/.../webhook-injector:<tag>
  command: ["/usr/bin/crd-injector"]
  args:
    - --configmap-name=my-crds
    - --target-kubeconfig=/etc/target/kubeconfig  # optional
```

## Certificate Lifecycle

**Generation triggers:**
1. ConfigMap changes (new webhook definitions)
2. Time-based rotation: remaining validity < threshold × total validity (default: 20%)
3. Config-based rotation: Service references or URLs change (new SANs required)
4. Manual rotation: Force-rotation annotation on Secret (`webhook-injector.sap.com/force-rotation: "true"`)

**Certificate approach:**
- Single self-signed certificate (not CA + server cert)
- Same certificate used for serving and validation (validates itself)
- Simpler Secret structure: 2 files (tls.crt, tls.key)

**Certificate SANs:**
- Service-based webhooks: `<service>.<namespace>.svc`, `<service>.<namespace>.svc.cluster.local`
- URL-based webhooks: hostname or IP extracted from URL — DNS names go into DNS SANs (e.g., `webhook.example.com`), IP addresses go into IP SANs (e.g., `172.18.0.3` for NodePort-based remote clusters)

**Storage flow:**
1. WebhookReconciler generates self-signed cert → stores in Secret (hosting cluster)
2. CertificateReloader watches Secret → writes to `/tmp/webhook-certs/`
3. Webhook server watches filesystem → reloads certificates (controller-runtime v0.17+ does this automatically)

## Important Implementation Details

**ForceRotationAnnotation constant:**
- Defined in `pkg/controllers/reconciler.go` as a public constant
- Value: `"webhook-injector.sap.com/force-rotation"`
- Used by WebhookReconciler to check for manual rotation triggers
- Used by tests and e2e tests to trigger rotation
- The e2e tests import this constant from the controllers package via replace directive in test/e2e/go.mod

**Import aliasing for k8s errors:**
- Always use `apierrors "k8s.io/apimachinery/pkg/api/errors"` to avoid conflicts with standard `errors` package
- Use `apierrors.IsNotFound()` not `errors.IsNotFound()`

**Leader election:**
- Configured in main.go via manager options
- WebhookReconciler: Uses default leader election behavior (no WithOptions) → requires leader
- CertificateReloader: Uses `WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)})` → runs on all replicas
- The explicit WithOptions approach is required because controller-runtime's builder doesn't always recognize the NeedLeaderElection interface method

**Logging:**
- Use `logger := log.FromContext(ctx)` not `log := log.FromContext(ctx)` to avoid shadowing imported `log` package
- Pattern: `logger.Info()`, `logger.Error()`, with key-value pairs

## Testing

**Unit tests:**
- Use `runtime.NewScheme()` and properly add types with error handling
- Use `t.Setenv()` not `os.Setenv()` in tests
- Test files must have Apache-2.0 license headers

**E2E Integration tests:**
- Located in `test/e2e/`
- Use two kind clusters (hosting and target)
- Test multi-cluster, multi-replica scenarios
- Require kind, kubectl, skaffold, docker
- Run with `make test-e2e` or manual workflow with `make e2e-setup`, `make e2e-dev`, `make e2e-teardown`

**Test patterns:**
- Create fake clients with proper schemes
- Use short certificate validities (seconds) for rotation testing
- Sleep strategically to test time-based rotation thresholds

## Configuration

All configuration via command-line flags. Run `webhook-injector --help` for full list.

The reconciler selects its mode at startup based on which of `--webhook-config-name` and `--webhook-label` are set:

**ConfigMap mode** (apply webhook definitions from a ConfigMap):
- `--webhook-config-name` - ConfigMap containing full `MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration` YAML. webhook-injector parses, injects CA bundle, and applies these to the target cluster (creating them if missing).
- `--webhook-label` *(optional)* - when set together with `--webhook-config-name`, webhook-injector additionally refreshes `caBundle` on `CustomResourceDefinition` resources in the target cluster carrying this label, and rewrites labeled-CRD Service-form `clientConfig` to URL-form using `--external-host` + `--external-port` (see below). Labeled `MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration` resources are intentionally ignored, since ConfigMap mode already owns admission-webhook creation.
- `--external-host` / `--external-port` *(required when `--webhook-label` is set)* - same semantics as in patch mode below, but applied only to labeled CRDs (not to the MWC/VWC parsed from the ConfigMap — those are applied as authored by the user).

**Patch mode** (patch pre-existing webhook configurations in the target cluster):
- `--webhook-label` - **required in patch mode**; label selector in `key=value` form (e.g., `webhook-injector/name=cert-manager`). webhook-injector reconciles `MutatingWebhookConfiguration`, `ValidatingWebhookConfiguration`, and `CustomResourceDefinition` resources in the target cluster carrying this label.
- `--cert-sans` - **required in patch mode**; comma-separated DNS names and/or IP addresses to include as certificate SANs. Each entry is auto-classified (parses as IP → IP SAN; otherwise → DNS SAN). This is the sole SAN source in patch mode. `--external-host` / `--external-port` do not affect SAN generation.
- `--external-host` - **required in patch mode, and in ConfigMap mode when `--webhook-label` is set**; DNS name or IP address (no port — the port is `--external-port`) the target cluster uses to reach this controller. Combined with `--external-port` to rewrite labeled MWC/VWC/CRD-conversion entries (or labeled-CRD entries only in ConfigMap mode) whose `clientConfig.Service` is non-nil to `clientConfig.URL = https://<external-host>:<external-port><Service.Path>` (Service.Path nil/empty → `/`); the `Service` field is then cleared. Entries that already use URL-form `clientConfig` are left untouched. `caBundle` patching is unchanged. Ignored (with a log line) in ConfigMap mode without `--webhook-label`.
- `--external-port` - **required in patch mode, and in ConfigMap mode when `--webhook-label` is set**; external port the target cluster uses to reach the labeled webhook callbacks. Distinct from `--admission-external-port`, which targets this controller's own admission endpoint.
- Resources are never created — only existing labeled resources are reconciled. Resources that match the label but have no `Webhook` conversion strategy (CRDs only) or no `clientConfig` are skipped.
- For each entry: `clientConfig.CABundle` is always updated. `clientConfig.URL` and `clientConfig.Service` are rewritten using `--external-host` + `--external-port` (see above).
- **MWC/VWC/CRD convergence**: handled by `TargetWebhookReconciler`, which Watches the target cluster's labeled `MutatingWebhookConfiguration`, `ValidatingWebhookConfiguration`, and `CustomResourceDefinition` resources via a label-scoped cache and reconciles each event. Drift is corrected within seconds, not bounded by `--sync-period`. For MWC/VWC this is the *only* correct mechanism (kube-apiserver bypasses admission webhooks on those kinds via `rules.IsExemptAdmissionConfigurationResource`). For CRDs this is the drift-correction safety net behind the synchronous admission webhook below.
- **Rotation propagation**: `WebhookReconciler` signals `TargetWebhookReconciler` on every detected cert rotation; the latter immediately re-stamps every labeled MWC/VWC/CRD with the new caBundle. Without this signal, on-cluster objects would keep stale caBundle until the target cluster cache `--sync-period` fires Update events.
- **Admission-webhook bootstrap**: the controller bootstraps a `MutatingWebhookConfiguration` in the target cluster (named via `--admission-webhook-name`, default `webhook-injector-mutator`) that registers webhook-injector itself as an admission mutator on labeled **CRD** resources only. CREATE/UPDATE on labeled CRDs is mutated synchronously — `caBundle` is stamped and Service-form `clientConfig` is rewritten to URL form — so the desired state is in place at admission time. The bootstrap MWC has only one webhook entry (`crd.webhook-injector.sap.com`); MWC/VWC entries are intentionally absent because kube-apiserver bypasses admission webhooks on those kinds. The bootstrap MWC's `clientConfig.URL` uses **`--external-host`** combined with **`--admission-external-port`** (a separate port from `--external-port`, which targets the labeled webhook callbacks rather than this controller's admission endpoint). The bootstrapped MWC is unlabeled by design so its `objectSelector` cannot match it. `failurePolicy: Ignore` keeps target-cluster CRD updates working when webhook-injector is down — `TargetWebhookReconciler` is the safety net for drift in that window.

When both `--webhook-config-name` and `--webhook-label` are set, webhook-injector runs ConfigMap mode and additionally refreshes `caBundle` on labeled CRDs and rewrites Service-form CRD-conversion `clientConfig` to URL-form (labeled admission webhooks are ignored). `--cert-sans` is optional in ConfigMap mode (entries are merged with SANs derived from the ConfigMap) and required in patch mode. `--external-host` / `--external-port` are required in patch mode and in ConfigMap mode when `--webhook-label` is set; in ConfigMap mode without `--webhook-label` they are ignored (logged).

Common flags:
- `--namespace` - Namespace for all resources (ConfigMap, Secret, Lease). Auto-detects from env vars or pod's serviceaccount if not set
  - Priority order: explicit flag → `NAMESPACE` env var → serviceaccount file → **fail**
  - The program will exit with an error if namespace cannot be determined
- `--cert-secret-name` - Where to store certificates
- `--cert-dir` - Where to write certificate files
- `--cert-validity` - Certificate lifetime as duration (e.g., "2160h", "90d", "1y")
- `--cert-rotation-threshold` - When to rotate (0-1)
- `--target-kubeconfig` - Path to target cluster kubeconfig (optional, defaults to in-cluster)
- `--enable-leader-election` - Enable HA with multiple replicas
- `--leader-election-id` - Leader election lease name. Default `webhook-injector-leader`.
- `--sync-period` - Reconciliation interval
- `--metrics-addr` - Metrics server bind address. Default `:8082`.
- `--health-addr` - Health probe server bind address. Default `:8083`.
- `--version` - Print version and exit. The binary exits 0 immediately after printing.

Patch-mode-only flags:
- `--admission-webhook-name` - name of the `MutatingWebhookConfiguration` the controller bootstraps in the target cluster to register itself as an admission mutator on labeled CRDs (default `webhook-injector-mutator`). Empty disables bootstrap; `TargetWebhookReconciler` continues to converge MWC/VWC/CRD via Watch, but CRDs lose synchronous stamping at admission time.
- `--admission-webhook-port` - port the admission webhook server binds to inside the pod (default `9444`). Configure your Service / NodePort / LoadBalancer to forward `--admission-external-port` to this port.
- `--admission-external-port` - **required when `--admission-webhook-name` is non-empty**; external port the target cluster uses to reach this controller's admission webhook server. Combined with `--external-host` to form the bootstrap MWC URL `https://<external-host>:<admission-external-port>/mutate-crd`. Distinct from `--external-port`, which targets the labeled webhook callbacks. No default — the value depends entirely on the user's Service / NodePort / LoadBalancer wiring, and a wrong default would silently produce a bootstrap MWC the apiserver cannot reach.
- `--managed-resource-mode` - opt-in (default `false`). When `true` (patch mode only), registers `ManagedResourceReconciler`, which stamps the `caBundle` and rewrites Service-form conversion `clientConfig` on CRDs embedded inside Gardener `ManagedResource` Secrets on the hosting cluster. Required flags `--webhook-label`, `--external-host`, `--external-port` are already required by patch mode. Without this flag the controller is not registered and the existing `TargetWebhookReconciler` is the only path that touches CRDs (correct for non-Gardener targets; wrong for Gardener seeds, where GRM would overwrite the shoot-side stamp on every sync).
- `--managed-resource-force-reconcile` - default `true`. When `--managed-resource-mode` is set, the controller patches `gardener.cloud/operation=reconcile` on each `ManagedResource` after a successful Secret stamp. This kicks `gardener-resource-manager` out of any backoff or debounce state so the new caBundle reaches the shoot promptly. The annotation is set **only** on reconciles that actually wrote a Secret — idempotent no-op reconciles do not churn GRM. The reconciler also skips the patch when the annotation is already present (GRM clears it after consuming, per the documented Gardener contract). Set to `false` to rely solely on GRM's own Secret watch — appropriate when the Secret-watch propagation latency is acceptable and you want one fewer hosting-cluster write per rotation.

## Remote Cluster Support

The target cluster client is created in `main.go`:
- If `--target-kubeconfig` flag set: Use explicit kubeconfig file
- Otherwise: Use in-cluster config (hosting cluster = target cluster)

**Important**: Use `--target-kubeconfig` not `KUBECONFIG` to avoid interfering with the manager's hosting cluster connection (which uses standard client-go config loading that checks KUBECONFIG).

## Metrics

All metrics are registered on controller-runtime's default Prometheus registry and exposed on `--metrics-addr` (default `:8082`). All metric names are prefixed `webhook_injector_`. Label cardinality is bounded — every label below has a fixed enum of values, declared as constants in `pkg/metrics/metrics.go`.

### Certificate metrics (always on)

- `webhook_injector_certificate_rotations_total{reason}` — counter. Labels: `reason ∈ {initial, expired, san_mismatch, force_rotation}`. Incremented by `WebhookReconciler` when a fresh cert is generated. The `force_rotation` value corresponds to the `webhook-injector.sap.com/force-rotation` annotation on the cert Secret.
- `webhook_injector_certificate_expiry_time_seconds` — gauge. Unix timestamp of the current cert's `NotAfter`. Use `(metric - time())` in alerts for "expires within N hours".
- `webhook_injector_cert_files_written_total` — counter. Incremented by `CertificateReloader` on every disk write. Each replica exposes its own value; a spike means rotation is propagating to the sidecar filesystem.

### Target-cluster stamping (patch mode)

- `webhook_injector_target_stamps_total{kind, result}` — counter. Labels: `kind ∈ {MutatingWebhookConfiguration, ValidatingWebhookConfiguration, CustomResourceDefinition}`; `result ∈ {stamped, converged, error}`. Emitted by `TargetWebhookReconciler.reconcile{MWC,VWC,CRD}` via a deferred classifier. `stamped` = `Update` succeeded; `converged` = idempotency-skip (caBundle already current, no Service→URL rewrite needed); `error` = Get or Update failure. The `converged/stamped` ratio is the natural "is rotation actually changing anything" signal on a steady-state cluster.
- `webhook_injector_admission_decisions_total{result}` — counter. Labels: `result ∈ {mutated, passthrough, error}`. Emitted by the admission mutator (`pkg/admission`) via a deferred classifier of the `admission.Response`. `mutated` = JSON patch returned; `passthrough` = `Allowed` without a patch (kind not handled, CA bundle unavailable/empty, CRD without Webhook conversion); `error` = `Errored` response.

### Rotation fan-out

- `webhook_injector_rotation_fanout_events_total{kind, outcome}` — counter. Labels: `kind ∈ {MutatingWebhookConfiguration, ValidatingWebhookConfiguration, CustomResourceDefinition, ManagedResource}`; `outcome ∈ {emitted, dropped}`. Emitted by `targetResyncTrigger.fanOut` and `managedResourceResyncTrigger.fanOut` for every per-kind event pushed to (or dropped from) the controller's reconcile channel on every cert rotation. A non-zero `dropped` rate is informational — the next rotation or the periodic `--sync-period` tick re-converges; alarming on it makes sense only if the rate is sustained.

### ManagedResource stamping (`--managed-resource-mode` only)

- `webhook_injector_managed_resource_stamps_total{result}` — counter. Labels: `result ∈ {stamped, converged, error}`. Per-MR reconcile outcome. `stamped` = at least one referenced Secret was rewritten; `converged` = all Secrets already in desired state; `error` = Get / decode / Update failure.
- `webhook_injector_managed_resource_crds_stamped_total` — counter (no labels). Per-CRD-document granularity, summed across all MRs and all Secrets. Distinct from the stamps counter because a single MR may carry multiple CRDs across multiple Secrets — this is the right signal for "how much work did the last rotation actually do".
- `webhook_injector_managed_resource_secret_keys_total{encoding}` — counter. Labels: `encoding ∈ {plain, brotli}`. Increments once per Secret data key processed. Lets a dashboard confirm the brotli codec path is exercised against the live fleet — a seed whose MRs only ship plain YAML will show `encoding=brotli` at zero.
- `webhook_injector_managed_resource_force_reconcile_total{result}` — counter. Labels: `result ∈ {patched, skipped, error}`. Tracks the `gardener.cloud/operation=reconcile` annotation patch. `patched` = annotation actually written (MR Patch succeeded); `skipped` = annotation already present (a previous reconcile set it and GRM has not yet consumed it); `error` = patch failed (reconcile itself does not fail in that case, so this counter is the sole signal of degraded GRM-kick).
- `webhook_injector_managed_resource_keep_objects_violations_total` — counter (no labels). Incremented once per reconcile of a labeled MR that ships at least one CRD but does not set `spec.keepObjects: true`. The check is non-blocking — the controller still stamps the CRD so the webhook keeps working — but the counter and an accompanying error-level log surface a destructive misconfiguration: deleting that MR would cascade-delete every shoot CRD it ships, taking every custom resource of those kinds with it. The check measures _MRs_, not _CRDs_, so an MR with five CRDs and no `keepObjects=true` increments once per reconcile, not five times. CRDs with `conversion.strategy=None` still count as "observed CRDs" for this check — the destructive shape applies regardless of whether we rewrite the conversion clientConfig.
- `webhook_injector_managed_resource_unhealthy_total{condition, status}` — counter. Labels: `condition ∈ {ResourcesApplied, ResourcesHealthy}`; `status ∈ {False, Progressing, Unknown}`. Incremented per condition per reconcile when the named condition is present on the MR with a status other than `True`. **Missing conditions are intentionally not counted** — a freshly created MR may not have status populated yet, and treating missing as unhealthy would generate noise during normal MR creation. Each increment is paired with an error-level log line carrying the condition's `Reason` and `Message`. The check is non-blocking — the stamp still happens — and runs on every reconcile pass (including periodic `--sync-period` ticks), so the rate naturally surfaces both "many MRs unhealthy briefly" and "one MR chronically unhealthy".

### Examples

The `converged` paths dominate in steady state, so a useful "is rotation propagating" panel is:

```
sum by (kind) (rate(webhook_injector_target_stamps_total{result="stamped"}[5m]))
+
rate(webhook_injector_managed_resource_crds_stamped_total[5m])
```

A useful "error budget" panel:

```
sum by (job) (rate(webhook_injector_target_stamps_total{result="error"}[5m]))
+ sum by (job) (rate(webhook_injector_managed_resource_stamps_total{result="error"}[5m]))
+ sum by (job) (rate(webhook_injector_admission_decisions_total{result="error"}[5m]))
+ sum by (job) (rate(webhook_injector_managed_resource_force_reconcile_total{result="error"}[5m]))
```

