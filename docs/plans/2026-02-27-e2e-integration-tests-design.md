<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# E2E Integration Tests Design

**Date:** 2026-02-27
**Status:** Approved

## Overview

This design replaces the existing envtest-based integration tests with a comprehensive E2E testing framework that uses two real kind clusters to validate webhook-injector's core value proposition: zero-downtime certificate rotation in a multi-replica, multi-cluster environment.

## Problem Statement

The current integration tests (test/integration/) use envtest with fake clients, which provide limited value beyond unit tests:

1. **Redundant coverage**: ~70% of integration test scenarios are already covered by unit tests
2. **Missing real integration**: No validation of actual webhook interception, certificate trust chains, or multi-cluster communication
3. **No HA testing**: Cannot validate zero-downtime during certificate rotation with multiple replicas
4. **Not developer-friendly**: Cannot be used for manual testing or development

## Solution Architecture

### Dual-Purpose E2E Framework

Create a test environment that serves both automated testing and manual development:

**Automated testing:**
```bash
gmake test-e2e    # Full E2E test with kind clusters
```

**Manual development:**
```bash
gmake e2e-setup   # Create clusters once
gmake e2e-dev     # Skaffold dev with hot reload
# ... iterate on code ...
gmake e2e-teardown
```

### Infrastructure

**Two kind clusters:**
- **kind-hosting**: Runs webhook server + webhook-injector sidecars (3 replicas)
- **kind-target**: Enforces webhooks, receives test ConfigMaps

**Deployment:**
- Skaffold with Ko builder for fast Go binary builds
- Deploys only to hosting cluster
- Target cluster accessed via admin kubeconfig Secret

