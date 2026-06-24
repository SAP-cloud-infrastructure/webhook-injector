// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
		CertRotationThreshold: 0.2,
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
		return
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
		CertRotationThreshold: 0.2,
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
		CertRotationThreshold: 0.2,
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
	_, rotationReason, err := reconciler.checkCertificates(context.Background(), services)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if rotationReason == "" {
		t.Error("Expected rotation reason for certificate with <20% validity remaining")
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
		CertRotationThreshold: 0.2,
		Scheme:                s,
	}

	// Check certificates - should NOT need rotation
	_, rotationReason, err := reconciler.checkCertificates(context.Background(), services)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if rotationReason != "" {
		t.Errorf("Expected no rotation for fresh certificate, got reason %q", rotationReason)
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
		CertRotationThreshold: 0.2,
		Scheme:                s,
	}

	// Check with NEW services (different from what's in cert)
	newServices := []certificates.ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},
		{Name: "new-webhook-svc", Namespace: "default"}, // Added service
	}

	// Should need rotation due to SAN mismatch
	_, rotationReason, err := reconciler.checkCertificates(context.Background(), newServices)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if rotationReason != "san_mismatch" {
		t.Errorf("Expected san_mismatch rotation reason, got %q", rotationReason)
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
		CertRotationThreshold: 0.2,
		Scheme:                s,
	}

	// Check certificates - should need rotation due to annotation
	_, rotationReason, err := reconciler.checkCertificates(context.Background(), services)
	if err != nil {
		t.Fatalf("checkCertificates failed: %v", err)
	}

	if rotationReason != "force_rotation" {
		t.Errorf("Expected force_rotation reason when annotation is present, got %q", rotationReason)
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
		CertRotationThreshold: 0.2,
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
			name:              "30 day cert with 0.2 threshold",
			certValidity:      30 * 24 * time.Hour,
			rotationThreshold: 0.2,
			expectedDuration:  24 * 24 * time.Hour, // 24 days
			tolerance:         1 * time.Hour,
		},
		{
			name:              "1 hour cert with 0.1 threshold",
			certValidity:      1 * time.Hour,
			rotationThreshold: 0.1,
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
		CertRotationThreshold: 0.2,
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
	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(
		[]certificates.ServiceReference{
			{Name: "test", Namespace: "default"},
		},
		1*time.Second,
	)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	// Wait for cert to be past rotation threshold (remaining < 0.2 * 1 second)
	time.Sleep(1 * time.Second)

	secret := &corev1.Secret{
		Data: map[string][]byte{
			"tls.crt": certPEM,
			"tls.key": keyPEM,
		},
	}

	reconciler := &WebhookReconciler{
		CertRotationThreshold: 0.2,
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
	expectedRequeue := time.Duration(float64(certValidity) * (1 - threshold))

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

// phaseTestScheme builds a scheme with everything the phase-machine tests
// need to construct hosting/target fake clients. Kept local to this file
// so it doesn't pull in fixtures from managed_resource_reconciler_test.go.
func phaseTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatalf("apiextensionsv1.AddToScheme: %v", err)
	}
	return s
}

// mintCert generates a self-signed cert/key pair via the same generator
// the controller uses, so the resulting bundle has identical shape to a
// real cert Secret.
func mintCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(
		[]certificates.ServiceReference{{Name: "svc", Namespace: "ns"}},
		time.Hour,
	)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	return certPEM, keyPEM
}

// makeRotationReconciler builds a WebhookReconciler around a fake
// hosting Client (seeded with `objects`) and a fake TargetClient (seeded
// with `targetObjects`). The CertSecretName/Namespace match the test
// fixtures so leafFromSecret/loadCABundle resolve correctly.
func makeRotationReconciler(t *testing.T, objects, targetObjects []client.Object) *WebhookReconciler {
	t.Helper()
	s := phaseTestScheme(t)
	hosting := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
	target := fake.NewClientBuilder().WithScheme(s).WithObjects(targetObjects...).Build()
	return &WebhookReconciler{
		Client:         hosting,
		TargetClient:   target,
		CertSecretName: "test-certs",
		Namespace:      "default",
		CertValidity:   time.Hour,
		Scheme:         s,
	}
}

// readSecret refetches the cert Secret from the reconciler's hosting
// client. Convenience to keep the tests readable.
func readSecret(t *testing.T, r *WebhookReconciler) *corev1.Secret {
	t.Helper()
	out := &corev1.Secret{}
	if err := r.Client.Get(context.Background(), types.NamespacedName{Name: r.CertSecretName, Namespace: r.Namespace}, out); err != nil {
		t.Fatalf("Get Secret: %v", err)
	}
	return out
}

