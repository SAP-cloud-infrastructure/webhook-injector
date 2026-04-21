// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package certificates

import (
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"
	"time"
)

func TestGenerateSelfSignedCert(t *testing.T) {
	validity := 30 * 24 * time.Hour

	// Define service references
	services := []ServiceReference{
		{Name: "webhook-service", Namespace: "default"},
		{Hostname: "webhook.example.com"},
	}

	certPEM, keyPEM, err := GenerateSelfSignedCert(services, validity)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	// Verify certificate PEM
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatal("Failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse certificate: %v", err)
	}

	// Verify it's self-signed (issuer == subject)
	if cert.Issuer.CommonName != cert.Subject.CommonName {
		t.Errorf("Certificate is not self-signed: issuer='%s', subject='%s'",
			cert.Issuer.CommonName, cert.Subject.CommonName)
	}

	// Verify IsCA is false
	if cert.IsCA {
		t.Error("Certificate should not be a CA")
	}

	// Verify subject CN
	if cert.Subject.CommonName != "webhook-injector" {
		t.Errorf("Expected CN 'webhook-injector', got '%s'", cert.Subject.CommonName)
	}

	// Verify SANs
	expectedSANs := []string{
		"webhook-service.default.svc",
		"webhook-service.default.svc.cluster.local",
		"webhook.example.com",
	}

	for _, expectedSAN := range expectedSANs {
		found := slices.Contains(cert.DNSNames, expectedSAN)
		if !found {
			t.Errorf("Expected SAN '%s' not found in certificate. Found: %v", expectedSAN, cert.DNSNames)
		}
	}

	// Verify key usage
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("Certificate should have DigitalSignature key usage")
	}
	if cert.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Error("Certificate should have KeyEncipherment key usage")
	}

	// Verify extended key usage
	hasServerAuth := slices.Contains(cert.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	if !hasServerAuth {
		t.Error("Certificate should have ServerAuth extended key usage")
	}

	// Verify private key PEM
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatal("Failed to decode private key PEM")
	}
}

func TestGenerateSelfSignedCert_ServiceReferences(t *testing.T) {
	validity := 30 * 24 * time.Hour

	// Define service references
	services := []ServiceReference{
		{Name: "webhook-service", Namespace: "default"},
		{Name: "my-webhook", Namespace: "test-ns"},
	}

	certPEM, keyPEM, err := GenerateSelfSignedCert(services, validity)
	if err != nil {
		t.Fatalf("Failed to generate self-signed cert: %v", err)
	}

	// Verify certificate
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatal("Failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse certificate: %v", err)
	}

	// Check SANs
	expectedSANs := []string{
		"webhook-service.default.svc",
		"webhook-service.default.svc.cluster.local",
		"my-webhook.test-ns.svc",
		"my-webhook.test-ns.svc.cluster.local",
	}

	for _, expectedSAN := range expectedSANs {
		found := slices.Contains(cert.DNSNames, expectedSAN)
		if !found {
			t.Errorf("Expected SAN '%s' not found in certificate", expectedSAN)
		}
	}

	// Verify key
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatal("Failed to decode private key PEM")
	}
}

func TestGenerateSelfSignedCert_WithHostnames(t *testing.T) {
	// Generate cert with hostnames (URL-based webhooks)
	services := []ServiceReference{
		{Hostname: "webhook.example.com"},
		{Hostname: "another-webhook.test.local"},
	}

	certPEM, _, err := GenerateSelfSignedCert(services, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to generate certificate: %v", err)
	}

	// Verify certificate
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("Failed to decode certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse certificate: %v", err)
	}

	// Verify SANs contain hostnames
	expectedSANs := []string{
		"webhook.example.com",
		"another-webhook.test.local",
	}

	for _, expectedSAN := range expectedSANs {
		found := slices.Contains(cert.DNSNames, expectedSAN)
		if !found {
			t.Errorf("Expected SAN '%s' not found in certificate. Found: %v", expectedSAN, cert.DNSNames)
		}
	}

	// Verify we have exactly 2 SANs (no extra service DNS names)
	if len(cert.DNSNames) != 2 {
		t.Errorf("Expected 2 SANs, got %d: %v", len(cert.DNSNames), cert.DNSNames)
	}
}

func TestGenerateSelfSignedCert_MixedServiceAndHostname(t *testing.T) {
	// Generate cert with mixed service and hostname references
	services := []ServiceReference{
		{Name: "webhook-svc", Namespace: "default"}, // Service-based
		{Hostname: "external-webhook.example.com"},  // URL-based
		{Name: "another-svc", Namespace: "test-ns"}, // Service-based
		{Hostname: "second-external.test.local"},    // URL-based
	}

	certPEM, _, err := GenerateSelfSignedCert(services, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Failed to generate certificate: %v", err)
	}

	// Verify certificate
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("Failed to decode certificate")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("Failed to parse certificate: %v", err)
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
		found := slices.Contains(cert.DNSNames, expectedSAN)
		if !found {
			t.Errorf("Expected SAN '%s' not found in certificate. Found: %v", expectedSAN, cert.DNSNames)
		}
	}

	// Verify we have exactly 6 SANs (4 service SANs + 2 hostname SANs)
	if len(cert.DNSNames) != 6 {
		t.Errorf("Expected 6 SANs, got %d: %v", len(cert.DNSNames), cert.DNSNames)
	}
}
