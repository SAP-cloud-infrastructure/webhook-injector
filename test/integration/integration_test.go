//go:build integration
// +build integration

package integration

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/config"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
)

func TestIntegration_EndToEnd(t *testing.T) {
	// Set up test environment
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = admissionv1.AddToScheme(scheme)

	testEnv := &envtest.Environment{
		Scheme: scheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("Failed to stop test environment: %v", err)
		}
	}()

	// Create Kubernetes client
	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	// Create namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-integration",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	// Load test webhook config
	webhookYAML, err := os.ReadFile("testdata/webhook-config.yaml")
	if err != nil {
		t.Fatalf("Failed to read webhook config: %v", err)
	}

	// Create ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-config",
			Namespace: "test-integration",
		},
		Data: map[string]string{
			"webhooks.yaml": string(webhookYAML),
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("Failed to create ConfigMap: %v", err)
	}

	// Create temp directory for certificates
	tmpDir := t.TempDir()

	// Set up controllers
	appCfg := &config.Config{
		WebhookConfigName:      "webhook-config",
		WebhookConfigNamespace: "test-integration",
		CertSecretName:         "test-certs",
		CertSecretNamespace:    "test-integration",
		CertDir:                tmpDir,
		CertValidityDays:       30,
		CertRotationThreshold:  0.8,
		SyncPeriod:             1 * time.Minute,
	}

	// Run leader reconciler once
	reconciler := &controllers.LeaderReconciler{
		Client:       k8sClient,
		TargetClient: k8sClient,
		Config:       appCfg,
		Scheme:       scheme,
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config",
			Namespace: "test-integration",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify Secret was created
	secret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-certs",
		Namespace: "test-integration",
	}, secret)
	if err != nil {
		t.Fatalf("Expected Secret to be created: %v", err)
	}

	// Verify certificate files
	certFiles := []string{"ca.crt", "tls.crt", "tls.key"}
	for _, file := range certFiles {
		if _, ok := secret.Data[file]; !ok {
			t.Errorf("Expected Secret to contain %s", file)
		}
	}

	// Verify webhook configuration was applied
	mwc := &admissionv1.MutatingWebhookConfiguration{}
	err = k8sClient.Get(ctx, types.NamespacedName{Name: "test-webhook"}, mwc)
	if err != nil {
		t.Fatalf("Expected MutatingWebhookConfiguration to be created: %v", err)
	}

	// Verify CA bundle was injected
	if len(mwc.Webhooks) == 0 {
		t.Fatal("Expected at least one webhook in configuration")
	}
	if len(mwc.Webhooks[0].ClientConfig.CABundle) == 0 {
		t.Error("Expected CA bundle to be injected into webhook")
	}

	t.Log("Integration test passed!")
}

