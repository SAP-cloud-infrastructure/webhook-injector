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

The application registers up to three independent controllers (the third is patch-mode only) with different leader election behavior:

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

3. **CertificateReloader** (runs on **all replicas**)
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

## Code Structure

```
cmd/webhook-injector/           Main entrypoint
  main.go                       Creates manager, sets up both controllers, handles target cluster client

pkg/certificates/               Certificate generation using crypto/x509
  generator.go                  GenerateSelfSignedCert() with dynamic SANs
  types.go                      ServiceReference struct

pkg/config/                     Environment variable configuration
  config.go                     LoadFromEnv(), Validate()

pkg/controllers/                Two independent controllers
  reconciler.go                 WebhookReconciler (leader-only, manages certificates and target cluster)
  reloader.go                   CertificateReloader (all replicas, syncs certs to filesystem)

pkg/metrics/                    Prometheus metric definitions
  metrics.go                    CertificateRotationsTotal, CertificateExpiryTime, CertFilesWrittenTotal

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
- `--sync-period` - Reconciliation interval

Patch-mode-only flags:
- `--admission-webhook-name` - name of the `MutatingWebhookConfiguration` the controller bootstraps in the target cluster to register itself as an admission mutator on labeled CRDs (default `webhook-injector-mutator`). Empty disables bootstrap; `TargetWebhookReconciler` continues to converge MWC/VWC/CRD via Watch, but CRDs lose synchronous stamping at admission time.
- `--admission-webhook-port` - port the admission webhook server binds to inside the pod (default `9444`). Configure your Service / NodePort / LoadBalancer to forward `--admission-external-port` to this port.
- `--admission-external-port` - **required when `--admission-webhook-name` is non-empty**; external port the target cluster uses to reach this controller's admission webhook server. Combined with `--external-host` to form the bootstrap MWC URL `https://<external-host>:<admission-external-port>/mutate-crd`. Distinct from `--external-port`, which targets the labeled webhook callbacks. No default — the value depends entirely on the user's Service / NodePort / LoadBalancer wiring, and a wrong default would silently produce a bootstrap MWC the apiserver cannot reach.

## Remote Cluster Support

The target cluster client is created in `main.go`:
- If `--target-kubeconfig` flag set: Use explicit kubeconfig file
- Otherwise: Use in-cluster config (hosting cluster = target cluster)

**Important**: Use `--target-kubeconfig` not `KUBECONFIG` to avoid interfering with the manager's hosting cluster connection (which uses standard client-go config loading that checks KUBECONFIG).
