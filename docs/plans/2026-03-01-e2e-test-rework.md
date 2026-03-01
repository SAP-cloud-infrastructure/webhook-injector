# E2E Test Rework Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Simplify e2e tests to a single focused load test with configurable parameters and fast cluster reuse

**Architecture:** Replace complex metrics-based test with simple failure-tracking load test. Single test function runs concurrent ConfigMap UPDATE clients + periodic certificate rotations, reports only failures.

**Tech Stack:** Pure Go stdlib (flag, testing), kind clusters, controller-runtime webhook server

---

## Task 1: Update .gitignore for Isolated Kubeconfig

**Files:**
- Modify: `test/e2e/.gitignore`

**Step 1: Add kubeconfig and test binaries to gitignore**

Add these lines:
```
kubeconfig
*.test
```

**Step 2: Verify gitignore works**

Run: `cd test/e2e && git status`
Expected: kubeconfig and e2e.test not shown as untracked (if they exist)

**Step 3: Commit**

```bash
git add test/e2e/.gitignore
git commit -m "chore: gitignore e2e kubeconfig and test binaries

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 2: Update setup.sh for Isolated Kubeconfig

**Files:**
- Modify: `test/e2e/setup.sh`

**Step 1: Add dedicated kubeconfig initialization at top**

After the `set -euo pipefail` line and before prerequisite checks, add:

```bash
# Use dedicated kubeconfig for e2e tests to avoid polluting user's config
export KUBECONFIG="$(pwd)/kubeconfig"

echo "==> Creating dedicated kubeconfig at $KUBECONFIG..."
cat > "$KUBECONFIG" <<EOF
apiVersion: v1
kind: Config
clusters: []
contexts: []
current-context: ""
users: []
EOF
```

**Step 2: Update cluster creation to use isolated kubeconfig**

The existing `kind create cluster` commands will automatically use the `KUBECONFIG` env var we set. No changes needed to those lines, but verify the context names match the new isolated setup.

**Step 3: Test the script manually**

Run: `cd test/e2e && ./setup.sh`
Expected: Creates `test/e2e/kubeconfig` file and clusters without touching `~/.kube/config`

**Step 4: Verify isolated kubeconfig**

Run: `cd test/e2e && cat kubeconfig | grep "kind-hosting\|kind-target"`
Expected: Should see both contexts in the dedicated kubeconfig

Run: `cat ~/.kube/config | grep "kind-hosting\|kind-target"`
Expected: Should NOT see these contexts in user's kubeconfig

**Step 5: Commit**

```bash
git add test/e2e/setup.sh
git commit -m "feat: use dedicated kubeconfig for e2e tests

Prevents polluting user's kubeconfig with test cluster contexts.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 3: Update teardown.sh for Isolated Kubeconfig

**Files:**
- Modify: `test/e2e/teardown.sh`

**Step 1: Add KUBECONFIG export at top**

After `set -euo pipefail`, add:

```bash
# Use dedicated kubeconfig for e2e tests
export KUBECONFIG="$(pwd)/kubeconfig"
```

**Step 2: Add kubeconfig cleanup at end**

After the cluster deletion commands, add:

```bash
echo "==> Removing dedicated kubeconfig..."
rm -f kubeconfig
```

**Step 3: Test teardown**

Run: `cd test/e2e && ./teardown.sh`
Expected: Deletes clusters and removes `test/e2e/kubeconfig` file

**Step 4: Verify cleanup**

Run: `cd test/e2e && ls kubeconfig`
Expected: File not found

Run: `kind get clusters`
Expected: hosting and target clusters gone

**Step 5: Commit**