**Test webhook server:**
- Simple validating webhook for ConfigMap CREATE operations
- Exposes `/metrics` endpoint for test assertions
- Uses controller-runtime certificate manager for hot reload
- Always allows requests (doesn't actually validate, just logs)

### Directory Structure

```
test/e2e/
├── integration_test.go           # Main test file
├── setup.sh                      # Creates kind clusters
├── teardown.sh                   # Destroys clusters
├── skaffold.yaml                 # Ko-based build and deploy
├── kind-hosting.yaml             # Hosting cluster config
├── kind-target.yaml              # Target cluster config
├── go.mod                        # Separate go.mod for e2e tests
├── go.sum
├── manifests/
│   ├── webhook-server-deployment.yaml
│   ├── webhook-config.yaml
│   ├── rbac.yaml
│   └── service.yaml
├── webhook-server/
│   └── main.go                   # Test webhook server
└── testdata/
    └── sample-configmap.yaml     # Example ConfigMap for manual testing
```

## Key Design Decisions

### 1. Single Skaffold Definition

**Decision:** Deploy only to hosting cluster, access target cluster via admin kubeconfig.

**Rationale:**
- Simpler setup (one skaffold.yaml)
- No RBAC needed in target cluster
- Mimics real use case where target cluster may be remote/restricted

**Implementation:**
```bash
# In setup.sh
kubectl config view --context kind-target --minify --flatten \
  | kubectl --context kind-hosting create secret generic target-kubeconfig \
    --from-file=kubeconfig=/dev/stdin
```

### 2. ConfigMap Validation Webhook

**Decision:** Webhook validates ConfigMap CREATE operations with `failurePolicy: Fail`.

**Rationale:**
- No need to deploy anything to target cluster
- High-frequency test resource (can create 100+ ConfigMaps quickly)
- Failure policy ensures we catch any webhook unavailability

**Webhook configuration:**
```yaml
webhooks:
  - name: configmap.validator.test
    clientConfig:
      url: https://webhook-service.default.svc.cluster.local:9443/validate
    rules:
      - operations: ["CREATE"]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["configmaps"]
    failurePolicy: Fail
    sideEffects: None
```

### 3. Go Duration for Certificate Validity

**Decision:** Replace `CERT_VALIDITY_DAYS` with `CERT_VALIDITY` accepting Go duration strings.

**Configuration:**
```yaml
# For testing
env:
  - name: CERT_VALIDITY
    value: "60s"  # Rotation after 30s (50% threshold)

# For production
env:
  - name: CERT_VALIDITY
    value: "720h"  # 30 days
```

**Rationale:**
- Single configuration option (no CERT_VALIDITY_DAYS vs CERT_VALIDITY_SECONDS)
- Flexible and intuitive (60s, 5m, 24h, 720h)
- Standard Go convention

**Code changes:**
```go
// pkg/config/config.go
type Config struct {
    CertValidity time.Duration  // Changed from CertValidityDays int
    // ...
}

func LoadFromEnv() (*Config, error) {
    validityStr := getEnv("CERT_VALIDITY", "720h")
    validity, err := time.ParseDuration(validityStr)
    if err != nil {
        return nil, fmt.Errorf("invalid CERT_VALIDITY: %w", err)
    }
    // ...
}
```

### 4. Annotation-Based Rotation Trigger

**Decision:** Annotate the certificate Secret to trigger immediate rotation.

**Annotation format:**
```bash
kubectl annotate secret webhook-injector-certs \
  webhook-injector.sap.com/force-rotation="true"
```

**WebhookReconciler behavior:**
1. Watch Secret for annotation `webhook-injector.sap.com/force-rotation=true`
2. If present: skip time/SAN checks, regenerate certificates immediately
3. After writing new certificates: remove annotation atomically

**Rationale:**
- Annotating Secret makes semantic sense (rotation is about certificates)
- Self-cleaning (annotation removed after rotation)
- Enables both automated testing (annotate) and time-based rotation (wait)
- Manual testing/debugging capability

### 5. Controller-Runtime Certificate Manager

**Decision:** Use controller-runtime's certificate manager for hot reload.

**Expectation:** Zero downtime during certificate rotation.

**Implementation in test webhook server:**
```go
import (
    "sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

watcher, err := certwatcher.New("/tmp/webhook-certs/tls.crt", "/tmp/webhook-certs/tls.key")
server := &http.Server{
    TLSConfig: &tls.Config{
        GetCertificate: watcher.GetCertificate,
    },
}
```

**Rationale:**
- Hot reload without downtime
- Battle-tested (used by controller-runtime webhooks)
- Matches production webhook server recommendations

### 6. Replace envtest Integration Tests

**Decision:** Remove `test/integration/` directory and replace with E2E tests.

**Makefile.maker.yaml changes:**
```yaml
verbatim: |
  check: test-e2e  # Changed from test-integration

  .PHONY: test-e2e
  test-e2e: FORCE
    @printf "\e[1;36m>> Running E2E tests\e[0m\n"
    cd test/e2e && go test -v -timeout 10m ./...

  .PHONY: e2e-setup
  e2e-setup: FORCE
    @printf "\e[1;36m>> Setting up E2E environment\e[0m\n"
    cd test/e2e && ./setup.sh

  .PHONY: e2e-dev
  e2e-dev: e2e-setup
    @printf "\e[1;36m>> Starting Skaffold dev mode\e[0m\n"
    cd test/e2e && skaffold dev

  .PHONY: e2e-teardown
  e2e-teardown: FORCE
    @printf "\e[1;36m>> Tearing down E2E environment\e[0m\n"
    cd test/e2e && ./teardown.sh
```

**Rationale:**
- Real Kubernetes clusters prove actual functionality
- E2E tests provide value beyond unit tests
- Dual-purpose framework supports development workflow

## Test Implementation

### Test Flow

```go
func TestE2E_WebhookHighAvailability(t *testing.T) {
    // Phase 1: Setup
    setupClusters(t)
    deployWithSkaffold(t)
    waitForWebhookReady(t, 3) // 3 replicas

    // Phase 2: Baseline validation
    results := createConfigMaps(t, 30)
    assertAllSucceeded(t, results)
    metrics := collectMetrics(t)
    assertAllReplicasServing(t, metrics) // All 3 replicas handled requests

    // Phase 3: Rotation under load
    stopChan := make(chan struct{})
    failures := startContinuousLoad(t, stopChan) // 1 ConfigMap/sec

    time.Sleep(30 * time.Second) // Let some load accumulate
    triggerRotation(t) // Annotate Secret
    time.Sleep(60 * time.Second) // Continue load during rotation

    close(stopChan)

    // Phase 4: Assert zero failures
    assertZeroFailures(t, failures)

    // Phase 5: Post-rotation verification
    newMetrics := collectMetrics(t)
    assertCertificateRotated(t, metrics, newMetrics)
    assertAllReplicasReloaded(t, newMetrics)
}
```

### Metrics Endpoint

**Test webhook server exposes:**
```go
type Metrics struct {
    ReplicaName          string    `json:"replica_name"`
    TotalRequests        int64     `json:"total_requests"`
    CertificateReloads   int       `json:"certificate_reloads"`
    CertificateSerial    string    `json:"certificate_serial"`
    LastRequestTime      time.Time `json:"last_request_time"`
    LastCertReloadTime   time.Time `json:"last_cert_reload_time"`
    CurrentlyReloading   bool      `json:"currently_reloading"`
}
```

**Test assertions use metrics:**
```go
// Assert all replicas participated
for _, m := range metrics {
    assert.Greater(t, m.TotalRequests, 0,
        "replica %s served no requests", m.ReplicaName)
}

// Assert all replicas reloaded
for _, m := range newMetrics {
    assert.Greater(t, m.CertificateReloads, 0,
        "replica %s did not reload", m.ReplicaName)
    assert.NotEqual(t, oldMetrics[m.ReplicaName].CertificateSerial,
        m.CertificateSerial, "certificate not rotated on %s", m.ReplicaName)
}
```

## Success Criteria

### Test Passes If:

1. **Baseline functionality:**
   - All 3 webhook server replicas reach Ready state
   - Webhook configuration applied to target cluster with CA bundle injected
   - 30 ConfigMap creates succeed (proves webhooks are working)
   - All 3 replicas handle at least 1 request (load balancing verified)

2. **Zero downtime during rotation:**
   - 100+ ConfigMap creates during rotation window (60s before + 30s after)
   - **Zero failures** - all admission requests succeed
   - No connection errors, timeout errors, or certificate validation errors

3. **All replicas reload independently:**
   - Certificate serial number changes on all 3 replicas
   - Reload counter increments on all 3 replicas
   - All replicas serve requests both before and after rotation

4. **Target cluster validation:**
   - Webhook configuration exists in target cluster
   - CA bundle matches current CA certificate
   - Webhook URL points to hosting cluster service

### Expected Output (Success):

```
✓ Clusters created (hosting + target)
✓ Webhook server deployed (3 replicas ready)
✓ Baseline: 30/30 requests succeeded
✓ All replicas serving (replica-0: 10, replica-1: 11, replica-2: 9)
✓ Rotation triggered
✓ Load test: 120/120 requests succeeded during rotation
✓ All replicas reloaded (serial: abc123 → def456)
✓ Post-rotation: 30/30 requests succeeded
PASS
```

### Expected Output (Failure):

```
✗ Load test: 3/120 requests failed during rotation
  - Error: x509: certificate signed by unknown authority
  - Failed at: replica-1, timestamp: 2026-02-27T12:34:56Z
  - Certificate serial at failure: abc123 (old cert)
FAIL
```

## Dependencies

**External tools required:**
- `kind` >= 0.20.0 (kind cluster management)
- `kubectl` >= 1.28 (cluster interaction)
- `skaffold` >= 2.10.0 (includes ko builder)
- Docker (required by kind)

**Go dependencies (test/e2e/go.mod):**
```go
require (
    k8s.io/api v0.35.1
    k8s.io/apimachinery v0.35.1
    k8s.io/client-go v0.35.1
    sigs.k8s.io/controller-runtime v0.23.1  // For certificate manager
)
```

**Prerequisites check:**
```bash
# In setup.sh
check_prerequisites() {
    for cmd in kind kubectl skaffold docker; do
        if ! command -v $cmd &> /dev/null; then
            echo "Error: $cmd is not installed"
            exit 1
        fi
    done
}
```

## Error Handling

### Cluster Creation Failures
```bash
# In setup.sh
if ! kind create cluster --config kind-hosting.yaml; then
    echo "Failed to create hosting cluster"
    exit 1
fi
```

### Skaffold Failures
```go
cmd := exec.Command("skaffold", "run")
if err := cmd.Run(); err != nil {
    t.Fatalf("Skaffold deploy failed: %v", err)
}
```

### Rotation Timeout
```go
ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
defer cancel()

if !waitForCertRotation(ctx, pods) {
    t.Fatal("Certificate rotation did not complete within timeout")
}
```

### Cleanup on Failure
```go
func TestE2E_WebhookHighAvailability(t *testing.T) {
    // Always cleanup, even on failure
    defer func() {
        cmd := exec.Command("./teardown.sh")
        cmd.Run() // Ignore errors during cleanup
    }()

    // Test logic...
}
```

### Metrics Collection Failures
- Retry port-forward up to 3 times
- Fall back to kubectl exec if port-forward fails
- Warn but don't fail test if metrics unavailable (they're for debugging)

