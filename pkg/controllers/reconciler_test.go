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
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
)

func TestWebhookReconciler_ReconcileConfigMap(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"webhooks.yaml": `---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: test-webhook
webhooks:
  - name: test.example.com
    clientConfig:
      service:
        name: webhook-svc
        namespace: default
        path: /mutate
      caBundle: ""
    admissionReviewVersions: ["v1"]
    sideEffects: None
`,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(configMap).
		Build()

	cfg := &config.Config{
		WebhookConfigName:      "webhook-config",
		WebhookConfigNamespace: "default",
		CertSecretName:         "test-certs",
		CertSecretNamespace:    "default",
		CertValidityDays:       30,
		CertDir:                t.TempDir(),
	}

	reconciler := &WebhookReconciler{
		Client:       fakeClient,
		TargetClient: fakeClient, // Same for test
		Config:       cfg,
		Scheme:       s,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config",
			Namespace: "default",
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify Secret was created
	secret := &corev1.Secret{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-certs",
		Namespace: "default",
	}, secret)
	if err != nil {
		t.Fatalf("Expected Secret to be created: %v", err)
	}

	// Verify Secret has certificate data
	requiredKeys := []string{"ca.crt", "tls.crt", "tls.key"}
	for _, key := range requiredKeys {
		if _, ok := secret.Data[key]; !ok {
			t.Errorf("Expected Secret to contain key '%s'", key)
		}
	}

	// Verify certificate SANs
	certBlock, _ := pem.Decode(secret.Data["tls.crt"])
	if certBlock == nil {
		t.Fatal("Failed to decode server certificate")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse server certificate: %v", err)
	}

	expectedSANs := []string{
		"webhook-svc.default.svc",
		"webhook-svc.default.svc.cluster.local",
	}
	for _, expectedSAN := range expectedSANs {
		found := slices.Contains(cert.DNSNames, expectedSAN)
		if !found {
			t.Errorf("Expected certificate to contain SAN '%s', found: %v", expectedSAN, cert.DNSNames)
		}
	}
}

func TestWebhookReconciler_ApplyWebhooks(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"webhooks.yaml": `---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: test-webhook
webhooks:
  - name: test.example.com
    clientConfig:
      service:
        name: webhook-svc
        namespace: default
        path: /mutate
      caBundle: ""
    admissionReviewVersions: ["v1"]
    sideEffects: None
`,
		},
	}

	hostingClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(configMap).
		Build()

	targetClient := fake.NewClientBuilder().
		WithScheme(s).
		Build()

	cfg := &config.Config{
		WebhookConfigName:      "webhook-config",
		WebhookConfigNamespace: "default",
		CertSecretName:         "test-certs",
		CertSecretNamespace:    "default",
		CertValidityDays:       30,
		CertDir:                t.TempDir(),
	}

	reconciler := &WebhookReconciler{
		Client:       hostingClient,
		TargetClient: targetClient,
		Config:       cfg,
		Scheme:       s,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config",
			Namespace: "default",
		},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify webhook was applied to target cluster
	mwc := &admissionv1.MutatingWebhookConfiguration{}
	err = targetClient.Get(context.Background(), types.NamespacedName{
		Name: "test-webhook",
	}, mwc)
	if err != nil {
		t.Fatalf("Expected MutatingWebhookConfiguration to be created in target cluster: %v", err)
	}

	// Verify CA bundle was injected
	if len(mwc.Webhooks) == 0 {
		t.Fatal("Expected at least one webhook")
	}
	if len(mwc.Webhooks[0].ClientConfig.CABundle) == 0 {
		t.Error("Expected CA bundle to be injected")
	}
}

