# webhook-injector

A Kubernetes controller that runs as a sidecar to webhook servers, managing the complete lifecycle of webhook configurations and TLS certificates for remote or local clusters.

## Features

- **Automatic Certificate Management**: Generates and rotates CA and server certificates
- **Smart Certificate Rotation**: Time-based (80% of validity) and config-based (SAN changes)
- **Remote Cluster Support**: Manage webhooks in clusters where the webhook server doesn't run
- **URL and Service Support**: Webhooks can reference services (in-cluster) or URLs (remote)
- **Generic Solution**: Works with any webhook server (any language/framework)
- **Dynamic SANs**: Generates certificates with SANs extracted from webhook service references or URLs
- **Leader Election**: Supports high-availability deployments with multiple replicas
- **Certificate Reloading**: All replicas stay synchronized with latest certificates
- **Performance Optimized**: Skips writing certificates when content unchanged to prevent unnecessary webhook server reloads

## Architecture

webhook-injector runs as a sidecar with two internal controllers:

### 1. WebhookReconciler (runs on leader only)
   - **What it does**: Manages webhook lifecycle and certificate generation
   - **Watches**:
     - Webhook ConfigMap for configuration changes
     - Certificate Secret for force-rotation annotation
   - **Generates**: CA + server certificates with dynamic SANs from service/URL references
   - **Rotates**: Certificates when approaching expiry, when SANs change, or when force-rotation annotation is set
   - **Stores**: Certificates in Secret in hosting cluster
   - **Injects**: CA bundle into webhook configurations
   - **Applies**: Webhook configurations to target cluster
   - **Why leader-only**: Prevents multiple replicas from generating conflicting certificates

### 2. CertificateReloader (runs on all replicas)
   - **What it does**: Syncs certificates from Secret to local filesystem
   - **Watches**: Certificate Secret in hosting cluster
   - **Writes**: Certificates to shared emptyDir volume (`/tmp/webhook-certs`)
   - **Optimizes**: Skips writes if certificate content unchanged
   - **Keeps**: All replicas synchronized with latest certificates
   - **Why all replicas**: Each webhook server pod needs certificates on its filesystem

### Leader Election Details

Uses controller-runtime's leader election with explicit configuration via `WithOptions`:
- **WebhookReconciler**: Uses default behavior (no WithOptions) → requires leader election
- **CertificateReloader**: Uses `WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)})` → runs on all replicas

Example with 3 replicas:
```
Replica 1 (Leader):    WebhookReconciler ✓  CertificateReloader ✓
Replica 2 (Follower):  WebhookReconciler ✗  CertificateReloader ✓
Replica 3 (Follower):  WebhookReconciler ✗  CertificateReloader ✓
```

### Remote Cluster Architecture

```
┌────────────────────────────────────────────────────────────────────────┐
│ Hosting Cluster (where webhook server runs)                            │
│                                                                        │
│  ┌──────────────────────────────────────────────────────────────────┐  │
│  │ Pod: webhook-server                                              │  │
│  │                                                                  │  │
│  │  ┌──────────────────┐      ┌──────────────────────────────────┐  │  │
│  │  │ webhook-server   │◄─────┤ webhook-injector (sidecar)       │  │  │
│  │  │                  │ certs│                                  │  │  │
│  │  │ Serves webhooks  │      │ ┌─────────────────────────────┐  │  │  │
│  │  │ on :9443         │      │ │ • Watches ConfigMap         │  │  │  │
│  │  └──────────────────┘      │ │ • Generates certs           │  │  │  │
│  │           ▲                │ │ • Updates Secret            │  │  │  │
│  │           │                │ │ • Reloads certs to disk     │  │  │  │
│  │           │                │ │ • Applies to target cluster │  │  │  │
│  │           │                │ └─────────────────────────────┘  │  │  │
│  │           │                │         ▲           │            │  │  │
│  │           │                └─────────┼───────────┼────────────┘  │  │
│  │           │  .                       │           │               │  │
│  └───────────┼──────────────────────────┼───────────┼───────────────┘  │
│              │                          │           │                  │
│              │                      watches      updates               │
│              │                          │           │                  │
│              │                          │           ▼                  │
│              │                    ┌─────┴─────┐ ┌─────────┐            │
│              │                    │ ConfigMap │ │ Secret  │            │
│              │                    │ webhook   │ │ certs   │            │
│              │                    │ configs   │ └─────────┘            │
│              │                    └───────────┘                        │
└──────────────┼─────────────────────────────────────────────────────────┘
               │                           │
               │                           │ applies webhook configs
               │                           │
               │                           ▼
               │   ┌─────────────────────────────────────────────────────┐
               │   │ Target Cluster (remote - where webhooks enforced)   │
               │   │                                                     │
               │   │  ┌─────────────────────────────────────────────┐    │
               │   │  │ MutatingWebhookConfiguration                │    │
               │   │  │ ValidatingWebhookConfig                     │    │
               │   │  │                                             │    │
               │   │  │ clientConfig:                               │    │
               │   │  │   url: https://webhook...                   │    │
               │   │  │   caBundle: <injected>                      │    │
               │   │  └─────────────────────────────────────────────┘    │
               │   │             │                                       │
               │   │             │ API Server calls webhook URL          │
               │   │             │ (validates against caBundle)          │
               │   └─────────────┼───────────────────────────────────────┘
               │                 │
               │                 │
               │                 │
               └─────────────────┘
```

