// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package certificates

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
)

// PEM block types used across this package. Centralised here so callers
// don't sprinkle string literals through the cert plumbing.
const (
	pemBlockCertificate   = "CERTIFICATE"
	pemBlockRSAPrivateKey = "RSA PRIVATE KEY"
)

// ParseChain decodes one or more concatenated CERTIFICATE PEM blocks from
// pemBytes and returns the parsed certificates in document order. Non-
// CERTIFICATE PEM blocks (e.g. an inlined private key) are skipped. Bytes
// that don't parse as PEM at all yield an empty slice and a nil error so
// callers can treat "no certs" as a legitimate state rather than an
// exceptional one — useful when the cert Secret hasn't been written yet
// or when staged/active phases evolve the contents of tls.crt.
//
// A non-nil error is returned only if a CERTIFICATE block is present but
// its DER fails to parse — that signals corruption, not absence.
func ParseChain(pemBytes []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return certs, nil
		}
		if block.Type != pemBlockCertificate {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return certs, fmt.Errorf("parse CERTIFICATE block: %w", err)
		}
		certs = append(certs, cert)
	}
}

// EncodeChain emits a single byte slice containing one PEM CERTIFICATE
// block per cert, in input order. The exact inverse of ParseChain on
// well-formed input. An empty or nil slice returns nil so callers can use
// the result directly as a Secret data value without nil-vs-empty fuss.
func EncodeChain(certs []*x509.Certificate) []byte {
	if len(certs) == 0 {
		return nil
	}
	var out []byte
	for _, cert := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{
			Type:  pemBlockCertificate,
			Bytes: cert.Raw,
		})...)
	}
	return out
}

// LeafCertificate returns the cert in tlsCrtPEM whose public key matches
// the RSA private key in tlsKeyPEM. This is how WebhookReconciler answers
// "which of these certificates is currently being served?" during the
// staged/active phases, where tls.crt holds a concatenated bundle but
// tls.key is unambiguous.
//
// Errors when:
//   - tlsCrtPEM is empty or contains no parseable CERTIFICATE blocks,
//   - tlsKeyPEM cannot be parsed as an RSA PKCS1 private key (the only
//     key type GenerateSelfSignedCert emits), or
//   - none of the certs in the chain has a public key matching the
//     private key (a catastrophic misconfiguration — caller should treat
//     this as "regenerate from scratch" rather than try to recover).
func LeafCertificate(tlsCrtPEM, tlsKeyPEM []byte) (*x509.Certificate, error) {
	certs, err := ParseChain(tlsCrtPEM)
	if err != nil {
		return nil, fmt.Errorf("parse tls.crt: %w", err)
	}
	if len(certs) == 0 {
		return nil, errors.New("tls.crt contains no CERTIFICATE blocks")
	}

	keyBlock, _ := pem.Decode(tlsKeyPEM)
	if keyBlock == nil {
		return nil, errors.New("tls.key is not valid PEM")
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse tls.key: %w", err)
	}

	for _, cert := range certs {
		certPub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			continue
		}
		// Compare on (N, E) — the canonical representation of an RSA
		// public key. bytes.Equal on PKIX-marshalled keys would also
		// work but is slower and less direct.
		if certPub.N.Cmp(privateKey.N) == 0 && certPub.E == privateKey.E {
			return cert, nil
		}
	}
	return nil, errors.New("no certificate in tls.crt matches the public key of tls.key")
}
