// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// loadCABundle reads tls.crt from the hosting-cluster certificate Secret. An
// empty or missing tls.crt is returned as an error so the caller retries
// with backoff — the WebhookReconciler runs on a different reconcile loop
// and will populate the Secret shortly after startup.
//
// Used by ManagedResourceReconciler when stamping CRDs embedded in
// Gardener ManagedResource Secrets on the hosting cluster.
func loadCABundle(ctx context.Context, hostingClient client.Client, namespace, secretName string) ([]byte, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Name: secretName, Namespace: namespace}
	if err := hostingClient.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("failed to get cert Secret %s: %w", key, err)
	}
	caCert := secret.Data[tlsCrtKey]
	if len(caCert) == 0 {
		return nil, errors.New("cert Secret has empty tls.crt; retrying")
	}
	return caCert, nil
}
