// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 SAP SE or an SAP affiliate company
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
)

// CertificateReloader watches the certificate Secret and writes certs to disk
type CertificateReloader struct {
	Client client.Client
	Config *config.Config
}

// NeedLeaderElection implements manager.LeaderElectionRunnable
// Returns false because certificate reloading should happen on ALL replicas,
// not just the leader. Each replica needs to sync certificates to its own filesystem.
func (r *CertificateReloader) NeedLeaderElection() bool {
	return false
}

// Reconcile reads certificates from Secret and writes to disk
func (r *CertificateReloader) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Secret
	secret := &corev1.Secret{}
	err := r.Client.Get(ctx, req.NamespacedName, secret)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reloading certificates from Secret", "secret", req.NamespacedName)

	// Write certificates to disk
	certFiles := map[string]string{
		"ca.crt":  "ca.crt",
		"tls.crt": "tls.crt",
		"tls.key": "tls.key",
	}

	for secretKey, fileName := range certFiles {
		data, ok := secret.Data[secretKey]
		if !ok {
			logger.Error(errors.New("missing key in secret"), "Missing certificate data", "key", secretKey)
			continue
		}

		if err := r.writeCertFile(fileName, data); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to write %s: %w", fileName, err)
		}
	}

	logger.Info("Certificates reloaded successfully")
	return ctrl.Result{}, nil
}

// writeCertFile writes certificate data to disk atomically (temp file + rename)
// Optimization: Only writes if content has changed to avoid unnecessary webhook server reloads
func (r *CertificateReloader) writeCertFile(fileName string, data []byte) error {
	targetPath := filepath.Join(r.Config.CertDir, fileName)

	// Check if file exists and compare content
	existingData, err := os.ReadFile(targetPath)
	if err == nil {
		// File exists, compare content
		if bytes.Equal(existingData, data) {
			// Content unchanged, skip write to avoid unnecessary file modification
			// This prevents triggering webhook server reloads when certs haven't changed
			return nil
		}
	}
	// If file doesn't exist (err != nil) or content differs, proceed with write

	tempPath := targetPath + ".tmp"

	// Write to temp file
	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, targetPath); err != nil {
		os.Remove(tempPath) // Clean up temp file on error
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *CertificateReloader) SetupWithManager(mgr ctrl.Manager) error {
	// Only watch the specific certificate Secret
	secretPredicate := predicate.NewPredicateFuncs(func(object client.Object) bool {
		return object.GetName() == r.Config.CertSecretName &&
			object.GetNamespace() == r.getNamespace()
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		WithEventFilter(secretPredicate).
		WithOptions(controller.Options{
			NeedLeaderElection: ptr.To(false),
		}).
		Complete(r)
}

// getNamespace returns the configured namespace or pod namespace
func (r *CertificateReloader) getNamespace() string {
	if r.Config.CertSecretNamespace != "" {
		return r.Config.CertSecretNamespace
	}
	// Try to read from pod
	if ns, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return string(ns)
	}
	return "default"
}
