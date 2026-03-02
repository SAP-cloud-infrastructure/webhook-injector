# Self-Signed Certificates - Remove CA Certificate Generation

**Date:** 2026-03-02
**Status:** Approved

## Problem

Current implementation generates both a CA certificate and a server certificate signed by that CA:

```
1. Generate CA cert + key
2. Generate server cert signed by CA cert
3. Store 3 files in Secret: ca.crt, tls.crt, tls.key
4. Inject ca.crt into webhook configurations as CABundle
5. Webhook server uses tls.crt + tls.key
```

**Issues:**
- CA certificate provides zero value since we regenerate both CA and server cert together
- Extra complexity: ~100 lines of CA generation code
- More files to manage: 3 instead of 2
- Harder to understand: why have a CA if we never issue multiple certs from it?

**The insight:** Since we regenerate everything atomically (both CA and server cert), having a separate CA is pure ceremony. A self-signed certificate works identically.

## Solution

Replace CA + server certificate with a single self-signed certificate that validates itself.

**Architecture:**
```
1. Generate self-signed cert + key
2. Store 2 files in Secret: tls.crt, tls.key
3. Inject tls.crt into webhook configurations as CABundle (same cert validates itself)
4. Webhook server uses tls.crt + tls.key
```

**How it works:**
- Certificate is self-signed (signed by its own private key)
- Same certificate is used as both server cert and CA bundle
- Kubernetes API server validates: "Is server cert signed by CA bundle?" → Yes (by itself)
- Security properties identical to current implementation

## Benefits

1. **Delete ~100 lines of code** - Remove entire `GenerateCA()` function
2. **Simpler Secret structure** - 2 files instead of 3 (standard Kubernetes TLS Secret)
3. **Clearer mental model** - One certificate, self-validates
4. **Same security** - Certificate rotation, SAN changes, time-based rotation all work identically
5. **Standard pattern** - Self-signed certs for webhook servers is common in Kubernetes

## Technical Details

### Certificate Generation

**Before (pkg/certificates/generator.go):**
```go
// Two functions, ~100 lines total
func GenerateCA(validity time.Duration) (certPEM, keyPEM []byte, err error)
func GenerateServerCert(caCert, caKey []byte, serviceRefs []ServiceReference, validity time.Duration) (certPEM, keyPEM []byte, err error)
```

**After:**
```go
// One function, ~60 lines
func GenerateSelfSignedCert(serviceRefs []ServiceReference, validity time.Duration) (certPEM, keyPEM []byte, err error)
```

**Implementation:**
1. Generate RSA private key (2048 bits)
2. Create x509.Certificate template with:
   - Subject: CN=webhook-injector
   - SANs: Extracted from serviceRefs (service DNS names + URL hostnames)
   - Validity: As specified
   - KeyUsage: KeyUsageDigitalSignature | KeyUsageKeyEncipherment
   - ExtKeyUsage: ExtKeyUsageServerAuth
   - IsCA: false (not a CA)
3. Self-sign: `x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)`
   - Note: template is used for both cert and parent (self-signed)
4. PEM encode cert and key
5. Return certPEM, keyPEM, nil

**Key difference:** Sign with own private key instead of CA's private key.

### Secret Structure

**Before:**
```yaml
apiVersion: v1
kind: Secret
type: Opaque
data:
  ca.crt: <base64 CA cert>
  tls.crt: <base64 server cert>
  tls.key: <base64 server key>
```

**After:**
```yaml
apiVersion: v1
kind: Secret
type: kubernetes.io/tls  # Standard TLS Secret type
data:
  tls.crt: <base64 self-signed cert>
  tls.key: <base64 private key>
```

**Benefits:**
- Standard Kubernetes TLS Secret type
- Simpler structure
- Less storage

### WebhookReconciler Changes

**File:** `pkg/controllers/reconciler.go`

**Function: generateCertificates()**

Before:
```go
caCert, caKey, err := certificates.GenerateCA(r.CertValidity)
// error handling
serverCert, serverKey, err := certificates.GenerateServerCert(caCert, caKey, serviceRefs, r.CertValidity)
// Store 3 files in Secret: ca.crt, tls.crt, tls.key
```

After:
```go
certPEM, keyPEM, err := certificates.GenerateSelfSignedCert(serviceRefs, r.CertValidity)
// Store 2 files in Secret: tls.crt, tls.key
```

**Function: ensureCertificateSecret()**

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

**Function: checkCertificates()**

Before:
```go
caCert := secret.Data["ca.crt"]
serverCert := secret.Data["tls.crt"]
// Parse and validate both
```

After:
```go
certPEM := secret.Data["tls.crt"]
// Parse and validate single cert
```

**Function: applyWebhooksToTarget()**

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

### CertificateReloader Changes

**File:** `pkg/controllers/reloader.go`

**Function: syncCertificatesToDisk()**

