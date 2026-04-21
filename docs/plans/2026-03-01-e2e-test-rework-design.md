<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# E2E Test Rework Design

**Date:** 2026-03-01
**Status:** Approved

## Overview

Substantial rework of the e2e integration tests to address:
- **Slow execution** - Tests take too long to run and iterate on
- **Poor coverage** - Missing focused load + rotation testing
- **Complexity** - Overly complex metrics collection via port-forwarding
- **Iteration friction** - Must recreate clusters every run

## Goals

1. **Single focused test** - Continuous ConfigMap UPDATE load with periodic certificate rotations
2. **Fast iteration** - Reusable clusters between test runs
3. **Configurable parameters** - Runtime, concurrency, rotation interval, webhook delay
4. **Simple reporting** - Failure rate + individual error details (no complex metrics)
5. **Clean isolation** - Dedicated kubeconfig that doesn't pollute user's config

## Non-Goals

- Metrics collection (request counts, certificate serials, replica tracking)
- Multiple test scenarios (just one focused load test)
- Performance benchmarking (focus on correctness/availability)

## Design

### 1. Test Configuration & Flags

**Approach:** Pure Go `flag` package with `TestMain()` parsing.

**Flags:**
```go
var (
    testRuntime       = flag.Duration("test.runtime", 2*time.Minute, "How long to run the load test")
    testConcurrency   = flag.Int("test.concurrency", 5, "Number of concurrent clients")
    rotationInterval  = flag.Duration("test.rotation-interval", 30*time.Second, "How often to trigger cert rotation")
    reuseClusters     = flag.Bool("test.reuse-clusters", false, "Skip cluster setup/teardown")
    webhookDelay      = flag.Duration("test.webhook-delay", 100*time.Millisecond, "Average webhook processing delay")
)
```

**Usage examples:**
```bash
# Quick test with defaults
go test -v -timeout 5m

# Stress test
go test -v -timeout 15m -test.runtime=10m -test.concurrency=20 -test.rotation-interval=1m

# Fast iteration (reuse clusters)
go test -v -timeout 5m -test.reuse-clusters=true
```

### 2. Test Structure & Flow

**Single test function:** `TestWebhookLoadAndRotation(t *testing.T)`

**Execution phases:**

1. **Setup (conditional)**
   - If `!reuse-clusters`: Run `setup.sh`, register `t.Cleanup()` for `teardown.sh`
   - If `reuse-clusters`: Log that we're reusing existing clusters
   - Wait for webhook-server deployment ready (3 replicas)

2. **Pre-flight validation**
   - Create N ConfigMaps for load clients: `load-test-cm-0`, `load-test-cm-1`, ..., `load-test-cm-N`
   - Verify webhook is working (single UPDATE succeeds)

3. **Concurrent load + rotation**
   - Start N goroutines (one per concurrency level) doing ConfigMap UPDATEs
   - Start rotation goroutine triggering cert rotations at configured interval
   - All failures collected in buffered channel
   - Run for configured duration via `context.WithTimeout()`

4. **Report results**
   - Stop all goroutines (cancel context)
   - Close failure channel, collect all failures
   - Print: total requests, failure count, failure rate
   - Print each individual failure with timestamp
   - Fail test if any failures occurred

5. **Teardown (conditional)**
   - Via `t.Cleanup()` if `!reuse-clusters`

### 3. Webhook Server Simplification

**Remove:**
- All metrics tracking (atomic counters, serial numbers, reload tracking)
- HTTP `/metrics` endpoint and port 8080
- Certificate reload monitoring goroutine
- Replica name tracking (`HOSTNAME` env var)

**Keep:**
- Admission handler at `/validate`
- Health probes on port 8082
- Controller-runtime manager with webhook server

**Add:**
- Configurable jittered delay (±20% of configured delay)
- Webhook delay flag: `-webhook-delay=100ms`

**Core handler logic:**
```go
func (v *ConfigMapValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
    // Jittered delay: ±20% of configured delay
    jitter := 1.0 + (rand.Float64()*0.4 - 0.2) // 0.8 to 1.2
    delay := time.Duration(float64(*webhookDelay) * jitter)
    time.Sleep(delay)

    return admission.Allowed("")
}
```