## Quick Start

### Prerequisites

- Go 1.22+
- Kubernetes cluster (hosting cluster)
- kubectl configured
- (Optional) Target cluster kubeconfig if managing remote cluster

### Build

```bash
make build
```

### Sidecar Deployment

Add webhook-injector as a sidecar to your webhook server deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: webhook-server
spec:
  replicas: 3
  template:
    spec:
      containers:
        # Your webhook server
        - name: webhook-server
          image: my-webhook:latest
          ports:
            - containerPort: 9443
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
              readOnly: true

        # webhook-injector sidecar
        - name: webhook-injector
          image: webhook-injector:latest
          env:
            - name: WEBHOOK_CONFIG_NAME
              value: webhook-config
            - name: CERT_DIR
              value: /tmp/webhook-certs
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs

      volumes:
        - name: webhook-certs
          emptyDir: {}
```

**Webhook ConfigMap format:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: webhook-config
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
          service:  # or url: "https://..." for remote clusters
            name: webhook-service
            namespace: default
            path: /mutate
            port: 9443
          caBundle: ""  # Injected automatically
        rules: [...]
        admissionReviewVersions: ["v1"]
        sideEffects: None
```

## Certificate Rotation

Certificates are automatically rotated when:

1. **Time-based rotation**: When remaining validity < `CERT_ROTATION_THRESHOLD` × total validity
   - Default: Rotates at 80% of validity (24 days into a 30-day certificate)

2. **Config-based rotation**: When webhook service references or URLs change
   - Adding new webhook with different service → new SANs → rotation triggered
   - Changing service name/namespace → SANs change → rotation triggered
   - Changing URL hostname → SAN change → rotation triggered

3. **Manual rotation**: Add annotation to certificate Secret to force immediate rotation
   - Add `webhook-injector.sap.com/force-rotation: "true"` to Secret annotations
   - WebhookReconciler watches for this annotation and triggers rotation
   - Annotation is automatically removed after rotation completes

## Configuration

All configuration via environment variables:

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| `WEBHOOK_CONFIG_NAME` | ConfigMap name containing webhook definitions | - | Yes |
| `NAMESPACE` | Namespace for all resources (ConfigMap, Secret, Lease). Auto-detects from pod's serviceaccount if not set | `default` | No |
| `CERT_SECRET_NAME` | Secret name for storing certificates | `webhook-injector-certs` | No |
| `CERT_DIR` | Directory to write certificates | `/tmp/webhook-certs` | No |
| `CERT_VALIDITY` | Certificate validity as duration (e.g., "720h", "30d", "1y") | `720h` (30 days) | No |
| `CERT_ROTATION_THRESHOLD` | Rotation threshold as decimal (0-1) | `0.8` | No |
| `TARGET_KUBECONFIG` | Path to target cluster kubeconfig | In-cluster config | No |
| `ENABLE_LEADER_ELECTION` | Enable leader election | `true` | No |
| `LEADER_ELECTION_ID` | Leader election lease name | `webhook-injector-leader` | No |
| `SYNC_PERIOD` | Reconciliation sync period | `5m` | No |
| `METRICS_ADDR` | Metrics bind address | `:8080` | No |
| `HEALTH_ADDR` | Health probe bind address | `:8081` | No |
| `LOG_LEVEL` | Logging level (debug/info/error) | `info` | No |

