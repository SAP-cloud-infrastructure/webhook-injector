<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# E2E Integration Tests Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace envtest-based integration tests with real kind clusters that validate zero-downtime certificate rotation in multi-cluster, multi-replica setup.

**Architecture:** Two kind clusters (hosting + target), Skaffold with ko builder, test webhook server with metrics, automated + manual testing support.

**Tech Stack:** Go 1.26, kind, kubectl, Skaffold, controller-runtime, Kubernetes 1.35

---

## Task 1: Update Config Package for Duration-Based Certificate Validity

**Files:**
- Modify: `pkg/config/config.go:35-36, 65-66, 226`
- Modify: `pkg/config/config_test.go` (add new test)
- Modify: `pkg/controllers/reconciler.go:226`

**Step 1: Write failing test for duration parsing**

Add to `pkg/config/config_test.go`:

```go
func TestLoadFromEnv_CertValidity_Duration(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected time.Duration
		wantErr  bool
	}{
		{
			name:     "seconds",
			envValue: "60s",
			expected: 60 * time.Second,
			wantErr:  false,
		},
		{
			name:     "minutes",
			envValue: "5m",
			expected: 5 * time.Minute,
			wantErr:  false,
		},
		{
			name:     "hours",
			envValue: "720h",
			expected: 720 * time.Hour,
			wantErr:  false,
		},
		{
			name:     "invalid duration",
			envValue: "invalid",
			expected: 0,
			wantErr:  true,
		},
		{
			name:     "default when empty",
			envValue: "",
			expected: 720 * time.Hour, // 30 days default
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv("CERT_VALIDITY", tt.envValue)
			}

			cfg, err := LoadFromEnv()
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if cfg.CertValidity != tt.expected {
				t.Errorf("Expected CertValidity %v, got %v", tt.expected, cfg.CertValidity)
			}
		})
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./pkg/config/ -run TestLoadFromEnv_CertValidity_Duration`
Expected: FAIL with "undefined: CertValidity"

**Step 3: Update Config struct**

In `pkg/config/config.go`, replace lines 35-36:

```go
// OLD:
// CertValidityDays      int

// NEW:
CertValidity time.Duration
```

**Step 4: Update LoadFromEnv function**

In `pkg/config/config.go`, modify LoadFromEnv function to return error and parse duration:

```go
// Update function signature (line 58)
func LoadFromEnv() (*Config, error) {
	// Parse certificate validity
	validityStr := getEnv("CERT_VALIDITY", "720h")
	validity, err := time.ParseDuration(validityStr)
	if err != nil {
		return nil, fmt.Errorf("invalid CERT_VALIDITY duration: %w", err)
	}

	return &Config{
		WebhookConfigName:       getEnv("WEBHOOK_CONFIG_NAME", ""),
		WebhookConfigNamespace:  getEnv("WEBHOOK_CONFIG_NAMESPACE", ""),
		CertSecretName:          getEnv("CERT_SECRET_NAME", "webhook-injector-certs"),
		CertSecretNamespace:     getEnv("CERT_SECRET_NAMESPACE", ""),
		CertDir:                 getEnv("CERT_DIR", "/tmp/webhook-certs"),
		CertValidity:            validity, // CHANGED
		CertRotationThreshold:   getEnvFloat("CERT_ROTATION_THRESHOLD", 0.8),
		Kubeconfig:              getEnv("KUBECONFIG", ""),
		EnableLeaderElection:    getEnvBool("ENABLE_LEADER_ELECTION", true),
		LeaderElectionID:        getEnv("LEADER_ELECTION_ID", "webhook-injector-leader"),
		LeaderElectionNamespace: getEnv("LEADER_ELECTION_NAMESPACE", ""),
		SyncPeriod:              getEnvDuration("SYNC_PERIOD", 5*time.Minute),
		MetricsAddr:             getEnv("METRICS_ADDR", ":8080"),
		HealthAddr:              getEnv("HEALTH_ADDR", ":8081"),
		LogLevel:                getEnv("LOG_LEVEL", "info"),
	}, nil
}
```

**Step 5: Update reconciler to use duration**

In `pkg/controllers/reconciler.go`, update line 226:

```go
// OLD:
// validity := time.Duration(r.Config.CertValidityDays) * 24 * time.Hour

// NEW:
validity := r.Config.CertValidity
```

**Step 6: Update main.go to handle config error**

In `cmd/webhook-injector/main.go`, update config loading:

```go
// Find the line with cfg := config.LoadFromEnv()
// Replace with:
cfg, err := config.LoadFromEnv()
if err != nil {
	setupLog.Error(err, "failed to load configuration")
	os.Exit(1)
}
```

**Step 7: Update all existing tests to handle new signature**

In `pkg/config/config_test.go`, update TestLoadFromEnv:

```go
func TestLoadFromEnv(t *testing.T) {
	t.Setenv("WEBHOOK_CONFIG_NAME", "test-config")
	t.Setenv("WEBHOOK_CONFIG_NAMESPACE", "test-ns")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv failed: %v", err)
	}

	if cfg.WebhookConfigName != "test-config" {
		t.Errorf("Expected WebhookConfigName 'test-config', got '%s'", cfg.WebhookConfigName)
	}
}
```

**Step 8: Run all config tests**

Run: `go test -v ./pkg/config/`
Expected: PASS

**Step 9: Run all tests to ensure nothing broke**

Run: `gmake test`
Expected: All tests PASS

**Step 10: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go pkg/controllers/reconciler.go cmd/webhook-injector/main.go
git commit -m "feat: replace CERT_VALIDITY_DAYS with CERT_VALIDITY duration

Replace integer days config with Go duration string for more flexible
certificate validity configuration. Supports 60s for testing, 720h for
production.

Breaking change: CERT_VALIDITY_DAYS env var replaced with CERT_VALIDITY.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 2: Add Annotation-Based Certificate Rotation Trigger

