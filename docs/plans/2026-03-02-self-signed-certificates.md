# Self-Signed Certificates Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace CA + server certificate with single self-signed certificate, reducing complexity and code size.

**Architecture:** Delete GenerateCA function, convert GenerateServerCert into GenerateSelfSignedCert that self-signs instead of using a CA. Update Secret structure to 2 files (tls.crt, tls.key). Use tls.crt as both server certificate and CA bundle.

**Tech Stack:** Go 1.26, crypto/x509, Kubernetes controller-runtime

---

## Task 1: Update certificate generator to use self-signed certificates

**Files:**
- Modify: `pkg/certificates/generator.go`
- Test: `pkg/certificates/generator_test.go`

**Step 1: Read existing generator.go and tests**

Run: `cat pkg/certificates/generator.go pkg/certificates/generator_test.go`

Expected: Understand current GenerateCA and GenerateServerCert implementations

**Step 2: Write failing test for GenerateSelfSignedCert**

In `pkg/certificates/generator_test.go`, add new test:

```go
func TestGenerateSelfSignedCert(t *testing.T) {
	services := []ServiceReference{
		{Name: "webhook-service", Namespace: "default"},
		{Hostname: "webhook.example.com"},
	}

	certPEM, keyPEM, err := GenerateSelfSignedCert(services, 1*time.Hour)
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert failed: %v", err)
	}

	if len(certPEM) == 0 {
		t.Error("certPEM is empty")
	}
	if len(keyPEM) == 0 {
		t.Error("keyPEM is empty")
	}

	// Parse and verify certificate
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	// Verify self-signed (issuer == subject)
	if cert.Issuer.CommonName != cert.Subject.CommonName {
		t.Errorf("certificate is not self-signed: issuer=%s, subject=%s",
			cert.Issuer.CommonName, cert.Subject.CommonName)
	}

	// Verify SANs
	expectedDNS := []string{
		"webhook-service.default.svc",
		"webhook-service.default.svc.cluster.local",
		"webhook.example.com",
	}

	if !reflect.DeepEqual(cert.DNSNames, expectedDNS) {
		t.Errorf("DNS SANs mismatch: got %v, want %v", cert.DNSNames, expectedDNS)
	}

	// Verify it's not a CA
	if cert.IsCA {
		t.Error("certificate should not be a CA")
	}

	// Verify key usage
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("missing KeyUsageDigitalSignature")
	}
	if cert.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Error("missing KeyUsageKeyEncipherment")
	}

	// Verify extended key usage
	found := false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			found = true
			break
		}
	}
	if !found {
		t.Error("missing ExtKeyUsageServerAuth")
	}
}
```

**Step 3: Run test to verify it fails**

Run: `go test -v ./pkg/certificates/ -run TestGenerateSelfSignedCert`

Expected: FAIL with "undefined: GenerateSelfSignedCert"

**Step 4: Implement GenerateSelfSignedCert function**

In `pkg/certificates/generator.go`, add after existing functions:

```go
// GenerateSelfSignedCert generates a self-signed certificate and private key
func GenerateSelfSignedCert(services []ServiceReference, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	// Generate private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	// Build DNS SANs from service references
	dnsNames := make([]string, 0, len(services)*2)
	for _, svc := range services {
		if svc.Hostname != "" {
			// URL-based webhook - use hostname directly
			dnsNames = append(dnsNames, svc.Hostname)
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
		NotBefore:             now,
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	// Self-sign the certificate (template is both cert and parent)
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create self-signed certificate: %w", err)
	}

	// Encode to PEM
	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	return certPEM, keyPEM, nil
}
```

**Step 5: Run test to verify it passes**

Run: `go test -v ./pkg/certificates/ -run TestGenerateSelfSignedCert`

Expected: PASS

**Step 6: Delete GenerateCA function and TestGenerateCA**

In `pkg/certificates/generator.go`, delete lines 32-77 (entire GenerateCA function)

In `pkg/certificates/generator_test.go`, delete TestGenerateCA function

Run: `go test ./pkg/certificates/`

Expected: Tests still pass (GenerateCA not used yet)

**Step 7: Delete GenerateServerCert function and update tests**

In `pkg/certificates/generator.go`, delete lines 79-159 (entire GenerateServerCert function)

In `pkg/certificates/generator_test.go`:
- Rename `TestGenerateServerCert` to `TestGenerateSelfSignedCert_Basic`
- Update to call `GenerateSelfSignedCert` instead of GenerateCA + GenerateServerCert
- Remove CA parsing logic
- Keep SAN verification

