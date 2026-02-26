package certificates

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestGenerateCA(t *testing.T) {
	validity := 30 * 24 * time.Hour

	caCert, caKey, err := GenerateCA(validity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	// Verify CA cert
	block, _ := pem.Decode(caCert)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("Failed to decode CA certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse CA certificate: %v", err)
	}

	if !cert.IsCA {
		t.Error("Certificate is not a CA")
	}
	if cert.Subject.CommonName != "webhook-injector-ca" {
		t.Errorf("Expected CN 'webhook-injector-ca', got '%s'", cert.Subject.CommonName)
	}

	// Verify CA key
	keyBlock, _ := pem.Decode(caKey)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatal("Failed to decode CA private key PEM")
	}
}

func TestGenerateServerCert(t *testing.T) {
	validity := 30 * 24 * time.Hour

	// Generate CA first
	caCertPEM, caKeyPEM, err := GenerateCA(validity)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	// Define service references
	services := []ServiceReference{
		{Name: "webhook-service", Namespace: "default"},
		{Name: "my-webhook", Namespace: "test-ns"},
	}

	serverCert, serverKey, err := GenerateServerCert(caCertPEM, caKeyPEM, services, validity)
	if err != nil {
		t.Fatalf("Failed to generate server cert: %v", err)
	}

	// Verify server cert
	block, _ := pem.Decode(serverCert)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("Failed to decode server certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse server certificate: %v", err)
	}

	// Check SANs
	expectedSANs := []string{
		"webhook-service.default.svc",
		"webhook-service.default.svc.cluster.local",
		"my-webhook.test-ns.svc",
		"my-webhook.test-ns.svc.cluster.local",
	}

	for _, expectedSAN := range expectedSANs {
		found := false
		for _, san := range cert.DNSNames {
			if san == expectedSAN {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected SAN '%s' not found in certificate", expectedSAN)
		}
	}

	// Verify server key
	keyBlock, _ := pem.Decode(serverKey)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatal("Failed to decode server private key PEM")
	}
}
