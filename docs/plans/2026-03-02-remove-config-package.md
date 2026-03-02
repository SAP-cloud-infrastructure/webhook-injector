# Remove Config Package - Use stdlib Flags Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Remove the config package entirely and use pure stdlib flags directly in main.go, eliminating ~436 lines of unnecessary abstraction.

**Architecture:** Delete pkg/config package. Add flag definitions to main.go. Update controllers to receive individual fields instead of Config struct. Change deployment manifests from env vars to args.

**Tech Stack:** Go 1.23, stdlib flag package, Kubernetes

---

## Task 1: Update WebhookReconciler struct and tests

**Files:**
- Modify: `pkg/controllers/reconciler.go:54-59` (struct definition)
- Modify: `pkg/controllers/reconciler.go` (all r.Config.* references)
- Test: `pkg/controllers/reconciler_test.go` (all test setups)

**Step 1: Update WebhookReconciler struct**

In `pkg/controllers/reconciler.go`, replace the struct definition (around lines 54-59):

Replace:
```go
type WebhookReconciler struct {
	Client       client.Client // Hosting cluster client
	TargetClient client.Client // Target cluster client
	Config       *config.Config
	Scheme       *runtime.Scheme
}
```

With:
```go
type WebhookReconciler struct {
	Client                client.Client  // Hosting cluster client
	TargetClient          client.Client  // Target cluster client
	WebhookConfigName     string
	Namespace             string
	CertSecretName        string
	CertValidity          time.Duration
	CertRotationThreshold float64
	Scheme                *runtime.Scheme
}
```

**Step 2: Update all r.Config references in reconciler.go**

Find and replace throughout the file:
- `r.Config.WebhookConfigName` → `r.WebhookConfigName`
- `r.Config.Namespace` → `r.Namespace`
- `r.Config.CertSecretName` → `r.CertSecretName`
- `r.Config.CertValidity` → `r.CertValidity`
- `r.Config.CertRotationThreshold` → `r.CertRotationThreshold`

Run: `grep -n "r.Config\." pkg/controllers/reconciler.go` to find all instances.

**Step 3: Update reconciler_test.go**

In `pkg/controllers/reconciler_test.go`, find all places where WebhookReconciler is created and update:

Replace pattern:
```go
reconciler := &WebhookReconciler{
	Client:       fakeClient,
	TargetClient: fakeTargetClient,
	Config: &config.Config{
		Namespace:             "default",
		CertSecretName:        "test-secret",
		CertValidity:          1 * time.Hour,
		CertRotationThreshold: 0.8,
	},
	Scheme: scheme,
}
```

With pattern:
```go
reconciler := &WebhookReconciler{
	Client:                fakeClient,
	TargetClient:          fakeTargetClient,
	WebhookConfigName:     "test-config",
	Namespace:             "default",
	CertSecretName:        "test-secret",
	CertValidity:          1 * time.Hour,
	CertRotationThreshold: 0.8,
	Scheme:                scheme,
}
```

**Step 4: Remove config package import**

Remove the import line:
```go
"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
```

**Step 5: Run reconciler tests**

Run: `go test -v ./pkg/controllers/... -run TestWebhookReconciler`

Expected: Compilation errors about config package not being needed anymore (this is expected, main.go still imports it)

**Step 6: Commit**

```bash
git add pkg/controllers/reconciler.go pkg/controllers/reconciler_test.go
git commit -m "refactor: update WebhookReconciler to use individual fields

Replace Config pointer with explicit fields: WebhookConfigName, Namespace,
CertSecretName, CertValidity, CertRotationThreshold. Update all references
from r.Config.Field to r.Field. Update tests accordingly.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 2: Update CertificateReloader struct and tests

**Files:**
- Modify: `pkg/controllers/reloader.go:39-42` (struct definition)
- Modify: `pkg/controllers/reloader.go` (all r.Config.* references)
- Test: `pkg/controllers/reloader_test.go` (all test setups)

**Step 1: Update CertificateReloader struct**

In `pkg/controllers/reloader.go`, replace the struct definition (around lines 39-42):

Replace:
```go
type CertificateReloader struct {
	Client client.Client
	Config *config.Config
}
```

With:
```go
type CertificateReloader struct {
	Client         client.Client
	Namespace      string
	CertSecretName string
	CertDir        string
}
```

**Step 2: Update all r.Config references in reloader.go**

Find and replace throughout the file:
- `r.Config.Namespace` → `r.Namespace`
- `r.Config.CertSecretName` → `r.CertSecretName`
- `r.Config.CertDir` → `r.CertDir`

Run: `grep -n "r.Config\." pkg/controllers/reloader.go` to find all instances.

**Step 3: Update reloader_test.go**

In `pkg/controllers/reloader_test.go`, find all places where CertificateReloader is created and update:

Replace pattern:
```go
reloader := &CertificateReloader{
	Client: fakeClient,
	Config: &config.Config{
		Namespace:      "default",
		CertSecretName: "test-secret",
		CertDir:        tmpDir,
	},
}
```

With pattern:
```go
reloader := &CertificateReloader{
	Client:         fakeClient,
	Namespace:      "default",
	CertSecretName: "test-secret",
	CertDir:        tmpDir,
}
```

**Step 4: Remove config package import**

Remove the import line:
```go
"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
```

**Step 5: Run reloader tests**

Run: `go test -v ./pkg/controllers/... -run TestCertificateReloader`

Expected: Tests should pass now that config import is removed

**Step 6: Commit**

```bash
git add pkg/controllers/reloader.go pkg/controllers/reloader_test.go
git commit -m "refactor: update CertificateReloader to use individual fields

