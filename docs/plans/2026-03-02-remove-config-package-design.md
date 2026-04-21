<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# Remove Config Package - Use Pure stdlib Flags

**Date:** 2026-03-02
**Status:** Approved

## Problem

The config package adds unnecessary abstraction and complexity:

- Entire package (`pkg/config/`) with Config struct, LoadFromEnv(), helper functions, custom error types
- ~172 lines of code + ~264 lines of tests = ~436 lines total
- Environment variable parsing with custom logic
- Indirection through Config struct makes code less direct
- Custom getEnv* helper functions for parsing different types
- Custom error types for validation

This violates YAGNI - we're using standard configuration that doesn't need a separate package.

## Solution

Remove the config package entirely and use pure stdlib flags directly in main.go.

### Changes

**Delete:**
- `pkg/config/config.go` (entire file)
- `pkg/config/config_test.go` (entire file)
- All imports of `github.com/sap-cloud-infrastructure/webhook-injector/pkg/config`

**Add flags to main.go:**
- Define flags using stdlib `flag` package
- Parse flags after zap options setup
- Validate required flags
- Auto-detect namespace if not provided

**Update controllers:**
- Replace `Config *config.Config` field with individual fields
- WebhookReconciler: WebhookConfigName, Namespace, CertSecretName, CertValidity, CertRotationThreshold
- CertificateReloader: Namespace, CertSecretName, CertDir
- Update all references from `r.Config.FieldName` to `r.FieldName`

## Architecture

### Current State

```
pkg/config/config.go (172 lines)
  ├── Config struct (13 fields)
  ├── LoadFromEnv() - parses env vars
  ├── loadFromEnvWithSAPath() - testable version
  ├── detectNamespaceWithPath() - namespace detection
  ├── Validate() - validation logic
  ├── getEnv() - string helper
  ├── getEnvBool() - bool helper
  ├── getEnvFloat() - float64 helper
  ├── getEnvDuration() - duration helper
  └── ConfigError - custom error type

pkg/config/config_test.go (264 lines)
  ├── TestLoadFromEnv
  ├── TestLoadFromEnvDefaults
  ├── TestLoadFromEnv_CertValidity_Duration
  ├── TestValidate
  └── TestLoadFromEnv_NamespaceResolution

cmd/webhook-injector/main.go
  └── cfg, _ := config.LoadFromEnv()

pkg/controllers/reconciler.go
  └── uses r.Config.FieldName throughout

pkg/controllers/reloader.go
  └── uses r.Config.FieldName throughout
```

### New State

```
cmd/webhook-injector/main.go
  ├── Flag definitions (13 variables)
  ├── flag.Parse()
  ├── Validation (required flags check)
  ├── detectNamespace() - simple helper
  └── Pass values directly to controllers

pkg/controllers/reconciler.go
  └── uses r.FieldName throughout (no Config struct)

pkg/controllers/reloader.go
  └── uses r.FieldName throughout (no Config struct)
```

## Implementation Details

### Flag Definitions in main.go

```go
var (
    webhookConfigName       = flag.String("webhook-config-name", "", "ConfigMap name containing webhook definitions (required)")
    namespace              = flag.String("namespace", "", "Namespace for all resources (auto-detects from pod if not set)")
    certSecretName         = flag.String("cert-secret-name", "webhook-injector-certs", "Secret name for storing certificates")
    certDir                = flag.String("cert-dir", "/tmp/webhook-certs", "Directory to write certificate files")
    certValidity           = flag.Duration("cert-validity", 720*time.Hour, "Certificate validity duration (e.g., 720h, 30d)")
    certRotationThreshold  = flag.Float64("cert-rotation-threshold", 0.8, "Certificate rotation threshold (0-1)")
    targetKubeconfig       = flag.String("target-kubeconfig", "", "Path to target cluster kubeconfig (optional, defaults to in-cluster)")
    enableLeaderElection   = flag.Bool("enable-leader-election", true, "Enable leader election for HA")
    leaderElectionID       = flag.String("leader-election-id", "webhook-injector-leader", "Leader election lease name")
    syncPeriod             = flag.Duration("sync-period", 5*time.Minute, "Controller reconciliation sync period")
    metricsAddr            = flag.String("metrics-addr", ":8080", "Metrics server bind address")
    healthAddr             = flag.String("health-addr", ":8081", "Health probe server bind address")
)
```

### Validation in main()

