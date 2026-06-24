// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package certificates

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// GenerateSelfSignedCert generates a self-signed server certificate and private key
func GenerateSelfSignedCert(services []ServiceReference, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	// Generate private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Build DNS SANs and IP SANs from service references
	dnsNames := make([]string, 0, len(services)*2)
	ipAddresses := make([]net.IP, 0)
	for _, svc := range services {
		if svc.Hostname != "" {
			// URL-based webhook - check if it's an IP address or a DNS hostname
			if ip := net.ParseIP(svc.Hostname); ip != nil {
				ipAddresses = append(ipAddresses, ip)
			} else {
				dnsNames = append(dnsNames, svc.Hostname)
			}
		} else if svc.Name != "" && svc.Namespace != "" {
			// Service-based webhook - generate Kubernetes service DNS names
			dnsNames = append(dnsNames,
				fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace),
				fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace),
			)
		}
	}

	// Create certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %w", err)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "webhook-injector",
		},
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
		NotBefore:             now,
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// Self-sign the certificate (template is both cert and parent)
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create certificate: %w", err)
	}

	// Encode to PEM
	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  pemBlockCertificate,
		Bytes: certDER,
	})

	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  pemBlockRSAPrivateKey,
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	return certPEM, keyPEM, nil
}