// countPEMCerts counts CERTIFICATE blocks in a PEM byte slice via the
// shared ParseChain helper. Tests assert on bundle cardinality before
// inspecting individual cert contents.
func countPEMCerts(t *testing.T, pemBytes []byte) int {
	t.Helper()
	certs, err := certificates.ParseChain(pemBytes)
	if err != nil {
		t.Fatalf("ParseChain: %v", err)
	}
	return len(certs)
}

// TestStageCertificate_FromActive covers the canonical first-rotation
// case: existing single-cert active Secret, stageCertificate runs, the
// resulting Secret has tls.crt = old||new, tls.key still pointing at
// old, tls.staged.key carrying the new key, phase=staged, started-at
// freshly stamped.
func TestStageCertificate_FromActive(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-certs", Namespace: "default"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{tlsCrtKey: oldCert, tlsKeyKey: oldKey},
	}
	r := makeRotationReconciler(t, []client.Object{existing}, nil)

	if err := r.stageCertificate(context.Background(), []certificates.ServiceReference{{Name: "svc", Namespace: "ns"}}); err != nil {
		t.Fatalf("stageCertificate: %v", err)
	}

	got := readSecret(t, r)
	if n := countPEMCerts(t, got.Data[tlsCrtKey]); n != 2 {
		t.Fatalf("tls.crt: expected 2 cert blocks (union bundle), got %d", n)
	}
	if !bytesContains(got.Data[tlsCrtKey], oldCert) {
		t.Errorf("tls.crt: must still contain the OLD cert verbatim")
	}
	if string(got.Data[tlsKeyKey]) != string(oldKey) {
		t.Errorf("tls.key: must still be the old key during staged phase")
	}
	if len(got.Data[tlsStagedKeyKey]) == 0 {
		t.Errorf("tls.staged.key: must be present during staged phase")
	}
	if got.Annotations[RotationPhaseAnnotation] != string(PhaseStaged) {
		t.Errorf("phase annotation: want staged, got %q", got.Annotations[RotationPhaseAnnotation])
	}
	if got.Annotations[RotationStartedAtAnnotation] == "" {
		t.Errorf("started-at annotation: must be set")
	}
}

// TestStageCertificate_InitialBootstrap covers the no-existing-Secret
// path: stageCertificate must write a fresh single-cert Secret in active
// phase (no overlap to construct).
func TestStageCertificate_InitialBootstrap(t *testing.T) {
	r := makeRotationReconciler(t, nil, nil)

	if err := r.stageCertificate(context.Background(), []certificates.ServiceReference{{Name: "svc", Namespace: "ns"}}); err != nil {
		t.Fatalf("stageCertificate: %v", err)
	}

	got := readSecret(t, r)
	if n := countPEMCerts(t, got.Data[tlsCrtKey]); n != 1 {
		t.Fatalf("initial bootstrap tls.crt: expected 1 cert, got %d", n)
	}
	if _, present := got.Data[tlsStagedKeyKey]; present {
		t.Errorf("initial bootstrap: tls.staged.key must NOT be present")
	}
	if phase := got.Annotations[RotationPhaseAnnotation]; phase != "" && phase != string(PhaseActive) {
		t.Errorf("initial bootstrap phase: want active or empty, got %q", phase)
	}
}

// TestStageCertificate_ReStage covers force-rotation re-triggered during
// the staged phase. The previously-staged candidate is replaced with a
// fresh one, NOT chained on top — the bundle stays at 2 certs.
func TestStageCertificate_ReStage(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	candidateCert, candidateKey := mintCert(t)
	bundle := append(append([]byte{}, oldCert...), candidateCert...)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				RotationPhaseAnnotation:     string(PhaseStaged),
				RotationStartedAtAnnotation: time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339),
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{tlsCrtKey: bundle, tlsKeyKey: oldKey, tlsStagedKeyKey: candidateKey},
	}
	r := makeRotationReconciler(t, []client.Object{existing}, nil)

	if err := r.stageCertificate(context.Background(), []certificates.ServiceReference{{Name: "svc", Namespace: "ns"}}); err != nil {
		t.Fatalf("stageCertificate: %v", err)
	}

	got := readSecret(t, r)
	if n := countPEMCerts(t, got.Data[tlsCrtKey]); n != 2 {
		t.Fatalf("re-stage tls.crt: expected 2 cert blocks (oldLeaf + fresh candidate), got %d", n)
	}
	if !bytesContains(got.Data[tlsCrtKey], oldCert) {
		t.Errorf("re-stage: oldLeaf must still be in the bundle")
	}
	if bytesContains(got.Data[tlsCrtKey], candidateCert) {
		t.Errorf("re-stage: previous-candidate must be replaced, not retained")
	}
}

