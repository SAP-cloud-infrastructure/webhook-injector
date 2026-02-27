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

echo "==> Pod status:"
kubectl --context kind-hosting --namespace default get pods -l app=webhook-server

echo ""
echo "==> Checking if cert Secret exists:"
kubectl --context kind-hosting --namespace default get secret webhook-injector-certs -o yaml 2>&1 || echo "Secret not found"

echo ""
echo "==> ConfigMap content:"
kubectl --context kind-hosting --namespace default get configmap webhook-config -o yaml

echo ""
echo "==> webhook-injector logs:"
POD=$(kubectl --context kind-hosting --namespace default get pods -l app=webhook-server -o jsonpath='{.items[0].metadata.name}')
kubectl --context kind-hosting --namespace default logs "$POD" -c webhook-injector --tail=50

echo ""
echo "==> webhook-server logs:"
kubectl --context kind-hosting --namespace default logs "$POD" -c webhook-server --tail=50

echo ""
echo "==> Checking files in webhook-server container:"
kubectl --context kind-hosting --namespace default exec "$POD" -c webhook-server -- ls -la /tmp/webhook-certs/ 2>&1 || echo "Failed to list files"