Replace Config pointer with explicit fields: Namespace, CertSecretName, CertDir.
Update all references from r.Config.Field to r.Field. Update tests accordingly.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 3: Update main.go with flags and remove config import

**Files:**
- Modify: `cmd/webhook-injector/main.go`

**Step 1: Remove config package import**

Remove the import:
```go
"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
```

**Step 2: Add flag definitions at package level**

After the existing var block (around line 42), add:

```go
var (
	webhookConfigName       = flag.String("webhook-config-name", "", "ConfigMap name containing webhook definitions (required)")
	namespace              = flag.String("namespace", "", "Namespace for all resources (auto-detects from pod if not set)")
	certSecretName         = flag.String("cert-secret-name", "webhook-injector-certs", "Secret name for storing certificates")
	certDir                = flag.String("cert-dir", "/tmp/webhook-certs", "Directory to write certificate files")
	certValidity           = flag.Duration("cert-validity", 720*time.Hour, "Certificate validity duration")
	certRotationThreshold  = flag.Float64("cert-rotation-threshold", 0.8, "Certificate rotation threshold (0-1)")
	targetKubeconfig       = flag.String("target-kubeconfig", "", "Path to target cluster kubeconfig")
	enableLeaderElection   = flag.Bool("enable-leader-election", true, "Enable leader election for HA")
	leaderElectionID       = flag.String("leader-election-id", "webhook-injector-leader", "Leader election lease name")
	syncPeriod             = flag.Duration("sync-period", 5*time.Minute, "Controller reconciliation sync period")
	metricsAddr            = flag.String("metrics-addr", ":8080", "Metrics server bind address")
	healthAddr             = flag.String("health-addr", ":8081", "Health probe server bind address")
)
```

**Step 3: Add detectNamespace helper function**

After main() function, add:

```go
func detectNamespace() string {
	const serviceAccountFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	data, err := os.ReadFile(serviceAccountFile)
	if err == nil && len(data) > 0 {
		return string(data)
	}
	return "default"
}
```

**Step 4: Replace config loading with flag parsing and validation**

In main(), replace these lines (around lines 65-74):

Remove:
```go
	// Load configuration
	cfg, err := config.LoadFromEnv()
	if err != nil {
		setupLog.Error(err, "failed to load configuration")
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		setupLog.Error(err, "Invalid configuration")
		os.Exit(1)
	}
```

Add:
```go
	// Validate required flags
	if *webhookConfigName == "" {
		setupLog.Error(nil, "webhook-config-name is required")
		flag.Usage()
		os.Exit(1)
	}

	// Auto-detect namespace if not provided
	if *namespace == "" {
		*namespace = detectNamespace()
	}

	// Validate cert validity
	if *certValidity < time.Minute {
		setupLog.Error(nil, "cert-validity too short, minimum 1m")
		os.Exit(1)
	}
```

**Step 5: Update startup logging**

Replace the logging (around line 76-81):

Replace:
```go
	setupLog.Info("Starting webhook-injector",
		"namespace", cfg.Namespace,
		"config-name", cfg.WebhookConfigName,
		"cert-secret", cfg.CertSecretName,
		"leader-election", cfg.EnableLeaderElection,
	)
```

With:
```go
	setupLog.Info("Starting webhook-injector",
		"namespace", *namespace,
		"webhook-config-name", *webhookConfigName,
		"cert-secret-name", *certSecretName,
		"enable-leader-election", *enableLeaderElection,
	)
```

**Step 6: Update manager options**

Replace the manager options (around lines 86-92):

Replace:
```go
		HealthProbeBindAddress:  cfg.HealthAddr,
		LeaderElection:          cfg.EnableLeaderElection,
		LeaderElectionID:        cfg.LeaderElectionID,
		LeaderElectionNamespace: cfg.LeaderElectionNamespace,
```

