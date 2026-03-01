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
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
)

func TestCertificateReloader(t *testing.T) {
	// Create temp directory for certificates
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Create test secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"ca.crt":  []byte("test-ca-cert"),
			"tls.crt": []byte("test-server-cert"),
			"tls.key": []byte("test-server-key"),
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(secret).
		Build()

	cfg := &config.Config{
		CertSecretName: "test-certs",
		Namespace:      "default",
		CertDir:        tmpDir,
	}

	reloader := &CertificateReloader{
		Client: client,
		Config: cfg,
	}

	// Reconcile
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-certs",
			Namespace: "default",
		},
	}

	_, err := reloader.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify files were written
	files := []string{"ca.crt", "tls.crt", "tls.key"}
	for _, file := range files {
		path := filepath.Join(tmpDir, file)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("Expected file %s to exist", path)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Failed to read file %s: %v", path, err)
		}

		expectedContent := secret.Data[file]
		if string(content) != string(expectedContent) {
			t.Errorf("File %s: expected content '%s', got '%s'", file, expectedContent, content)
		}
	}
}

func TestCertificateReloader_SkipsUnchangedCertificates(t *testing.T) {
	// Create temp directory for certificates
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Pre-populate files with existing certificate data
	existingCerts := map[string][]byte{
		"ca.crt":  []byte("existing-ca-cert"),
		"tls.crt": []byte("existing-server-cert"),
		"tls.key": []byte("existing-server-key"),
	}

	for file, content := range existingCerts {
		path := filepath.Join(tmpDir, file)
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatalf("Failed to write initial file %s: %v", file, err)
		}
	}

	// Get initial mtimes
	initialMtimes := make(map[string]int64)
	for file := range existingCerts {
		path := filepath.Join(tmpDir, file)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Failed to stat file %s: %v", file, err)
		}
		initialMtimes[file] = info.ModTime().UnixNano()
	}

	// Create secret with SAME content (no changes)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Data: existingCerts, // Same content as existing files
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(secret).
		Build()

	cfg := &config.Config{
		CertSecretName: "test-certs",
		Namespace:      "default",
		CertDir:        tmpDir,
	}

	reloader := &CertificateReloader{
		Client: client,
		Config: cfg,
	}

	// Sleep briefly to ensure mtime would differ if file was rewritten
	// (some filesystems have 1ms precision)
	// time.Sleep(10 * time.Millisecond)

	// Reconcile
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-certs",
			Namespace: "default",
		},
	}

	_, err := reloader.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify files were NOT rewritten (mtimes unchanged)
	for file := range existingCerts {
		path := filepath.Join(tmpDir, file)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Failed to stat file %s: %v", file, err)
		}

		newMtime := info.ModTime().UnixNano()
		if newMtime != initialMtimes[file] {
			t.Errorf("File %s was rewritten unnecessarily (mtime changed from %d to %d)",
				file, initialMtimes[file], newMtime)
		}

		// Also verify content is still correct
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Failed to read file %s: %v", file, err)
		}
		if string(content) != string(existingCerts[file]) {
			t.Errorf("File %s: content changed unexpectedly", file)
		}
	}
}

func TestCertificateReloader_UpdatesChangedCertificates(t *testing.T) {
	// Create temp directory for certificates
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Pre-populate files with OLD certificate data
	oldCerts := map[string][]byte{
		"ca.crt":  []byte("old-ca-cert"),
		"tls.crt": []byte("old-server-cert"),
		"tls.key": []byte("old-server-key"),
	}

	for file, content := range oldCerts {
		path := filepath.Join(tmpDir, file)
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatalf("Failed to write initial file %s: %v", file, err)
		}
	}

	// Create secret with NEW content (changed)
	newCerts := map[string][]byte{
		"ca.crt":  []byte("new-ca-cert"),
		"tls.crt": []byte("new-server-cert"),
		"tls.key": []byte("new-server-key"),
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Data: newCerts, // Different content
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(secret).
		Build()

	cfg := &config.Config{
		CertSecretName: "test-certs",
		Namespace:      "default",
		CertDir:        tmpDir,
	}

	reloader := &CertificateReloader{
		Client: client,
		Config: cfg,
	}

	// Reconcile
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-certs",
			Namespace: "default",
		},
	}

	_, err := reloader.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify files were updated with new content
	for file, expectedContent := range newCerts {
		path := filepath.Join(tmpDir, file)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Failed to read file %s: %v", file, err)
		}

		if string(content) != string(expectedContent) {
			t.Errorf("File %s: expected updated content '%s', got '%s'",
				file, expectedContent, content)
		}
	}
}

func TestCertificateReloader_UpdatesOnlyChangedFiles(t *testing.T) {
	// Create temp directory for certificates
	tmpDir := t.TempDir()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Pre-populate files
	existingCerts := map[string][]byte{
		"ca.crt":  []byte("unchanged-ca-cert"),
		"tls.crt": []byte("old-server-cert"),
		"tls.key": []byte("unchanged-server-key"),
	}

	for file, content := range existingCerts {
		path := filepath.Join(tmpDir, file)
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatalf("Failed to write initial file %s: %v", file, err)
		}
	}

	// Get initial mtimes
	initialMtimes := make(map[string]int64)
	for file := range existingCerts {
		path := filepath.Join(tmpDir, file)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Failed to stat file %s: %v", file, err)
		}
		initialMtimes[file] = info.ModTime().UnixNano()
	}

	// Create secret with ONE changed file (tls.crt)
	newCerts := map[string][]byte{
		"ca.crt":  []byte("unchanged-ca-cert"),    // SAME
		"tls.crt": []byte("new-server-cert"),      // CHANGED
		"tls.key": []byte("unchanged-server-key"), // SAME
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Data: newCerts,
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(secret).
		Build()

	cfg := &config.Config{
		CertSecretName: "test-certs",
		Namespace:      "default",
		CertDir:        tmpDir,
	}

	reloader := &CertificateReloader{
		Client: client,
		Config: cfg,
	}

	// Reconcile
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-certs",
			Namespace: "default",
		},
	}

	_, err := reloader.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify ca.crt and tls.key were NOT rewritten (unchanged)
	for _, file := range []string{"ca.crt", "tls.key"} {
		path := filepath.Join(tmpDir, file)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Failed to stat file %s: %v", file, err)
		}

		newMtime := info.ModTime().UnixNano()
		if newMtime != initialMtimes[file] {
			t.Errorf("Unchanged file %s was rewritten unnecessarily", file)
		}
	}

	// Verify tls.crt WAS updated
	tlsCrtPath := filepath.Join(tmpDir, "tls.crt")
	content, err := os.ReadFile(tlsCrtPath)
	if err != nil {
		t.Fatalf("Failed to read tls.crt: %v", err)
	}
	if string(content) != string(newCerts["tls.crt"]) {
		t.Errorf("tls.crt was not updated to new content")
	}
}