Example update:
```go
func TestGenerateSelfSignedCert_Basic(t *testing.T) {
	services := []ServiceReference{
		{Name: "webhook-service", Namespace: "default"},
	}

	certPEM, keyPEM, err := GenerateSelfSignedCert(services, 1*time.Hour)
	// ... rest of test
}
```

Similarly update:
- `TestGenerateServerCert_WithHostnames` → `TestGenerateSelfSignedCert_WithHostnames`
- `TestGenerateServerCert_MixedServiceAndHostname` → `TestGenerateSelfSignedCert_MixedServiceAndHostname`

**Step 8: Run all certificate tests**

Run: `go test -v ./pkg/certificates/`

Expected: All tests PASS

**Step 9: Commit certificate generator changes**

```bash
git add pkg/certificates/generator.go pkg/certificates/generator_test.go
git commit -m "refactor: replace CA + server cert with self-signed certificate

- Delete GenerateCA() function (32-77 lines removed)
- Delete GenerateServerCert() function (79-159 lines removed)
- Add GenerateSelfSignedCert() function that self-signs instead of using CA
- Update all tests to use new function
- Certificate self-validates: issuer == subject
- Net reduction: ~100 lines to ~60 lines

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 2: Update WebhookReconciler to use self-signed certificates

**Files:**
- Modify: `pkg/controllers/reconciler.go`
- Test: `pkg/controllers/reconciler_test.go`

**Step 1: Find generateCertificates function**

Run: `grep -n "func (r \*WebhookReconciler) generateCertificates" pkg/controllers/reconciler.go`

Expected: Find line number of function

**Step 2: Update generateCertificates to use GenerateSelfSignedCert**

In `pkg/controllers/reconciler.go`, find `generateCertificates` function and replace CA generation logic:

Before:
```go
// Generate CA certificate
caCert, caKey, err := certificates.GenerateCA(r.CertValidity)
if err != nil {
	return fmt.Errorf("failed to generate CA: %w", err)
}

// Generate server certificate
serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, serviceRefs, r.CertValidity)
if err != nil {
	return fmt.Errorf("failed to generate server certificate: %w", err)
}
```

After:
```go
// Generate self-signed certificate
certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(serviceRefs, r.CertValidity)
if err != nil {
	return fmt.Errorf("failed to generate self-signed certificate: %w", err)
}
```

**Step 3: Update ensureCertificateSecret to store 2 files**

In same function, find Secret data assignment and update:

Before:
```go
secret.Data = map[string][]byte{
	"ca.crt":  caCert,
	"tls.crt": serverCert,
	"tls.key": serverKey,
}
```

After:
```go
secret.Type = corev1.SecretTypeTLS
secret.Data = map[string][]byte{
	"tls.crt": certPEM,
	"tls.key": keyPEM,
}
```

**Step 4: Update checkCertificates to read 2 files**

Find `checkCertificates` function, update to read only tls.crt:

Before:
```go
caCert := secret.Data["ca.crt"]
serverCert := secret.Data["tls.crt"]

if len(caCert) == 0 || len(serverCert) == 0 {
	return false, ""
}

// Parse server certificate
block, _ := pem.Decode(serverCert)
```

After:
```go
certPEM := secret.Data["tls.crt"]

if len(certPEM) == 0 {
	return false, ""
}

// Parse certificate
block, _ := pem.Decode(certPEM)
```

**Step 5: Update applyWebhooksToTarget to inject tls.crt as CA bundle**

Find `applyWebhooksToTarget` function:

Before:
```go
caCert := secret.Data["ca.crt"]
webhooks.InjectCABundle(configs, caCert)
```

After:
```go
certPEM := secret.Data["tls.crt"]
webhooks.InjectCABundle(configs, certPEM)
```

**Step 6: Build to verify compilation**

Run: `go build ./pkg/controllers/`

Expected: Successful compilation

**Step 7: Update reconciler tests to expect 2 files**

In `pkg/controllers/reconciler_test.go`, find all Secret creation/expectations and update:

Before:
```go
Data: map[string][]byte{
	"ca.crt":  []byte("fake-ca-cert"),
	"tls.crt": []byte("fake-server-cert"),
	"tls.key": []byte("fake-server-key"),
},
```

After:
```go
Type: corev1.SecretTypeTLS,
Data: map[string][]byte{
	"tls.crt": []byte("fake-cert"),
	"tls.key": []byte("fake-key"),
},
```

Update all test cases that check Secret data.

**Step 8: Run reconciler tests**

Run: `go test -v ./pkg/controllers/ -run TestWebhookReconciler`

Expected: All tests PASS

**Step 9: Commit reconciler changes**

```bash
git add pkg/controllers/reconciler.go pkg/controllers/reconciler_test.go
git commit -m "refactor: update WebhookReconciler for self-signed certificates

