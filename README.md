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
   - **Watches**: Webhook ConfigMap for changes
   - **Generates**: CA + server certificates with dynamic SANs from service/URL references
   - **Rotates**: Certificates when approaching expiry or when SANs change
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

Uses controller-runtime's `LeaderElectionRunnable` interface:
- **WebhookReconciler**: Does NOT implement `NeedLeaderElection()` → defaults to requiring leader election
- **CertificateReloader**: Implements `NeedLeaderElection() = false` → runs on all replicas

Example with 3 replicas:
```
Replica 1 (Leader):    WebhookReconciler ✓  CertificateReloader ✓
Replica 2 (Follower):  WebhookReconciler ✗  CertificateReloader ✓
Replica 3 (Follower):  WebhookReconciler ✗  CertificateReloader ✓
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

### Run Locally

1. Create webhook ConfigMap:

```bash
kubectl apply -f - <<EOF
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
        # Option 1: Service reference (in-cluster webhook)
        clientConfig:
          service:
            name: webhook-service
            namespace: default
            path: /mutate
            port: 9443
          caBundle: ""  # Automatically injected
        rules:
          - operations: ["CREATE", "UPDATE"]
            apiGroups: ["apps"]
            apiVersions: ["v1"]
            resources: ["deployments"]
        admissionReviewVersions: ["v1"]
        sideEffects: None
        failurePolicy: Fail
    ---
    apiVersion: admissionregistration.k8s.io/v1
    kind: ValidatingWebhookConfiguration
    metadata:
      name: remote-webhook
    webhooks:
      - name: validate.example.com
        # Option 2: URL reference (remote cluster webhook)
        clientConfig:
          url: "https://webhook.remote-cluster.com:9443/validate"
          caBundle: ""  # Automatically injected
        rules:
          - operations: ["CREATE"]
            apiGroups: [""]
            apiVersions: ["v1"]
            resources: ["pods"]
        admissionReviewVersions: ["v1"]
        sideEffects: None
EOF
```

2. Run webhook-injector:

```bash
export WEBHOOK_CONFIG_NAME=webhook-config
export WEBHOOK_CONFIG_NAMESPACE=default
./bin/webhook-injector
```

## Certificate Rotation

Certificates are automatically rotated when:

1. **Time-based rotation**: When remaining validity < `CERT_ROTATION_THRESHOLD` × total validity
   - Default: Rotates at 80% of validity (24 days into a 30-day certificate)

2. **Config-based rotation**: When webhook service references or URLs change
   - Adding new webhook with different service → new SANs → rotation triggered
   - Changing service name/namespace → SANs change → rotation triggered
   - Changing URL hostname → SAN change → rotation triggered

## Configuration

All configuration via environment variables:

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

## Development

### Run Tests

```bash
# Unit tests
make test

# Integration tests (requires envtest)
make test-integration
```

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
