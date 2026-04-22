#!/usr/bin/env bash

# Copyright 2026 SAP SE or an SAP affiliate company
# SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# Use dedicated kubeconfig for e2e tests to avoid polluting user's config
KUBECONFIG="$(pwd)/kubeconfig"
export KUBECONFIG

if [ ! -f "$KUBECONFIG" ]; then
    echo "==> Creating dedicated kubeconfig at $KUBECONFIG..."
    cat > "$KUBECONFIG" <<EOF
apiVersion: v1
kind: Config
clusters: []
contexts: []
current-context: ""
users: []
EOF
else
    echo "==> Using existing kubeconfig at $KUBECONFIG"
fi

echo "==> Checking prerequisites..."
for cmd in kind kubectl skaffold docker; do
    if ! command -v "$cmd" &> /dev/null; then
        echo "Error: $cmd is not installed"
        exit 1
    fi
done

echo "==> Creating hosting cluster..."
if kind get clusters | grep -q "^hosting$"; then
    echo "Hosting cluster already exists, skipping creation"
else
    kind create cluster --config kind-hosting.yaml
fi

echo "==> Creating target cluster..."
if kind get clusters | grep -q "^target$"; then
    echo "Target cluster already exists, skipping creation"
else
    kind create cluster --config kind-target.yaml
fi

echo "==> Waiting for clusters to be ready..."
kubectl --context kind-hosting wait --for=condition=Ready nodes --all --timeout=120s
kubectl --context kind-target wait --for=condition=Ready nodes --all --timeout=120s

echo "==> Creating default namespace in hosting cluster..."
kubectl --context kind-hosting create namespace default --dry-run=client -o yaml | \
    kubectl --context kind-hosting apply -f -

echo "==> Exporting target cluster kubeconfig as Secret..."
# Get target cluster kubeconfig and discover reachable address
TARGET_KUBECONFIG=$(mktemp)
kind get kubeconfig --name target > "$TARGET_KUBECONFIG"

# Replace internal server address with address reachable from hosting cluster
# In Docker Desktop, kind clusters can reach each other via container names
# Get target cluster's API server address
TARGET_API_SERVER="https://target-control-plane:6443"

# Update kubeconfig to use container-to-container address
kubectl --kubeconfig="$TARGET_KUBECONFIG" config set-cluster kind-target \
    --server="$TARGET_API_SERVER"

# Create Secret in hosting cluster with target kubeconfig
kubectl --context kind-hosting create secret generic target-kubeconfig \
    --namespace=default \
    --from-file=kubeconfig="$TARGET_KUBECONFIG" \
    --dry-run=client -o yaml | \
    kubectl --context kind-hosting apply --namespace=default -f -

rm "$TARGET_KUBECONFIG"

echo "==> Discovering webhook endpoint address..."
# Get hosting control plane IP address for webhook URL.
# The target cluster's API server runs as a Docker container, so it must reach
# the NodePort (30443) on the hosting control-plane container via Docker networking.
# Use host.docker.internal or actual hosting-control-plane container IP.
WEBHOOK_HOST="host.docker.internal"

# For Linux, host.docker.internal doesn't work. Use the hosting control-plane
# container IP — reachable from all containers on the same Docker network.
# NOTE: the hosting-control-plane container only exists after cluster creation above.
if [[ "$OSTYPE" == "linux-gnu"* ]]; then
    WEBHOOK_HOST=$(docker inspect hosting-control-plane \
        --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
    if [[ -z "$WEBHOOK_HOST" || "$WEBHOOK_HOST" == "null" ]]; then
        echo "ERROR: Could not determine IP of hosting-control-plane container."
        echo "       'docker inspect hosting-control-plane' output:"
        docker inspect hosting-control-plane || true
        exit 1
    fi
fi

export WEBHOOK_ENDPOINT="https://${WEBHOOK_HOST}:30443"
echo "Webhook endpoint: $WEBHOOK_ENDPOINT"

# Write webhook endpoint to file for skaffold to use
echo "$WEBHOOK_ENDPOINT" > webhook-endpoint.txt

# Pre-render the webhook config with the endpoint
echo "==> Generating webhook configuration..."
envsubst < manifests/webhook-config.yaml > manifests/webhook-config-rendered.yaml

echo ""
echo "==> Setup complete!"
echo ""
echo "Clusters:"
echo "  - kind-hosting (context: kind-hosting)"
echo "  - kind-target (context: kind-target)"
echo ""
echo "Next steps:"
echo "  1. Deploy with: skaffold dev"
echo "  2. Test with: kubectl --context kind-target create configmap test-1 --from-literal=key=value"
echo "  3. Cleanup with: ./teardown.sh"