**Files:**
- Modify: `pkg/controllers/reconciler.go:119-142` (checkCertificates function)
- Add test: `pkg/controllers/reconciler_test.go`

**Step 1: Write failing test for annotation trigger**

Add to `pkg/controllers/reconciler_test.go`:

```go
func TestWebhookReconciler_CheckCertificates_ForceRotation_Annotation(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	cfg := &config.Config{
		CertSecretName:        "test-certs",
		CertSecretNamespace:   "default",
		CertValidity:          720 * time.Hour,
		CertRotationThreshold: 0.8,
	}

	// Generate a fresh certificate (should not need rotation based on time)
	validity := 720 * time.Hour
	caCert, caKey, err := certificates.GenerateCA(validity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	services := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, services, validity)
	if err != nil {
		t.Fatalf("Failed to generate server cert: %v", err)
	}

	// Create secret WITH force-rotation annotation
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				"webhook-injector.sap.com/force-rotation": "true",
			},
		},
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"ca.key":  caKey,
			"tls.crt": serverCert,
			"tls.key": serverKey,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client: fakeClient,
		Config: cfg,
		Scheme: s,
	}

	// Check certificates - should need rotation due to annotation
	_, needsRotation, err := reconciler.checkCertificates(context.Background(), services)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if !needsRotation {
		t.Error("Expected needsRotation=true when force-rotation annotation is present")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./pkg/controllers/ -run TestWebhookReconciler_CheckCertificates_ForceRotation_Annotation`
Expected: FAIL (annotation check not implemented)

**Step 3: Implement annotation check in checkCertificates**

In `pkg/controllers/reconciler.go`, modify checkCertificates function (around line 119):

```go
// checkCertificates checks if certificates exist and are valid
func (r *WebhookReconciler) checkCertificates(ctx context.Context, services []certificates.ServiceReference) (*corev1.Secret, bool, error) {
	secretKey := types.NamespacedName{
		Name:      r.Config.CertSecretName,
		Namespace: r.getNamespace(r.Config.CertSecretNamespace),
	}

	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, secretKey, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, nil // Needs generation
		}
		return nil, false, err
	}

	// Check for force-rotation annotation
	if secret.Annotations != nil {
		if forceRotation, exists := secret.Annotations["webhook-injector.sap.com/force-rotation"]; exists && forceRotation == "true" {
			return secret, true, nil // Force rotation
		}
	}

	// Check if certificate needs rotation based on time/SANs
	needsRotation, err := r.needsCertificateRotation(secret, services)
	if err != nil {
		return nil, false, fmt.Errorf("failed to check certificate rotation: %w", err)
	}

	return secret, needsRotation, nil
}
```

**Step 4: Run test to verify it passes**

Run: `go test -v ./pkg/controllers/ -run TestWebhookReconciler_CheckCertificates_ForceRotation_Annotation`
Expected: PASS

**Step 5: Implement annotation removal after rotation**

Modify `generateAndStoreCertificates` function in `pkg/controllers/reconciler.go` (around line 224):

```go
// generateAndStoreCertificates generates certificates and stores them in a Secret
func (r *WebhookReconciler) generateAndStoreCertificates(ctx context.Context, services []certificates.ServiceReference) error {
	validity := r.Config.CertValidity

	// Generate CA
	caCert, caKey, err := certificates.GenerateCA(validity)
	if err != nil {
		return fmt.Errorf("failed to generate CA: %w", err)
	}

	// Generate server certificate
	serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, services, validity)
	if err != nil {
		return fmt.Errorf("failed to generate server certificate: %w", err)
	}

	// Create or update Secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.Config.CertSecretName,
			Namespace: r.getNamespace(r.Config.CertSecretNamespace),
			// Remove force-rotation annotation if present
			Annotations: map[string]string{},
		},
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"tls.crt": serverCert,
			"tls.key": serverKey,
		},
	}

	// Rest of function remains the same...
```

**Step 6: Write test for annotation removal**

Add to `pkg/controllers/reconciler_test.go`:

```go
func TestWebhookReconciler_RemovesAnnotationAfterRotation(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Create secret with annotation
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				"webhook-injector.sap.com/force-rotation": "true",
			},
		},
		Data: map[string][]byte{
			"ca.crt":  []byte("old-ca"),
			"tls.crt": []byte("old-cert"),
			"tls.key": []byte("old-key"),
		},
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"webhooks.yaml": `---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: test-webhook
webhooks:
  - name: test.example.com
    clientConfig:
      service:
        name: webhook-svc
        namespace: default
        path: /mutate
    admissionReviewVersions: ["v1"]
    sideEffects: None`,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret, configMap).
		Build()

	cfg := &config.Config{
		WebhookConfigName:      "webhook-config",
		WebhookConfigNamespace: "default",
		CertSecretName:         "test-certs",
		CertSecretNamespace:    "default",
		CertValidity:           720 * time.Hour,
		CertDir:                t.TempDir(),
	}

	reconciler := &WebhookReconciler{
		Client:       fakeClient,
		TargetClient: fakeClient,
		Config:       cfg,
		Scheme:       s,
	}

	// Reconcile - should trigger rotation due to annotation
	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config",
			Namespace: "default",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify annotation was removed
	updatedSecret := &corev1.Secret{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-certs",
		Namespace: "default",
	}, updatedSecret)
	if err != nil {
		t.Fatalf("Failed to get updated secret: %v", err)
	}

	if updatedSecret.Annotations != nil {
		if _, exists := updatedSecret.Annotations["webhook-injector.sap.com/force-rotation"]; exists {
			t.Error("Expected force-rotation annotation to be removed after rotation")
		}
	}
}
```

**Step 7: Run test**

Run: `go test -v ./pkg/controllers/ -run TestWebhookReconciler_RemovesAnnotationAfterRotation`
Expected: PASS