Before:
```go
// Write 3 files
caCert := secret.Data["ca.crt"]
serverCert := secret.Data["tls.crt"]
serverKey := secret.Data["tls.key"]

files := map[string][]byte{
    "ca.crt":  caCert,
    "tls.crt": serverCert,
    "tls.key": serverKey,
}

for filename, content := range files {
    // Compare existing content, write if changed
}
```

After:
```go
// Write 2 files
certPEM := secret.Data["tls.crt"]
keyPEM := secret.Data["tls.key"]

files := map[string][]byte{
    "tls.crt": certPEM,
    "tls.key": keyPEM,
}

for filename, content := range files {
    // Compare existing content, write if changed
}
```

**Benefits:**
- Fewer file operations
- Simpler comparison logic
- No ca.crt orphaned on disk

### Certificate Validation

**No changes needed** - Kubernetes API server validates identically:

Before:
1. API server connects to webhook server
2. Server presents `tls.crt` (signed by CA)
3. API server validates against `ca.crt` from ClientConfig.CABundle
4. Validation succeeds

After:
1. API server connects to webhook server
2. Server presents `tls.crt` (self-signed)
3. API server validates against `tls.crt` from ClientConfig.CABundle (same cert)
4. Validation succeeds (cert validates itself)

## Rotation Behavior

**No changes** - All rotation triggers work identically:

1. **Time-based rotation** (80% of validity)
   - Regenerate self-signed cert with same SANs
   - Update Secret with new cert
   - Update webhook configs with new cert as CABundle
   - Reloader syncs new cert to disk

2. **SAN-based rotation** (service refs change)
   - Regenerate self-signed cert with new SANs
   - Update Secret with new cert
   - Update webhook configs with new cert as CABundle
   - Reloader syncs new cert to disk

3. **Force rotation** (annotation on Secret)
   - Same as above

**Atomicity preserved:** Secret and webhook configs updated together in same reconciliation loop.

## Migration Impact

**For running deployments:**

Migration is automatic - next certificate rotation will:
1. Generate self-signed cert instead of CA + server cert
2. Update Secret to 2-file format
3. Old `ca.crt` remains in Secret but is unused
4. Webhook server continues working (uses `tls.crt` and `tls.key`)

**Optional cleanup:** Manually remove `ca.crt` from Secret after rotation.

**For new deployments:**
- Secret created with 2 files from the start
- No ca.crt ever created

## Testing Strategy

**Unit tests:**
- `pkg/certificates/generator_test.go`
  - Remove `TestGenerateCA`
  - Rename `TestGenerateServerCert` → `TestGenerateSelfSignedCert`
  - Test that cert is self-signed (issuer == subject)
  - Test that SANs are correct

- `pkg/controllers/reconciler_test.go`
  - Update all Secret expectations (2 files not 3)
  - Verify self-signed cert in Secret
  - Test rotation with self-signed certs

- `pkg/controllers/reloader_test.go`
  - Update file write expectations (2 files not 3)
  - Verify ca.crt is not written

**E2E tests:**
- `test/e2e/integration_test.go`
  - Verify webhook requests succeed with self-signed cert
  - Test certificate rotation
  - Verify all replicas reload correctly

**Manual testing:**
- Deploy to kind cluster
- Verify webhook requests work
- Force rotation, verify new cert works
- Check Secret structure (2 files)
- Check filesystem (no ca.crt)

## Files to Change

**Delete:** None (functions removed, not files)

**Modify:**
- `pkg/certificates/generator.go` - Remove GenerateCA, simplify GenerateServerCert → GenerateSelfSignedCert
- `pkg/certificates/generator_test.go` - Update tests for self-signed certs
- `pkg/controllers/reconciler.go` - Update certificate generation, Secret structure, CA bundle injection
- `pkg/controllers/reconciler_test.go` - Update test expectations
- `pkg/controllers/reloader.go` - Remove ca.crt file writing
- `pkg/controllers/reloader_test.go` - Update test expectations
- `test/e2e/integration_test.go` - Update e2e test expectations
- `README.md` - Update Certificate Output section
- `CLAUDE.md` - Update Certificate Lifecycle section

## Completion Criteria

- [ ] GenerateCA function removed
- [ ] GenerateSelfSignedCert function implemented and tested
- [ ] Secret structure changed to 2 files (tls.crt, tls.key)
- [ ] Secret type changed to kubernetes.io/tls
- [ ] CA bundle injection uses tls.crt instead of ca.crt
- [ ] CertificateReloader writes 2 files, not 3
- [ ] All unit tests passing
- [ ] E2E tests passing
- [ ] Documentation updated
- [ ] Manual testing confirms webhooks work with self-signed certs

## Code Size Impact

**Before:**
- `pkg/certificates/generator.go`: ~159 lines
- Total certificate-related code: ~159 lines

**After:**
- `pkg/certificates/generator.go`: ~100 lines (estimated)
- Net reduction: ~60 lines

**Secret operations:**
- 3 files → 2 files (33% reduction)
- Simpler to understand and maintain
