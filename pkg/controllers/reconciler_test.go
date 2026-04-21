// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

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

	reconciler := &WebhookReconciler{
		Client:                fakeClient,
		TargetClient:          fakeClient, // Same for test
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          30 * 24 * time.Hour,
		CertRotationThreshold: 0.8,
		Scheme:                s,
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
	requiredKeys := []string{"tls.crt", "tls.key"}
	for _, key := range requiredKeys {
		if _, ok := secret.Data[key]; !ok {
			t.Errorf("Expected Secret to contain key '%s'", key)
		}
	}

	// Verify Secret type is kubernetes.io/tls
	if secret.Type != corev1.SecretTypeTLS {
		t.Errorf("Expected Secret type %s, got %s", corev1.SecretTypeTLS, secret.Type)
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

	reconciler := &WebhookReconciler{
		Client:                hostingClient,
		TargetClient:          targetClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          30 * 24 * time.Hour,
		CertRotationThreshold: 0.8,
		Scheme:                s,
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

	// Generate a certificate with short validity (6 hours)
	// This simulates an old certificate that's about to expire
	validity := 6 * time.Hour
	services := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(services, validity)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client:                fakeClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          30 * 24 * time.Hour,
		CertRotationThreshold: 0.8,
		Scheme:                s,
	}

	// Sleep briefly to let some time pass, reducing remaining validity below threshold
	time.Sleep(2 * time.Second)

	// Better approach: Generate with 1 second validity, sleep 0.9s, check
	// Remaining = 0.1s out of 1s = 10% < 80%

	// Regenerate with 1 second validity
	validity2 := 1 * time.Second
	certPEM2, keyPEM2, err := certificates.GenerateSelfSignedCert(services, validity2)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	secret.Data["tls.crt"] = certPEM2
	secret.Data["tls.key"] = keyPEM2

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

	// Generate a fresh certificate (30 days validity, well above 80% threshold)
	validity := 30 * 24 * time.Hour
	services := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(services, validity)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client:                fakeClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          30 * 24 * time.Hour,
		CertRotationThreshold: 0.8,
		Scheme:                s,
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

	// Generate certificate with one service
	validity := 30 * 24 * time.Hour
	oldServices := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(oldServices, validity)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client:                fakeClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          30 * 24 * time.Hour,
		CertRotationThreshold: 0.8,
		Scheme:                s,
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

func TestWebhookReconciler_CheckCertificates_ForceRotation_Annotation(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Generate a fresh certificate (should not need rotation based on time)
	validity := 720 * time.Hour
	services := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
	}
	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(services, validity)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	// Create secret WITH force-rotation annotation
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				ForceRotationAnnotation: "true",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret).
		Build()

	reconciler := &WebhookReconciler{
		Client:                fakeClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          720 * time.Hour,
		CertRotationThreshold: 0.8,
		Scheme:                s,
	}

	// Check certificates - should need rotation due to annotation
	_, needsRotation, err := reconciler.checkCertificates(context.Background(), services)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if !needsRotation {
		t.Error("Expected needsRotation=true when force-rotation annotation is present")
	}
}