**Step 8: Run all tests**

Run: `gmake test`
Expected: All tests PASS

**Step 9: Commit**

```bash
git add pkg/controllers/reconciler.go pkg/controllers/reconciler_test.go
git commit -m "feat: add annotation-based certificate rotation trigger

Add support for forcing certificate rotation by annotating the
certificate Secret with webhook-injector.sap.com/force-rotation=true.
Annotation is automatically removed after rotation completes.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 3: Create E2E Test Directory Structure

**Files:**
- Create: `test/e2e/.gitignore`
- Create: `test/e2e/go.mod`
- Create: `test/e2e/README.md`

**Step 1: Create e2e directory**

Run: `mkdir -p test/e2e/manifests test/e2e/webhook-server test/e2e/testdata`

**Step 2: Create .gitignore**

Create `test/e2e/.gitignore`:

```
# Ignore kind cluster kubeconfig files
*.kubeconfig

# Ignore test artifacts
*.log
```

**Step 3: Create go.mod**

Create `test/e2e/go.mod`:

```go
module github.com/sap-cloud-infrastructure/webhook-injector/test/e2e

go 1.26

require (
	k8s.io/api v0.35.1
	k8s.io/apimachinery v0.35.1
	k8s.io/client-go v0.35.1
	sigs.k8s.io/controller-runtime v0.23.1
)
```

**Step 4: Initialize go.mod**

Run: `cd test/e2e && go mod tidy`
Expected: Downloads dependencies, creates go.sum

**Step 5: Create README**

Create `test/e2e/README.md`:

```markdown
# E2E Integration Tests

End-to-end tests using two kind clusters to validate webhook-injector's multi-cluster,
multi-replica behavior with real certificate rotation.

## Prerequisites

- kind >= 0.20.0
- kubectl >= 1.28
- skaffold >= 2.10.0
- docker

## Quick Start

### Automated Testing

```bash
# From repository root
gmake test-e2e
```

### Manual Development

```bash
# From repository root
gmake e2e-setup      # Create clusters
gmake e2e-dev        # Start Skaffold in dev mode

# In another terminal, test webhooks
kubectl --context kind-target create configmap test-1 --from-literal=key=value

# Watch webhook logs
kubectl --context kind-hosting logs -l app=webhook-server -c webhook-server -f

# Cleanup
gmake e2e-teardown
```

## Architecture

- **kind-hosting**: Runs webhook-server (3 replicas) + webhook-injector sidecars
- **kind-target**: Enforces webhooks, receives test ConfigMaps
- **Test webhook**: Validates ConfigMap CREATE operations, always allows

## What's Tested

1. Multi-cluster communication (hosting → target)
2. Load balancing across 3 replicas
3. Zero-downtime during certificate rotation
4. All replicas independently reload certificates
```

**Step 6: Commit**

```bash
git add test/e2e/
git commit -m "chore: create E2E test directory structure

Initialize test/e2e with go.mod, README, and directory structure
for kind-based integration tests.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 4: Create Kind Cluster Configurations

**Files:**
- Create: `test/e2e/kind-hosting.yaml`
- Create: `test/e2e/kind-target.yaml`

**Step 1: Create hosting cluster config**

Create `test/e2e/kind-hosting.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: hosting
nodes:
  - role: control-plane
    extraPortMappings:
      # Map NodePort for webhook service so target cluster can reach it
      - containerPort: 30443
        hostPort: 30443
        protocol: TCP
  - role: worker
    # Additional worker for pod anti-affinity testing
```

**Step 2: Create target cluster config**

Create `test/e2e/kind-target.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: target
nodes:
  - role: control-plane
```

**Step 3: Commit**

```bash
git add test/e2e/kind-hosting.yaml test/e2e/kind-target.yaml
git commit -m "feat: add kind cluster configurations for E2E tests

Add hosting cluster with extraPortMappings for webhook service and
target cluster config for multi-cluster testing.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 5: Create Setup Script

**Files:**
- Create: `test/e2e/setup.sh`

**Step 1: Create setup.sh**

Create `test/e2e/setup.sh`:

```bash
#!/usr/bin/env bash

# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

echo "==> Checking prerequisites..."
for cmd in kind kubectl skaffold docker; do
    if ! command -v "$cmd" &> /dev/null; then
        echo "Error: $cmd is not installed"
        exit 1
    fi
done

echo "==> Creating hosting cluster..."
if kind get clusters | grep -q "^hosting$"; then
    echo "Hosting cluster already exists, skipping creation"
else
    kind create cluster --config kind-hosting.yaml
fi

echo "==> Creating target cluster..."
if kind get clusters | grep -q "^target$"; then
    echo "Target cluster already exists, skipping creation"
else
    kind create cluster --config kind-target.yaml
fi

echo "==> Waiting for clusters to be ready..."
kubectl --context kind-hosting wait --for=condition=Ready nodes --all --timeout=120s
kubectl --context kind-target wait --for=condition=Ready nodes --all --timeout=120s

echo "==> Creating default namespace in hosting cluster..."
kubectl --context kind-hosting create namespace default --dry-run=client -o yaml | \
    kubectl --context kind-hosting apply -f -

echo "==> Exporting target cluster kubeconfig as Secret..."
# Get target cluster kubeconfig and discover reachable address
TARGET_KUBECONFIG=$(mktemp)
kind get kubeconfig --name target > "$TARGET_KUBECONFIG"

# Replace internal server address with address reachable from hosting cluster
# In Docker Desktop, kind clusters can reach each other via container names
# Get target cluster's API server address
TARGET_API_SERVER="https://target-control-plane:6443"

# Update kubeconfig to use container-to-container address
kubectl --kubeconfig="$TARGET_KUBECONFIG" config set-cluster kind-target \
    --server="$TARGET_API_SERVER"

