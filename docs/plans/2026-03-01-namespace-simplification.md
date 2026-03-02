# Namespace Configuration Simplification Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace 3 namespace env vars with single NAMESPACE env var and move namespace auto-detection into config initialization.

**Architecture:** Simplify configuration by replacing WebhookConfigNamespace, CertSecretNamespace, and LeaderElectionNamespace with a single Namespace field. Move namespace resolution from main.go's getPodNamespace() into config.LoadFromEnv() so it happens once at startup. Resolution order: NAMESPACE env var → serviceaccount file → "default".

**Tech Stack:** Go 1.26, Kubernetes client-go, controller-runtime

---

## Task 1: Update Config struct and add namespace resolution

**Files:**
- Modify: `pkg/config/config.go:27-63` (Config struct and LoadFromEnv)
- Modify: `pkg/config/config.go:107-139` (Helper functions)
- Test: `pkg/config/config_test.go`

**Step 1: Write failing test for namespace resolution priority**

Add to `pkg/config/config_test.go`:

```go
func TestLoadFromEnv_NamespaceResolution(t *testing.T) {
	tests := []struct {
		name              string
		envNamespace      string
		setupServiceAccount bool
		expected          string
	}{
		{
			name:         "NAMESPACE env var takes precedence",
			envNamespace: "custom-namespace",
			setupServiceAccount: true,
			expected:     "custom-namespace",
		},
		{
			name:         "Falls back to serviceaccount file",
			envNamespace: "",
			setupServiceAccount: true,
			expected:     "sa-namespace",
		},
		{
			name:         "Defaults to 'default' if no env and no serviceaccount",
			envNamespace: "",
			setupServiceAccount: false,
			expected:     "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup
			if tt.envNamespace != "" {
				t.Setenv("NAMESPACE", tt.envNamespace)
			}
			t.Setenv("WEBHOOK_CONFIG_NAME", "test-config")

			var saPath string
			if tt.setupServiceAccount {
				// Create temp serviceaccount file
				tmpDir := t.TempDir()
				saPath = filepath.Join(tmpDir, "namespace")
				err := os.WriteFile(saPath, []byte("sa-namespace"), 0600)
				if err != nil {
					t.Fatalf("Failed to create test serviceaccount file: %v", err)
				}
			}

			// Test
			cfg, err := LoadFromEnvWithSAPath(saPath)
			if err != nil {
				t.Fatalf("LoadFromEnv failed: %v", err)
			}

			if cfg.Namespace != tt.expected {
				t.Errorf("Expected namespace %q, got %q", tt.expected, cfg.Namespace)
			}
		})
	}
}

// Add helper function for testing with custom serviceaccount path
func LoadFromEnvWithSAPath(saPath string) (*Config, error) {
	// This will be implemented to allow injecting custom serviceaccount path
	// For now, this test will fail
	return nil, errors.New("not implemented")
}
```

**Step 2: Run test to verify it fails**

Run: `go test -v ./pkg/config/... -run TestLoadFromEnv_NamespaceResolution`

Expected: FAIL - "LoadFromEnvWithSAPath not implemented"

**Step 3: Update Config struct to replace 3 namespace fields with 1**

In `pkg/config/config.go`, replace lines 28-36:

```go
// Config holds all configuration for webhook-injector
type Config struct {
	// Webhook ConfigMap settings
	WebhookConfigName string
	Namespace         string

	// Certificate Secret settings
	CertSecretName string
	CertDir        string

	// Certificate generation settings
	CertValidity          time.Duration
	CertRotationThreshold float64
```

**Step 4: Add namespace resolution to LoadFromEnv**

In `pkg/config/config.go`, add helper function before LoadFromEnv:

```go
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// detectNamespace resolves namespace from env var, serviceaccount file, or default
func detectNamespace() string {
	return detectNamespaceWithPath(serviceAccountNamespaceFile)
}

// detectNamespaceWithPath allows testing with custom serviceaccount path
func detectNamespaceWithPath(saPath string) string {
	// Try NAMESPACE env var first
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		return ns
	}

	// Try reading from serviceaccount file
	if data, err := os.ReadFile(saPath); err == nil {
		if ns := string(data); ns != "" {
			return ns
		}
	}

	// Default to "default"
	return "default"
}
```

**Step 5: Update LoadFromEnv to use single namespace field**

In `pkg/config/config.go`, update LoadFromEnv function (around lines 66-91):