- Use GenerateSelfSignedCert instead of GenerateCA + GenerateServerCert
- Store 2 files in Secret (tls.crt, tls.key) instead of 3
- Change Secret type to kubernetes.io/tls (standard TLS Secret)
- Inject tls.crt as CA bundle instead of ca.crt
- Update all tests to expect 2-file Secret structure

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 3: Update CertificateReloader to write 2 files

**Files:**
- Modify: `pkg/controllers/reloader.go`
- Test: `pkg/controllers/reloader_test.go`

**Step 1: Find syncCertificatesToDisk function**

Run: `grep -n "func (r \*CertificateReloader) syncCertificatesToDisk" pkg/controllers/reloader.go`

Expected: Find line number of function

**Step 2: Update syncCertificatesToDisk to write 2 files**

In `pkg/controllers/reloader.go`, find file writing logic:

Before:
```go
caCert := secret.Data["ca.crt"]
serverCert := secret.Data["tls.crt"]
serverKey := secret.Data["tls.key"]

if len(caCert) == 0 || len(serverCert) == 0 || len(serverKey) == 0 {
	return fmt.Errorf("secret missing required keys")
}

files := map[string][]byte{
	"ca.crt":  caCert,
	"tls.crt": serverCert,
	"tls.key": serverKey,
}
```

After:
```go
certPEM := secret.Data["tls.crt"]
keyPEM := secret.Data["tls.key"]

if len(certPEM) == 0 || len(keyPEM) == 0 {
	return fmt.Errorf("secret missing required keys: tls.crt and tls.key")
}

files := map[string][]byte{
	"tls.crt": certPEM,
	"tls.key": keyPEM,
}
```

**Step 3: Build to verify compilation**

Run: `go build ./pkg/controllers/`

Expected: Successful compilation

**Step 4: Update reloader tests**

In `pkg/controllers/reloader_test.go`, update all Secret fixtures:

Before:
```go
Data: map[string][]byte{
	"ca.crt":  []byte("fake-ca"),
	"tls.crt": []byte("fake-cert"),
	"tls.key": []byte("fake-key"),
},
```

After:
```go
Type: corev1.SecretTypeTLS,
Data: map[string][]byte{
	"tls.crt": []byte("fake-cert"),
	"tls.key": []byte("fake-key"),
},
```

Update file existence checks to not expect ca.crt.

**Step 5: Run reloader tests**

Run: `go test -v ./pkg/controllers/ -run TestCertificateReloader`

Expected: All tests PASS

**Step 6: Commit reloader changes**

```bash
git add pkg/controllers/reloader.go pkg/controllers/reloader_test.go
git commit -m "refactor: update CertificateReloader to write 2 files

- Write only tls.crt and tls.key to disk (not ca.crt)
- Update error messages to reflect 2-file structure
- Update all tests to expect 2 files
- Webhook servers get same certificates, no ca.crt orphaned

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 4: Update e2e tests

**Files:**
- Modify: `test/e2e/integration_test.go`

**Step 1: Find Secret verification in e2e tests**

Run: `grep -n "ca.crt" test/e2e/integration_test.go`

Expected: Find references to ca.crt in test assertions

**Step 2: Update Secret expectations in e2e tests**

In `test/e2e/integration_test.go`, update all Secret checks:

Before:
```go
if len(secret.Data["ca.crt"]) == 0 {
	return fmt.Errorf("ca.crt is empty")
}
if len(secret.Data["tls.crt"]) == 0 {
	return fmt.Errorf("tls.crt is empty")
}
```

After:
```go
if secret.Type != corev1.SecretTypeTLS {
	return fmt.Errorf("secret type should be kubernetes.io/tls, got %s", secret.Type)
}
if len(secret.Data["tls.crt"]) == 0 {
	return fmt.Errorf("tls.crt is empty")
}
```

Remove all ca.crt checks.

**Step 3: Run e2e tests**

Run: `gmake test-e2e`

Expected: All e2e tests PASS (may take ~2 minutes)

**Step 4: Commit e2e test changes**

```bash
git add test/e2e/integration_test.go
git commit -m "test: update e2e tests for self-signed certificates

- Expect Secret type kubernetes.io/tls instead of Opaque
- Remove ca.crt verification checks
- Verify only tls.crt and tls.key are present
- Tests confirm webhook requests work with self-signed certs

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 5: Update documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

**Step 1: Update README.md Certificate Output section**

Find "Certificate Output" section in README.md and update:

Before:
```markdown
Files written to `--cert-dir`:
- `ca.crt`: CA certificate (PEM)
- `tls.crt`: Server certificate with dynamic SANs (PEM)
- `tls.key`: Server private key (PEM)
```