# Create Secret in hosting cluster with target kubeconfig
kubectl --context kind-hosting create secret generic target-kubeconfig \
    --from-file=kubeconfig="$TARGET_KUBECONFIG" \
    --dry-run=client -o yaml | \
    kubectl --context kind-hosting apply -f -

rm "$TARGET_KUBECONFIG"

echo "==> Discovering webhook endpoint address..."
# Get hosting control plane IP address for webhook URL
# The target cluster needs to reach the webhook service in hosting cluster
# Use host.docker.internal or actual Docker bridge IP
WEBHOOK_HOST="host.docker.internal"

# For Linux, host.docker.internal doesn't work, use docker bridge IP
if [[ "$OSTYPE" == "linux-gnu"* ]]; then
    WEBHOOK_HOST=$(docker network inspect kind | jq -r '.[0].IPAM.Config[0].Gateway')
fi

export WEBHOOK_ENDPOINT="https://${WEBHOOK_HOST}:30443"
echo "Webhook endpoint: $WEBHOOK_ENDPOINT"

# Write webhook endpoint to file for skaffold to use
echo "$WEBHOOK_ENDPOINT" > webhook-endpoint.txt

echo ""
echo "==> Setup complete!"
echo ""
echo "Clusters:"
echo "  - kind-hosting (context: kind-hosting)"
echo "  - kind-target (context: kind-target)"
echo ""
echo "Next steps:"
echo "  1. Deploy with: skaffold dev"
echo "  2. Test with: kubectl --context kind-target create configmap test-1 --from-literal=key=value"
echo "  3. Cleanup with: ./teardown.sh"
```

**Step 2: Make script executable**

Run: `chmod +x test/e2e/setup.sh`

**Step 3: Test script (manual verification)**

Run: `cd test/e2e && ./setup.sh`
Expected: Creates both kind clusters, outputs "Setup complete!"

**Step 4: Verify clusters exist**

Run: `kind get clusters`
Expected: Shows "hosting" and "target"

**Step 5: Commit**

```bash
git add test/e2e/setup.sh
git commit -m "feat: add E2E cluster setup script

Create setup.sh to provision kind-hosting and kind-target clusters,
export target kubeconfig as Secret, and discover webhook endpoint
address for cross-cluster communication.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 6: Create Teardown Script

**Files:**
- Create: `test/e2e/teardown.sh`

**Step 1: Create teardown.sh**

Create `test/e2e/teardown.sh`:

```bash
#!/usr/bin/env bash

# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

echo "==> Deleting kind clusters..."

if kind get clusters | grep -q "^hosting$"; then
    echo "Deleting hosting cluster..."
    kind delete cluster --name hosting
fi

if kind get clusters | grep -q "^target$"; then
    echo "Deleting target cluster..."
    kind delete cluster --name target
fi

# Clean up temporary files
rm -f webhook-endpoint.txt

echo "==> Teardown complete!"
```

**Step 2: Make script executable**

Run: `chmod +x test/e2e/teardown.sh`

**Step 3: Test script**

Run: `cd test/e2e && ./teardown.sh`
Expected: Deletes both clusters, outputs "Teardown complete!"

**Step 4: Verify clusters deleted**

Run: `kind get clusters`
Expected: Empty output or no "hosting"/"target"

**Step 5: Commit**

```bash
git add test/e2e/teardown.sh
git commit -m "feat: add E2E cluster teardown script

Create teardown.sh to clean up kind clusters and temporary files.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 7: Create Test Webhook Server

**Files:**
- Create: `test/e2e/webhook-server/main.go`

**Step 1: Create webhook server implementation**

Create `test/e2e/webhook-server/main.go`:

```go
// Copyright 2026 SAP SE or an SAP affiliate company
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

var (
	totalRequests      atomic.Int64
	certificateReloads atomic.Int32
	lastRequestTime    atomic.Value
	lastReloadTime     atomic.Value
	replicaName        string
)

type Metrics struct {
	ReplicaName        string    `json:"replica_name"`
	TotalRequests      int64     `json:"total_requests"`
	CertificateReloads int       `json:"certificate_reloads"`
	CertificateSerial  string    `json:"certificate_serial"`
	LastRequestTime    time.Time `json:"last_request_time"`
	LastCertReloadTime time.Time `json:"last_cert_reload_time"`
}

func main() {
	replicaName = os.Getenv("HOSTNAME")
	if replicaName == "" {
		replicaName = "unknown"
	}

	certDir := "/tmp/webhook-certs"
	certFile := fmt.Sprintf("%s/tls.crt", certDir)
	keyFile := fmt.Sprintf("%s/tls.key", certDir)

	// Initialize atomic values
	lastRequestTime.Store(time.Time{})
	lastReloadTime.Store(time.Time{})

	// Create certificate watcher for hot reload
	watcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to create certificate watcher: %v", err)
	}

	// Start certificate watcher
	go func() {
		if err := watcher.Start(context.Background()); err != nil {
			log.Fatalf("Certificate watcher failed: %v", err)
		}
	}()

	// Track certificate reloads
	go trackCertificateReloads(certFile)

	// HTTP handlers
	http.HandleFunc("/validate", handleValidate)
	http.HandleFunc("/metrics", handleMetrics(certFile))
	http.HandleFunc("/healthz", handleHealth)
	http.HandleFunc("/readyz", handleReady)

	// HTTPS server with certificate watcher
	server := &http.Server{
		Addr: ":9443",
		TLSConfig: &tls.Config{
			GetCertificate: watcher.GetCertificate,
		},
	}

	log.Printf("Starting webhook server on :9443 (replica: %s)", replicaName)
	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleValidate(w http.ResponseWriter, r *http.Request) {
	totalRequests.Add(1)
	lastRequestTime.Store(time.Now())

	var admissionReview admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&admissionReview); err != nil {
		http.Error(w, fmt.Sprintf("failed to decode admission review: %v", err), http.StatusBadRequest)
		return
	}

	log.Printf("Webhook request from %s: %s %s/%s",
		replicaName,
		admissionReview.Request.Operation,
		admissionReview.Request.Namespace,
		admissionReview.Request.Name,
	)

	// Always allow (this is a test webhook)
	response := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     admissionReview.Request.UID,
			Allowed: true,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleMetrics(certFile string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lastReq := lastRequestTime.Load().(time.Time)
		lastReload := lastReloadTime.Load().(time.Time)

		metrics := Metrics{
			ReplicaName:        replicaName,
			TotalRequests:      totalRequests.Load(),
			CertificateReloads: int(certificateReloads.Load()),
			CertificateSerial:  getCertificateSerial(certFile),
			LastRequestTime:    lastReq,
			LastCertReloadTime: lastReload,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(metrics)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	// Ready if certificate exists
	if _, err := os.Stat("/tmp/webhook-certs/tls.crt"); err != nil {
		http.Error(w, "certificates not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ready"))
}

func getCertificateSerial(certFile string) string {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return "error-reading-cert"
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return "error-decoding-pem"
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "error-parsing-cert"
	}

	return cert.SerialNumber.String()
}