With:
```go
		HealthProbeBindAddress:  *healthAddr,
		LeaderElection:          *enableLeaderElection,
		LeaderElectionID:        *leaderElectionID,
		LeaderElectionNamespace: *namespace,
```

And:
```go
			BindAddress: cfg.MetricsAddr,
```

With:
```go
			BindAddress: *metricsAddr,
```

And in Cache Options:
```go
				cfg.Namespace: {},
```

With:
```go
				*namespace: {},
```

**Step 7: Update getTargetClient function**

Replace:
```go
	if cfg.Kubeconfig != "" {
		// Load from explicit kubeconfig
		targetConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig from %s: %w", cfg.Kubeconfig, err)
		}
		setupLog.Info("Using explicit kubeconfig for target cluster", "path", cfg.Kubeconfig)
```

With:
```go
	if *targetKubeconfig != "" {
		// Load from explicit kubeconfig
		targetConfig, err = clientcmd.BuildConfigFromFlags("", *targetKubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to load kubeconfig from %s: %w", *targetKubeconfig, err)
		}
		setupLog.Info("Using explicit kubeconfig for target cluster", "path", *targetKubeconfig)
```

**Step 8: Update controller instantiation**

Replace CertificateReloader creation (around lines 146-148):

Replace:
```go
	certReloader := &controllers.CertificateReloader{
		Client: mgr.GetClient(),
		Config: cfg,
	}
```

With:
```go
	certReloader := &controllers.CertificateReloader{
		Client:         mgr.GetClient(),
		Namespace:      *namespace,
		CertSecretName: *certSecretName,
		CertDir:        *certDir,
	}
```

Replace WebhookReconciler creation (around lines 157-162):

Replace:
```go
	webhookReconciler := &controllers.WebhookReconciler{
		Client:       mgr.GetClient(),
		TargetClient: targetClient,
		Config:       cfg,
		Scheme:       mgr.GetScheme(),
	}
```

With:
```go
	webhookReconciler := &controllers.WebhookReconciler{
		Client:                mgr.GetClient(),
		TargetClient:          targetClient,
		WebhookConfigName:     *webhookConfigName,
		Namespace:             *namespace,
		CertSecretName:        *certSecretName,
		CertValidity:          *certValidity,
		CertRotationThreshold: *certRotationThreshold,
		Scheme:                mgr.GetScheme(),
	}
```

**Step 9: Update getTargetClient signature**

Change function signature from:
```go
func getTargetClient(cfg *config.Config) (client.Client, error) {
```

To:
```go
func getTargetClient() (client.Client, error) {
```

And update the call site (around line 110):
```go
	targetClient, err := getTargetClient()
```

**Step 10: Build to verify compilation**

Run: `go build ./cmd/webhook-injector`

Expected: Should compile successfully

**Step 11: Test --help flag**

Run: `./build/webhook-injector --help`

Expected: Should show all flag options with descriptions

**Step 12: Commit**

```bash
git add cmd/webhook-injector/main.go
git commit -m "refactor: replace config package with stdlib flags in main.go

Add flag definitions for all configuration options. Remove config package import.
Update controller instantiation to pass individual fields. Add detectNamespace()
helper for auto-detection. Add validation for required flags and cert validity.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 4: Delete config package

**Files:**
- Delete: `pkg/config/config.go`
- Delete: `pkg/config/config_test.go`

**Step 1: Verify no remaining imports**

Run: `grep -r "webhook-injector/pkg/config" --include="*.go" .`

Expected: No matches (all imports removed)

**Step 2: Delete config files**

```bash
git rm pkg/config/config.go pkg/config/config_test.go
```

**Step 3: Try to remove the directory**

```bash
rmdir pkg/config 2>/dev/null || echo "Directory not empty or doesn't exist"
```

**Step 4: Run all tests**

Run: `go test ./...`

Expected: All tests pass

**Step 5: Commit**

```bash
git commit -m "refactor: delete config package

Remove pkg/config package entirely (config.go and config_test.go). Configuration
is now handled by stdlib flags in main.go. Eliminates ~436 lines of code.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 5: Update deployment manifests to use flags

**Files:**
- Modify: `config/deployment.yaml`
- Modify: `test/e2e/manifests/webhook-server-deployment.yaml`

**Step 1: Update config/deployment.yaml**

Find the webhook-injector container env section (around lines 101-115) and replace with args:

Replace:
```yaml
        - name: webhook-injector
          image: webhook-injector:latest
          env:
            - name: WEBHOOK_CONFIG_NAME
              value: webhook-config
            - name: NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
```