```go
// LoadFromEnv loads configuration from environment variables with defaults
func LoadFromEnv() (*Config, error) {
	return loadFromEnvWithSAPath(serviceAccountNamespaceFile)
}

// loadFromEnvWithSAPath allows testing with custom serviceaccount path
func loadFromEnvWithSAPath(saPath string) (*Config, error) {
	// Parse certificate validity
	validityStr := getEnv("CERT_VALIDITY", "720h")
	validity, err := time.ParseDuration(validityStr)
	if err != nil {
		return nil, fmt.Errorf("invalid CERT_VALIDITY duration: %w", err)
	}

	return &Config{
		WebhookConfigName:       getEnv("WEBHOOK_CONFIG_NAME", ""),
		Namespace:               detectNamespaceWithPath(saPath),
		CertSecretName:          getEnv("CERT_SECRET_NAME", "webhook-injector-certs"),
		CertDir:                 getEnv("CERT_DIR", "/tmp/webhook-certs"),
		CertValidity:            validity,
		CertRotationThreshold:   getEnvFloat("CERT_ROTATION_THRESHOLD", 0.8),
		Kubeconfig:              getEnv("TARGET_KUBECONFIG", ""),
		EnableLeaderElection:    getEnvBool("ENABLE_LEADER_ELECTION", true),
		LeaderElectionID:        getEnv("LEADER_ELECTION_ID", "webhook-injector-leader"),
		LeaderElectionNamespace: detectNamespaceWithPath(saPath),
		SyncPeriod:              getEnvDuration("SYNC_PERIOD", 5*time.Minute),
		MetricsAddr:             getEnv("METRICS_ADDR", ":8080"),
		HealthAddr:              getEnv("HEALTH_ADDR", ":8081"),
		LogLevel:                getEnv("LOG_LEVEL", "info"),
	}, nil
}
```

**Step 6: Update test helper function**

In `pkg/config/config_test.go`, update LoadFromEnvWithSAPath:

```go
func LoadFromEnvWithSAPath(saPath string) (*Config, error) {
	return loadFromEnvWithSAPath(saPath)
}
```

**Step 7: Add missing import to test file**

At the top of `pkg/config/config_test.go`:

```go
import (
	"os"
	"path/filepath"
	"testing"
	"time"
)
```

**Step 8: Run test to verify it passes**

Run: `go test -v ./pkg/config/... -run TestLoadFromEnv_NamespaceResolution`

Expected: PASS - All 3 test cases should pass

**Step 9: Update existing config tests**

Find and update any existing tests in `pkg/config/config_test.go` that reference the old namespace fields. Search for `WebhookConfigNamespace`, `CertSecretNamespace`, `LeaderElectionNamespace` and replace with `Namespace`.

**Step 10: Run all config tests**

Run: `go test -v ./pkg/config/...`

Expected: All tests PASS

**Step 11: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go
git commit -m "refactor: consolidate namespace configuration into single field

Replace WebhookConfigNamespace, CertSecretNamespace, and LeaderElectionNamespace
with single Namespace field. Move namespace resolution into config.LoadFromEnv()
with priority: NAMESPACE env var → serviceaccount file → default.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 2: Update WebhookReconciler to use cfg.Namespace

**Files:**
- Modify: `pkg/controllers/reconciler.go` (multiple locations)
- Test: `pkg/controllers/reconciler_test.go`

**Step 1: Find all namespace field references in reconciler**

Run: `grep -n "WebhookConfigNamespace\|CertSecretNamespace" pkg/controllers/reconciler.go`

Expected: Shows all lines where old namespace fields are used

**Step 2: Update reconciler.go namespace references**

Replace all occurrences:
- `r.Config.WebhookConfigNamespace` → `r.Config.Namespace`
- `r.Config.CertSecretNamespace` → `r.Config.Namespace`

Common locations:
- `checkCertificates()` function - Secret lookup
- `generateAndStoreCertificates()` function - Secret creation/update
- `applyWebhookConfigurations()` function - logging
- `SetupWithManager()` function - watch configuration

**Step 3: Update reconciler test config creation**

In `pkg/controllers/reconciler_test.go`, find Config struct creation and update:

Replace:
```go
Config: &config.Config{
	WebhookConfigNamespace: "default",
	CertSecretNamespace:    "default",
	// ...
}
```

With:
```go
Config: &config.Config{
	Namespace: "default",
	// ...
}
```

**Step 4: Run reconciler tests**