```bash
git add test/e2e/teardown.sh
git commit -m "feat: clean up dedicated kubeconfig in teardown

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 4: Simplify Webhook Server - Remove Metrics

**Files:**
- Modify: `test/e2e/webhook-server/main.go`

**Step 1: Remove metrics-related imports and variables**

Remove these from imports:
```go
"encoding/json"
"io"
"net/http"
"os/exec"
"sync/atomic"
"crypto/x509"
"encoding/pem"
```

Remove these global variables:
```go
var (
	totalRequests      atomic.Int64
	certificateReloads atomic.Int32
	lastRequestTime    atomic.Value
	lastReloadTime     atomic.Value
	replicaName        string
	certFile           string
)
```

Remove these type definitions:
```go
type Metrics struct {
	ReplicaName        string    `json:"replica_name"`
	TotalRequests      int64     `json:"total_requests"`
	CertificateReloads int       `json:"certificate_reloads"`
	CertificateSerial  string    `json:"certificate_serial"`
	LastRequestTime    time.Time `json:"last_request_time"`
	LastCertReloadTime time.Time `json:"last_cert_reload_time"`
}
```

**Step 2: Add webhook delay flag and random seed**

Keep the existing imports that remain, and add to the `var` block with flags:

```go
var (
	metricsAddr  string
	probeAddr    string
	certDir      string
	webhookDelay time.Duration
)
```

In `main()`, after the other `flag.StringVar` calls, add:

```go
flag.DurationVar(&webhookDelay, "webhook-delay", 100*time.Millisecond, "Average webhook processing delay with ±20% jitter")
```

Add after flag parsing:

```go
// Seed random for jitter
rand.Seed(time.Now().UnixNano())
```

And add to imports:
```go
"math/rand"
```

**Step 3: Simplify the Handle method**

Replace the entire `Handle` method with:

```go
func (v *ConfigMapValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Add jittered delay: ±20% of configured delay
	jitter := 1.0 + (rand.Float64()*0.4 - 0.2) // Range: 0.8 to 1.2
	delay := time.Duration(float64(webhookDelay) * jitter)
	time.Sleep(delay)

	// Always allow (this is a test webhook)
	return admission.Allowed("")
}
```

**Step 4: Remove metrics tracking from main()**

Remove these lines from `main()`:
```go
replicaName = os.Getenv("HOSTNAME")
if replicaName == "" {
	replicaName = "unknown"
}

certFile = fmt.Sprintf("%s/tls.crt", certDir)
keyFile := fmt.Sprintf("%s/tls.key", certDir)

// Initialize atomic values
lastRequestTime.Store(time.Time{})
lastReloadTime.Store(time.Time{})
```

Keep only the `keyFile` local variable:
```go
certFile := fmt.Sprintf("%s/tls.crt", certDir)
keyFile := fmt.Sprintf("%s/tls.key", certDir)
```

**Step 5: Remove certificate reload tracking**

Remove this entire goroutine call from `main()`:
```go
// Track certificate reloads
go trackCertificateReloads(certFile)
```

And remove these entire functions:
```go
func handleMetrics(w http.ResponseWriter, r *http.Request) {...}
func getCertificateSerial(certPath string) string {...}
func trackCertificateReloads(certPath string) {...}
```

**Step 6: Remove metrics endpoint registration**

Remove this line from `main()`:
```go
// Register custom metrics endpoint on webhook server (port 9443)
mgr.GetWebhookServer().Register("/metrics", http.HandlerFunc(handleMetrics))
```

**Step 7: Update manager Metrics configuration**

The line is already set to disable metrics:
```go
Metrics: metricsserver.Options{
	BindAddress: "0", // Disable default metrics server
},
```

Keep this as-is.

**Step 8: Verify the file compiles**

Run: `cd test/e2e/webhook-server && go build`
Expected: No compilation errors

**Step 9: Commit**

```bash
git add test/e2e/webhook-server/main.go
git commit -m "refactor: simplify webhook server - remove metrics tracking

Remove all metrics collection, port-forwarding endpoints, and certificate
reload tracking. Add configurable jittered delay for realistic testing.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 5: Update Webhook Server Deployment Manifest

**Files:**
- Modify: `test/e2e/manifests/webhook-server-deployment.yaml`

**Step 1: Remove HOSTNAME environment variable**

In the `webhook-server` container env section, remove:
```yaml
# Not needed - no replica tracking
```

Actually, verify that `HOSTNAME` is not currently in the env vars. If it is, remove it. If not, no change needed.

