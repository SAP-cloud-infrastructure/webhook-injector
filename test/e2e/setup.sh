#!/usr/bin/env bash

# Copyright 2026 SAP SE or an SAP affiliate company
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

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
# Get hosting control plane IP address for webhook URL
# The target cluster needs to reach the webhook service in hosting cluster
# Use host.docker.internal or actual Docker bridge IP
WEBHOOK_HOST="host.docker.internal"

# For Linux, host.docker.internal doesn't work, use docker bridge IP
if [[ "$OSTYPE" == "linux-gnu"* ]]; then
    WEBHOOK_HOST=$(docker network inspect kind | jq -r '.[0].IPAM.Config[0].Gateway')
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