Run: `go test -v ./pkg/controllers/... -run TestWebhookReconciler`

Expected: All reconciler tests PASS

**Step 5: Commit**

```bash
git add pkg/controllers/reconciler.go pkg/controllers/reconciler_test.go
git commit -m "refactor: use cfg.Namespace in WebhookReconciler

Replace references to cfg.WebhookConfigNamespace and cfg.CertSecretNamespace
with cfg.Namespace throughout reconciler.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 3: Update CertificateReloader to use cfg.Namespace

**Files:**
- Modify: `pkg/controllers/reloader.go`
- Test: `pkg/controllers/reloader_test.go`

**Step 1: Find all namespace field references in reloader**

Run: `grep -n "CertSecretNamespace" pkg/controllers/reloader.go`

Expected: Shows all lines where old namespace field is used

**Step 2: Update reloader.go namespace references**

Replace all occurrences of `r.Config.CertSecretNamespace` with `r.Config.Namespace`.

Common location: `SetupWithManager()` function - watch configuration

**Step 3: Update reloader test config creation**

In `pkg/controllers/reloader_test.go`, find Config struct creation and update:

Replace:
```go
Config: &config.Config{
	CertSecretNamespace: "default",
	CertDir:            tmpDir,
}
```

With:
```go
Config: &config.Config{
	Namespace: "default",
	CertDir:   tmpDir,
}
```

**Step 4: Run reloader tests**

Run: `go test -v ./pkg/controllers/... -run TestCertificateReloader`

Expected: All reloader tests PASS

**Step 5: Commit**

```bash
git add pkg/controllers/reloader.go pkg/controllers/reloader_test.go
git commit -m "refactor: use cfg.Namespace in CertificateReloader

Replace references to cfg.CertSecretNamespace with cfg.Namespace
throughout reloader.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 4: Update main.go to use cfg.Namespace and remove getPodNamespace

**Files:**
- Modify: `cmd/webhook-injector/main.go:76-80` (logging)
- Modify: `cmd/webhook-injector/main.go:92-101` (cache config)
- Modify: `cmd/webhook-injector/main.go:215-231` (remove getPodNamespace function)

**Step 1: Update startup logging**

In `cmd/webhook-injector/main.go`, around line 76, replace:

```go
setupLog.Info("Starting webhook-injector",
	"config-name", cfg.WebhookConfigNamespace+"/"+cfg.WebhookConfigName,
	"cert-secret", cfg.CertSecretNamespace+"/"+cfg.CertSecretName,
	"leader-election", cfg.EnableLeaderElection,
)
```

With:

```go
setupLog.Info("Starting webhook-injector",
	"namespace", cfg.Namespace,
	"config-name", cfg.WebhookConfigName,
	"cert-secret", cfg.CertSecretName,
	"leader-election", cfg.EnableLeaderElection,
)
```

**Step 2: Update cache namespace configuration**

In `cmd/webhook-injector/main.go`, around line 98, replace:

```go
Cache: cache.Options{
	DefaultNamespaces: map[string]cache.Config{
		getPodNamespace(cfg): {},
	},
},
```

With:

```go
Cache: cache.Options{
	DefaultNamespaces: map[string]cache.Config{
		cfg.Namespace: {},
	},
},
```

**Step 3: Remove getPodNamespace function**

Delete the entire `getPodNamespace()` function (lines 215-231 approximately):

```go
// getPodNamespace returns the namespace the pod is running in
func getPodNamespace(cfg *config.Config) string {
	// ... delete entire function ...
}
```

**Step 4: Build to verify compilation**

Run: `make build`

Expected: Build succeeds with no errors

**Step 5: Commit**

```bash
git add cmd/webhook-injector/main.go
git commit -m "refactor: use cfg.Namespace in main and remove getPodNamespace

Namespace resolution now happens in config.LoadFromEnv(), so getPodNamespace()
is no longer needed. Updated logging and cache config to use cfg.Namespace.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 5: Update deployment manifests

**Files:**
- Modify: `config/deployment.yaml:102-115` (webhook-injector env vars)
- Modify: `test/e2e/manifests/webhook-server-deployment.yaml` (webhook-injector env vars)

**Step 1: Update config/deployment.yaml**

In `config/deployment.yaml`, replace the env section (lines 101-115):

Replace:
```yaml
          env:
            - name: WEBHOOK_CONFIG_NAME
              value: webhook-config
            - name: WEBHOOK_CONFIG_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: CERT_SECRET_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: LEADER_ELECTION_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