// TestStageCertificate_PreservesNonControllerAnnotations confirms that a
// rotation does NOT clobber annotations that the controller doesn't own
// (helm release markers, kubectl last-applied, etc.).
func TestStageCertificate_PreservesNonControllerAnnotations(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				ForceRotationAnnotation:                     "true", // must be cleared
				"meta.helm.sh/release-name":                 "ipam-capi-remote",
				"kubectl.kubernetes.io/last-applied-config": "{...}",
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{tlsCrtKey: oldCert, tlsKeyKey: oldKey},
	}
	r := makeRotationReconciler(t, []client.Object{existing}, nil)
	if err := r.stageCertificate(context.Background(), []certificates.ServiceReference{{Name: "svc", Namespace: "ns"}}); err != nil {
		t.Fatalf("stageCertificate: %v", err)
	}

	got := readSecret(t, r)
	if got.Annotations[ForceRotationAnnotation] != "" {
		t.Errorf("force-rotation annotation should be cleared after rotation")
	}
	if got.Annotations["meta.helm.sh/release-name"] != "ipam-capi-remote" {
		t.Errorf("helm annotation was lost: got %q", got.Annotations["meta.helm.sh/release-name"])
	}
	if got.Annotations["kubectl.kubernetes.io/last-applied-config"] != "{...}" {
		t.Errorf("kubectl annotation was lost")
	}
}

// TestPromoteCertificate switches tls.key to the staged key, clears
// tls.staged.key, and REORDERS tls.crt so the new (now-leaf) cert is
// first in the bundle. Both certs remain in tls.crt (the apiserver-side
// caBundle still trusts both). Phase becomes active and the promoted-at
// marker is set.
func TestPromoteCertificate(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	newCert, newKey := mintCert(t)
	bundle := append(append([]byte{}, oldCert...), newCert...)
	startedAt := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				RotationPhaseAnnotation:     string(PhaseStaged),
				RotationStartedAtAnnotation: startedAt,
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{tlsCrtKey: bundle, tlsKeyKey: oldKey, tlsStagedKeyKey: newKey},
	}
	r := makeRotationReconciler(t, []client.Object{existing}, nil)
	secret := readSecret(t, r)

	if err := r.promoteCertificate(context.Background(), secret); err != nil {
		t.Fatalf("promoteCertificate: %v", err)
	}

	got := readSecret(t, r)
	if string(got.Data[tlsKeyKey]) != string(newKey) {
		t.Errorf("tls.key must be swapped to the staged key after promote")
	}
	if _, present := got.Data[tlsStagedKeyKey]; present {
		t.Errorf("tls.staged.key must be removed after promote")
	}
	// tls.crt must still carry BOTH certs (so the apiserver-side caBundle
	// continues trusting the old cert until prune), but the NEW cert
	// must be FIRST so tls.LoadX509KeyPair pairs it with the new key.
	if n := countPEMCerts(t, got.Data[tlsCrtKey]); n != 2 {
		t.Fatalf("tls.crt after promote: want 2 cert blocks, got %d", n)
	}
	postPromoteLeaf, err := certificates.LeafCertificate(got.Data[tlsCrtKey], got.Data[tlsKeyKey])
	if err != nil {
		t.Fatalf("leaf detection on promoted bundle: %v", err)
	}
	// The post-promote leaf must be the NEW cert. Compare by DER bytes:
	// equivalent to ordering check on the bundle.
	certs, err := certificates.ParseChain(got.Data[tlsCrtKey])
	if err != nil {
		t.Fatalf("ParseChain post-promote: %v", err)
	}
	if !certs[0].Equal(postPromoteLeaf) {
		t.Errorf("post-promote tls.crt: new leaf must be first in the bundle")
	}
	if got.Annotations[RotationPhaseAnnotation] != string(PhaseActive) {
		t.Errorf("phase must be active after promote, got %q", got.Annotations[RotationPhaseAnnotation])
	}
	if got.Annotations[RotationStartedAtAnnotation] != startedAt {
		t.Errorf("started-at must be preserved across promote")
	}
	if got.Annotations[rotationPromotedAtAnnotation] == "" {
		t.Errorf("promoted-at marker must be set on promote")
	}
}