## What This Proves (That Unit Tests Cannot)

1. **Certificate trust chain validation** - Real Kubernetes TLS validation
2. **Multi-cluster communication** - Hosting cluster → target cluster
3. **Load balancing** - Kubernetes Service distributes across 3 replicas
4. **Zero downtime** - No failed admission requests during certificate rotation
5. **Hot reload behavior** - Controller-runtime certificate manager works correctly
6. **End-to-end integration** - ConfigMap create → webhook intercept → validation

## Implementation Timeline

- Setup scripts + skaffold config: 2 hours
- Test webhook server: 2 hours
- Integration test implementation: 3 hours
- Config changes (duration parsing, annotation): 1 hour
- Testing and refinement: 2 hours
- **Total: ~10 hours**

## Migration Plan

1. Implement new E2E framework in `test/e2e/`
2. Update `Makefile.maker.yaml` to add `test-e2e` targets
3. Verify E2E tests pass
4. Update `check` target to depend on `test-e2e` instead of `test-integration`
5. Remove `test/integration/` directory
6. Update CI/CD workflows
7. Update README with new testing instructions

## References

- Current integration tests: `test/integration/integration_test.go`
- Existing deployment manifests: `config/deployment.yaml`
- Controller-runtime certificate watcher: https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/certwatcher
- Skaffold ko builder: https://skaffold.dev/docs/builders/builder-types/ko/
