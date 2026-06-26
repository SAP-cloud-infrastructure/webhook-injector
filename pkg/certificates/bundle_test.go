// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package certificates

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// generateTestCert returns a freshly minted self-signed CERTIFICATE PEM
// and its matching RSA private key PEM. Used by the bundle tests as a
// lightweight stand-in for GenerateSelfSignedCert — that helper drags in
// ServiceReference plumbing we don't need here.
func generateTestCert(t *testing.T, cn string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

func TestParseChainRoundTrip(t *testing.T) {
	// Two distinct certs concatenated. ParseChain must return them in
	// document order, and EncodeChain must be the byte-exact inverse on
	// the parsed result.
	certA, _ := generateTestCert(t, "cert-A")
	certB, _ := generateTestCert(t, "cert-B")
	bundle := append(append([]byte{}, certA...), certB...)

	parsed, err := ParseChain(bundle)
	if err != nil {
		t.Fatalf("ParseChain: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("ParseChain: got %d certs, want 2", len(parsed))
	}
	if parsed[0].Subject.CommonName != "cert-A" || parsed[1].Subject.CommonName != "cert-B" {
		t.Fatalf("order wrong: %q, %q", parsed[0].Subject.CommonName, parsed[1].Subject.CommonName)
	}

	reencoded := EncodeChain(parsed)
	if !bytes.Equal(reencoded, bundle) {
		t.Fatalf("EncodeChain not byte-exact inverse of ParseChain:\n got=%q\nwant=%q", reencoded, bundle)
	}
}

func TestParseChainSkipsNonCertificateBlocks(t *testing.T) {
	// A PEM stream with an inlined private key between two certs: the
	// key block must be skipped silently and both certs returned.
	certA, keyA := generateTestCert(t, "cert-A")
	certB, _ := generateTestCert(t, "cert-B")
	bundle := append(append(append([]byte{}, certA...), keyA...), certB...)

	parsed, err := ParseChain(bundle)
	if err != nil {
		t.Fatalf("ParseChain: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expected 2 certs (key block skipped), got %d", len(parsed))
	}
}

func TestParseChainGarbageReturnsEmpty(t *testing.T) {
	// Non-PEM bytes are a legitimate "empty Secret" state. Must NOT
	// error — callers want to treat this as "no certs yet" and continue.
	got, err := ParseChain([]byte("not pem at all"))
	if err != nil {
		t.Fatalf("ParseChain on garbage: unexpected error %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ParseChain on garbage: expected empty, got %d certs", len(got))
	}

	// Empty input: same contract.
	got, err = ParseChain(nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("ParseChain(nil): got %d certs, err=%v; want 0, nil", len(got), err)
	}
}

func TestParseChainErrorsOnCorruptCertBlock(t *testing.T) {
	// A CERTIFICATE PEM block whose DER body is garbage. We treat this
	// as corruption (not absence), so a non-nil error is correct.
	corrupt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not der")})
	if _, err := ParseChain(corrupt); err == nil {
		t.Fatal("ParseChain on corrupt CERTIFICATE block: expected error, got nil")
	}
}

func TestEncodeChainEmpty(t *testing.T) {
	// Empty / nil input → nil output. The caller writes this into a
	// Secret data field; nil-vs-empty consistency matters.
	if got := EncodeChain(nil); got != nil {
		t.Fatalf("EncodeChain(nil): got %q, want nil", got)
	}
	if got := EncodeChain([]*x509.Certificate{}); got != nil {
		t.Fatalf("EncodeChain([]): got %q, want nil", got)
	}
}

func TestLeafCertificatePicksMatchingCert(t *testing.T) {
	// Two cert+key pairs. Build a bundle containing both certs. Provide
	// the SECOND key. LeafCertificate must return the second cert.
	certA, _ := generateTestCert(t, "cert-A")
	certB, keyB := generateTestCert(t, "cert-B")
	bundle := append(append([]byte{}, certA...), certB...)

	leaf, err := LeafCertificate(bundle, keyB)
	if err != nil {
		t.Fatalf("LeafCertificate: %v", err)
	}
	if leaf.Subject.CommonName != "cert-B" {
		t.Fatalf("wrong leaf: got CN=%q, want cert-B", leaf.Subject.CommonName)
	}

	// Sanity: the same bundle with key A should pick cert A.
	_, keyA := generateTestCert(t, "cert-A-fresh") // fresh key, won't match anything in bundle
	if _, err := LeafCertificate(bundle, keyA); err == nil {
		t.Fatal("LeafCertificate with non-matching key: expected error, got nil")
	}
}

func TestLeafCertificateErrors(t *testing.T) {
	cert, key := generateTestCert(t, "cert-A")

	// Empty bundle.
	if _, err := LeafCertificate(nil, key); err == nil {
		t.Fatal("empty tls.crt: expected error, got nil")
	}
	// Non-PEM key.
	if _, err := LeafCertificate(cert, []byte("garbage")); err == nil {
		t.Fatal("non-PEM tls.key: expected error, got nil")
	}
	// PEM key with bad DER.
	bad := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("nonsense")})
	if _, err := LeafCertificate(cert, bad); err == nil {
		t.Fatal("corrupt tls.key: expected error, got nil")
	}
}