func trackCertificateReloads(certFile string) {
	var lastSerial string

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		currentSerial := getCertificateSerial(certFile)
		if currentSerial != lastSerial && lastSerial != "" {
			certificateReloads.Add(1)
			lastReloadTime.Store(time.Now())
			log.Printf("Certificate reloaded on %s: %s -> %s", replicaName, lastSerial, currentSerial)
		}
		lastSerial = currentSerial
	}
}
```

**Step 2: Add missing import**

Add to imports in `test/e2e/webhook-server/main.go`:

```go
import (
	"context" // ADD THIS
	// ... rest of imports
)
```

**Step 3: Initialize e2e go.mod dependencies**

Run: `cd test/e2e && go mod tidy`

**Step 4: Verify it compiles**

Run: `cd test/e2e/webhook-server && go build .`
Expected: Compiles successfully, creates `webhook-server` binary

**Step 5: Clean up binary**

Run: `cd test/e2e/webhook-server && rm webhook-server`

**Step 6: Commit**

```bash
git add test/e2e/webhook-server/main.go test/e2e/go.mod test/e2e/go.sum
git commit -m "feat: add test webhook server with metrics

Create simple validating webhook server that:
- Always allows requests (test webhook)
- Tracks metrics per replica (requests, reloads, serial)
- Uses controller-runtime certwatcher for hot reload
- Exposes /metrics, /healthz, /readyz endpoints

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 8: Create Kubernetes Manifests

**Files:**
- Create: `test/e2e/manifests/rbac.yaml`
- Create: `test/e2e/manifests/service.yaml`
- Create: `test/e2e/manifests/webhook-server-deployment.yaml`
- Create: `test/e2e/manifests/webhook-config.yaml`

**Step 1: Create RBAC**

Create `test/e2e/manifests/rbac.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: webhook-injector
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: webhook-injector
  namespace: default
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: webhook-injector
  namespace: default
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: webhook-injector
subjects:
  - kind: ServiceAccount
    name: webhook-injector
    namespace: default
```

**Step 2: Create Service**

Create `test/e2e/manifests/service.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

---
apiVersion: v1
kind: Service
metadata:
  name: webhook-service
  namespace: default
spec:
  type: NodePort
  selector:
    app: webhook-server
  ports:
    - port: 9443
      targetPort: 9443
      nodePort: 30443
      protocol: TCP
```

**Step 3: Create Deployment**

Create `test/e2e/manifests/webhook-server-deployment.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: webhook-server
  namespace: default
spec:
  replicas: 3
  selector:
    matchLabels:
      app: webhook-server
  template:
    metadata:
      labels:
        app: webhook-server
    spec:
      serviceAccountName: webhook-injector
      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                labelSelector:
                  matchLabels:
                    app: webhook-server
                topologyKey: kubernetes.io/hostname
      containers:
        - name: webhook-server
          image: test-webhook-server  # Replaced by ko
          ports:
            - containerPort: 9443
          livenessProbe:
            httpGet:
              path: /healthz
              port: 9443
              scheme: HTTPS
            initialDelaySeconds: 10
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: 9443
              scheme: HTTPS
            initialDelaySeconds: 5
            periodSeconds: 5
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
              readOnly: true

        - name: webhook-injector
          image: webhook-injector  # Replaced by ko
          env:
            - name: WEBHOOK_CONFIG_NAME
              value: webhook-config
            - name: WEBHOOK_CONFIG_NAMESPACE
              value: default
            - name: CERT_SECRET_NAMESPACE
              value: default
            - name: CERT_VALIDITY
              value: "60s"  # Short validity for testing
            - name: CERT_ROTATION_THRESHOLD
              value: "0.5"
            - name: KUBECONFIG
              value: /etc/target-kubeconfig/kubeconfig
            - name: ENABLE_LEADER_ELECTION
              value: "true"
            - name: LEADER_ELECTION_NAMESPACE
              value: default
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
            - name: target-kubeconfig
              mountPath: /etc/target-kubeconfig
              readOnly: true

      volumes:
        - name: webhook-certs
          emptyDir: {}
        - name: target-kubeconfig
          secret:
            secretName: target-kubeconfig
```

**Step 4: Create webhook ConfigMap (template)**

Create `test/e2e/manifests/webhook-config.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

---
apiVersion: v1
kind: ConfigMap
metadata:
  name: webhook-config
  namespace: default
data:
  webhooks.yaml: |
    apiVersion: admissionregistration.k8s.io/v1
    kind: ValidatingWebhookConfiguration
    metadata:
      name: test-configmap-validator
    webhooks:
      - name: configmap.validator.test
        clientConfig:
          # This URL must be accessible from target cluster
          # Will be templated by setup.sh with actual endpoint
          url: ${WEBHOOK_ENDPOINT}/validate
          caBundle: ""  # Injected by webhook-injector
        rules:
          - operations: ["CREATE"]
            apiGroups: [""]
            apiVersions: ["v1"]
            resources: ["configmaps"]
        failurePolicy: Fail
        sideEffects: None
        admissionReviewVersions: ["v1"]
        timeoutSeconds: 10
```