```

With:
```yaml
          env:
            - name: WEBHOOK_CONFIG_NAME
              value: webhook-config
            - name: NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
```

**Step 2: Find e2e webhook-server-deployment.yaml env section**

Run: `grep -n "WEBHOOK_CONFIG_NAMESPACE\|CERT_SECRET_NAMESPACE\|LEADER_ELECTION_NAMESPACE" test/e2e/manifests/webhook-server-deployment.yaml`

Expected: Shows line numbers of old env vars

**Step 3: Update test/e2e/manifests/webhook-server-deployment.yaml**

Replace the same pattern as in step 1, consolidating 3 namespace env vars into 1 NAMESPACE env var.

**Step 4: Verify YAML syntax**

Run: `yamllint config/deployment.yaml test/e2e/manifests/webhook-server-deployment.yaml || echo "yamllint not installed, skipping"`

Expected: No YAML syntax errors

**Step 5: Commit**

```bash
git add config/deployment.yaml test/e2e/manifests/webhook-server-deployment.yaml
git commit -m "refactor: consolidate namespace env vars in deployment manifests

Replace WEBHOOK_CONFIG_NAMESPACE, CERT_SECRET_NAMESPACE, and
LEADER_ELECTION_NAMESPACE with single NAMESPACE env var.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 6: Update CLAUDE.md documentation

**Files:**
- Modify: `CLAUDE.md` (Configuration section around line 160)

**Step 1: Update configuration documentation**

In `CLAUDE.md`, find the Configuration section (around line 160) and update the environment variables list:

Replace:
```markdown
All configuration via environment variables (no flags). See `pkg/config/config.go` for:
- `WEBHOOK_CONFIG_NAME` (required) - ConfigMap with webhook definitions
- `CERT_SECRET_NAME` - Where to store certificates
- `CERT_DIR` - Where to write certificate files
```

Add after `WEBHOOK_CONFIG_NAME`:
```markdown
- `NAMESPACE` - Namespace for all resources (ConfigMap, Secret, Lease). Auto-detects from pod's serviceaccount if not set, defaults to "default"
```

Remove any mentions of:
- `WEBHOOK_CONFIG_NAMESPACE`
- `CERT_SECRET_NAMESPACE`
- `LEADER_ELECTION_NAMESPACE`

**Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md for simplified namespace configuration

Document new NAMESPACE env var and remove references to old namespace vars.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 7: Run full test suite and verify

**Step 1: Run all unit tests**

Run: `make test`

Expected: All tests PASS

**Step 2: Run static checks**

Run: `make static-check`

Expected: No linting errors

**Step 3: Build binary**

Run: `make build`

Expected: Binary builds successfully to `build/webhook-injector`

**Step 4: Run e2e tests (optional, requires kind)**

Run: `make test-e2e`

Expected: E2E tests PASS with new NAMESPACE env var

**Step 5: Verify no references to old env vars remain**

Run: `grep -r "WEBHOOK_CONFIG_NAMESPACE\|CERT_SECRET_NAMESPACE\|LEADER_ELECTION_NAMESPACE" --include="*.go" --include="*.yaml" --exclude-dir=docs .`

Expected: No matches (except possibly in old design docs in docs/plans/)

**Step 6: Final commit if any fixes needed**

```bash
# If any issues found and fixed
git add .
git commit -m "fix: address remaining namespace configuration issues

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Completion

All tasks complete! The namespace configuration is now simplified to a single `NAMESPACE` environment variable with automatic detection from the pod's serviceaccount namespace file, falling back to "default". The Config struct has a single `Namespace` field, and all namespace resolution happens once at startup in `config.LoadFromEnv()`.

**Summary of changes:**
- ✅ Config struct: 3 namespace fields → 1 Namespace field
- ✅ Config loading: Added namespace auto-detection with priority order
- ✅ Controllers: Updated to use cfg.Namespace
- ✅ main.go: Removed getPodNamespace(), uses cfg.Namespace for cache
- ✅ Deployment manifests: 3 env vars → 1 NAMESPACE env var
- ✅ Documentation: Updated CLAUDE.md
- ✅ Tests: All passing with new configuration

**Verification commands:**
```bash
make test          # All unit tests pass
make static-check  # No linting errors
make build         # Binary builds successfully
make test-e2e      # E2E tests pass (optional)
```