func TestWebhookReconciler_CheckCertificates_NeedsRotation_Expiry(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Create a certificate that will expire soon relative to total validity
	// For 80% threshold on 30 day validity: rotate when < 24 days remain
	// Generate cert with 25 days validity, then simulate it was created 20 days ago
	// So remaining = 5 days out of 25 day total = 20% remaining < 80% threshold
	cfg := &config.Config{
		CertSecretName:        "test-certs",
		CertSecretNamespace:   "default",
		CertValidityDays:      30,
		CertRotationThreshold: 0.8, // Rotate at 80% of validity
	}

	// Generate a certificate with short validity (6 hours)
	// This simulates an old certificate that's about to expire
	validity := 6 * time.Hour
	caCert, caKey, err := certificates.GenerateCA(validity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	services := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, services, validity)
	if err != nil {
		t.Fatalf("Failed to generate server cert: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"ca.key":  caKey,
			"tls.crt": serverCert,
			"tls.key": serverKey,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client: fakeClient,
		Config: cfg,
		Scheme: s,
	}

	// Sleep briefly to let some time pass, reducing remaining validity below threshold
	time.Sleep(2 * time.Second)

	// Better approach: Generate with 1 second validity, sleep 0.9s, check
	// Remaining = 0.1s out of 1s = 10% < 80%

	// Regenerate with 1 second validity
	validity2 := 1 * time.Second
	caCert2, caKey2, err := certificates.GenerateCA(validity2)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}
	serverCert2, serverKey2, err := certificates.GenerateServerCert(caCert2, caKey2, services, validity2)
	if err != nil {
		t.Fatalf("Failed to generate server cert: %v", err)
	}

	secret.Data["ca.crt"] = caCert2
	secret.Data["ca.key"] = caKey2
	secret.Data["tls.crt"] = serverCert2
	secret.Data["tls.key"] = serverKey2

	err = fakeClient.Update(context.Background(), secret)
	if err != nil {
		t.Fatalf("Failed to update secret: %v", err)
	}

	// Sleep to consume most of validity
	time.Sleep(900 * time.Millisecond)

	// Now check - should need rotation (10% remaining < 80% threshold)
	_, needsRotation, err := reconciler.checkCertificates(context.Background(), services)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if !needsRotation {
		t.Error("Expected needsRotation=true for certificate with <20% validity remaining")
	}
}

func TestWebhookReconciler_CheckCertificates_NoRotation_ValidCert(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	cfg := &config.Config{
		CertSecretName:        "test-certs",
		CertSecretNamespace:   "default",
		CertValidityDays:      30,
		CertRotationThreshold: 0.8,
	}

	// Generate a fresh certificate (30 days validity, well above 80% threshold)
	validity := 30 * 24 * time.Hour
	caCert, caKey, err := certificates.GenerateCA(validity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	services := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, services, validity)
	if err != nil {
		t.Fatalf("Failed to generate server cert: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"ca.key":  caKey,
			"tls.crt": serverCert,
			"tls.key": serverKey,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client: fakeClient,
		Config: cfg,
		Scheme: s,
	}

	// Check certificates - should NOT need rotation
	_, needsRotation, err := reconciler.checkCertificates(context.Background(), services)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if needsRotation {
		t.Error("Expected needsRotation=false for fresh certificate")
	}
}

func TestWebhookReconciler_CheckCertificates_NeedsRotation_SANsChanged(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	cfg := &config.Config{
		CertSecretName:        "test-certs",
		CertSecretNamespace:   "default",
		CertValidityDays:      30,
		CertRotationThreshold: 0.8,
	}

	// Generate certificate with one service
	validity := 30 * 24 * time.Hour
	caCert, caKey, err := certificates.GenerateCA(validity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	oldServices := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, oldServices, validity)
	if err != nil {
		t.Fatalf("Failed to generate server cert: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"ca.crt":  caCert,
			"ca.key":  caKey,
			"tls.crt": serverCert,
			"tls.key": serverKey,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client: fakeClient,
		Config: cfg,
		Scheme: s,
	}

	// Check with NEW services (different from what's in cert)
	newServices := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
		{Name: "new-webhook-svc", Namespace: "default"}, // Added service
	}

	// Should need rotation due to SAN mismatch
	_, needsRotation, err := reconciler.checkCertificates(context.Background(), newServices)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if !needsRotation {
		t.Error("Expected needsRotation=true when service references change")
	}
}