After:
```markdown
Files written to `--cert-dir`:
- `tls.crt`: Self-signed certificate with dynamic SANs (PEM)
- `tls.key`: Private key (PEM)

Note: The same certificate is used as both the server certificate and CA bundle for validation.
```

**Step 2: Update CLAUDE.md Certificate Lifecycle section**

Find "Certificate Lifecycle" section in CLAUDE.md and update:

Before description of generation:
```markdown
**Generation triggers:**
1. ConfigMap changes (new webhook definitions)
2. Time-based rotation: remaining validity < threshold × total validity (default: 80%)
3. Config-based rotation: Service references or URLs change (new SANs required)
4. Manual rotation: Force-rotation annotation on Secret

**Certificate SANs:**
- Service-based webhooks: `<service>.<namespace>.svc`, `<service>.<namespace>.svc.cluster.local`
- URL-based webhooks: hostname extracted from URL

**Storage flow:**
1. WebhookReconciler generates CA + server certs → stores in Secret (hosting cluster)
2. CertificateReloader watches Secret → writes to `/tmp/webhook-certs/`
3. Webhook server watches filesystem → automatically reloads
```

After:
```markdown
**Generation triggers:**
1. ConfigMap changes (new webhook definitions)
2. Time-based rotation: remaining validity < threshold × total validity (default: 80%)
3. Config-based rotation: Service references or URLs change (new SANs required)
4. Manual rotation: Force-rotation annotation on Secret

**Certificate approach:**
- Single self-signed certificate (not CA + server cert)
- Same certificate used for serving and validation (validates itself)
- Simpler Secret structure: 2 files (tls.crt, tls.key)

**Certificate SANs:**
- Service-based webhooks: `<service>.<namespace>.svc`, `<service>.<namespace>.svc.cluster.local`
- URL-based webhooks: hostname extracted from URL

**Storage flow:**
1. WebhookReconciler generates self-signed cert → stores in Secret (hosting cluster)
2. CertificateReloader watches Secret → writes to `/tmp/webhook-certs/`
3. Webhook server watches filesystem → automatically reloads
```

**Step 3: Commit documentation changes**

```bash
git add README.md CLAUDE.md
git commit -m "docs: update for self-signed certificate architecture

- Remove references to CA certificate
- Document 2-file Secret structure (tls.crt, tls.key)
- Explain self-signed certificate approach
- Update certificate output section in README
- Update certificate lifecycle section in CLAUDE.md

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 6: Final verification

**Files:** None (verification only)

**Step 1: Run all unit tests**

Run: `go test ./...`

Expected: All tests PASS

**Step 2: Run static checks**

Run: `gmake static-check`

Expected: No errors

**Step 3: Build binary**

Run: `gmake build`

Expected: Binary builds successfully at `build/webhook-injector`

**Step 4: Verify help output**

Run: `./build/webhook-injector --help | grep -A2 cert`

Expected: See cert-related flags with correct defaults

**Step 5: Check code size reduction**

Run: `wc -l pkg/certificates/generator.go`

Expected: ~100 lines (down from ~159 lines)

**Step 6: Verify Secret type in code**

Run: `grep -r "SecretTypeTLS" pkg/`

Expected: Find references in reconciler.go and tests

**Step 7: Summary check**

Review completion criteria:
- [ ] GenerateCA function removed ✓
- [ ] GenerateSelfSignedCert function implemented ✓
- [ ] Secret structure changed to 2 files ✓
- [ ] Secret type changed to kubernetes.io/tls ✓
- [ ] CA bundle injection uses tls.crt ✓
- [ ] CertificateReloader writes 2 files ✓
- [ ] All unit tests passing ✓
- [ ] E2E tests passing ✓
- [ ] Documentation updated ✓

**Step 8: Final commit summary**

Run: `git log --oneline | head -6`

Expected: See 6 commits for this feature

---

## Completion

All tasks complete! Self-signed certificate implementation is done.

**Summary of changes:**
- ✅ Deleted GenerateCA function (~45 lines)
- ✅ Deleted GenerateServerCert function (~80 lines)
- ✅ Added GenerateSelfSignedCert function (~60 lines)
- ✅ Net code reduction: ~65 lines
- ✅ Secret structure simplified: 3 files → 2 files
- ✅ Secret type standardized: Opaque → kubernetes.io/tls
- ✅ Same security properties maintained
- ✅ All tests passing

**Verification commands:**
```bash
go test ./...              # All tests pass
gmake build               # Binary builds
gmake test-e2e            # E2E tests pass (optional)
wc -l pkg/certificates/generator.go  # ~100 lines (was ~159)
```
