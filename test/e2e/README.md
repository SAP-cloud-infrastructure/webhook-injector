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