func TestIntegration_CertificateWithSANs(t *testing.T) {
	// Set up test environment
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = admissionv1.AddToScheme(scheme)

	testEnv := &envtest.Environment{
		Scheme: scheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("Failed to stop test environment: %v", err)
		}
	}()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sans",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	// Create webhook config with multiple services
	webhookYAML := `---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: test-multi-webhook
webhooks:
  - name: webhook1.example.com
    clientConfig:
      service:
        name: webhook-service-1
        namespace: test-sans
        path: /mutate
        port: 9443
    rules:
      - operations: ["CREATE"]
        apiGroups: ["apps"]
        apiVersions: ["v1"]
        resources: ["deployments"]
    admissionReviewVersions: ["v1"]
    sideEffects: None
  - name: webhook2.example.com
    clientConfig:
      service:
        name: webhook-service-2
        namespace: test-sans
        path: /validate
        port: 9443
    rules:
      - operations: ["UPDATE"]
        apiGroups: ["apps"]
        apiVersions: ["v1"]
        resources: ["deployments"]
    admissionReviewVersions: ["v1"]
    sideEffects: None
`

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-config-sans",
			Namespace: "test-sans",
		},
		Data: map[string]string{
			"webhooks.yaml": webhookYAML,
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("Failed to create ConfigMap: %v", err)
	}

	tmpDir := t.TempDir()

	appCfg := &config.Config{
		WebhookConfigName:      "webhook-config-sans",
		WebhookConfigNamespace: "test-sans",
		CertSecretName:         "test-certs-sans",
		CertSecretNamespace:    "test-sans",
		CertDir:                tmpDir,
		CertValidityDays:       30,
		CertRotationThreshold:  0.8,
		SyncPeriod:             1 * time.Minute,
	}

	reconciler := &controllers.LeaderReconciler{
		Client:       k8sClient,
		TargetClient: k8sClient,
		Config:       appCfg,
		Scheme:       scheme,
	}

	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config-sans",
			Namespace: "test-sans",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify Secret and extract certificate
	secret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-certs-sans",
		Namespace: "test-sans",
	}, secret)
	if err != nil {
		t.Fatalf("Expected Secret to be created: %v", err)
	}

	// Parse the TLS certificate and verify SANs
	certPEM := secret.Data["tls.crt"]
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("Failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse certificate: %v", err)
	}

	// Verify SANs contain all service references
	expectedDNSNames := []string{
		"webhook-service-1.test-sans.svc",
		"webhook-service-1.test-sans.svc.cluster.local",
		"webhook-service-2.test-sans.svc",
		"webhook-service-2.test-sans.svc.cluster.local",
	}

	dnsNameMap := make(map[string]bool)
	for _, dns := range cert.DNSNames {
		dnsNameMap[dns] = true
	}

	for _, expected := range expectedDNSNames {
		if !dnsNameMap[expected] {
			t.Errorf("Expected SAN %s not found in certificate. Found SANs: %v", expected, cert.DNSNames)
		}
	}

	t.Log("Certificate SANs verification passed!")
}

func TestIntegration_CertificateReloader(t *testing.T) {
	// Set up test environment
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)

	testEnv := &envtest.Environment{
		Scheme: scheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("Failed to stop test environment: %v", err)
		}
	}()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-reloader",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	tmpDir := t.TempDir()

	appCfg := &config.Config{
		CertSecretName:      "reloader-certs",
		CertSecretNamespace: "test-reloader",
		CertDir:             tmpDir,
	}

	// Create certificate secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "reloader-certs",
			Namespace: "test-reloader",
		},
		Data: map[string][]byte{
			"ca.crt":  []byte("test-ca-cert"),
			"tls.crt": []byte("test-tls-cert"),
			"tls.key": []byte("test-tls-key"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("Failed to create Secret: %v", err)
	}

	// Set up certificate reloader
	reloader := &controllers.CertificateReloader{
		Client: k8sClient,
		Config: appCfg,
	}

	_, err = reloader.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "reloader-certs",
			Namespace: "test-reloader",
		},
	})
	if err != nil {
		t.Fatalf("Reloader reconcile failed: %v", err)
	}

	// Verify certificates were written to disk
	certFiles := map[string]string{
		"ca.crt":  "test-ca-cert",
		"tls.crt": "test-tls-cert",
		"tls.key": "test-tls-key",
	}

	for fileName, expectedContent := range certFiles {
		filePath := filepath.Join(tmpDir, fileName)
		content, err := os.ReadFile(filePath)
		if err != nil {
			t.Errorf("Failed to read certificate file %s: %v", fileName, err)
			continue
		}
		if string(content) != expectedContent {
			t.Errorf("Certificate file %s has incorrect content. Expected: %s, Got: %s",
				fileName, expectedContent, string(content))
		}
	}

	// Update the secret and verify reload
	secret.Data["tls.crt"] = []byte("updated-tls-cert")
	if err := k8sClient.Update(ctx, secret); err != nil {
		t.Fatalf("Failed to update Secret: %v", err)
	}

	_, err = reloader.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "reloader-certs",
			Namespace: "test-reloader",
		},
	})
	if err != nil {
		t.Fatalf("Reloader reconcile after update failed: %v", err)
	}

	// Verify updated certificate
	updatedContent, err := os.ReadFile(filepath.Join(tmpDir, "tls.crt"))
	if err != nil {
		t.Fatalf("Failed to read updated certificate: %v", err)
	}
	if string(updatedContent) != "updated-tls-cert" {
		t.Errorf("Certificate was not updated. Expected: updated-tls-cert, Got: %s", string(updatedContent))
	}

	t.Log("Certificate reloader test passed!")
}