## Certificate Output

Files written to `CERT_DIR`:
- `ca.crt`: CA certificate (PEM)
- `tls.crt`: Server certificate with dynamic SANs (PEM)
- `tls.key`: Server private key (PEM)

**Certificate SANs include:**
- For service-based webhooks: `<service>.<namespace>.svc` and `<service>.<namespace>.svc.cluster.local`
- For URL-based webhooks: hostname extracted from URL (e.g., `webhook.example.com`)

**Performance optimization:** Files are only written when content changes, preventing unnecessary webhook server reloads.

## Webhook Server Certificate Reloading

For controller-runtime based webhook servers (v0.17+), certificate reloading is **automatic**. Simply configure the webhook server with the certificate directory:

```go
import (
    ctrl "sigs.k8s.io/controller-runtime"
    "sigs.k8s.io/controller-runtime/pkg/webhook"
)

func main() {
    mgr, _ := ctrl.NewManager(cfg, ctrl.Options{
        WebhookServer: webhook.NewServer(webhook.Options{
            Port:     9443,
            CertDir:  "/tmp/webhook-certs",
            CertName: "tls.crt",
            KeyName:  "tls.key",
        }),
    })

    // Register webhooks...
    mgr.GetWebhookServer().Register("/mutate", &webhook.Admission{...})

    mgr.Start(ctx)
}
```

**That's it!** The webhook server automatically:
- Creates an internal `certwatcher` to monitor certificate files using fsnotify
- Hot-reloads certificates when `tls.crt` or `tls.key` change
- Provides zero-downtime certificate rotation
- No manual setup or code required

## Testing

### Unit Tests

```bash
gmake test
```

### E2E Integration Tests

End-to-end tests use two kind clusters to validate multi-cluster, multi-replica behavior:

**Prerequisites:**
- [kind](https://kind.sigs.k8s.io/) >= 0.20.0 - `brew install kind`
- [kubectl](https://kubernetes.io/docs/tasks/tools/) >= 1.28 - `brew install kubectl`
- [skaffold](https://skaffold.dev/) >= 2.10.0 - `brew install skaffold`
- [docker](https://www.docker.com/) - Docker Desktop or Docker Engine running

**Automated E2E test:**
```bash
gmake test-e2e
```

**Manual development workflow:**
```bash
gmake e2e-setup      # Create kind clusters (hosting + target)
gmake e2e-dev        # Build images with ko, deploy with Skaffold (hot reload)

# Test webhook manually
kubectl --context kind-target create configmap test-1 --from-literal=key=value

# Watch logs
kubectl --context kind-hosting logs -l app=webhook-server -f

# Cleanup
gmake e2e-teardown
```

**What's tested:**
- **Multi-cluster architecture**: Hosting cluster (webhook server runs) and target cluster (webhooks enforced)
- **Cross-cluster networking**: Target API server calls webhook in hosting cluster via NodePort + host.docker.internal
- **High availability**: 3 replicas with leader election - leader generates certs, all replicas reload certs
- **Load balancing**: Kubernetes Service distributes webhook requests across all 3 replicas
- **Zero-downtime rotation**: Webhook requests continue succeeding during certificate regeneration and reload
- **Certificate hot reload**: All replicas independently detect cert changes and reload without restart
- **Leader election verification**: Only leader generates certificates, but all replicas serve webhook requests

**Test infrastructure:**
- Uses [ko](https://ko.build/) via Skaffold for fast Go binary builds without Dockerfiles
- Two kind clusters: `kind-hosting` (2 nodes) and `kind-target` (1 node)
- NodePort 30443 with kind extraPortMappings for cross-cluster access
- Test webhook validates ConfigMap creations in target cluster
- Metrics endpoint tracks request counts and certificate serial numbers per replica

## Development

### Linting

```bash
make fmt
make vet
```

### Build Docker Image

```bash
make docker-build
```

## Deployment

See `config/` directory for Kubernetes manifests:
- `rbac.yaml` - ServiceAccount, Role, ClusterRole, RoleBindings
- `deployment.yaml` - Deployment with sidecar pattern, ConfigMap example, Service
- `pdb.yaml` - PodDisruptionBudget for HA
- `kustomization.yaml` - Kustomize configuration

Deploy with:
```bash
kubectl apply -k config/
```

## License

Apache 2.0