func TestWebhookReconciler_RemovesAnnotationAfterRotation(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Create secret with annotation
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				ForceRotationAnnotation: "true",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": []byte("old-cert"),
			"tls.key": []byte("old-key"),
		},
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
    admissionReviewVersions: ["v1"]
    sideEffects: None`,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret, configMap).
		Build()

	reconciler := &WebhookReconciler{
		Client:                fakeClient,
		TargetClient:          fakeClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          720 * time.Hour,
		CertRotationThreshold: 0.8,
		Scheme:                s,
	}

	// Reconcile - should trigger rotation due to annotation
	_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config",
			Namespace: "default",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify annotation was removed
	updatedSecret := &corev1.Secret{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-certs",
		Namespace: "default",
	}, updatedSecret)
	if err != nil {
		t.Fatalf("Failed to get updated secret: %v", err)
	}

	if updatedSecret.Annotations != nil {
		if _, exists := updatedSecret.Annotations[ForceRotationAnnotation]; exists {
			t.Error("Expected force-rotation annotation to be removed after rotation")
		}
	}
}

func TestWebhookReconciler_CalculateNextRotation(t *testing.T) {
	tests := []struct {
		name              string
		certValidity      time.Duration
		rotationThreshold float64
		expectedDuration  time.Duration
		tolerance         time.Duration
	}{
		{
			name:              "60 second cert with 0.5 threshold",
			certValidity:      60 * time.Second,
			rotationThreshold: 0.5,
			expectedDuration:  30 * time.Second,
			tolerance:         2 * time.Second,
		},
		{
			name:              "30 day cert with 0.8 threshold",
			certValidity:      30 * 24 * time.Hour,
			rotationThreshold: 0.8,
			expectedDuration:  24 * 24 * time.Hour, // 24 days
			tolerance:         1 * time.Hour,
		},
		{
			name:              "1 hour cert with 0.9 threshold",
			certValidity:      1 * time.Hour,
			rotationThreshold: 0.9,
			expectedDuration:  54 * time.Minute,
			tolerance:         1 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Generate certificate
			certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(
				[]certificates.ServiceReference{
					{Name: "test", Namespace: "default"},
				},
				tt.certValidity,
			)
			if err != nil {
				t.Fatalf("Failed to generate self-signed cert: %v", err)
			}

			// Create secret with certificate
			secret := &corev1.Secret{
				Data: map[string][]byte{
					"tls.crt": certPEM,
					"tls.key": keyPEM,
				},
			}

			reconciler := &WebhookReconciler{
				CertRotationThreshold: tt.rotationThreshold,
			}

			// Calculate next rotation
			nextRotation := reconciler.calculateNextRotation(secret)

			// Verify duration is approximately correct (within tolerance)
			diff := nextRotation - tt.expectedDuration
			if diff < 0 {
				diff = -diff
			}

			if diff > tt.tolerance {
				t.Errorf("Expected next rotation in ~%v, got %v (diff: %v, tolerance: %v)",
					tt.expectedDuration, nextRotation, diff, tt.tolerance)
			}

			t.Logf("Certificate valid for %v, threshold %.1f%%, will rotate in %v",
				tt.certValidity, tt.rotationThreshold*100, nextRotation)
		})
	}
}

func TestWebhookReconciler_CalculateNextRotation_InvalidCert(t *testing.T) {
	tests := []struct {
		name   string
		secret *corev1.Secret
	}{
		{
			name: "missing tls.crt",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"tls.key": []byte("key"),
				},
			},
		},
		{
			name: "invalid PEM",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"tls.crt": []byte("not a pem"),
				},
			},
		},
		{
			name: "invalid certificate",
			secret: &corev1.Secret{
				Data: map[string][]byte{
					"tls.crt": []byte("-----BEGIN CERTIFICATE-----\ninvalid\n-----END CERTIFICATE-----"),
				},
			},
		},
	}

	reconciler := &WebhookReconciler{
		CertRotationThreshold: 0.8,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextRotation := reconciler.calculateNextRotation(tt.secret)
			if nextRotation != 0 {
				t.Errorf("Expected 0 for invalid certificate, got %v", nextRotation)
			}
		})
	}
}

func TestWebhookReconciler_CalculateNextRotation_AlreadyPastThreshold(t *testing.T) {
	// Generate certificate that's already past rotation threshold
	certPEM, _, err := certificates.GenerateSelfSignedCert(
		[]certificates.ServiceReference{
			{Name: "test", Namespace: "default"},
		},
		1*time.Second,
	)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	// Wait for cert to be past rotation threshold (0.8 * 1 second = 0.8 seconds)
	time.Sleep(1 * time.Second)

	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
		},
	}

	reconciler := &WebhookReconciler{
		CertRotationThreshold: 0.8,
	}

	nextRotation := reconciler.calculateNextRotation(secret)

	// Should return 1 second (minimum) since rotation time has passed
	if nextRotation != 1*time.Second {
		t.Errorf("Expected 1 second for past-threshold cert, got %v", nextRotation)
	}
}

func TestWebhookReconciler_RequeueAfterUsesCalculatedRotation(t *testing.T) {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	// Generate certificate with known expiry
	certValidity := 60 * time.Second
	threshold := 0.5
	expectedRequeue := time.Duration(float64(certValidity) * threshold)

	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(
		[]certificates.ServiceReference{
			{Name: "webhook-service", Namespace: "default"},
		},
		certValidity,
	)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-config",
			Namespace: "default",
		},
		Data: map[string]string{
			"webhooks.yaml": `---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: test-webhook
webhooks:
  - name: validate.example.com
    clientConfig:
      service:
        name: webhook-service
        namespace: default
        path: /validate
      caBundle: ""
    rules:
      - operations: ["CREATE"]
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["configmaps"]
    admissionReviewVersions: ["v1"]
    sideEffects: None`,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(secret, configMap).
		Build()

	reconciler := &WebhookReconciler{
		Client:                fakeClient,
		TargetClient:          fakeClient,
		WebhookConfigName:     "webhook-config",
		Namespace:             "default",
		CertSecretName:        "test-certs",
		CertValidity:          certValidity,
		CertRotationThreshold: threshold,
		Scheme:                s,
	}

	result, err := reconciler.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config",
			Namespace: "default",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify RequeueAfter is based on certificate rotation time, not a fixed sync period
	tolerance := 2 * time.Second
	diff := result.RequeueAfter - expectedRequeue
	if diff < 0 {
		diff = -diff
	}

	if diff > tolerance {
		t.Errorf("Expected RequeueAfter ~%v (calculated from cert), got %v (diff: %v)",
			expectedRequeue, result.RequeueAfter, diff)
	}

	t.Logf("RequeueAfter: %v (expected: ~%v)", result.RequeueAfter, expectedRequeue)
}
