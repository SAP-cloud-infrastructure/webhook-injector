<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

webhook-injector is a Kubernetes controller that runs as a **native sidecar** (Kubernetes 1.29+) to webhook servers, managing webhook configurations and TLS certificates. The key innovation is enabling webhook servers to serve webhooks for remote clusters (clusters they don't run in) while providing a language-agnostic solution that works with any webhook server that can watch and reload certificate files.

**Webhook Server Requirement**: The webhook server must watch certificate files (`tls.crt`, `tls.key`) and reload them when changed. Controller-runtime webhook servers (v0.17+) provide this automatically via an internal `certwatcher`. Other frameworks need custom implementation using inotify/fsnotify or periodic polling.

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

The application registers up to three independent controllers (the second is opt-in via `--managed-resource-label`) with different leader election behavior:

1. **WebhookReconciler** (runs on **leader only**)
   - Location: `pkg/controllers/reconciler.go`
   - Watches: webhook-definitions ConfigMap (`--webhook-config-name`) + certificate Secret on the hosting cluster.
   - Generates/rotates self-signed certificates with dynamic SANs.
   - Stores certificates in Secret (in hosting cluster).
   - Injects CA bundle into `MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration` parsed from the ConfigMap and applies them to the target cluster (creates if missing, otherwise updates).
   - Does **not** author CRDs. CRDs flow through `ManagedResource` objects on the hosting cluster and are stamped by `ManagedResourceReconciler`.
   - Signals `ManagedResourceReconciler` (when registered) on every detected cert rotation so the new caBundle reaches MR-embedded CRDs within seconds.
   - Leader election: DEFAULT behavior (requires leader).

2. **ManagedResourceReconciler** (registered when `--managed-resource-label` is set; runs on **leader only**)
   - Location: `pkg/controllers/managed_resource_reconciler.go`
   - Watches: labeled `ManagedResource` (`resources.gardener.cloud/v1alpha1`) objects on the **hosting** cluster (not the target cluster). Cache is label-scoped, cluster-wide.
   - Why this exists: on a Gardener seed, CRDs are not applied directly to the shoot — they live inside `ManagedResource` Secrets, and gardener-resource-manager (GRM) reconciles them into the shoot. Stamping the shoot-side CRD directly would produce a thrash loop: GRM would overwrite the stamp on its next sync. Stamping the *source* (this controller) makes the stamp survive GRM's next sync.
   - For each labeled MR: walks `spec.secretRefs[*]`, fetches the referenced Secret, decodes each data key (with brotli `.br` support to match GRM's compression), finds CRDs with `conversion.strategy=Webhook`, stamps `clientConfig.caBundle`, and — when `--external-host` is set and the entry is Service-form — rewrites to URL-form via `webhookhost.BuildExternalURL`. Documents the controller doesn't touch survive byte-for-byte (no YAML round-trip), preserving user-authored formatting.
   - Idempotency: a no-op reconcile must produce zero Secret updates. Both per-doc (`maybeStampCRDDoc` returns the original bytes when already converged) and per-Secret (skip `Update` when no doc in that Secret changed).
   - **Force-reconcile annotation** (`--force-reconcile`, default `true`): after a successful Secret stamp, patches `gardener.cloud/operation=reconcile` on the MR so GRM picks up the change immediately even if it is in backoff. Set only on reconciles that actually wrote a Secret, and skipped when the annotation is already present — no-op reconciles do not churn GRM. GRM clears the annotation itself after consuming it.
   - **keepObjects=true invariant**: when an MR ships at least one CRD, the reconciler verifies that `spec.keepObjects` is explicitly `true`. If unset or `false`, the reconcile still stamps the CRD (we'd rather have a working webhook than refuse to write), but logs a warning (Info level with a `WARNING:` prefix — not Error, so no stack trace) and increments `webhook_injector_managed_resource_keep_objects_violations_total`. The counter is the alertable signal; the log line is its human-readable companion. The destructive shape: without `keepObjects=true`, deleting the MR cascades into the shoot's CRDs and every custom resource of those kinds. CRDs with `conversion.strategy=None` still count for this check.
   - **GRM health observation**: every reconcile reads `mr.Status.Conditions` and checks `ResourcesApplied` and `ResourcesHealthy`. If either is present and not `Status=True`, the reconcile logs a warning (Info level with a `WARNING:` prefix — not Error, so no stack trace) including `Reason`/`Message`, and increments `webhook_injector_managed_resource_unhealthy_total{condition,status}`. The counter is the alertable signal; the log is its human-readable companion. Missing conditions (freshly created MRs before GRM evaluates) are deliberately ignored to avoid noise during normal MR creation. The check is non-blocking — the stamp still happens — and runs on every reconcile pass (including periodic `--sync-period` ticks), so dashboards reflect current GRM state.
   - **Rotation fan-out**: subscribes to the rotation signal from `WebhookReconciler`. On rotation, lists every labeled MR on the hosting cluster and enqueues each for reconcile so the new caBundle propagates immediately rather than waiting for the next `--sync-period` tick. Buffered single-slot channel + non-blocking send: a missed signal is harmless because a single signal is enough to trigger a full re-stamp pass. The enqueued reconcile reads the cert Secret via the uncached APIReader path (see the next bullet) so the new caBundle is visible inside the same reconcile pass rather than after the informer catches up.
   - **Cert-Secret read bypasses the cache** (`CertSecretReader = mgr.GetAPIReader()`): every other read goes through the manager's cached client, but the cert Secret is fetched directly from the apiserver. This dodges the rotation-propagation race: `WebhookReconciler` writes a fresh cert Secret and signals a rotation fan-out before the shared informer cache has absorbed the write, so a cached Get would still return the stale `tls.crt`, the MR-embedded CRD's `caBundle` would compare equal, and the re-stamp would silently no-op until the cache catches up (~minutes) or the next `--sync-period` tick. One extra apiserver Get per reconcile pass, bounded by the per-MR reconcile fan-out — seed-wide cost is one Get per labeled MR per rotation.
   - Leader election: DEFAULT behavior (requires leader)

3. **CertificateReloader** (runs on **all replicas**)
   - Location: `pkg/controllers/reloader.go`
   - Watches: Certificate Secret (in hosting cluster)
   - Writes certificates to local filesystem (`/tmp/webhook-certs` by default)
   - Optimization: Only writes when certificate content changes (prevents unnecessary webhook server reloads)
   - Leader election: Uses `WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)})`

### Key Architectural Decisions

**Two-cluster model:**
- **Hosting cluster**: Where webhook-injector and the webhook server run. Also where Gardener `ManagedResource` objects live on a seed.
- **Target cluster**: Where webhooks are enforced (may be same or remote). Receives the `MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration` authored by `WebhookReconciler`. CRDs reach the target cluster only via GRM unpacking the `ManagedResource` Secrets — webhook-injector never authors CRDs directly on the target cluster.
- This enables serving webhooks for clusters you don't run in.

**Why this controller split:**
- `WebhookReconciler` must run on leader only to prevent multiple replicas from generating conflicting certificates or racing to update the target cluster.
- `ManagedResourceReconciler` must run on leader only to avoid concurrent writes to the same MR Secret.
- `CertificateReloader` must run on all replicas so each webhook server pod gets certificates on its local filesystem.

**Native sidecar pattern (Kubernetes 1.29+):**
- webhook-injector is deployed as a native sidecar: an init container with `restartPolicy: Always`.
- Starts before the webhook server, ensuring certificates are available at startup.
- Terminates after the webhook server for clean shutdown ordering.
- Shares emptyDir volume for certificate files.
- Works with any webhook server that can watch certificate files and reload on changes.
- Controller-runtime v0.17+ webhook servers handle this automatically via fsnotify.
- For clusters < 1.29, can fall back to a regular sidecar container (no startup ordering guarantee).

**ConfigMap-driven admission webhooks:**
- Users provide full `MutatingWebhookConfiguration` or `ValidatingWebhookConfiguration` YAML in a ConfigMap.
- webhook-injector parses, injects CA bundle, and applies to target cluster.
- Supports service references (in-cluster) and URLs (remote).

**Gardener seed support (canonical CRD path):**
- On a Gardener seed, CRDs are wrapped inside `ManagedResource` Secrets and applied to the shoot by `gardener-resource-manager`. Stamping the shoot-side CRD alone produces a thrash loop because GRM re-applies the original Secret on every sync.
- `--managed-resource-label` enables `ManagedResourceReconciler`, which stamps the *source* (the MR's referenced Secret on the seed) so the stamp survives GRM's next sync.
- Brotli (`.br` suffix) compression is supported because GRM transparently compresses larger payloads.
- This is the only path webhook-injector uses for CRDs — direct target-cluster CRD authoring is not supported, and removing it kept the controller surface small.

## Code Structure

```
cmd/webhook-injector/           Main entrypoint
  main.go                       Creates manager, sets up controllers, handles target cluster client

pkg/certificates/               Certificate generation using crypto/x509
  generator.go                  GenerateSelfSignedCert() with dynamic SANs
  types.go                      ServiceReference struct

pkg/controllers/                Reconcilers
  reconciler.go                 WebhookReconciler (leader-only, manages certificates and target-cluster MWC/VWC)
  reloader.go                   CertificateReloader (all replicas, syncs certs to filesystem)
  managed_resource_reconciler.go ManagedResourceReconciler (--managed-resource-label only, stamps CRDs inside Gardener ManagedResource Secrets on the hosting cluster)
  cabundle.go                    Shared loadCABundle helper used by ManagedResourceReconciler

pkg/managedresource/            Helpers for decoding/re-encoding Gardener ManagedResource Secret data
  codec.go                      Brotli-aware decode/encode + multi-doc YAML split/join (preserves untouched docs byte-for-byte)

pkg/metrics/                    Prometheus metric definitions
  metrics.go                    Cert rotations / expiry / files-written, target-cluster MWC/VWC stamps, rotation fan-out, ManagedResource stamps + force-reconcile + brotli encoding (see Metrics section)

pkg/webhooks/                   Webhook configuration parsing
  parser.go                     ParseWebhookConfigurations(), ExtractServiceReferences()
  injector.go                   InjectCABundle()

test/e2e/                       E2E integration tests with kind clusters
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
- `--cert-sans` adds extra entries (auto-classified) on top of the ConfigMap-derived SANs.

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
- WebhookReconciler / ManagedResourceReconciler: Use default leader election behavior (no WithOptions) → require leader
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

There is exactly one mode: ConfigMap mode, mandatory. `--webhook-config-name`
must be set or startup fails. `--managed-resource-label` opts in to the
Gardener `ManagedResource` reconciler on the hosting cluster.

**Required:**
- `--webhook-config-name` - ConfigMap (in `--namespace`, hosting cluster) carrying a `webhooks.yaml` key whose value contains the `MutatingWebhookConfiguration` / `ValidatingWebhookConfiguration` YAML to apply to the target cluster. webhook-injector parses, injects the caBundle, and applies these to the target cluster (creating them if missing). **CRDs are not handled here** — CRDs flow through Gardener `ManagedResource` objects on the hosting cluster, see `--managed-resource-label`.

**Optional — host-cluster ManagedResource reconciler:**
- `--managed-resource-label` - label selector (`key=value`) used to scope `ManagedResourceReconciler` on the *hosting* cluster. When set, the controller watches labeled `ManagedResource`s, walks their referenced Secrets, finds embedded CRDs with `conversion.strategy=Webhook`, stamps `caBundle`, and (when `--external-host`/`--external-port` are set) rewrites Service-form `clientConfig` to URL-form. When unset, the controller is not registered.
- `--external-host` - **required when `--managed-resource-label` is set**; DNS name or IP (no port — the port is `--external-port`) the target cluster uses to reach this controller. Combined with `--external-port` to rewrite Service-form conversion `clientConfig` inside MR-embedded CRDs to `clientConfig.URL = https://<external-host>:<external-port><Service.Path>` (path defaults to `/`); the `Service` field is then cleared. Entries that already use URL-form `clientConfig` are left untouched. `caBundle` patching is unchanged. Ignored (with a log line) when `--managed-resource-label` is unset.
- `--external-port` - **required when `--managed-resource-label` is set**; external port the target cluster uses to reach the labeled webhook callbacks.
- `--force-reconcile` - default `true`. When `--managed-resource-label` is set, the controller patches `gardener.cloud/operation=reconcile` on each `ManagedResource` after a successful Secret stamp. This kicks `gardener-resource-manager` out of any backoff or debounce state so the new caBundle reaches the shoot promptly. The annotation is set **only** on reconciles that actually wrote a Secret — idempotent no-op reconciles do not churn GRM. The reconciler also skips the patch when the annotation is already present (GRM clears it after consuming, per the documented Gardener contract). Set to `false` to rely solely on GRM's own Secret watch.

**Cert lifecycle:**
- `--cert-sans` - comma-separated DNS names and/or IP addresses to include as additional certificate SANs (merged with SANs derived from the ConfigMap's webhook services/URLs). Each entry is auto-classified: parses as IP → IP SAN; otherwise → DNS SAN.

**Common:**
- `--namespace` - Namespace for all resources (ConfigMap, Secret, Lease). Auto-detects from env vars or pod's serviceaccount if not set.
  - Priority order: explicit flag → `NAMESPACE` env var → serviceaccount file → **fail**
  - The program will exit with an error if namespace cannot be determined.
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

### Target-cluster stamping (ConfigMap mode)

- `webhook_injector_target_stamps_total{kind, result}` — counter. Labels: `kind ∈ {MutatingWebhookConfiguration, ValidatingWebhookConfiguration}`; `result ∈ {stamped, converged, error}`. Emitted by `WebhookReconciler.applyWebhookConfigurations`. `stamped` = `Create` or `Update` succeeded; `converged` = idempotency-skip; `error` = Get/Create/Update failure. CRDs are intentionally absent — CRDs flow through `ManagedResource` Secrets and are accounted under the ManagedResource metrics below.

### Rotation fan-out

- `webhook_injector_rotation_fanout_events_total{kind, outcome}` — counter. Labels: `kind ∈ {ManagedResource}`; `outcome ∈ {emitted, dropped}`. Emitted by `managedResourceResyncTrigger.fanOut` for every per-kind event pushed to (or dropped from) the controller's reconcile channel on every cert rotation. A non-zero `dropped` rate is informational — the next rotation or the periodic `--sync-period` tick re-converges; alarming on it makes sense only if the rate is sustained.

### ManagedResource stamping (`--managed-resource-label` only)

- `webhook_injector_managed_resource_stamps_total{result}` — counter. Labels: `result ∈ {stamped, converged, error}`. Per-MR reconcile outcome. `stamped` = at least one referenced Secret was rewritten; `converged` = all Secrets already in desired state; `error` = Get / decode / Update failure.
- `webhook_injector_managed_resource_crds_stamped_total` — counter (no labels). Per-CRD-document granularity, summed across all MRs and all Secrets. Distinct from the stamps counter because a single MR may carry multiple CRDs across multiple Secrets — this is the right signal for "how much work did the last rotation actually do".
- `webhook_injector_managed_resource_secret_keys_total{encoding}` — counter. Labels: `encoding ∈ {plain, brotli}`. Increments once per Secret data key processed. Lets a dashboard confirm the brotli codec path is exercised against the live fleet — a seed whose MRs only ship plain YAML will show `encoding=brotli` at zero.
- `webhook_injector_managed_resource_force_reconcile_total{result}` — counter. Labels: `result ∈ {patched, skipped, error}`. Tracks the `gardener.cloud/operation=reconcile` annotation patch. `patched` = annotation actually written (MR Patch succeeded); `skipped` = annotation already present (a previous reconcile set it and GRM has not yet consumed it); `error` = patch failed (reconcile itself does not fail in that case, so this counter is the sole signal of degraded GRM-kick).
- `webhook_injector_managed_resource_keep_objects_violations_total` — counter (no labels). Incremented once per reconcile of a labeled MR that ships at least one CRD but does not set `spec.keepObjects: true`. The check is non-blocking — the controller still stamps the CRD so the webhook keeps working — but the counter and an accompanying warning-level log (Info-with-`WARNING:`-prefix, no stack trace) surface a destructive misconfiguration: deleting that MR would cascade-delete every shoot CRD it ships, taking every custom resource of those kinds with it. The counter is the alertable signal; the log is its human-readable companion. The check measures _MRs_, not _CRDs_, so an MR with five CRDs and no `keepObjects=true` increments once per reconcile, not five times. CRDs with `conversion.strategy=None` still count as "observed CRDs" for this check — the destructive shape applies regardless of whether we rewrite the conversion clientConfig.
- `webhook_injector_managed_resource_unhealthy_total{condition, status}` — counter. Labels: `condition ∈ {ResourcesApplied, ResourcesHealthy}`; `status ∈ {False, Progressing, Unknown}`. Incremented per condition per reconcile when the named condition is present on the MR with a status other than `True`. **Missing conditions are intentionally not counted** — a freshly created MR may not have status populated yet, and treating missing as unhealthy would generate noise during normal MR creation. Each increment is paired with a warning-level log line (Info-with-`WARNING:`-prefix, no stack trace) carrying the condition's `Reason` and `Message` — the counter is the alertable signal; the log is its human-readable companion. The check is non-blocking — the stamp still happens — and runs on every reconcile pass (including periodic `--sync-period` ticks), so the rate naturally surfaces both "many MRs unhealthy briefly" and "one MR chronically unhealthy". `Status=Unknown` with reason `ChecksPending` is a normal transient state right after GRM applies resources, which is precisely why this is a warning, not an error.

### Examples

A useful "is rotation propagating" panel:

```
sum by (kind) (rate(webhook_injector_target_stamps_total{result="stamped"}[5m]))
+
rate(webhook_injector_managed_resource_crds_stamped_total[5m])
```

A useful "error budget" panel:

```
sum by (job) (rate(webhook_injector_target_stamps_total{result="error"}[5m]))
+ sum by (job) (rate(webhook_injector_managed_resource_stamps_total{result="error"}[5m]))
+ sum by (job) (rate(webhook_injector_managed_resource_force_reconcile_total{result="error"}[5m]))
```