**Line count:** ~100 lines (down from 261)

### 4. Cluster Lifecycle & Reusability

**Bash scripts remain:** `setup.sh` and `teardown.sh`

**Conditional execution in Go test:**
```go
func TestWebhookLoadAndRotation(t *testing.T) {
    if !*reuseClusters {
        t.Log("Setting up clusters...")
        if err := runCommand("./setup.sh"); err != nil {
            t.Fatalf("Setup failed: %v", err)
        }

        t.Cleanup(func() {
            t.Log("Tearing down clusters...")
            runCommand("./teardown.sh") // Ignore errors
        })
    } else {
        t.Log("Reusing existing clusters (skipping setup/teardown)")
    }

    // Wait for deployment ready (tolerant of transient not-ready state)...
}
```

**Fast iteration workflow:**
```bash
# First run - create clusters
go test -v -timeout 5m

# Subsequent runs - reuse (saves ~60s per run)
go test -v -timeout 5m -test.reuse-clusters=true
go test -v -timeout 5m -test.reuse-clusters=true

# When done, manual cleanup
cd test/e2e && ./teardown.sh
```

### 5. Isolated Kubeconfig Management

**Problem:** Current approach pollutes user's kubeconfig with `kind-hosting` and `kind-target` contexts.

**Solution:** Dedicated kubeconfig file at `test/e2e/kubeconfig`

**setup.sh changes:**
```bash
#!/usr/bin/env bash
set -euo pipefail

# Use dedicated kubeconfig for e2e tests
export KUBECONFIG="$(pwd)/kubeconfig"

# Create fresh kubeconfig file
cat > "$KUBECONFIG" <<EOF
apiVersion: v1
kind: Config
clusters: []
contexts: []
current-context: ""
users: []
EOF

# Create clusters (they'll add to our dedicated kubeconfig)
kind create cluster --config kind-hosting.yaml --name hosting
kind create cluster --config kind-target.yaml --name target

# All kubectl/kind commands use KUBECONFIG automatically
kubectl --context kind-hosting wait --for=condition=Ready nodes --all
# ... rest of setup
```

**teardown.sh changes:**
```bash
#!/usr/bin/env bash
set -euo pipefail

export KUBECONFIG="$(pwd)/kubeconfig"
kind delete cluster --name hosting
kind delete cluster --name target
rm -f kubeconfig
```

**Go test:**
```go
func TestWebhookLoadAndRotation(t *testing.T) {
    // Set KUBECONFIG for this test
    kubeconfigPath := filepath.Join(".", "kubeconfig")
    t.Setenv("KUBECONFIG", kubeconfigPath)

    // Now all k8s clients use dedicated kubeconfig
}
```

**.gitignore addition:**
```
test/e2e/kubeconfig
test/e2e/*.test
```

### 6. Load Generation & Failure Tracking

**Load generation approach:**
- Each client goroutine owns a dedicated ConfigMap: `load-test-cm-{clientID}`
- Continuously UPDATEs its ConfigMap (no update conflicts)
- Avoids etcd bloat (UPDATE vs CREATE)

**Client goroutine:**
```go
func runLoadClient(ctx context.Context, clientID int, client *kubernetes.Clientset,
                   failures chan<- FailureRecord, totalRequests *atomic.Int64) {
    ticker := time.NewTicker(500 * time.Millisecond) // ~2 req/sec per client
    defer ticker.Stop()

    cmName := fmt.Sprintf("load-test-cm-%d", clientID)
    reqCount := 0

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            reqCount++
            totalRequests.Add(1)

            cm, err := client.CoreV1().ConfigMaps("default").Get(ctx, cmName, metav1.GetOptions{})
            if err != nil {
                failures <- FailureRecord{time.Now(), clientID, "get", err}
                continue
            }

            cm.Data["counter"] = fmt.Sprintf("%d", reqCount)
            _, err = client.CoreV1().ConfigMaps("default").Update(ctx, cm, metav1.UpdateOptions{})
            if err != nil {
                failures <- FailureRecord{time.Now(), clientID, "update", err}
            }
        }
    }
}
```