**Step 5: Commit**

```bash
git add test/e2e/manifests/
git commit -m "feat: add Kubernetes manifests for E2E tests

Add RBAC, Service (NodePort 30443), Deployment (3 replicas), and
webhook ConfigMap for E2E testing environment.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 9: Create Skaffold Configuration

**Files:**
- Create: `test/e2e/skaffold.yaml`

**Step 1: Create skaffold.yaml**

Create `test/e2e/skaffold.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

apiVersion: skaffold/v4beta11
kind: Config
metadata:
  name: webhook-injector-e2e

build:
  artifacts:
    - image: webhook-injector
      ko:
        main: ../../cmd/webhook-injector
        dependencies:
          paths:
            - ../../pkg
            - ../../cmd/webhook-injector
    - image: test-webhook-server
      ko:
        main: ./webhook-server
        dependencies:
          paths:
            - ./webhook-server
  local:
    push: false
    useDockerCLI: true

deploy:
  kubectl:
    hooks:
      before:
        - host:
            command: ["sh", "-c", "if [ -f webhook-endpoint.txt ]; then export WEBHOOK_ENDPOINT=$(cat webhook-endpoint.txt) && envsubst < manifests/webhook-config.yaml > manifests/webhook-config-rendered.yaml; fi"]
    manifests:
      - manifests/rbac.yaml
      - manifests/service.yaml
      - manifests/webhook-server-deployment.yaml
      - manifests/webhook-config-rendered.yaml

portForward:
  - resourceType: service
    resourceName: webhook-service
    namespace: default
    port: 9443
    localPort: 9443

profiles:
  - name: dev
    activation:
      - command: dev
    deploy:
      kubectl:
        defaultNamespace: default
```

**Step 2: Update setup.sh to use envsubst**

The manifests use `${WEBHOOK_ENDPOINT}` which needs to be substituted. Skaffold's pre-hook handles this using envsubst.

**Step 3: Test skaffold build (after clusters are up)**

This will be tested in integration test, skip manual verification for now.

**Step 4: Commit**

```bash
git add test/e2e/skaffold.yaml
git commit -m "feat: add Skaffold configuration for E2E tests

Configure Skaffold to build with ko and deploy to hosting cluster.
Uses envsubst to template webhook endpoint URL from setup.sh.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 10: Update Makefile.maker.yaml

**Files:**
- Modify: `Makefile.maker.yaml:58-63`

**Step 1: Write test that Makefile has correct targets**

This is verified by running make commands after changes.

**Step 2: Update Makefile.maker.yaml verbatim section**

Replace the `verbatim` section in `Makefile.maker.yaml`:

```yaml
verbatim: |
  check: test-e2e

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

**Step 3: Regenerate Makefile**

Run: `gmake -f Makefile.maker.yaml`
Expected: Regenerates Makefile with new targets

**Step 4: Verify new targets exist**

Run: `gmake -n e2e-setup`
Expected: Shows what would be executed (dry run)

**Step 5: Commit**

```bash
git add Makefile.maker.yaml Makefile
git commit -m "feat: add E2E test targets to Makefile

Replace test-integration with test-e2e and add e2e-setup, e2e-dev,
and e2e-teardown targets for manual development workflow.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 11: Remove Old Integration Tests

**Files:**
- Delete: `test/integration/` directory

**Step 1: Verify old tests are redundant**

Run: `ls -la test/integration/`
Expected: Shows integration_test.go and testdata/

**Step 2: Remove directory**

Run: `git rm -r test/integration/`

**Step 3: Commit**

```bash
git commit -m "refactor: remove envtest-based integration tests

Remove old integration tests in favor of new E2E tests with real
kind clusters. Old tests were redundant with unit tests and didn't
validate real integration scenarios.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 12: Implement E2E Integration Test

**Files:**
- Create: `test/e2e/integration_test.go`

**Step 1: Create test file**

Create `test/e2e/integration_test.go`:

```go
// Copyright 2026 SAP SE or an SAP affiliate company
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type Metrics struct {
	ReplicaName        string    `json:"replica_name"`
	TotalRequests      int64     `json:"total_requests"`
	CertificateReloads int       `json:"certificate_reloads"`
	CertificateSerial  string    `json:"certificate_serial"`
	LastRequestTime    time.Time `json:"last_request_time"`
	LastCertReloadTime time.Time `json:"last_cert_reload_time"`
}

