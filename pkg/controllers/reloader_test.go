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
	_ = corev1.AddToScheme(scheme)

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
		CertSecretName:      "test-certs",
		CertSecretNamespace: "default",
		CertDir:             tmpDir,
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