```go
func main() {
    // Setup zap logging
    opts := zap.Options{Development: true}
    opts.BindFlags(flag.CommandLine)
    flag.Parse()
    ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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

    setupLog.Info("Starting webhook-injector",
        "namespace", *namespace,
        "webhook-config-name", *webhookConfigName,
        "cert-secret-name", *certSecretName,
        "enable-leader-election", *enableLeaderElection,
    )

    // ... rest of main
}
```

### Namespace Auto-Detection Helper

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

### Controller Structure Changes

**WebhookReconciler:**

```go
type WebhookReconciler struct {
    Client                client.Client
    TargetClient          client.Client
    WebhookConfigName     string
    Namespace             string
    CertSecretName        string
    CertValidity          time.Duration
    CertRotationThreshold float64
    Scheme                *runtime.Scheme
}
```

**CertificateReloader:**

```go
type CertificateReloader struct {
    Client         client.Client
    Namespace      string
    CertSecretName string
    CertDir        string
}
```

### Controller Instantiation

```go
// WebhookReconciler
reconciler := &controllers.WebhookReconciler{
    Client:                mgr.GetClient(),
    TargetClient:          targetClient,
    WebhookConfigName:     *webhookConfigName,
    Namespace:             *namespace,
    CertSecretName:        *certSecretName,
    CertValidity:          *certValidity,
    CertRotationThreshold: *certRotationThreshold,
    Scheme:                mgr.GetScheme(),
}

// CertificateReloader
reloader := &controllers.CertificateReloader{
    Client:         mgr.GetClient(),
    Namespace:      *namespace,
    CertSecretName: *certSecretName,
    CertDir:        *certDir,
}
```

## Benefits

1. **Massive code reduction**: Delete ~436 lines (config package + tests)
2. **Standard Go idiom**: Pure stdlib flags, no custom parsing
3. **Built-in help**: `--help` flag automatically generated by stdlib
4. **Less indirection**: Direct field access instead of `cfg.FieldName`
5. **Simpler testing**: Controllers easier to test with explicit fields
6. **No boilerplate**: No getEnv* helpers, no custom error types
7. **Flag validation**: stdlib flag package handles type validation
8. **Standard patterns**: Follows common Go service patterns

## Trade-offs

1. **More controller fields**: Controllers have 3-5 individual fields instead of 1 Config pointer
   - **Acceptable**: Makes dependencies explicit, easier to understand what each controller needs
2. **No environment variable support**: Pure flags only
   - **Acceptable**: Flags are standard for Go services, Kubernetes can inject env vars as args if needed
3. **Validation in main**: Validation logic moves from Config.Validate() to main()
   - **Acceptable**: Simpler, more direct, validation is straightforward
4. **Breaking change**: Anyone importing pkg/config will break
   - **Acceptable**: Internal tool, no external consumers
5. **Less testable config loading**: No separate config tests
   - **Acceptable**: Flag parsing is stdlib-tested, validation is trivial

## Migration Notes

**For deployment manifests:**

Change from environment variables to command args:

Before (env vars):
```yaml
env:
  - name: WEBHOOK_CONFIG_NAME
    value: webhook-config
  - name: NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
```

After (args):
```yaml
args:
  - --webhook-config-name=webhook-config
  - --namespace=$(NAMESPACE)
env:
  - name: NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
```

Or use Kubernetes downward API directly:
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

## Testing Strategy

- **Unit tests**: Update controller tests to pass individual fields instead of Config struct
- **Integration tests**: Update e2e tests to use flag-based configuration
- **Validation**: Test required flag validation (missing webhook-config-name)
- **Defaults**: Test that default values work correctly
- **Namespace detection**: Test detectNamespace() fallback to "default"

## Files to Change

- Delete: `pkg/config/config.go`
- Delete: `pkg/config/config_test.go`
- Modify: `cmd/webhook-injector/main.go` (add flags, validation, helper)
- Modify: `pkg/controllers/reconciler.go` (struct fields, field references)
- Modify: `pkg/controllers/reconciler_test.go` (test setup)
- Modify: `pkg/controllers/reloader.go` (struct fields, field references)
- Modify: `pkg/controllers/reloader_test.go` (test setup)
- Modify: `config/deployment.yaml` (env vars → args)
- Modify: `test/e2e/manifests/webhook-server-deployment.yaml` (env vars → args)
- Modify: `CLAUDE.md` (update configuration documentation)
- Modify: `README.md` (update configuration table)

## Completion Criteria

- Config package deleted
- All tests passing
- Controllers use individual fields
- Deployment manifests use flags
- Documentation updated
- `--help` flag shows all configuration options