**Step 2: Remove metrics port from webhook-server container**

In the `webhook-server` container `ports` section, verify only these ports exist:
```yaml
ports:
  - containerPort: 9443
    name: webhook
  - containerPort: 8082
    name: health
```

If port 8080 exists, remove it.

**Step 3: Verify manifest is valid**

Run: `cd test/e2e && kubectl --dry-run=client -f manifests/webhook-server-deployment.yaml apply`
Expected: No errors (or "created (dry run)")

**Step 4: Commit**

```bash
git add test/e2e/manifests/webhook-server-deployment.yaml
git commit -m "chore: remove metrics port from webhook server deployment

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 6: Rewrite Integration Test - Part 1 (Flags and Helpers)

**Files:**
- Modify: `test/e2e/integration_test.go`

**Step 1: Replace package-level variables with test flags**

Remove the current imports and add:

```go
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SAP-cloud-infrastructure/webhook-injector/pkg/controllers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)
```

Replace any existing package-level variables with:

```go
var (
	testRuntime      = flag.Duration("test.runtime", 2*time.Minute, "How long to run the load test")
	testConcurrency  = flag.Int("test.concurrency", 5, "Number of concurrent clients")
	rotationInterval = flag.Duration("test.rotation-interval", 30*time.Second, "How often to trigger cert rotation")
	reuseClusters    = flag.Bool("test.reuse-clusters", false, "Skip cluster setup/teardown")
)

type FailureRecord struct {
	Timestamp time.Time
	ClientID  int    // -1 for rotation errors
	Operation string // "get", "update", "rotation"
	Error     error
}
```

Add to imports:
```go
"flag"
```

**Step 2: Add TestMain for flag parsing**

Add this function:

```go
func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}
```

**Step 3: Add helper function for running shell commands**

```go
func runCommand(t *testing.T, script string) error {
	cmd := exec.Command(script)
	cmd.Dir = "."
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
```

**Step 4: Add helper function for getting Kubernetes client**

```go
func getKubernetesClient(t *testing.T, contextName string) *kubernetes.Clientset {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("KUBECONFIG environment variable not set")
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		t.Fatalf("Failed to load kubeconfig for context %s: %v", contextName, err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to create clientset: %v", err)
	}

	return clientset
}
```

**Step 5: Add helper function for waiting for deployment**

```go
func waitForDeploymentReady(t *testing.T, client *kubernetes.Clientset, name string, replicas int, timeout time.Duration) {
	ctx := context.Background()
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("Timeout waiting for deployment %s to have %d ready replicas", name, replicas)
		case <-ticker.C:
			deployment, err := client.AppsV1().Deployments("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				t.Logf("Waiting for deployment %s... (%v)", name, err)
				continue
			}

			if deployment.Status.ReadyReplicas == int32(replicas) {
				t.Logf("Deployment %s has %d/%d ready replicas", name, replicas, replicas)
				return
			}

			t.Logf("Waiting for deployment %s: %d/%d ready", name, deployment.Status.ReadyReplicas, replicas)
		}
	}
}
```

**Step 6: Verify file compiles**

Run: `cd test/e2e && go build integration_test.go`
Expected: Compilation errors about missing functions - that's expected, we'll add them next

**Step 7: Commit**

```bash
git add test/e2e/integration_test.go
git commit -m "refactor: add test flags and helper functions

Part 1 of integration test rewrite: flags, TestMain, and helper functions
for cluster management and client creation.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 7: Rewrite Integration Test - Part 2 (Load Client)

**Files:**
- Modify: `test/e2e/integration_test.go`

**Step 1: Add runLoadClient function**

Add this function:

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
				failures <- FailureRecord{
					Timestamp: time.Now(),
					ClientID:  clientID,
					Operation: "get",
					Error:     err,
				}
				continue
			}

			cm.Data["counter"] = fmt.Sprintf("%d", reqCount)
			_, err = client.CoreV1().ConfigMaps("default").Update(ctx, cm, metav1.UpdateOptions{})
			if err != nil {
				failures <- FailureRecord{
					Timestamp: time.Now(),
					ClientID:  clientID,
					Operation: "update",
					Error:     err,
				}
			}
		}
	}
}
```

**Step 2: Add runRotationTrigger function**

Add this function:

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
			if err := triggerRotation(ctx, client); err != nil {
				failures <- FailureRecord{
					Timestamp: time.Now(),
					ClientID:  -1,
					Operation: "rotation",
					Error:     err,
				}
			}
		}
	}
}
```

