# webhook-injector Design

**Date:** 2026-02-25
**Status:** Approved

## Overview

**webhook-injector** is a Kubernetes controller that runs as a sidecar to webhook servers, managing the complete lifecycle of webhook configurations and TLS certificates for remote or local clusters.

The key innovation is enabling webhook servers to serve webhooks for clusters they don't run in, while providing a generic solution that works with any webhook server (regardless of language or framework) as long as it can watch and reload certificate files.

## Problem Statement

Kubernetes admission webhooks require:
1. Valid TLS certificates for the webhook server
2. `MutatingWebhookConfiguration` and `ValidatingWebhookConfiguration` resources in the target cluster
3. CA bundle injection into webhook configurations
4. Certificate rotation before expiry

When the webhook server runs outside the target cluster, these operations become complex. Existing solutions like cert-manager or Gardener's certificate libraries require the webhook server to run in the same cluster or have tight integration with specific frameworks.

## Solution Architecture

### Core Components

**webhook-injector** runs as a sidecar with two internal controllers:

1. **WebhookReconciler (leader-only):**
   - Watches webhook ConfigMap for changes
   - Generates/rotates CA + server certificates with dynamic SANs
   - Writes certificates to Secret in hosting cluster
   - Parses webhook configurations, injects CA bundle
   - Applies webhook configurations to target cluster
   - Implements default leader election behavior (runs only on leader)

2. **CertificateReloader (all replicas):**
   - Watches certificate Secret in hosting cluster
   - Writes certificates to shared emptyDir volume (only when content changes)
   - All replicas stay synchronized with latest certificates
   - Implements `NeedLeaderElection() = false` to run on all replicas

### Deployment Pattern

```yaml
Pod:
  - container: webhook-server
    volumeMounts:
      - name: webhook-certs
        mountPath: /tmp/webhook-certs
        readOnly: true

  - container: webhook-injector (sidecar)
    volumeMounts:
      - name: webhook-certs
        mountPath: /tmp/webhook-certs

  volumes:
    - name: webhook-certs
      emptyDir: {}
```

### Cluster Architecture

```
Hosting Cluster:
  ConfigMap: webhook-config (webhook definitions)
  Secret: webhook-injector-certs (CA + server certs)
  Lease: webhook-injector-leader (leader election)

Leader Pod:
  ├─ WebhookReconciler: ConfigMap → Generate Certs → Secret → Target Cluster
  └─ CertificateReloader: Secret → emptyDir

Non-Leader Pods:
  └─ CertificateReloader: Secret → emptyDir

Target Cluster:
  MutatingWebhookConfiguration (created/updated by leader)
  ValidatingWebhookConfiguration (created/updated by leader)
```

## Webhook Definition Format

The sidecar watches a ConfigMap containing full Kubernetes webhook configurations in multi-document YAML format.

**ConfigMap Structure:**

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: webhook-config
  namespace: default
data:
  webhooks.yaml: |
    ---
    apiVersion: admissionregistration.k8s.io/v1
    kind: MutatingWebhookConfiguration
    metadata:
      name: my-mutating-webhook
    webhooks:
      - name: webhook1.example.com
        clientConfig:
          service:
            name: webhook-service
            namespace: default
            path: /mutate
            port: 9443
          caBundle: ""  # Injected by sidecar
        rules:
          - operations: ["CREATE", "UPDATE"]
            apiGroups: ["apps"]
            apiVersions: ["v1"]
            resources: ["deployments"]
        admissionReviewVersions: ["v1"]
        sideEffects: None
        failurePolicy: Fail
        timeoutSeconds: 10
    ---
    apiVersion: admissionregistration.k8s.io/v1
    kind: ValidatingWebhookConfiguration
    metadata:
      name: my-validating-webhook
    webhooks:
      - name: validate.example.com
        clientConfig:
          url: "https://webhook.remote-cluster.com:9443/validate"
          caBundle: ""  # Injected by sidecar
        rules:
          - operations: ["CREATE"]
            apiGroups: [""]
            apiVersions: ["v1"]
            resources: ["pods"]
        admissionReviewVersions: ["v1"]
        sideEffects: None
        failurePolicy: Fail
