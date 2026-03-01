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

# Use dedicated kubeconfig for e2e tests
KUBECONFIG="$(pwd)/kubeconfig"
export KUBECONFIG

echo "==> Deleting kind clusters..."

if kind get clusters | grep -q "^hosting$"; then
    echo "Deleting hosting cluster..."
    kind delete cluster --name hosting
fi

if kind get clusters | grep -q "^target$"; then
    echo "Deleting target cluster..."
    kind delete cluster --name target
fi

# Clean up temporary files
rm -f webhook-endpoint.txt

echo "==> Removing dedicated kubeconfig..."
rm -f kubeconfig

echo "==> Teardown complete!"