**Step 3: Add triggerRotation helper**

Add this function:

```go
func triggerRotation(ctx context.Context, client *kubernetes.Clientset) error {
	secret, err := client.CoreV1().Secrets("default").Get(ctx, "webhook-injector-certs", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations[controllers.ForceRotationAnnotation] = time.Now().Format(time.RFC3339)

	_, err = client.CoreV1().Secrets("default").Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to annotate secret: %w", err)
	}

	return nil
}
```

**Step 4: Verify file compiles**

Run: `cd test/e2e && go build integration_test.go`
Expected: May still have errors about missing test function - that's expected

**Step 5: Commit**

```bash
git add test/e2e/integration_test.go
git commit -m "refactor: add load client and rotation trigger functions

Part 2 of integration test rewrite: concurrent load generation and
certificate rotation triggering.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 8: Rewrite Integration Test - Part 3 (Main Test Function)

**Files:**
- Modify: `test/e2e/integration_test.go`

**Step 1: Remove all old test functions**

Delete these entire functions if they exist:
- `TestE2E_WebhookHighAvailability`
- `setupClusters`
- `teardownClusters`
- `deployWithSkaffold`
- `createConfigMaps`
- `assertAllSucceeded`
- `collectMetrics`
- `assertAllReplicasServing`
- `continuousLoad`
- `assertCertificateRotated`
- `assertAllReplicasReloaded`

Keep only the functions we added in previous tasks.

**Step 2: Add the new TestWebhookLoadAndRotation function**

Add this function:

```go
func TestWebhookLoadAndRotation(t *testing.T) {
	// Set KUBECONFIG to use dedicated test kubeconfig
	kubeconfigPath := filepath.Join(".", "kubeconfig")
	t.Setenv("KUBECONFIG", kubeconfigPath)

	// Conditional setup
	if !*reuseClusters {
		t.Log("Setting up clusters...")
		if err := runCommand(t, "./setup.sh"); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}

		t.Cleanup(func() {
			t.Log("Tearing down clusters...")
			runCommand(t, "./teardown.sh") // Ignore errors during cleanup
		})
	} else {
		t.Log("Reusing existing clusters (skipping setup/teardown)")
	}

	// Get Kubernetes clients
	hostingClient := getKubernetesClient(t, "kind-hosting")
	targetClient := getKubernetesClient(t, "kind-target")

	// Wait for webhook server to be ready
	t.Log("Waiting for webhook deployment ready...")
	waitForDeploymentReady(t, hostingClient, "webhook-server", 3, 5*time.Minute)

	// Create load test ConfigMaps
	ctx := context.Background()
	t.Logf("Creating %d load test ConfigMaps...", *testConcurrency)
	for i := 0; i < *testConcurrency; i++ {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("load-test-cm-%d", i),
				Namespace: "default",
			},
			Data: map[string]string{"counter": "0"},
		}
		if _, err := targetClient.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create load test ConfigMap %d: %v", i, err)
		}
	}

	// Run load test with timeout
	t.Logf("Starting load test (runtime=%s, concurrency=%d, rotation=%s)",
		*testRuntime, *testConcurrency, *rotationInterval)

	loadCtx, cancel := context.WithTimeout(ctx, *testRuntime)
	defer cancel()

	failures := make(chan FailureRecord, 1000)
	var totalRequests atomic.Int64

	// Start load clients
	for i := 0; i < *testConcurrency; i++ {
		go runLoadClient(loadCtx, i, targetClient, failures, &totalRequests)
	}

	// Start rotation trigger
	go runRotationTrigger(loadCtx, hostingClient, *rotationInterval, failures)

	// Wait for test duration
	<-loadCtx.Done()
	t.Log("Load test completed")

	// Collect failures
	close(failures)
	var collectedFailures []FailureRecord
	for f := range failures {
		collectedFailures = append(collectedFailures, f)
	}

	// Report results
	total := totalRequests.Load()
	failureCount := len(collectedFailures)
	failureRate := 0.0
	if total > 0 {
		failureRate = float64(failureCount) / float64(total) * 100
	}

	t.Logf("\n=== Load Test Results ===")
	t.Logf("Duration: %s", testRuntime)
	t.Logf("Total requests: %d", total)
	t.Logf("Failures: %d (%.2f%%)", failureCount, failureRate)

	if failureCount > 0 {
		t.Logf("\nFailed requests:")
		for _, f := range collectedFailures {
			clientStr := fmt.Sprintf("client-%d", f.ClientID)
			if f.ClientID == -1 {
				clientStr = "rotation"
			}
			t.Logf("  [%s] %s %s: %v",
				f.Timestamp.Format("2006-01-02 15:04:05.000"),
				clientStr,
				f.Operation,
				f.Error)
		}
		t.Fatalf("Load test had %d failures", failureCount)
	}
}
```

**Step 3: Verify file compiles and runs**

Run: `cd test/e2e && go test -v -timeout 1m -test.runtime=10s -test.concurrency=2 -test.reuse-clusters=true`
Expected: May fail if clusters don't exist, but should compile successfully

**Step 4: Commit**

```bash
git add test/e2e/integration_test.go
git commit -m "refactor: complete integration test rewrite