// TestPruneCertificate drops the old cert from the bundle, leaving only
// the leaf. All rotation annotations are cleared.
func TestPruneCertificate(t *testing.T) {
	oldCert, _ := mintCert(t)
	newCert, newKey := mintCert(t)
	bundle := append(append([]byte{}, oldCert...), newCert...)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-certs",
			Namespace: "default",
			Annotations: map[string]string{
				RotationPhaseAnnotation:      string(PhaseActive),
				RotationStartedAtAnnotation:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
				rotationPromotedAtAnnotation: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{tlsCrtKey: bundle, tlsKeyKey: newKey},
	}
	r := makeRotationReconciler(t, []client.Object{existing}, nil)
	secret := readSecret(t, r)

	if err := r.pruneCertificate(context.Background(), secret); err != nil {
		t.Fatalf("pruneCertificate: %v", err)
	}

	got := readSecret(t, r)
	if n := countPEMCerts(t, got.Data[tlsCrtKey]); n != 1 {
		t.Fatalf("pruned tls.crt: expected 1 cert, got %d", n)
	}
	if bytesContains(got.Data[tlsCrtKey], oldCert) {
		t.Errorf("pruned bundle must NOT contain the old cert")
	}
	if got.Annotations[RotationPhaseAnnotation] != "" {
		t.Errorf("phase annotation must be cleared after prune, got %q", got.Annotations[RotationPhaseAnnotation])
	}
	if got.Annotations[rotationPromotedAtAnnotation] != "" {
		t.Errorf("promoted-at must be cleared after prune")
	}
}

// TestCheckPropagation_EmptyGate confirms that when --rotation-gate-crds
// is unset (empty list), checkPropagation returns true immediately —
// the time-based gate timeout is the only constraint in that mode.
func TestCheckPropagation_EmptyGate(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	newCert, _ := mintCert(t)
	bundle := append(append([]byte{}, oldCert...), newCert...)
	secret := &corev1.Secret{
		Data: map[string][]byte{tlsCrtKey: bundle, tlsKeyKey: oldKey},
	}
	r := makeRotationReconciler(t, nil, nil) // RotationGateCRDs is unset
	propagated, err := r.checkPropagation(context.Background(), secret)
	if err != nil {
		t.Fatalf("checkPropagation: %v", err)
	}
	if !propagated {
		t.Errorf("empty gate should return propagated=true")
	}
}

// TestCheckPropagation_BothCertsPresent confirms the positive case: every
// gated CRD on the target has both certs in its conversion caBundle.
func TestCheckPropagation_BothCertsPresent(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	newCert, _ := mintCert(t)
	bundle := append(append([]byte{}, oldCert...), newCert...)
	secret := &corev1.Secret{
		Data: map[string][]byte{tlsCrtKey: bundle, tlsKeyKey: oldKey},
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{CABundle: bundle},
				},
			},
		},
	}
	r := makeRotationReconciler(t, nil, []client.Object{crd})
	r.RotationGateCRDs = []string{"widgets.example.com"}

	propagated, err := r.checkPropagation(context.Background(), secret)
	if err != nil {
		t.Fatalf("checkPropagation: %v", err)
	}
	if !propagated {
		t.Errorf("expected propagated=true when CRD has both certs")
	}
}

// TestCheckPropagation_OnlyOldCert covers the in-flight case: the gated
// CRD has only the old cert (the staged cert hasn't reached it yet).
// Must return false.
func TestCheckPropagation_OnlyOldCert(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	newCert, _ := mintCert(t)
	bundle := append(append([]byte{}, oldCert...), newCert...)
	secret := &corev1.Secret{
		Data: map[string][]byte{tlsCrtKey: bundle, tlsKeyKey: oldKey},
	}

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ClientConfig: &apiextensionsv1.WebhookClientConfig{CABundle: oldCert}, // only old
				},
			},
		},
	}
	r := makeRotationReconciler(t, nil, []client.Object{crd})
	r.RotationGateCRDs = []string{"widgets.example.com"}

	propagated, err := r.checkPropagation(context.Background(), secret)
	if err != nil {
		t.Fatalf("checkPropagation: %v", err)
	}
	if propagated {
		t.Errorf("expected propagated=false when CRD only has old cert")
	}
}

// TestCheckPropagation_MissingCRD covers the bootstrap edge case: the
// CRD listed in --rotation-gate-crds is not (yet) present on the target.
// Must return false (NOT error) so the staged loop waits it out.
func TestCheckPropagation_MissingCRD(t *testing.T) {
	oldCert, oldKey := mintCert(t)
	newCert, _ := mintCert(t)
	bundle := append(append([]byte{}, oldCert...), newCert...)
	secret := &corev1.Secret{
		Data: map[string][]byte{tlsCrtKey: bundle, tlsKeyKey: oldKey},
	}
	r := makeRotationReconciler(t, nil, nil) // no CRDs on target
	r.RotationGateCRDs = []string{"missing.example.com"}

	propagated, err := r.checkPropagation(context.Background(), secret)
	if err != nil {
		t.Fatalf("checkPropagation: %v", err)
	}
	if propagated {
		t.Errorf("expected propagated=false when gated CRD is absent on target")
	}
}

// bytesContains is a substring check on byte slices. Used to verify
// that a concatenated PEM bundle still contains a specific cert block
// verbatim. Local to this file so the test stays self-contained.
func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
