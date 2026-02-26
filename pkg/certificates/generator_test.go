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

func TestGenerateServerCert_WithHostnames(t *testing.T) {
	// Generate CA
	caCert, caKey, err := GenerateCA(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	// Generate server cert with hostnames (URL-based webhooks)
	services := []ServiceReference{
		{Hostname: "webhook.example.com"},
		{Hostname: "another-webhook.test.local"},
	}

	serverCert, _, err := GenerateServerCert(caCert, caKey, services, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to generate server certificate: %v", err)
	}

	// Verify certificate
	block, _ := pem.Decode(serverCert)
	if block == nil {
		t.Fatal("Failed to decode server certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse server certificate: %v", err)
	}

	// Verify SANs contain hostnames
	expectedSANs := []string{
		"webhook.example.com",
		"another-webhook.test.local",
	}

	for _, expectedSAN := range expectedSANs {
		found := false
		for _, dnsName := range cert.DNSNames {
			if dnsName == expectedSAN {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected SAN '%s' not found in certificate. Found: %v", expectedSAN, cert.DNSNames)
		}
	}

	// Verify we have exactly 2 SANs (no extra service DNS names)
	if len(cert.DNSNames) != 2 {
		t.Errorf("Expected 2 SANs, got %d: %v", len(cert.DNSNames), cert.DNSNames)
	}
}

func TestGenerateServerCert_MixedServiceAndHostname(t *testing.T) {
	// Generate CA
	caCert, caKey, err := GenerateCA(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("Failed to generate CA: %v", err)
	}

	// Generate server cert with mixed service and hostname references
	services := []ServiceReference{
		{Name: "webhook-svc", Namespace: "default"},      // Service-based
		{Hostname: "external-webhook.example.com"},      // URL-based
		{Name: "another-svc", Namespace: "test-ns"},     // Service-based
		{Hostname: "second-external.test.local"},        // URL-based
	}

	serverCert, _, err := GenerateServerCert(caCert, caKey, services, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to generate server certificate: %v", err)
	}

	// Verify certificate
	block, _ := pem.Decode(serverCert)
	if block == nil {
		t.Fatal("Failed to decode server certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse server certificate: %v", err)
	}

	// Verify SANs contain both service DNS names and hostnames
	expectedSANs := []string{
		// Service-based SANs (2 formats each)
		"webhook-svc.default.svc",
		"webhook-svc.default.svc.cluster.local",
		"another-svc.test-ns.svc",
		"another-svc.test-ns.svc.cluster.local",
		// URL-based SANs
		"external-webhook.example.com",
		"second-external.test.local",
	}

	for _, expectedSAN := range expectedSANs {
		found := false
		for _, dnsName := range cert.DNSNames {
			if dnsName == expectedSAN {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected SAN '%s' not found in certificate. Found: %v", expectedSAN, cert.DNSNames)
		}
	}

	// Verify we have exactly 6 SANs (4 service SANs + 2 hostname SANs)
	if len(cert.DNSNames) != 6 {
		t.Errorf("Expected 6 SANs, got %d: %v", len(cert.DNSNames), cert.DNSNames)
	}
}