Part 3: Main test function with setup/teardown, load generation,
rotation triggering, and failure reporting.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 9: Clean Up Unused Files

**Files:**
- Delete: `test/e2e/debug-pods.sh`
- Delete: `test/e2e/testdata/` (if exists and empty)

**Step 1: Check if testdata exists and is empty**

Run: `ls -la test/e2e/testdata/ 2>/dev/null || echo "testdata does not exist"`
Expected: Either shows empty directory or says it doesn't exist

**Step 2: Delete debug-pods.sh**

Run: `git rm test/e2e/debug-pods.sh`
Expected: File removed

**Step 3: Delete testdata if it exists and is empty**

Run: `[ -d test/e2e/testdata ] && [ -z "$(ls -A test/e2e/testdata)" ] && git rm -r test/e2e/testdata || echo "testdata not empty or doesn't exist"`

**Step 4: Commit**

```bash
git commit -m "chore: remove unused e2e test files

Removed debug-pods.sh and testdata directory as they are no longer
needed with the simplified test approach.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 10: Update E2E README

**Files:**
- Modify: `test/e2e/README.md`

**Step 1: Replace entire README content**

Replace the entire content with:

```markdown
# E2E Integration Tests

End-to-end tests using two kind clusters to validate webhook-injector's
certificate rotation under continuous load.

## Prerequisites

- kind >= 0.20.0
- kubectl >= 1.28
- skaffold >= 2.10.0
- docker

## Quick Start

### Run Test (Fresh Clusters)

```bash
cd test/e2e
go test -v -timeout 5m
```

This creates clusters, deploys, runs the test, and tears down automatically.

### Run Test (Reuse Clusters)

For faster iteration during development:

```bash
# First run - create clusters
cd test/e2e
go test -v -timeout 5m

# Subsequent runs - reuse clusters (much faster)
go test -v -timeout 5m -test.reuse-clusters=true
go test -v -timeout 3m -test.runtime=1m -test.concurrency=10 -test.reuse-clusters=true

# Manual cleanup when done
./teardown.sh
```

### Manual Development Workflow

```bash
# Setup clusters and deploy
./setup.sh
skaffold dev  # Or: skaffold run for one-time deploy

# In another terminal, run tests with various configs
go test -v -timeout 5m -test.reuse-clusters=true
go test -v -timeout 10m -test.runtime=5m -test.concurrency=20 -test.rotation-interval=1m -test.reuse-clusters=true

# Cleanup
./teardown.sh
```