```

**Benefits:**
- Full control over webhook configuration
- No custom abstraction layer
- Supports all Kubernetes webhook features
- Easy to validate with standard Kubernetes tools

**Processing:**
1. Read `webhooks.yaml` from ConfigMap
2. Split on `---` and parse each YAML document
3. Filter for `MutatingWebhookConfiguration` and `ValidatingWebhookConfiguration` kinds
4. Inject CA bundle into all `clientConfig.caBundle` fields (replacing empty or existing values)
5. Apply to target cluster

## Certificate Management

### Certificate Generation

**Generation is driven by webhook configuration parsing:**

1. Parse webhook ConfigMap to extract references from `clientConfig`
2. Collect service references from `clientConfig.service` (name + namespace)
3. Collect URL references from `clientConfig.url` (extract hostname)
4. Generate certificates with appropriate SANs

**Certificate Details:**

- **CA Certificate:**
  - Self-signed root CA
  - Configurable validity (default: 30 days)
  - Standard RSA 2048-bit key
  - Common Name: `webhook-injector-ca`

- **Server Certificate:**
  - Signed by the CA
  - Configurable validity (default: 30 days)
  - Standard RSA 2048-bit key
  - **Subject Alternative Names (SANs):** Extracted from webhook configurations
    - **For service references:** `<service-name>.<namespace>.svc` and `<service-name>.<namespace>.svc.cluster.local`
    - **For URL references:** hostname extracted from URL (e.g., `webhook.remote-cluster.com`)
    - Supports mixed configurations (both service and URL references in same certificate)

**Examples:**

- Service reference `{name: webhook-service, namespace: default}` generates SANs:
  - `webhook-service.default.svc`
  - `webhook-service.default.svc.cluster.local`

- URL reference `https://webhook.remote-cluster.com:9443/validate` generates SAN:
  - `webhook.remote-cluster.com`

**Implementation:**
- Uses Go's `crypto/x509` package
- URL parsing uses `net/url` package to extract hostnames
- SANs are dynamically generated from parsed webhook configs

### Certificate Storage

**In Hosting Cluster (Secret):**
- Used for coordination between replicas
- Leader writes, all replicas read
- Survives pod restarts

**In Shared Volume (emptyDir):**
- Standard Kubernetes TLS secret format:
  - `ca.crt`: CA certificate (PEM)
  - `tls.crt`: Server certificate (PEM)
  - `tls.key`: Server private key (PEM)
- Written by all replicas from Secret
- Consumed by webhook server

### Certificate Rotation

**Rotation Triggers:**
- **Time-based:** Automatic when remaining validity < `CERT_ROTATION_THRESHOLD` × total validity
  - Default threshold: 0.8 (rotates at 80% of validity, e.g., 24 days into a 30-day certificate)
  - Check runs during every reconciliation (ConfigMap changes and periodic sync)
- **Config-based:** When webhook service references or URLs change
  - Adding new webhook with different service → new SANs → rotation triggered
  - Changing service name/namespace → SANs change → rotation triggered
  - Changing URL hostname → SAN change → rotation triggered
- **Check frequency:** Runs during ConfigMap reconciliation and periodic sync (default: 5 minutes)

**Rotation Logic Implementation:**
The WebhookReconciler checks certificates using `needsCertificateRotation()` which:
1. Parses the existing certificate from Secret
2. Calculates remaining validity: `(NotAfter - Now) / (NotAfter - NotBefore)`
3. Compares to `CERT_ROTATION_THRESHOLD` (triggers if below threshold)
4. Extracts expected SANs from current webhook configuration
5. Compares certificate SANs with expected SANs (triggers if mismatch)

**Rotation Process (leader only):**
1. Detect rotation needed (expiry approaching OR SANs changed)
2. Read current webhook ConfigMap to extract service references and URLs
3. Generate new CA + server cert with SANs matching current references
4. Write new certificates to Secret in hosting cluster
5. Parse webhook configurations and inject new CA bundle
6. Update webhook configurations in target cluster with new CA bundle