func TestE2E_WebhookHighAvailability(t *testing.T) {
	// Phase 1: Setup
	t.Log("Phase 1: Setting up environment...")
	if err := setupClusters(t); err != nil {
		t.Fatalf("Failed to setup clusters: %v", err)
	}

	// Always teardown on exit
	defer func() {
		t.Log("Tearing down environment...")
		teardownClusters(t)
	}()

	if err := deployWithSkaffold(t); err != nil {
		t.Fatalf("Failed to deploy with Skaffold: %v", err)
	}

	hostingClient := getKubernetesClient(t, "kind-hosting")
	targetClient := getKubernetesClient(t, "kind-target")

	if err := waitForWebhookReady(t, hostingClient, 3); err != nil {
		t.Fatalf("Webhook server not ready: %v", err)
	}

	// Phase 2: Baseline validation
	t.Log("Phase 2: Baseline validation...")
	baselineResults := createConfigMaps(t, targetClient, "baseline", 30)
	assertAllSucceeded(t, baselineResults)

	baselineMetrics := collectMetrics(t, hostingClient)
	assertAllReplicasServing(t, baselineMetrics)

	// Phase 3: Rotation under load
	t.Log("Phase 3: Starting load test with rotation...")
	ctx, cancel := context.WithCancel(context.Background())
	failuresChan := make(chan error, 200)

	go continuousLoad(ctx, targetClient, failuresChan)

	time.Sleep(30 * time.Second) // Let load accumulate

	t.Log("Triggering certificate rotation...")
	if err := triggerRotation(t, hostingClient); err != nil {
		t.Fatalf("Failed to trigger rotation: %v", err)
	}

	time.Sleep(60 * time.Second) // Continue load during rotation

	cancel() // Stop load

	// Phase 4: Assert zero failures
	t.Log("Phase 4: Checking for failures...")
	close(failuresChan)
	var failures []error
	for err := range failuresChan {
		failures = append(failures, err)
	}

	if len(failures) > 0 {
		t.Errorf("Load test had %d failures during rotation:", len(failures))
		for i, err := range failures {
			if i < 5 { // Show first 5 failures
				t.Errorf("  - %v", err)
			}
		}
		if len(failures) > 5 {
			t.Errorf("  ... and %d more", len(failures)-5)
		}
	}

	// Phase 5: Post-rotation verification
	t.Log("Phase 5: Post-rotation verification...")
	postMetrics := collectMetrics(t, hostingClient)
	assertCertificateRotated(t, baselineMetrics, postMetrics)
	assertAllReplicasReloaded(t, postMetrics)

	// Final validation
	finalResults := createConfigMaps(t, targetClient, "final", 30)
	assertAllSucceeded(t, finalResults)

	t.Log("E2E test completed successfully!")
}

func setupClusters(t *testing.T) error {
	cmd := exec.Command("./setup.sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func teardownClusters(t *testing.T) {
	cmd := exec.Command("./teardown.sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run() // Ignore errors during cleanup
}

func deployWithSkaffold(t *testing.T) error {
	cmd := exec.Command("skaffold", "run")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func getKubernetesClient(t *testing.T, contextName string) *kubernetes.Clientset {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
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

func waitForWebhookReady(t *testing.T, client *kubernetes.Clientset, expectedReplicas int) error {
	ctx := context.Background()
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for %d replicas to be ready", expectedReplicas)
		case <-ticker.C:
			deployment, err := client.AppsV1().Deployments("default").Get(ctx, "webhook-server", metav1.GetOptions{})
			if err != nil {
				t.Logf("Waiting for deployment... (%v)", err)
				continue
			}

			if deployment.Status.ReadyReplicas == int32(expectedReplicas) {
				t.Logf("All %d replicas are ready", expectedReplicas)
				return nil
			}

			t.Logf("Waiting for replicas: %d/%d ready", deployment.Status.ReadyReplicas, expectedReplicas)
		}
	}
}

func createConfigMaps(t *testing.T, client *kubernetes.Clientset, prefix string, count int) []error {
	var results []error
	ctx := context.Background()

	for i := 0; i < count; i++ {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", prefix, i),
				Namespace: "default",
			},
			Data: map[string]string{
				"key": "value",
			},
		}

		_, err := client.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
		results = append(results, err)
	}

	return results
}

func assertAllSucceeded(t *testing.T, results []error) {
	for i, err := range results {
		if err != nil {
			t.Errorf("ConfigMap creation %d failed: %v", i, err)
		}
	}
}

func collectMetrics(t *testing.T, client *kubernetes.Clientset) map[string]Metrics {
	ctx := context.Background()
	pods, err := client.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=webhook-server",
	})
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	metricsMap := make(map[string]Metrics)
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		// Port-forward to pod
		localPort := 19443
		portForwardCmd := exec.Command("kubectl", "--context", "kind-hosting",
			"port-forward", pod.Name, fmt.Sprintf("%d:9443", localPort), "-n", "default")

		if err := portForwardCmd.Start(); err != nil {
			t.Logf("Failed to port-forward to %s: %v", pod.Name, err)
			continue
		}

		time.Sleep(2 * time.Second) // Wait for port-forward to establish

		// Fetch metrics
		resp, err := http.Get(fmt.Sprintf("https://localhost:%d/metrics", localPort))
		if err != nil {
			t.Logf("Failed to fetch metrics from %s: %v", pod.Name, err)
			portForwardCmd.Process.Kill()
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		portForwardCmd.Process.Kill()

		var metrics Metrics
		if err := json.Unmarshal(body, &metrics); err != nil {
			t.Logf("Failed to parse metrics from %s: %v", pod.Name, err)
			continue
		}

		metricsMap[metrics.ReplicaName] = metrics
		t.Logf("Metrics from %s: requests=%d, reloads=%d, serial=%s",
			metrics.ReplicaName, metrics.TotalRequests, metrics.CertificateReloads, metrics.CertificateSerial)
	}

	return metricsMap
}

func assertAllReplicasServing(t *testing.T, metrics map[string]Metrics) {
	if len(metrics) != 3 {
		t.Errorf("Expected metrics from 3 replicas, got %d", len(metrics))
	}

	for name, m := range metrics {
		if m.TotalRequests == 0 {
			t.Errorf("Replica %s served no requests", name)
		}
	}
}

func continuousLoad(ctx context.Context, client *kubernetes.Clientset, failures chan<- error) {
	i := 0
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("load-%d", i),
					Namespace: "default",
				},
				Data: map[string]string{
					"key": "value",
				},
			}

			_, err := client.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
			if err != nil {
				failures <- err
			}
			i++
		}
	}
}

