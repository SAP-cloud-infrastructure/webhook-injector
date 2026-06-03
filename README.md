<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# webhook-injector

A Kubernetes controller that runs as a sidecar to webhook servers, managing webhook configurations and TLS certificates for remote or local clusters.

## Features

- **Automatic Certificate Management**: Generates and rotates self-signed certificates with dynamic SANs
- **Smart Rotation**: Time-based (20% remaining validity threshold), config-based (SAN changes), or manual (annotation)
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
- Kubernetes 1.29+ (for native sidecar support)
- kubectl configured
- (Optional) Target cluster kubeconfig if managing remote cluster
- **Webhook server requirement**: Must watch certificate files and reload on changes (controller-runtime v0.17+ does this automatically)

### Build

```bash
make build
```

### Sidecar Deployment

webhook-injector is deployed as a [native sidecar](https://kubernetes.io/docs/concepts/workloads/pods/sidecar-containers/) (Kubernetes 1.29+). Native sidecars are init containers with `restartPolicy: Always` — they start before and terminate after the main containers, which guarantees certificates are written to disk before the webhook server starts.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: webhook-server
spec:
  replicas: 3
  template:
    spec:
      initContainers:
        - name: webhook-injector
          image: webhook-injector:latest
          restartPolicy: Always
          args:
            - --webhook-config-name=webhook-config
          startupProbe:
            httpGet:
              path: /readyz
              port: 8083
            initialDelaySeconds: 5
            periodSeconds: 5
            failureThreshold: 6
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
      containers:
        - name: webhook-server
          image: my-webhook:latest
          volumeMounts:
            - name: webhook-certs
              mountPath: /tmp/webhook-certs
              readOnly: true
      volumes:
        - name: webhook-certs
          emptyDir: {}
```

> **Kubernetes < 1.29:** If you need to support older clusters, place `webhook-injector` in `containers` instead of `initContainers` and remove the `restartPolicy` field. The tradeoff is that the webhook server may start before certificates are available, so its `initialDelaySeconds` must account for certificate generation time.

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
1. Time-based: When remaining validity < threshold × total (default: 20%, i.e., 18 days of a 90-day cert)
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
| `--cert-validity` | Certificate lifetime (e.g., "2160h", "90d") | `2160h` |
| `--cert-rotation-threshold` | Rotation threshold (0-1) | `0.2` |
| `--target-kubeconfig` | Target cluster kubeconfig path | in-cluster |
| `--enable-leader-election` | Enable HA mode | `true` |
| `--leader-election-id` | Lease name | `webhook-injector-leader` |
| `--sync-period` | Reconciliation interval | `5m` |
| `--external-host` | Patch-mode only (required there): `host:port` to substitute into labeled MWC/VWC/CRD `clientConfig` (Service form is rewritten to `https://<host>:<port><Service.Path>`; URL-form entries are left untouched) | unset |
| `--admission-webhook-name` | Patch-mode only: name of the `MutatingWebhookConfiguration` the controller bootstraps in the target cluster to register itself as an admission mutator on labeled MWC/VWC/CRD. Empty disables bootstrap. | `webhook-injector-mutator` |
| `--admission-webhook-port` | Patch-mode only: port the in-process admission webhook server binds to. The target cluster reaches it via `--external-host`'s port; configure your Service / NodePort / LoadBalancer to forward to this port. | `9444` |
| `--metrics-addr` | Metrics bind address | `:8082` |
| `--health-addr` | Health probe address | `:8083` |
| `--log-level` | Log level (debug/info/error) | `info` |

**Example:**
```bash
webhook-injector --webhook-config-name=my-config
```

**Patch mode with external endpoint** (webhook server is reached at an external `host:port` from the target cluster — e.g., NodePort or LoadBalancer). `--external-host` is required in patch mode:

```bash
webhook-injector \
  --webhook-label=webhook-injector/name=cert-manager \
  --cert-sans=webhook.example.com \
  --external-host=webhook.example.com:9443
```

`--external-host` rewrites every labeled `MutatingWebhookConfiguration`, `ValidatingWebhookConfiguration`, and `CustomResourceDefinition` conversion-webhook entry whose `clientConfig.Service` is set: the Service is replaced with `clientConfig.URL = https://<external-host><Service.Path>` (Service.Path nil/empty → `/`). Entries that already use URL-form `clientConfig` are left untouched. `caBundle` is patched as usual. The flag does not affect SAN generation — `--cert-sans` is still the sole SAN source in patch mode.

In patch mode the controller also bootstraps a `MutatingWebhookConfiguration` in the target cluster (named via `--admission-webhook-name`) that registers webhook-injector itself as an admission mutator on labeled MWC/VWC/CRD resources. CREATE/UPDATE on those resources is then mutated synchronously — `caBundle` is stamped and Service-form `clientConfig` is rewritten to URL form — so the desired state is in place at admission time, not after the next reconcile. The bootstrapped MWC is unlabeled and the handler also short-circuits any object whose name equals `--admission-webhook-name`, so it cannot match its own `objectSelector` and never triggers self-admission. `failurePolicy: Ignore` keeps target-cluster updates working when webhook-injector is down — the periodic patch loop is the safety net for drift in that window. The webhook server inside the controller pod listens on `--admission-webhook-port` (default `9444`); the target cluster reaches it via `--external-host`'s port, so configure your Service / NodePort / LoadBalancer accordingly.

## Metrics

webhook-injector exposes Prometheus metrics at `<metrics-addr>/metrics` (default `:8082`).

| Metric | Type | Description |
|---|---|---|
| `webhook_injector_certificate_rotations_total` | Counter | Certificate rotations by `reason` label: `initial`, `expired`, `san_mismatch`, `force_rotation` |
| `webhook_injector_certificate_expiry_time_seconds` | Gauge | Unix timestamp when the current TLS certificate expires |
| `webhook_injector_cert_files_written_total` | Counter | Times certificate files were written to disk by the reloader |

Standard controller-runtime metrics (reconcile latency, queue depth, etc.) are also included.

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
