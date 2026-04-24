<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# webhook-injector

A Kubernetes controller that runs as a sidecar to webhook servers, managing webhook configurations and TLS certificates for remote or local clusters.

## Features

- **Automatic Certificate Management**: Generates and rotates self-signed certificates with dynamic SANs
- **Smart Rotation**: Time-based (80% validity threshold), config-based (SAN changes), or manual (annotation)
- **Remote Cluster Support**: Manage webhooks in clusters where the webhook server doesn't run
- **Language Agnostic**: Works with any webhook server that can watch certificate files for changes
- **High Availability**: Leader election with multi-replica support, zero-downtime rotation

## Architecture

webhook-injector runs as a sidecar with two controllers:

**WebhookReconciler** (leader only): Manages webhook lifecycle and certificate generation. Watches ConfigMap and Secret, generates self-signed certificates with dynamic SANs, injects CA bundle, and applies webhook configurations to target cluster.

**CertificateReloader** (all replicas): Syncs certificates from Secret to local filesystem (`/tmp/webhook-certs`). Optimizes by skipping writes when content unchanged. Ensures all replicas stay synchronized.

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
│  │           │                          │           │               │  │
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

- Go 1.26+
- Kubernetes cluster (hosting cluster)
- kubectl configured
- (Optional) Target cluster kubeconfig if managing remote cluster
- **Webhook server requirement**: Must watch certificate files and reload on changes (controller-runtime v0.17+ does this automatically)

### Build

```bash
make build
```

### Sidecar Deployment

Add webhook-injector as a sidecar container with shared emptyDir volume:

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
        - name: webhook-server
          image: my-webhook:latest
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
              readOnly: true
        - name: webhook-injector
          image: webhook-injector:latest
          args:
            - --webhook-config-name=webhook-config
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
      volumes:
        - name: webhook-certs
          emptyDir: {}
```

Create a ConfigMap with webhook definitions (standard Kubernetes webhook YAML):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: webhook-config
data:
  webhooks.yaml: |
    apiVersion: admissionregistration.k8s.io/v1
    kind: MutatingWebhookConfiguration
    metadata:
      name: my-webhook
    webhooks:
      - name: mutate.example.com
        clientConfig:
          service:              # or url: for remote clusters
            name: webhook-service
            namespace: default
            path: /mutate
          caBundle: ""          # Injected automatically
        # ... standard webhook config
```

**Note:** Your webhook server must watch the certificate directory and reload certificates when `tls.crt` or `tls.key` change. Controller-runtime webhook servers (v0.17+) provide this functionality automatically via an internal `certwatcher`. For other frameworks, implement file watching using inotify/fsnotify or periodic polling.

## Certificate Management

**Rotation triggers:**
1. Time-based: When remaining validity < threshold × total (default: 80% of 30 days)
2. Config-based: When webhook service references or URLs change (new SANs required)
3. Manual: Annotate Secret with `webhook-injector.sap.com/force-rotation: "true"`

**Certificate output** (`/tmp/webhook-certs`):
- `tls.crt`: Self-signed certificate with SANs from service/URL references
- `tls.key`: Private key

The same certificate serves as both server cert and CA bundle. Your webhook server must reload these files when they change - controller-runtime webhook servers (v0.17+) handle this automatically via fsnotify.

## Configuration

Configure via command-line flags. Only `--webhook-config-name` is required; namespace auto-detects from flag → `NAMESPACE` env var → serviceaccount file.

| Flag | Description | Default |
|------|-------------|---------|
| `--webhook-config-name` | ConfigMap containing webhook definitions | *required* |
| `--namespace` | Namespace for ConfigMap/Secret/Lease | auto-detect |
| `--cert-secret-name` | Secret for certificates | `webhook-injector-certs` |
| `--cert-dir` | Certificate output directory | `/tmp/webhook-certs` |
| `--cert-validity` | Certificate lifetime (e.g., "720h", "30d") | `720h` |
| `--cert-rotation-threshold` | Rotation threshold (0-1) | `0.8` |
| `--target-kubeconfig` | Target cluster kubeconfig path | in-cluster |
| `--enable-leader-election` | Enable HA mode | `true` |
| `--leader-election-id` | Lease name | `webhook-injector-leader` |
| `--sync-period` | Reconciliation interval | `5m` |
| `--metrics-addr` | Metrics bind address | `:8082` |
| `--health-addr` | Health probe address | `:8083` |
| `--log-level` | Log level (debug/info/error) | `info` |

**Example:**
```bash
webhook-injector --webhook-config-name=my-config
```

## Testing

```bash
# Unit tests
gmake test

# E2E integration tests (creates kind clusters, tests multi-cluster setup)
gmake test-e2e

# Manual E2E workflow
gmake e2e-setup      # Create hosting + target kind clusters
gmake e2e-dev        # Skaffold dev mode with hot reload
gmake e2e-teardown   # Cleanup
```

E2E tests validate multi-cluster architecture, HA with 3 replicas, leader election, load balancing, and zero-downtime certificate rotation under continuous load. See [test/e2e/README.md](test/e2e/README.md) for details.

## Development

```bash
make build           # Build binary
make check           # Run all checks (MUST pass before commit)
make static-check    # Linters only
golangci-lint run --fix  # Auto-fix lint issues

# Docker
make docker-build

# Deployment
kubectl apply -k config/  # Deploy from config/ manifests
```

**Note:** Use `gmake` on macOS (built-in make is too old). All Go files must have Apache-2.0 license headers (auto-added by addlicense).

## License

Apache 2.0
