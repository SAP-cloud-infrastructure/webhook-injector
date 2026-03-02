# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

webhook-injector is a Kubernetes controller that runs as a **sidecar** to webhook servers, managing the complete lifecycle of webhook configurations and TLS certificates. The key innovation is enabling webhook servers to serve webhooks for remote clusters (clusters they don't run in) while providing a generic solution that works with any webhook server framework.

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

### Two Controllers Pattern

The application contains two independent controllers with different leader election behavior:

1. **WebhookReconciler** (runs on **leader only**)
   - Location: `pkg/controllers/reconciler.go`
   - Watches:
     - ConfigMap containing webhook definitions (in hosting cluster)
     - Certificate Secret for force-rotation annotation (in hosting cluster)
   - Generates/rotates CA and server certificates with dynamic SANs
   - Stores certificates in Secret (in hosting cluster)
   - Injects CA bundle into webhook configurations
   - Applies webhook configurations to **target cluster** (may be remote)
   - Leader election: DEFAULT behavior (requires leader)

2. **CertificateReloader** (runs on **all replicas**)
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

**Why two controllers:**
- WebhookReconciler must run on leader only to prevent multiple replicas from generating conflicting certificates or racing to update target cluster
- CertificateReloader must run on all replicas so each webhook server pod gets certificates on its local filesystem

**Sidecar pattern:**
- webhook-injector runs as sidecar container alongside webhook server
- Shares emptyDir volume for certificate files
- Works with any webhook server (any language/framework) that can watch files

**ConfigMap-driven:**
- Users provide full `MutatingWebhookConfiguration` or `ValidatingWebhookConfiguration` YAML in a ConfigMap
- webhook-injector parses, injects CA bundle, and applies to target cluster
- Supports service references (in-cluster) and URLs (remote)

## Code Structure

```
cmd/webhook-injector/           Main entrypoint
  main.go                       Creates manager, sets up both controllers, handles target cluster client

pkg/certificates/               Certificate generation using crypto/x509
  generator.go                  GenerateCA(), GenerateServerCert() with dynamic SANs
  types.go                      ServiceReference struct

pkg/config/                     Environment variable configuration
  config.go                     LoadFromEnv(), Validate()

pkg/controllers/                Two independent controllers
  reconciler.go                 WebhookReconciler (leader-only, manages certificates and target cluster)
  reloader.go                   CertificateReloader (all replicas, syncs certs to filesystem)

pkg/webhooks/                   Webhook configuration parsing
  parser.go                     ParseWebhookConfigurations(), ExtractServiceReferences()
  injector.go                   InjectCABundle()

test/e2e/                       E2E integration tests with kind clusters
```

## Certificate Lifecycle

**Generation triggers:**
1. ConfigMap changes (new webhook definitions)
2. Time-based rotation: remaining validity < threshold × total validity (default: 80%)
3. Config-based rotation: Service references or URLs change (new SANs required)
4. Manual rotation: Force-rotation annotation on Secret (`webhook-injector.sap.com/force-rotation: "true"`)

**Certificate SANs:**
- Service-based webhooks: `<service>.<namespace>.svc`, `<service>.<namespace>.svc.cluster.local`
- URL-based webhooks: hostname extracted from URL (e.g., `webhook.example.com`)

**Storage flow:**
1. WebhookReconciler generates certs → stores in Secret (hosting cluster)
2. CertificateReloader watches Secret → writes to `/tmp/webhook-certs/`
3. Webhook server watches filesystem → automatically reloads (if using controller-runtime v0.17+)

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

All configuration via command-line flags. Run `webhook-injector --help` for full list:
- `--webhook-config-name` (required) - ConfigMap with webhook definitions
- `--namespace` - Namespace for all resources (ConfigMap, Secret, Lease). Auto-detects from NAMESPACE env var or pod's serviceaccount if not set
  - Priority order: explicit flag → NAMESPACE env var → serviceaccount file → **fail**
  - The program will exit with an error if namespace cannot be determined
- `--cert-secret-name` - Where to store certificates
- `--cert-dir` - Where to write certificate files
- `--cert-validity` - Certificate lifetime as duration (e.g., "720h", "30d", "1y")
- `--cert-rotation-threshold` - When to rotate (0-1)
- `--target-kubeconfig` - Path to target cluster kubeconfig (optional, defaults to in-cluster)
- `--enable-leader-election` - Enable HA with multiple replicas
- `--sync-period` - Reconciliation interval

## Remote Cluster Support

The target cluster client is created in `main.go`:
- If `--target-kubeconfig` flag set: Use explicit kubeconfig file
- Otherwise: Use in-cluster config (hosting cluster = target cluster)

**Important**: Use `--target-kubeconfig` not `KUBECONFIG` to avoid interfering with the manager's hosting cluster connection (which uses standard client-go config loading that checks KUBECONFIG).