**Propagation (all replicas):**
1. Watch detects Secret change
2. Read new certificates from Secret
3. Compare content with existing files on disk
4. Skip write if content unchanged (optimization to prevent unnecessary reloads)
5. If changed: Atomic write to shared emptyDir (temp file + rename)
6. Webhook server detects file change via inotify/certwatcher
7. Webhook server hot-reloads certificates

**Configuration:**
- `CERT_VALIDITY_DAYS`: CA and server cert validity (default: 30)
- `CERT_ROTATION_THRESHOLD`: Rotation threshold as decimal 0-1 (default: 0.8)
- `CERT_DIR`: Output directory (default: /tmp/webhook-certs)

## Target Cluster Authentication

### Auto-detection Strategy

The sidecar attempts authentication in this order:

1. **Explicit kubeconfig** (if `KUBECONFIG` env var is set):
   - Load kubeconfig from specified path
   - Useful for remote cluster scenarios
   - Supports multiple contexts

2. **In-cluster config** (fallback):
   - Uses ServiceAccount token from `/var/run/secrets/kubernetes.io/serviceaccount/`
   - Works when sidecar runs in the target cluster
   - No additional configuration needed

### Required RBAC in Target Cluster

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: webhook-injector-target
rules:
  - apiGroups: ["admissionregistration.k8s.io"]
    resources: ["mutatingwebhookconfigurations", "validatingwebhookconfigurations"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
```

### Required RBAC in Hosting Cluster

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: webhook-injector
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
```

## Leader Election & Multi-Replica Coordination

Following Gardener's two-component pattern for managing certificates across replicas, implemented using controller-runtime's `LeaderElectionRunnable` interface.

### Leader Election Implementation

**Mechanism:**
- Uses controller-runtime's built-in leader election with Kubernetes Lease API
- Manager is configured with `LeaderElection: true`
- Controllers choose their election behavior via `LeaderElectionRunnable` interface:
  - **Not implementing `NeedLeaderElection()`**: Defaults to `true` → runs only on leader
  - **Implementing `NeedLeaderElection() = false`**: Runs on all replicas

**Controller Election Behavior:**
- **WebhookReconciler:** Does NOT implement `NeedLeaderElection()` → defaults to true → runs only on leader
- **CertificateReloader:** Implements `NeedLeaderElection() = false` → runs on all replicas

**Why this design:**
- Only leader generates certificates (prevents conflicts from multiple replicas generating different certificates)
- All replicas reload certificates (each pod's webhook server needs certs on its local filesystem)
- Leader election lease name: `webhook-injector-leader` (configurable via `LEADER_ELECTION_ID`)

**Why not mount Secret directly instead of using CertificateReloader?**

The CertificateReloader exists to avoid a critical timing issue during certificate rotation:

1. Leader updates Secret with new certificates
2. Leader updates webhook configurations in target cluster with new CA bundle
3. **Problem**: Kubernetes Secret volume mounts have ~60 second propagation delay (kubelet sync period)
4. **Result**: Webhook calls fail for up to 60 seconds because:
   - Target cluster has new CA bundle in webhook config
   - Webhook server still has old certificates (not yet updated by kubelet)
   - TLS verification fails (CA bundle doesn't match server cert)

The CertificateReloader solves this by:
- Using watch-based propagation (<1 second instead of ~60 seconds)
- Ensuring webhook servers have new certificates BEFORE target cluster sees new CA bundle
- Providing observability (logs show when each replica reloaded certs)

This is essential for zero-downtime certificate rotation in production environments.

### WebhookReconciler (leader-only)

**Responsibilities:**
- Watch webhook ConfigMap for changes
- Generate/rotate CA + server certificates with dynamic SANs
- Write certificates to Secret in hosting cluster
- Parse webhook configurations, inject CA bundle
- Apply webhook configurations to target cluster
- Run time-based sync every 5 minutes (configurable)
- Does NOT implement `NeedLeaderElection()` → runs only on leader by default

**Reconciliation Triggers:**
- ConfigMap create/update/delete events
- Certificate rotation (time-based and config-based)
- Periodic resync (every 5 minutes)

**Reconciliation Logic:**
```
On ConfigMap change or cert rotation or periodic sync:
  1. Read webhook ConfigMap
  2. Parse multi-document YAML
  3. Extract service references from clientConfig.service for all webhooks
  4. Extract URL hostnames from clientConfig.url for all webhooks
  5. Check if certificate needs generation/rotation:
     a. No cert exists → generate new
     b. Cert expiry approaching (remaining validity < threshold) → rotate
     c. Service references or URLs changed (different SANs needed) → regenerate
  6. If cert operation needed:
     a. Generate CA + server cert with SANs from steps 3-4
     b. Write certificates to Secret in hosting cluster
  7. For each MutatingWebhookConfiguration/ValidatingWebhookConfiguration:
     a. Inject current CA bundle into clientConfig.caBundle
     b. Apply to target cluster (create or update)
  8. Update metrics and health status
```

### CertificateReloader (all replicas)

**Responsibilities:**
- Watch certificate Secret in hosting cluster
- Read certificates from Secret
- Write to CERT_DIR (emptyDir volume) in standard format
- Runs on all replicas (including leader)
- Implements `NeedLeaderElection() = false` → bypasses leader election

**Certificate Write Process:**
```
On Secret change or startup:
  1. Read Secret from hosting cluster
  2. Extract ca.crt, tls.crt, tls.key from Secret data
  3. Compare with existing files on disk (if present)
  4. Skip write if content unchanged (optimization to prevent unnecessary reloads)
  5. If content changed or files missing:
     a. Write to temporary files in CERT_DIR
     b. Atomic rename to final names (ca.crt, tls.crt, tls.key)
     c. Webhook server detects file change and reloads
```

**Performance Optimization:**
- Uses byte-by-byte comparison (`bytes.Equal()`) before writing
- Skips file writes when certificate content is unchanged
- Prevents spurious webhook server reloads when Secret is updated but certs haven't actually changed
- Atomic writes (temp + rename) ensure webhook server never reads partial certificates

### Configuration

- `ENABLE_LEADER_ELECTION`: Enable leader election (default: true)
- `LEADER_ELECTION_ID`: Leader election resource name (default: webhook-injector-leader)
- `LEADER_ELECTION_NAMESPACE`: Leader election lease namespace (default: pod namespace)
- `WEBHOOK_CONFIG_NAME`: ConfigMap name containing webhook definitions (required)
- `WEBHOOK_CONFIG_NAMESPACE`: ConfigMap namespace (default: pod namespace)
- `CERT_SECRET_NAME`: Secret name for certificates (default: webhook-injector-certs)
- `CERT_SECRET_NAMESPACE`: Secret namespace (default: pod namespace)
- `SYNC_PERIOD`: Reconciliation sync period (default: 5m)

## Data Flow & Component Interaction

### Startup Sequence

**Leader Replica:**
1. Start leader election, become leader
2. Read webhook ConfigMap
3. Parse webhook configurations and extract service references and URL hostnames
4. Check if certificate Secret exists
   - If not: Generate CA + server cert with SANs from step 3, write to Secret
   - If yes: Validate expiry (check remaining validity vs threshold) and SANs match current webhook config
     - If expired or SANs mismatch: Regenerate with correct SANs
     - If valid: Use existing
5. Inject CA bundle into all `clientConfig.caBundle` fields
6. Connect to target cluster (via `KUBECONFIG` or in-cluster config)
7. Apply webhook configurations to target cluster
8. Start ConfigMap watch
9. Start time-based sync loop (every 5 minutes)
10. Start certificate reloader (watches Secret, writes to emptyDir only when content changes)

**Non-Leader Replicas:**
1. Participate in leader election (remain non-leader)
2. Start certificate reloader (watches Secret, writes to emptyDir only when content changes)
3. Wait for leader to generate certificates

### Runtime Operations

**On webhook config ConfigMap change (leader only):**
```
ConfigMap change detected
  → Read and parse webhooks.yaml
  → Extract service references from clientConfig.service
  → Extract URL hostnames from clientConfig.url
  → Check if SANs changed (different services or URLs)
  → If SANs changed: regenerate certificates with new SANs, write to Secret
  → Inject current CA bundle into webhooks
  → Apply to target cluster
  → Log success/failure
  → Update metrics
```

**On certificate rotation (leader only):**
```
Expiry threshold reached (remaining validity < CERT_ROTATION_THRESHOLD)
  → Read webhook ConfigMap to get current service references and URLs
  → Generate new CA + server cert with SANs from webhook config
  → Write to Secret in hosting cluster
  → Inject new CA bundle into webhook configurations
  → Apply updated webhooks to target cluster
  → Log rotation event
  → Update metrics
```

**On certificate Secret change (all replicas):**
```
Secret change detected
  → Read certificates from Secret
  → Compare with existing files on disk
  → Skip write if content unchanged (performance optimization)
  → If changed: Write ca.crt, tls.crt, tls.key to CERT_DIR (atomic)
  → Webhook server detects file change
  → Webhook server reloads certificates
```

### Error Scenarios

| Error | Handling Strategy |
|-------|------------------|
| Invalid webhook YAML in ConfigMap | Log error with details, skip invalid entries, continue with valid ones |
| Target cluster unreachable | Retry with exponential backoff (1s, 2s, 4s, ..., max 1m), mark readiness degraded, expose via metrics |
| Certificate generation failure | Log critical error with details, exit (Kubernetes restarts pod) |
| Webhook apply failure in target | Retry with backoff, continue reconciliation, expose via metrics and logs |
| ConfigMap parse error | Log error, use last known good configuration if available, mark readiness degraded |
| Secret write failure in hosting | Retry with backoff, log error, exit if persistent (restart) |
| Leader election lost | Stop reconciliation immediately, clean up, participate in new election |

## Error Handling & Observability

### Health Checks

Standard Kubernetes health endpoints:

**`/healthz` (Liveness):**
- Process is running
- No deadlocks detected

**`/readyz` (Readiness):**
- Certificates exist in CERT_DIR
- Leader only: Target cluster connectivity verified within last 2 minutes
- Leader only: Last successful reconciliation within last 15 minutes
- Leader only: Certificate Secret write successful

### Metrics

Prometheus metrics exposed on `/metrics`:

```
# Reconciliation metrics
webhook_injector_reconcile_total{result="success|failure",type="webhook|certificate"} counter
webhook_injector_reconcile_duration_seconds{type="webhook|certificate"} histogram
webhook_injector_last_reconcile_timestamp{type="webhook|certificate"} gauge

# Certificate metrics
webhook_injector_certificate_expiry_seconds gauge
webhook_injector_certificate_rotation_total counter

# Cluster connectivity
webhook_injector_target_cluster_reachable{cluster="target|hosting"} gauge

# Leader election
webhook_injector_leader_election_status gauge  # 1 if leader, 0 if not
```

### Logging

Structured JSON logging with standard levels:

**Info Level:**
- Leader election events (became leader, lost leadership)
- Reconciliation start/success
- Certificate generation/rotation events
- Webhook configuration updates
- Certificate file writes

**Error Level:**
- Failed reconciliations with retry count
- Target cluster unreachable
- Invalid webhook YAML with details
- Certificate operation failures
- Secret operation failures

**Debug Level:**
- Detailed reconciliation steps
- File operations
- Certificate validation details
- ConfigMap/Secret watch events

**Log Format:**
```json
{
  "level": "info",
  "ts": "2026-02-25T10:30:00.000Z",
  "msg": "Certificate rotated successfully",
  "component": "leader-reconciler",
  "is_leader": true,
  "cert_expiry": "2026-03-27T10:30:00Z",
  "validity_days": 30
}
```

## Configuration Reference

### Environment Variables

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `WEBHOOK_CONFIG_NAME` | ConfigMap name containing webhook definitions | - | Yes |
| `WEBHOOK_CONFIG_NAMESPACE` | ConfigMap namespace | Pod namespace | No |
| `CERT_SECRET_NAME` | Secret name for storing certificates | `webhook-injector-certs` | No |
| `CERT_SECRET_NAMESPACE` | Secret namespace | Pod namespace | No |
| `CERT_DIR` | Directory to write certificates | `/tmp/webhook-certs` | No |
| `CERT_VALIDITY_DAYS` | Certificate validity period in days | `30` | No |
| `CERT_ROTATION_THRESHOLD` | Rotation threshold as decimal (0-1) | `0.8` | No |
| `KUBECONFIG` | Path to target cluster kubeconfig | In-cluster config | No |
| `ENABLE_LEADER_ELECTION` | Enable leader election | `true` | No |
| `LEADER_ELECTION_ID` | Leader election lease name | `webhook-injector-leader` | No |
| `LEADER_ELECTION_NAMESPACE` | Leader election lease namespace | Pod namespace | No |
| `SYNC_PERIOD` | Reconciliation sync period | `5m` | No |
| `HEALTH_ADDR` | Health probe bind address | `:8081` | No |
| `METRICS_ADDR` | Metrics bind address | `:8080` | No |
| `LOG_LEVEL` | Logging level (debug/info/error) | `info` | No |

### Certificate Output Format

Files written to `CERT_DIR`:
- `ca.crt`: CA certificate (PEM encoded)
- `tls.crt`: Server certificate (PEM encoded)
- `tls.key`: Server private key (PEM encoded)

## Deployment Example

### Minimal Deployment (Same Cluster)

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: webhook-config
  namespace: default
data:
  webhooks.yaml: |
    ---
    apiVersion: admissionregistration.k8s.io/v1
    kind: MutatingWebhookConfiguration
    metadata:
      name: my-webhook
    webhooks:
      - name: mutate.example.com
        clientConfig:
          service:
            name: webhook-service
            namespace: default
            path: /mutate
            port: 9443
          caBundle: ""  # Injected by sidecar
        rules:
          - operations: ["CREATE", "UPDATE"]
            apiGroups: ["apps"]
            apiVersions: ["v1"]
            resources: ["deployments"]
        admissionReviewVersions: ["v1"]
        sideEffects: None
        failurePolicy: Fail
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: webhook-server
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
    name: webhook-server
    namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: webhook-injector-target
rules:
  - apiGroups: ["admissionregistration.k8s.io"]
    resources: ["mutatingwebhookconfigurations", "validatingwebhookconfigurations"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: webhook-injector-target
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: webhook-injector-target
subjects:
  - kind: ServiceAccount
    name: webhook-server
    namespace: default
---
apiVersion: v1
kind: Service
metadata:
  name: webhook-service
  namespace: default
spec:
  selector:
    app: webhook-server
  ports:
    - port: 9443
      targetPort: 9443
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
      serviceAccountName: webhook-server
      containers:
        - name: webhook-server
          image: my-webhook-server:latest
          ports:
            - containerPort: 9443
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
              readOnly: true
          # Webhook server watches /tmp/webhook-certs for cert changes

        - name: webhook-injector
          image: webhook-injector:latest
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
          ports:
            - name: metrics
              containerPort: 8080
            - name: health
              containerPort: 8081
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8081
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8081
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
      volumes:
        - name: webhook-certs
          emptyDir: {}
```

### Remote Cluster Deployment

For managing webhooks in a remote cluster, add kubeconfig Secret:

```yaml
---
apiVersion: v1
kind: Secret
metadata:
  name: target-cluster-kubeconfig
  namespace: default
type: Opaque
data:
  kubeconfig: <base64-encoded-kubeconfig>
---
# Update webhook-injector container in Deployment:
spec:
  template:
    spec:
      containers:
        - name: webhook-injector
          # ... existing config ...
          env:
            - name: KUBECONFIG
              value: /etc/kubeconfig/kubeconfig
            # ... other env vars ...
          volumeMounts:
            - name: kubeconfig
              mountPath: /etc/kubeconfig
              readOnly: true
            # ... other volume mounts ...
      volumes:
        - name: kubeconfig
          secret:
            secretName: target-cluster-kubeconfig
        # ... other volumes ...
```

**Note:** For remote cluster scenarios, the RBAC in the target cluster must be configured separately using the provided kubeconfig credentials.

## Implementation Notes

### Technology Stack

- **Language:** Go 1.22+
- **Framework:** controller-runtime v0.17+
- **Certificate Generation:** Go `crypto/x509` (may leverage Gardener utilities if applicable)
- **YAML Parsing:** `sigs.k8s.io/yaml` with support for multi-document streams
- **File Watching:** `fsnotify` for certificate Secret changes

### Code Structure

```
webhook-injector/
├── cmd/
│   └── webhook-injector/
│       └── main.go                    # Entry point, leader election setup
├── pkg/
│   ├── certificates/
│   │   ├── generator.go               # Certificate generation with dynamic SANs
│   │   └── types.go                   # ServiceReference type (Name, Namespace, Hostname)
│   ├── controllers/
│   │   ├── reconciler.go              # WebhookReconciler (leader-only)
│   │   ├── reconciler_test.go         # Reconciler tests
│   │   ├── reloader.go                # CertificateReloader (all replicas)
│   │   └── reloader_test.go           # Reloader tests
│   ├── webhooks/
│   │   ├── parser.go                  # Multi-doc YAML parsing, service/URL extraction
│   │   ├── parser_test.go             # Parser tests
│   │   └── injector.go                # CA bundle injection
│   ├── config/
│   │   └── config.go                  # Configuration loading
│   └── health/
│       └── checks.go                  # Health/readiness checks
├── test/
│   └── integration/
│       ├── integration_test.go        # End-to-end tests
│       └── testdata/
│           └── webhook-config.yaml    # Test webhook configurations
├── docs/
│   └── plans/
│       └── 2026-02-25-webhook-injector-design.md
├── go.mod
├── go.sum
└── README.md
```

### Potential Gardener Library Reuse

May reuse from `github.com/gardener/gardener/extensions/pkg/webhook`:
- Certificate generation utilities (if they're cleanly extractable)
- CA bundle injection helpers
- Webhook configuration building utilities

Will NOT reuse:
- In-process reconciler/reloader (designed for different architecture)
- Secrets manager integration (we use simpler Secret-based approach)

### Future Enhancements

**Out of scope for initial implementation:**

1. **Multi-cluster management:** Single sidecar managing webhooks in multiple target clusters
2. **Custom CA support:** Allow using externally provided CA instead of self-signed
3. **Certificate backends:** Support for storing certificates in external systems (Vault, etc.)
4. **Webhook validation:** Pre-apply validation of webhook configurations
5. **Namespace-scoped webhooks:** Support for namespace-scoped webhook configurations
6. **Dry-run mode:** Test webhook configurations without applying
7. **Webhook deletion:** Automatic cleanup when ConfigMap is deleted
8. **Status reporting:** Status field in ConfigMap or separate Status CRD

## Success Criteria

**The implementation is successful when:**

1. ✅ A webhook server with no knowledge of certificate management can be deployed with the sidecar
2. ✅ Certificates are automatically generated, rotated, and propagated to all replicas
3. ✅ Webhook configurations are automatically installed into target clusters
4. ✅ CA bundle is automatically updated when certificates rotate
5. ✅ Multi-replica deployments work correctly with leader election
6. ✅ The solution works for both same-cluster and remote-cluster scenarios
7. ✅ Health checks and metrics provide observability into the sidecar's operation
8. ✅ The webhook server can be written in any language/framework

## Appendix: Prior Art

This design is informed by:

1. **Gardener Extension Certificate Management** (`github.com/gardener/gardener/extensions/pkg/webhook/certificates`):
   - Two-component pattern (reconciler + reloader)
   - Secrets-based coordination between replicas
   - Leader-only certificate generation
   - All-replica certificate distribution

2. **cert-manager:** Industry-standard certificate management, but requires running in same cluster

3. **Persephone PR #204:** Migration from cert-manager to Gardener's in-process solution, demonstrating the value of integrated certificate management

## Revision History

| Date | Version | Changes |
|------|---------|---------|
| 2026-02-25 | 1.0 | Initial design |
| 2026-02-26 | 1.1 | Updated to reflect current implementation: renamed Leader Reconciler to WebhookReconciler, added URL support for webhooks, documented leader election implementation with NeedLeaderElection interface, added certificate rotation logic details, added performance optimization for unchanged certificates |
| 2026-02-26 | 1.2 | Added rationale for CertificateReloader vs direct Secret mount (timing issue during rotation: ~60s kubelet sync delay causes webhook failures when target cluster has new CA but webhook server has old certs) |