func TestIntegration_ErrorHandling(t *testing.T) {
	// Set up test environment
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = admissionv1.AddToScheme(scheme)

	testEnv := &envtest.Environment{
		Scheme: scheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("Failed to stop test environment: %v", err)
		}
	}()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-errors",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	tmpDir := t.TempDir()

	appCfg := &config.Config{
		WebhookConfigName:      "missing-config",
		WebhookConfigNamespace: "test-errors",
		CertSecretName:         "test-certs-err",
		CertSecretNamespace:    "test-errors",
		CertDir:                tmpDir,
		CertValidityDays:       30,
		CertRotationThreshold:  0.8,
		SyncPeriod:             1 * time.Minute,
	}

	reconciler := &controllers.LeaderReconciler{
		Client:       k8sClient,
		TargetClient: k8sClient,
		Config:       appCfg,
		Scheme:       scheme,
	}

	// Test with non-existent ConfigMap - should return no error (NotFound is ignored)
	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "missing-config",
			Namespace: "test-errors",
		},
	})
	if err != nil {
		t.Errorf("Expected no error for missing ConfigMap, got: %v", err)
	}

	// Create ConfigMap with invalid webhook YAML
	invalidCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-config",
			Namespace: "test-errors",
		},
		Data: map[string]string{
			"webhooks.yaml": "invalid: yaml: content: [[[",
		},
	}
	if err := k8sClient.Create(ctx, invalidCM); err != nil {
		t.Fatalf("Failed to create invalid ConfigMap: %v", err)
	}

	appCfg.WebhookConfigName = "invalid-config"
	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "invalid-config",
			Namespace: "test-errors",
		},
	})
	if err == nil {
		t.Error("Expected error for invalid webhook YAML, got nil")
	}

	// Create ConfigMap without webhooks.yaml key
	noKeyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-key-config",
			Namespace: "test-errors",
		},
		Data: map[string]string{
			"other.yaml": "some data",
		},
	}
	if err := k8sClient.Create(ctx, noKeyCM); err != nil {
		t.Fatalf("Failed to create ConfigMap without key: %v", err)
	}

	appCfg.WebhookConfigName = "no-key-config"
	_, err = reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "no-key-config",
			Namespace: "test-errors",
		},
	})
	if err == nil {
		t.Error("Expected error for missing webhooks.yaml key, got nil")
	}

	t.Log("Error handling test passed!")
}

