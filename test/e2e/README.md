<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# E2E Integration Tests

End-to-end tests using two kind clusters to validate webhook-injector's
certificate rotation under continuous load.

## Prerequisites

- kind >= 0.20.0
- kubectl >= 1.28
- skaffold >= 2.10.0
- docker

## Quick Start

### Run Test (Fresh Clusters)

```bash
cd test/e2e
go test -v -timeout 5m
```

This creates kind clusters, builds and deploys with skaffold, runs the test,
and tears down the clusters automatically when done.

### Reuse Clusters for Development

For faster iteration, create clusters once and rerun tests without the
setup/teardown overhead:

```bash
cd test/e2e

# Create clusters and deploy (once)
./setup.sh
skaffold run

# Run tests repeatedly — clusters stay up between runs
go test -v -timeout 5m -test.reuse-clusters=true
go test -v -timeout 3m -test.runtime=1m -test.concurrency=10 -test.reuse-clusters=true

# Use skaffold dev for hot-reload during development
skaffold dev

# Cleanup when done
./teardown.sh
```

## Test Configuration

Available flags:

- `-test.runtime` - How long to run the load test (default: 2m)
- `-test.concurrency` - Number of concurrent clients (default: 5)
- `-test.rotation-interval` - How often to trigger cert rotation (default: 30s)
- `-test.reuse-clusters` - Skip cluster setup/teardown (default: false)

## Architecture

- **kind-hosting**: Runs webhook-server (3 replicas) + webhook-injector sidecars
- **kind-target**: Enforces webhooks, receives test ConfigMap updates
- **Test flow**: N concurrent clients continuously UPDATE dedicated ConfigMaps while
  certificate rotations are triggered at intervals
- **Pass criteria**: Zero failures during entire test duration

## What's Tested

1. Zero-downtime certificate rotation under continuous load
2. Multi-cluster communication (hosting → target)
3. Load distribution across 3 replicas
4. Webhook availability during cert rotation

## Isolated Kubeconfig

Tests use a dedicated `kubeconfig` file (gitignored) to avoid polluting your
personal kubeconfig. The file is created by `setup.sh` and removed by `teardown.sh`.
