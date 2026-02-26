# webhook-injector

A Kubernetes controller that runs as a sidecar to webhook servers, managing the complete lifecycle of webhook configurations and TLS certificates for remote or local clusters.

## Features

- **Automatic Certificate Management**: Generates and rotates CA and server certificates
- **Remote Cluster Support**: Manage webhooks in clusters where the webhook server doesn't run
- **Generic Solution**: Works with any webhook server (any language/framework)
- **Dynamic SANs**: Generates certificates with SANs extracted from webhook service references
- **Leader Election**: Supports high-availability deployments with multiple replicas
- **Certificate Reloading**: All replicas stay synchronized with latest certificates

## Architecture

webhook-injector runs as a sidecar with two internal controllers:

1. **Leader Reconciler** (leader-only):
   - Watches webhook ConfigMap for changes
   - Generates/rotates CA + server certificates with dynamic SANs
   - Writes certificates to Secret in hosting cluster
   - Injects CA bundle into webhook configurations
   - Applies webhook configurations to target cluster

2. **Certificate Reloader** (all replicas):
   - Watches certificate Secret in hosting cluster
   - Writes certificates to shared emptyDir volume
   - All replicas stay synchronized

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
        clientConfig:
          service:
            name: webhook-service
            namespace: default
            path: /mutate
            port: 9443
          caBundle: ""
        rules:
          - operations: ["CREATE", "UPDATE"]
            apiGroups: ["apps"]
            apiVersions: ["v1"]
            resources: ["deployments"]
        admissionReviewVersions: ["v1"]
        sideEffects: None
        failurePolicy: Fail
EOF
```

2. Run webhook-injector:

```bash
export WEBHOOK_CONFIG_NAME=webhook-config
export WEBHOOK_CONFIG_NAMESPACE=default
./bin/webhook-injector
```

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
- `tls.crt`: Server certificate (PEM)
- `tls.key`: Server private key (PEM)

## Development

### Run Tests

```bash
make test
```

### Linting

```bash
make fmt
make vet
```

## License

Apache 2.0