func TestIntegration_MultiReplicaBehavior(t *testing.T) {
	// Set up test environment
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = admissionv1.AddToScheme(scheme)

	testEnv := &envtest.Environment{
		Scheme: scheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("Failed to start test environment: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Logf("Failed to stop test environment: %v", err)
		}
	}()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	ctx := context.Background()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-replicas",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	// Create webhook config
	webhookYAML, err := os.ReadFile("testdata/webhook-config.yaml")
	if err != nil {
		t.Fatalf("Failed to read webhook config: %v", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-config-replicas",
			Namespace: "test-replicas",
		},
		Data: map[string]string{
			"webhooks.yaml": string(webhookYAML),
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("Failed to create ConfigMap: %v", err)
	}

	// Simulate leader replica - generates certificates
	leaderTmpDir := t.TempDir()
	leaderCfg := &config.Config{
		WebhookConfigName:      "webhook-config-replicas",
		WebhookConfigNamespace: "test-replicas",
		CertSecretName:         "test-certs-replicas",
		CertSecretNamespace:    "test-replicas",
		CertDir:                leaderTmpDir,
		CertValidityDays:       30,
		CertRotationThreshold:  0.8,
		SyncPeriod:             1 * time.Minute,
	}

	leaderReconciler := &controllers.LeaderReconciler{
		Client:       k8sClient,
		TargetClient: k8sClient,
		Config:       leaderCfg,
		Scheme:       scheme,
	}

	// Leader generates certificates
	_, err = leaderReconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "webhook-config-replicas",
			Namespace: "test-replicas",
		},
	})
	if err != nil {
		t.Fatalf("Leader reconcile failed: %v", err)
	}

	// Verify Secret was created by leader
	secret := &corev1.Secret{}
	err = k8sClient.Get(ctx, types.NamespacedName{
		Name:      "test-certs-replicas",
		Namespace: "test-replicas",
	}, secret)
	if err != nil {
		t.Fatalf("Expected Secret to be created by leader: %v", err)
	}

	// Leader also runs CertificateReloader to write certs to its own disk
	leaderReloader := &controllers.CertificateReloader{
		Client: k8sClient,
		Config: leaderCfg,
	}
	_, err = leaderReloader.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-certs-replicas",
			Namespace: "test-replicas",
		},
	})
	if err != nil {
		t.Fatalf("Leader reloader reconcile failed: %v", err)
	}

	// Simulate follower replica - only reloads certificates
	follower1TmpDir := t.TempDir()
	follower1Cfg := &config.Config{
		CertSecretName:      "test-certs-replicas",
		CertSecretNamespace: "test-replicas",
		CertDir:             follower1TmpDir,
	}

	follower1Reloader := &controllers.CertificateReloader{
		Client: k8sClient,
		Config: follower1Cfg,
	}

	_, err = follower1Reloader.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-certs-replicas",
			Namespace: "test-replicas",
		},
	})
	if err != nil {
		t.Fatalf("Follower 1 reloader reconcile failed: %v", err)
	}

	// Simulate another follower replica
	follower2TmpDir := t.TempDir()
	follower2Cfg := &config.Config{
		CertSecretName:      "test-certs-replicas",
		CertSecretNamespace: "test-replicas",
		CertDir:             follower2TmpDir,
	}

	follower2Reloader := &controllers.CertificateReloader{
		Client: k8sClient,
		Config: follower2Cfg,
	}

	_, err = follower2Reloader.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-certs-replicas",
			Namespace: "test-replicas",
		},
	})
	if err != nil {
		t.Fatalf("Follower 2 reloader reconcile failed: %v", err)
	}

	// Verify all replicas have certificates on disk
	for _, dir := range []string{leaderTmpDir, follower1TmpDir, follower2TmpDir} {
		for _, fileName := range []string{"ca.crt", "tls.crt", "tls.key"} {
			filePath := filepath.Join(dir, fileName)
			if _, err := os.Stat(filePath); os.IsNotExist(err) {
				t.Errorf("Expected certificate file %s to exist in %s", fileName, dir)
			}
		}
	}

	// Verify all replicas have the same certificate content
	leaderCert, _ := os.ReadFile(filepath.Join(leaderTmpDir, "tls.crt"))
	follower1Cert, _ := os.ReadFile(filepath.Join(follower1TmpDir, "tls.crt"))
	follower2Cert, _ := os.ReadFile(filepath.Join(follower2TmpDir, "tls.crt"))

	if string(leaderCert) != string(follower1Cert) || string(leaderCert) != string(follower2Cert) {
		t.Error("All replicas should have the same certificate content")
	}

	t.Log("Multi-replica behavior test passed!")
}