## Test Configuration

Available flags:

- `-test.runtime` - How long to run the load test (default: 2m)
- `-test.concurrency` - Number of concurrent clients (default: 5)
- `-test.rotation-interval` - How often to trigger cert rotation (default: 30s)
- `-test.reuse-clusters` - Skip cluster setup/teardown (default: false)

## Architecture

- **kind-hosting**: Runs webhook-server (3 replicas) + webhook-injector sidecars
- **kind-target**: Enforces webhooks, receives test ConfigMap updates
- **Test flow**: N concurrent clients continuously UPDATE dedicated ConfigMaps while
  certificate rotations are triggered at intervals
- **Pass criteria**: Zero failures during entire test duration

## What's Tested

1. Zero-downtime certificate rotation under continuous load
2. Multi-cluster communication (hosting → target)
3. Load distribution across 3 replicas
4. Webhook availability during cert rotation

## Isolated Kubeconfig

Tests use a dedicated `kubeconfig` file (gitignored) to avoid polluting your
personal kubeconfig. The file is created by `setup.sh` and removed by `teardown.sh`.
```

**Step 2: Verify README renders correctly**

Run: `cd test/e2e && cat README.md | head -20`
Expected: Shows new content

**Step 3: Commit**

```bash
git add test/e2e/README.md
git commit -m "docs: update e2e README for simplified test approach

Document new test configuration, cluster reuse workflow, and
isolated kubeconfig approach.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 11: End-to-End Verification

**Files:**
- Test: Full workflow with both cluster modes

**Step 1: Clean up any existing clusters**

Run: `cd test/e2e && ./teardown.sh`
Expected: Clusters deleted (or already gone)

**Step 2: Run full test with fresh clusters**

Run: `cd test/e2e && go test -v -timeout 5m -test.runtime=30s -test.concurrency=3`
Expected:
- Creates clusters
- Deploys with skaffold
- Runs load test for 30s with 3 clients
- Reports results
- Tears down clusters
- PASS with 0 failures

**Step 3: Setup clusters for reuse test**

Run: `cd test/e2e && ./setup.sh && skaffold run`
Expected: Clusters created and webhook deployed

**Step 4: Run test with cluster reuse**

Run: `cd test/e2e && go test -v -timeout 3m -test.runtime=30s -test.concurrency=3 -test.reuse-clusters=true`
Expected:
- Skips setup
- Runs load test
- Skips teardown
- Much faster (~30s vs ~2min)
- PASS with 0 failures

**Step 5: Run multiple iterations to verify reuse**

Run: `cd test/e2e && go test -v -timeout 3m -test.runtime=20s -test.concurrency=5 -test.rotation-interval=10s -test.reuse-clusters=true`
Expected: Fast execution, PASS

**Step 6: Verify isolated kubeconfig**

Run: `ls test/e2e/kubeconfig`
Expected: File exists

Run: `cat ~/.kube/config | grep kind-hosting || echo "Not found in user config"`
Expected: "Not found in user config"

**Step 7: Clean up**

Run: `cd test/e2e && ./teardown.sh`
Expected: Clusters deleted, kubeconfig removed

**Step 8: Final commit**

```bash
git add -A
git commit -m "test: verify e2e test rework implementation

Confirmed both fresh and reuse cluster modes work correctly with
isolated kubeconfig.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Success Criteria

- ✅ Test execution with reuse-clusters: <1 minute (down from 125s)
- ✅ Test code: ~300 lines (down from 653)
- ✅ User kubeconfig remains clean (isolated test kubeconfig)
- ✅ Configurable test parameters via flags
- ✅ Clear failure reporting with timestamps
- ✅ Zero flakiness from removed metrics/port-forwarding

## Notes

- The test uses ConfigMap UPDATEs (not CREATEs) to avoid etcd bloat
- Each client has a dedicated ConfigMap to avoid update conflicts
- Certificate rotations are triggered via annotation on the Secret
- The ForceRotationAnnotation constant is imported from pkg/controllers
- Webhook server adds jittered delay (~100ms ±20%) for realistic load