**Rotation goroutine:**
```go
func runRotationTrigger(ctx context.Context, client *kubernetes.Clientset,
                       interval time.Duration, failures chan<- FailureRecord) {
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if err := triggerRotation(client); err != nil {
                failures <- FailureRecord{time.Now(), -1, "rotation", err}
            }
        }
    }
}
```

**Failure tracking:**
```go
type FailureRecord struct {
    Timestamp time.Time
    ClientID  int    // -1 for rotation errors
    Operation string // "get", "update", "rotation"
    Error     error
}

// Buffered channel to avoid blocking
failures := make(chan FailureRecord, 1000)

// Collector goroutine
var collectedFailures []FailureRecord
go func() {
    for f := range failures {
        collectedFailures = append(collectedFailures, f)
    }
}()
```

### 7. Error Reporting & Output

**Output format (success):**
```
=== RUN   TestWebhookLoadAndRotation
    integration_test.go:45: Setting up clusters...
    integration_test.go:52: Waiting for webhook deployment ready...
    integration_test.go:58: Creating 10 load test ConfigMaps...
    integration_test.go:65: Starting load test (runtime=2m0s, concurrency=10, rotation=30s)
    integration_test.go:120: Load test completed

=== Load Test Results ===
Duration: 2m0s
Total requests: 2400
Failures: 0 (0.00%)

--- PASS: TestWebhookLoadAndRotation (125.34s)
```

**Output format (with failures):**
```
=== Load Test Results ===
Duration: 2m0s
Total requests: 2403
Failures: 3 (0.12%)

Failed requests:
  [2026-03-01 10:23:45.123] client-3 update: connection refused
  [2026-03-01 10:24:12.456] client-7 update: context deadline exceeded
  [2026-03-01 10:24:45.789] rotation: failed to annotate secret: not found

--- FAIL: TestWebhookLoadAndRotation (125.34s)
```

**Pass/fail criteria:**
- **Pass:** Zero failures
- **Fail:** Any failure occurred

### 8. File Structure & Organization

**New file structure:**
```
test/e2e/
├── .gitignore                      # Add: kubeconfig, *.test
├── go.mod                          # Simplified dependencies
├── go.sum
├── integration_test.go             # Single test file (~200 lines)
├── setup.sh                        # Modified: use dedicated kubeconfig
├── teardown.sh                     # Modified: use dedicated kubeconfig
├── kind-hosting.yaml               # Unchanged
├── kind-target.yaml                # Unchanged
├── skaffold.yaml                   # Unchanged
├── README.md                       # Updated with new usage
├── webhook-server/
│   └── main.go                     # Simplified (~100 lines)
└── manifests/
    ├── rbac.yaml                   # Unchanged
    ├── service.yaml                # Unchanged
    ├── webhook-config.yaml         # Unchanged
    └── webhook-server-deployment.yaml  # Modified: remove port 8080
```

**Files to delete:**
- `debug-pods.sh` (no longer needed)
- `testdata/` directory (if empty/unused)

**Estimated line counts:**
- `integration_test.go`: ~200 lines (down from 392)
- `webhook-server/main.go`: ~100 lines (down from 261)
- **Total Go code:** ~300 lines (down from 653)

## Migration Plan

1. Create new simplified webhook server in parallel
2. Update deployment manifests (remove metrics port)
3. Rewrite integration_test.go with new structure
4. Update setup.sh and teardown.sh for isolated kubeconfig
5. Update README.md with new usage examples
6. Delete old files (debug-pods.sh, etc.)
7. Test thoroughly with both cluster modes (fresh + reuse)

## Success Metrics

- Test execution time with reuse-clusters: <5s (down from 125s)
- Test code maintainability: ~300 lines (down from 653)
- Zero flakiness from port-forwarding or metrics collection
- Clear failure reporting for debugging

## Trade-offs

**Accepted:**
- Less observability (no metrics) - acceptable since failures are the signal
- Single test function - acceptable since it's focused and configurable
- Still depends on bash scripts - acceptable since they're simple and working

**Rejected alternatives:**
- Testify framework - unnecessary complexity for simple pass/fail test
- Multiple test functions - would slow down execution
- Config file approach - flags are more flexible and idiomatic
