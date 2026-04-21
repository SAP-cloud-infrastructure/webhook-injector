#!/usr/bin/env bash

# Copyright 2026 SAP SE or an SAP affiliate company
# SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

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