With:
```yaml
        - name: webhook-injector
          image: webhook-injector:latest
          args:
            - --webhook-config-name=webhook-config
            - --namespace=$(POD_NAMESPACE)
          env:
            - name: POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
```

**Step 2: Find and update test/e2e/manifests/webhook-server-deployment.yaml**

Run: `grep -n "WEBHOOK_CONFIG_NAME\|NAMESPACE" test/e2e/manifests/webhook-server-deployment.yaml`

Replace the webhook-injector container env section with args following the same pattern.

**Step 3: Verify YAML syntax**

Run: `yamllint config/deployment.yaml test/e2e/manifests/webhook-server-deployment.yaml || echo "yamllint not installed, visual check OK"`

**Step 4: Commit**

```bash
git add config/deployment.yaml test/e2e/manifests/webhook-server-deployment.yaml
git commit -m "refactor: convert deployment manifests from env vars to flags

Replace environment variables with command-line flags for webhook-injector
container. Use downward API to inject namespace as POD_NAMESPACE env var,
then pass to --namespace flag.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 6: Update documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

**Step 1: Update README.md configuration table**

Find the Configuration section (around line 212) and update the table to show flags instead of env vars:

Replace the table header and note:
```markdown
## Configuration

All configuration via environment variables:

| Variable | Description | Default | Required |
```

With:
```markdown
## Configuration

All configuration via command-line flags:

| Flag | Description | Default | Required |
```

Update each row from env var format to flag format:
- `WEBHOOK_CONFIG_NAME` → `--webhook-config-name`
- `NAMESPACE` → `--namespace`
- `CERT_SECRET_NAME` → `--cert-secret-name`
- etc.

Add note about usage:
```markdown
**Usage:**
```bash
webhook-injector --webhook-config-name=my-config --namespace=default
```

Or in Kubernetes deployment:
```yaml
args:
  - --webhook-config-name=webhook-config
  - --namespace=$(POD_NAMESPACE)
env:
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
```
```

**Step 2: Update CLAUDE.md**

Find the Configuration section (around line 181) and update:

Replace:
```markdown
All configuration via environment variables (no flags). See `pkg/config/config.go` for:
- `WEBHOOK_CONFIG_NAME` (required) - ConfigMap with webhook definitions
- `NAMESPACE` - Namespace for all resources...
```

With:
```markdown
All configuration via command-line flags. Run `webhook-injector --help` for full list:
- `--webhook-config-name` (required) - ConfigMap with webhook definitions
- `--namespace` - Namespace for all resources (auto-detects from pod if not set, defaults to "default")
```

Update all other env var references to use flag format.

**Step 3: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "docs: update configuration documentation for flags

Replace environment variable documentation with command-line flag documentation.
Update examples to show flag usage. Add note about Kubernetes downward API
pattern for injecting pod metadata.

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 7: Run full test suite and verify

**Step 1: Run all unit tests**

Run: `go test ./...`

Expected: All tests PASS

**Step 2: Run static checks**

Run: `gmake static-check`

Expected: No errors (may have pre-existing warnings)

**Step 3: Build binary**

Run: `gmake build`

Expected: Binary builds successfully

**Step 4: Test help flag**

Run: `./build/webhook-injector --help`

Expected: Shows all 12 flags with descriptions

**Step 5: Verify config package is gone**

Run: `ls pkg/config 2>/dev/null && echo "ERROR: config package still exists" || echo "OK: config package deleted"`

Expected: "OK: config package deleted"

**Step 6: Verify line count reduction**

Run: `git diff HEAD~6 --stat | tail -1`

Expected: Should show significant net deletion (around -400 lines)

**Step 7: Final verification**

Run through this checklist:
- [ ] config package deleted
- [ ] All tests passing
- [ ] Controllers use individual fields
- [ ] main.go uses flags
- [ ] Deployment manifests use args
- [ ] Documentation updated
- [ ] --help works
- [ ] Binary builds

**Step 8: No final commit needed**

All changes are committed per-task.

---

## Completion

All tasks complete! The config package has been removed and replaced with pure stdlib flags. The codebase is now ~436 lines simpler with standard Go configuration patterns.

**Summary of changes:**
- ✅ Deleted pkg/config package (config.go + config_test.go)
- ✅ Added 12 flag definitions to main.go
- ✅ Updated WebhookReconciler to use individual fields
- ✅ Updated CertificateReloader to use individual fields
- ✅ Updated all controller tests
- ✅ Converted deployment manifests from env vars to flags
- ✅ Updated documentation (README.md + CLAUDE.md)
- ✅ All tests passing

**Verification commands:**
```bash
go test ./...           # All tests pass
gmake build            # Binary builds
./build/webhook-injector --help  # Shows all flags
```