func triggerRotation(t *testing.T, client *kubernetes.Clientset) error {
	ctx := context.Background()
	secret, err := client.CoreV1().Secrets("default").Get(ctx, "webhook-injector-certs", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations["webhook-injector.sap.com/force-rotation"] = "true"

	_, err = client.CoreV1().Secrets("default").Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

func assertCertificateRotated(t *testing.T, before, after map[string]Metrics) {
	for name, beforeMetrics := range before {
		afterMetrics, exists := after[name]
		if !exists {
			t.Errorf("Replica %s not found in post-rotation metrics", name)
			continue
		}

		if beforeMetrics.CertificateSerial == afterMetrics.CertificateSerial {
			t.Errorf("Replica %s certificate not rotated (serial unchanged: %s)",
				name, beforeMetrics.CertificateSerial)
		}
	}
}

func assertAllReplicasReloaded(t *testing.T, metrics map[string]Metrics) {
	for name, m := range metrics {
		if m.CertificateReloads == 0 {
			t.Errorf("Replica %s did not reload certificate", name)
		}
	}
}
```

**Step 2: Fix metrics collection (skip TLS verification)**

The metrics endpoint uses self-signed certs, need to skip verification. Update collectMetrics function:

```go
// In collectMetrics, update HTTP client creation:
httpClient := &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	},
	Timeout: 5 * time.Second,
}

resp, err := httpClient.Get(fmt.Sprintf("https://localhost:%d/metrics", localPort))
```

Add import: `"crypto/tls"`

**Step 3: Run go mod tidy**

Run: `cd test/e2e && go mod tidy`

**Step 4: Commit**

```bash
git add test/e2e/integration_test.go test/e2e/go.mod test/e2e/go.sum
git commit -m "feat: implement E2E integration test

Add comprehensive E2E test that validates:
- Multi-cluster communication (hosting → target)
- Load balancing across 3 replicas
- Zero-downtime during certificate rotation
- All replicas independently reload certificates

Test creates kind clusters, deploys via Skaffold, runs continuous
load during rotation, and verifies no failures occurred.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 13: Add Sample ConfigMap for Manual Testing

**Files:**
- Create: `test/e2e/testdata/sample-configmap.yaml`

**Step 1: Create sample ConfigMap**

Create `test/e2e/testdata/sample-configmap.yaml`:

```yaml
# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

apiVersion: v1
kind: ConfigMap
metadata:
  name: test-configmap
  namespace: default
data:
  message: "This ConfigMap creation will be intercepted by the webhook"
```

**Step 2: Commit**

```bash
git add test/e2e/testdata/sample-configmap.yaml
git commit -m "docs: add sample ConfigMap for manual E2E testing

Add example ConfigMap that users can apply to test webhook
interception during manual development with 'gmake e2e-dev'.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 14: Update Documentation

**Files:**
- Modify: `README.md` (add E2E testing section)
- Modify: `CLAUDE.md` (update testing commands)

**Step 1: Update README.md**

Add to README.md before the "Development" section:

```markdown
## Testing

### Unit Tests

```bash
gmake test
```

### E2E Integration Tests

End-to-end tests use two kind clusters to validate multi-cluster, multi-replica behavior:

```bash
# Automated E2E test
gmake test-e2e

# Manual development workflow
gmake e2e-setup      # Create clusters
gmake e2e-dev        # Skaffold dev mode (hot reload)

# Test webhook manually
kubectl --context kind-target create configmap test-1 --from-literal=key=value

# Watch logs
kubectl --context kind-hosting logs -l app=webhook-server -f

# Cleanup
gmake e2e-teardown
```

**Prerequisites:** kind, kubectl, skaffold, docker
```

**Step 2: Update CLAUDE.md**

In CLAUDE.md, update the testing section:

```markdown
# Integration tests
make test-e2e          # E2E tests with kind clusters (replaces test-integration)

# Manual E2E development
make e2e-setup         # Create kind clusters
make e2e-dev           # Skaffold dev mode
make e2e-teardown      # Cleanup clusters
```

**Step 3: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "docs: update testing documentation for E2E tests

Update README and CLAUDE.md with E2E testing instructions,
replacing old integration test references.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 15: Final Verification and Testing

**Files:**
- None (verification only)

**Step 1: Run all unit tests**

Run: `gmake test`
Expected: All unit tests PASS

**Step 2: Run static checks**

Run: `gmake static-check`
Expected: No lint errors

**Step 3: Test E2E setup manually**

Run: `gmake e2e-setup`
Expected: Creates both kind clusters successfully

**Step 4: Verify clusters exist**

Run: `kind get clusters`
Expected: Shows "hosting" and "target"

**Step 5: Test Skaffold build**

Run: `cd test/e2e && skaffold build`
Expected: Builds both images successfully

**Step 6: Teardown clusters**

Run: `gmake e2e-teardown`
Expected: Deletes both clusters

**Step 7: Run full E2E test (if time permits)**

Run: `gmake test-e2e`
Expected: Creates clusters, deploys, runs test, passes (may take 8-10 minutes)

**Step 8: Final commit message**

```bash
git add -A
git commit -m "chore: final E2E integration test implementation

Complete implementation of E2E testing framework with:
- Two kind clusters (hosting + target)
- Skaffold + ko for builds
- Test webhook server with metrics
- Automated HA testing during cert rotation
- Manual development workflow support

Replaces envtest-based integration tests with real Kubernetes
clusters that prove actual multi-cluster, multi-replica behavior.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Implementation Complete

All tasks completed. The E2E integration test framework is now fully implemented with:

✅ Config package updated for Go duration strings
✅ Annotation-based rotation trigger
✅ Complete E2E directory structure
✅ Kind cluster configurations with cross-cluster networking
✅ Setup and teardown scripts
✅ Test webhook server with metrics
✅ Kubernetes manifests (RBAC, Service, Deployment, ConfigMap)
✅ Skaffold configuration with ko builder
✅ Makefile targets for automated and manual testing
✅ Comprehensive integration test
✅ Documentation updates

**Next steps:**
1. Run `gmake test-e2e` to verify everything works
2. Fix any issues that arise during first run
3. Iterate on test timing/thresholds if needed
