<!--
SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company

SPDX-License-Identifier: Apache-2.0
-->

# Target-Cluster Patch Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add opt-in target-cluster patch mode (`--target-label`) that watches labeled CRD/MWC/VWC on the target cluster and keeps their `caBundle` in sync, make `--webhook-config-name` optional, and change nothing about existing ConfigMap/ManagedResource behavior.

**Architecture:** Extract shared caBundle-stamp helpers. Add a leader-only `TargetWebhookReconciler` running three controllers (`target-crd`, `target-mwc`, `target-vwc`) on a second, label-scoped `cluster.Cluster` handle against the target cluster. It patches only `caBundle` via MergeFrom, publishes stamped CRD names to the shared rotation gate, and re-stamps on rotation fan-out. `--webhook-config-name` becomes optional (at least one of it or `--target-label` required); `WebhookReconciler` runs a cert-only mode driven by `--cert-sans` when the ConfigMap is absent. `WebhookReconciler`'s single rotation trigger becomes a slice. Unset `--target-label` → no target cache, no new reconciler, byte-for-byte unchanged behavior.

**Tech Stack:** Go 1.26, controller-runtime v0.23.1 (`cluster.New`, `source.Kind`), k8s.io/apiextensions-apiserver, k8s.io/api/admissionregistration/v1, crypto/x509, Prometheus client_golang.

**Spec:** `docs/plans/2026-07-15-target-patch-mode-design.md`

**Backward-compat invariant (verify after every task):** `rg -n "RotationTrigger[^s]" pkg/ cmd/ test/` returns nothing, and `go test ./pkg/controllers/ -run 'TestManagedResource|TestWebhookReconciler|TestForceRotation|TestIsRotationInFlight'` stays green.

---

## Task 1: Extract shared caBundle-stamp helpers (refactor, no behavior change)

**Files:**
- Modify: `pkg/controllers/managed_resource_reconciler.go` (`maybeStampCRDDoc`, lines ~508-564)
- Create: `pkg/controllers/stamp.go`
- Test: `pkg/controllers/stamp_test.go`

- [ ] **Step 1: Read the current stamp logic**

Run: `sed -n '505,565p' pkg/controllers/managed_resource_reconciler.go`

Expected: See the inline `caChanged := !bytes.Equal(cc.CABundle, caCert)` + `cc.CABundle = caCert` logic inside `maybeStampCRDDoc`.

- [ ] **Step 2: Write failing tests for the shared helpers**

Create `pkg/controllers/stamp_test.go`:

```go
// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestStampCRDConversionCABundle(t *testing.T) {
	ca := []byte("ca-bytes")
	webhookCRD := func(existing []byte) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Conversion: &apiextensionsv1.CustomResourceConversion{
					Strategy: apiextensionsv1.WebhookConverter,
					Webhook: &apiextensionsv1.WebhookConversion{
						ClientConfig: &apiextensionsv1.WebhookClientConfig{CABundle: existing},
					},
				},
			},
		}
	}

	if got := stampCRDConversionCABundle(webhookCRD(nil), ca); !got {
		t.Error("expected changed=true when caBundle empty")
	}
	if got := stampCRDConversionCABundle(webhookCRD(ca), ca); got {
		t.Error("expected changed=false when already converged")
	}
	none := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter},
		},
	}
	if got := stampCRDConversionCABundle(none, ca); got {
		t.Error("expected changed=false for strategy=None")
	}
}

func TestStampMutatingCABundle(t *testing.T) {
	ca := []byte("ca-bytes")
	mwc := &admissionv1.MutatingWebhookConfiguration{
		Webhooks: []admissionv1.MutatingWebhook{
			{ClientConfig: admissionv1.WebhookClientConfig{CABundle: nil}},
			{ClientConfig: admissionv1.WebhookClientConfig{CABundle: ca}},
		},
	}
	if got := stampMutatingCABundle(mwc, ca); !got {
		t.Error("expected changed=true when one webhook has stale caBundle")
	}
	if got := stampMutatingCABundle(mwc, ca); got {
		t.Error("expected changed=false on second pass (converged)")
	}
}

func TestStampValidatingCABundle(t *testing.T) {
	ca := []byte("ca-bytes")
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		Webhooks: []admissionv1.ValidatingWebhook{
			{ClientConfig: admissionv1.WebhookClientConfig{CABundle: nil}},
		},
	}
	if got := stampValidatingCABundle(vwc, ca); !got {
		t.Error("expected changed=true")
	}
	if got := stampValidatingCABundle(vwc, ca); got {
		t.Error("expected changed=false on second pass")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./pkg/controllers/ -run 'TestStamp' -v`

Expected: FAIL with "undefined: stampCRDConversionCABundle" (and the two admission helpers).

- [ ] **Step 4: Create the shared helpers**

Create `pkg/controllers/stamp.go`:

```go
// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// stampCRDConversionCABundle sets caBundle on a CRD's conversion webhook
// clientConfig when it differs from caCert. Returns changed=false when the
// CRD has no Webhook conversion strategy, has no clientConfig, or is already
// converged. Pure: mutates crd in place, no I/O, no YAML (de)serialization.
// Shared by the MR-Secret path (maybeStampCRDDoc, which wraps this with YAML
// round-tripping and the optional Service→URL rewrite) and the direct-target
// path (TargetWebhookReconciler, on the typed object it Gets from the target).
func stampCRDConversionCABundle(crd *apiextensionsv1.CustomResourceDefinition, caCert []byte) (changed bool) {
	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter ||
		conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		return false
	}
	cc := conv.Webhook.ClientConfig
	if bytes.Equal(cc.CABundle, caCert) {
		return false
	}
	cc.CABundle = caCert
	return true
}

// stampMutatingCABundle sets caBundle on every webhook entry of a
// MutatingWebhookConfiguration whose current caBundle differs from caCert.
// Returns changed=true if at least one entry was updated. Pure.
func stampMutatingCABundle(mwc *admissionv1.MutatingWebhookConfiguration, caCert []byte) (changed bool) {
	for i := range mwc.Webhooks {
		if !bytes.Equal(mwc.Webhooks[i].ClientConfig.CABundle, caCert) {
			mwc.Webhooks[i].ClientConfig.CABundle = caCert
			changed = true
		}
	}
	return changed
}

// stampValidatingCABundle is the ValidatingWebhookConfiguration analogue of
// stampMutatingCABundle. The webhook slice element type differs, so this is a
// separate typed function rather than a generic.
func stampValidatingCABundle(vwc *admissionv1.ValidatingWebhookConfiguration, caCert []byte) (changed bool) {
	for i := range vwc.Webhooks {
		if !bytes.Equal(vwc.Webhooks[i].ClientConfig.CABundle, caCert) {
			vwc.Webhooks[i].ClientConfig.CABundle = caCert
			changed = true
		}
	}
	return changed
}
```

- [ ] **Step 5: Re-point `maybeStampCRDDoc` at the shared CRD helper**

In `pkg/controllers/managed_resource_reconciler.go`, replace the caBundle-comparison section inside `maybeStampCRDDoc` (the block from `cc := conv.Webhook.ClientConfig` through `cc.CABundle = caCert`, lines ~541-557) with:

```go
	cc := conv.Webhook.ClientConfig
	urlChanged := false
	if r.ExternalHost != "" && cc.Service != nil {
		url := webhookhost.BuildExternalURL(r.ExternalHost, cc.Service.Path)
		cc.URL = &url
		cc.Service = nil
		urlChanged = true
	}
	// caBundle stamp is the shared core; URL rewrite (above) is MR-path-only.
	caChanged := stampCRDConversionCABundle(crd, caCert)
	if !caChanged && !urlChanged {
		// Already converged — preserve the original bytes so we don't
		// trigger a Secret update for this doc. We still return the CRD
		// name so the caller can publish it on the gate.
		return doc, false, true, crd.Name, nil
	}
```

Leave the outer `conv == nil || ...` guard and the strategy=None `return doc, false, true, "", nil` in `maybeStampCRDDoc` intact (it drives `observed=true`). `bytes` stays imported (still used elsewhere in the file? verify next step).

- [ ] **Step 6: Verify the `bytes` import in managed_resource_reconciler.go**

Run: `rg -n '\bbytes\.' pkg/controllers/managed_resource_reconciler.go`

Expected: if no matches remain, remove `"bytes"` from that file's import block (it moved to `stamp.go`). If matches remain, leave it.

- [ ] **Step 7: Run all stamp + MR tests**

Run: `go test ./pkg/controllers/ -run 'TestStamp|TestManagedResource' -v`

Expected: PASS (new stamp tests + existing MR tests — the refactor safety net).

- [ ] **Step 8: Build + vet the package**

Run: `go build ./pkg/controllers/ && go vet ./pkg/controllers/`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add pkg/controllers/stamp.go pkg/controllers/stamp_test.go pkg/controllers/managed_resource_reconciler.go
git commit -m "refactor: extract shared caBundle-stamp helpers for CRD and MWC/VWC"
```

---

## Task 2: Add target-CRD stamp metric

**Files:**
- Modify: `pkg/metrics/metrics.go`

- [ ] **Step 1: Add the counter var**

In `pkg/metrics/metrics.go`, add after the `TargetStampsTotal` block (after line 94):

```go
	// TargetCRDStampsTotal counts reconcile outcomes on target-cluster
	// CustomResourceDefinition conversion-webhook caBundle stamping (the
	// --target-label patch mode), partitioned by result. "stamped" =
	// we patched clientConfig.caBundle; "converged" = already current (no
	// patch); "error" = Get/Patch failure. Distinct from TargetStampsTotal
	// (MWC/VWC only) so the kind enum there stays bounded — patch-mode
	// MWC/VWC reuse TargetStampsTotal.
	TargetCRDStampsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhook_injector_target_crd_stamps_total",
			Help: "Total reconcile outcomes for target-cluster CRD conversion-webhook caBundle stamping, labeled by result (stamped|converged|error).",
		},
		[]string{"result"},
	)
```

- [ ] **Step 2: Register it**

In `init()`, add `TargetCRDStampsTotal,` after `TargetStampsTotal,` (line 242).

- [ ] **Step 3: Build**

Run: `go build ./pkg/metrics/`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add pkg/metrics/metrics.go
git commit -m "feat(metrics): add target_crd_stamps_total for patch-mode CRD stamping"
```

---

## Task 3: Convert WebhookReconciler.RotationTrigger to a slice

**Files:**
- Modify: `pkg/controllers/reconciler.go` (field ~line 146, `signalRotation` ~358-368)
- Modify: `cmd/webhook-injector/main.go` (wiring ~297-316, 338)
- Test: `pkg/controllers/reconciler_test.go`

- [ ] **Step 1: Change the field**

In `pkg/controllers/reconciler.go`, replace `RotationTrigger chan<- struct{}` (line 146, keep the doc comment) with:

```go
	// RotationTriggers is the set of downstream consumers signaled
	// (non-blocking send to each) on every detected certificate rotation.
	// Consumers: ManagedResourceReconciler (--managed-resource-label) and
	// TargetWebhookReconciler (--target-label). Each channel should be
	// buffered 1 so a missed send is harmless — a single signal triggers a
	// full re-stamp pass. Nil/empty when no stamping reconciler is wired.
	RotationTriggers []chan<- struct{}
```

- [ ] **Step 2: Update `signalRotation`**

Replace the body of `signalRotation` (lines ~358-368) with:

```go
func (r *WebhookReconciler) signalRotation(logger logr.Logger, reason rotationReason) {
	for _, trigger := range r.RotationTriggers {
		if trigger == nil {
			continue
		}
		select {
		case trigger <- struct{}{}:
			logger.Info("Signaled rotation downstream consumer", "reason", reason)
		default:
			logger.V(1).Info("Rotation signal channel full, skipping (already pending)")
		}
	}
}
```

- [ ] **Step 3: Update main.go wiring**

In `cmd/webhook-injector/main.go`, replace the `rotationTrigger` block (lines ~297-300) with:

```go
	// Rotation fan-out targets. WebhookReconciler signals each on every
	// detected rotation. One channel per registered stamping reconciler.
	var rotationTriggers []chan<- struct{}
	var mrTrigger chan struct{}
	if mrLabelOK {
		mrTrigger = make(chan struct{}, 1)
		rotationTriggers = append(rotationTriggers, mrTrigger)
	}
```

Change the struct-literal field `RotationTrigger: rotationTrigger,` (line 315) to `RotationTriggers: rotationTriggers,`, and the MR setup call (line 338) `mrReconciler.SetupWithManager(mgr, rotationTrigger)` to `mrReconciler.SetupWithManager(mgr, mrTrigger)`.

- [ ] **Step 4: Write failing fan-out test**

In `pkg/controllers/reconciler_test.go`, add (ensure `"github.com/go-logr/logr"` is imported):

```go
func TestSignalRotationFansOutToAllTriggers(t *testing.T) {
	a := make(chan struct{}, 1)
	b := make(chan struct{}, 1)
	r := &WebhookReconciler{RotationTriggers: []chan<- struct{}{a, b}}
	r.signalRotation(logr.Discard(), rotationReasonForced)
	for name, ch := range map[string]chan struct{}{"A": a, "B": b} {
		select {
		case <-ch:
		default:
			t.Errorf("trigger %s did not receive rotation signal", name)
		}
	}
}
```

- [ ] **Step 5: Run test + fix stale references**

Run: `go test ./pkg/controllers/ -run TestSignalRotationFansOutToAllTriggers -v && rg -n "RotationTrigger[^s]" pkg/ cmd/ test/`

Expected: test PASS; rg returns no matches. Update any test constructing `WebhookReconciler{RotationTrigger: ...}` to `RotationTriggers: []chan<- struct{}{...}`.

- [ ] **Step 6: Build all + regression**

Run: `go build ./... && go test ./pkg/controllers/`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add pkg/controllers/reconciler.go pkg/controllers/reconciler_test.go cmd/webhook-injector/main.go
git commit -m "refactor: fan rotation signal out to multiple downstream consumers"
```

---

## Task 4: TargetWebhookReconciler — CRD reconcile core

**Files:**
- Create: `pkg/controllers/target_webhook_reconciler.go`
- Test: `pkg/controllers/target_webhook_reconciler_test.go`

- [ ] **Step 1: Write the failing CRD-stamp test**

Create `pkg/controllers/target_webhook_reconciler_test.go`:

```go
// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
)

const (
	targetLabelKey   = "webhook-injector/target"
	targetLabelValue = "metal-operator"
	tcCRDName    = "servers.metal.ironcore.dev"
	tcCA         = "target-ca-bundle-bytes"
)

func tcCertSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certSecretName, Namespace: mrNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{tlsCrtKey: []byte(tcCA)},
	}
}

func tcWebhookCRD(caBundle string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: tcCRDName, Labels: map[string]string{targetLabelKey: targetLabelValue}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook:  &apiextensionsv1.WebhookConversion{ClientConfig: &apiextensionsv1.WebhookClientConfig{}},
			},
		},
	}
	if caBundle != "" {
		crd.Spec.Conversion.Webhook.ClientConfig.CABundle = []byte(caBundle)
	}
	return crd
}

// countingTargetClient counts Patch calls so tests can assert the idempotent
// no-op path issues zero writes.
type countingTargetClient struct {
	client.Client
	patches int
}

func (c *countingTargetClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func newTargetReconciler(c client.Client, gate *rotationgate.Gate) *TargetWebhookReconciler {
	return &TargetWebhookReconciler{
		TargetClient: c, CertSecretReader: c,
		Namespace: mrNamespace, CertSecretName: certSecretName,
		LabelKey: targetLabelKey, LabelValue: targetLabelValue, Gate: gate,
	}
}

var crdGVKForTest = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

func TestTargetReconcileCRDStampsCABundle(t *testing.T) {
	s := testScheme(t)
	crd := tcWebhookCRD("")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(crd, tcCertSecret()).Build()
	gate := rotationgate.New()
	r := newTargetReconciler(c, gate)

	if _, err := r.ReconcileCRD(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: tcCRDName},
	}); err != nil {
		t.Fatalf("ReconcileCRD: %v", err)
	}
	got := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: tcCRDName}, got); err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	if string(got.Spec.Conversion.Webhook.ClientConfig.CABundle) != tcCA {
		t.Errorf("caBundle = %q, want %q", got.Spec.Conversion.Webhook.ClientConfig.CABundle, tcCA)
	}
	if snap := gate.Snapshot(); len(snap) != 1 || snap[0] != tcCRDName {
		t.Errorf("gate snapshot = %v, want [%s]", snap, tcCRDName)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./pkg/controllers/ -run TestTargetReconcileCRDStampsCABundle -v`

Expected: FAIL with "undefined: TargetWebhookReconciler".

- [ ] **Step 3: Create the reconciler + the CRD reconcile method**

Create `pkg/controllers/target_webhook_reconciler.go`:

```go
// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"fmt"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
)

// TargetWebhookReconciler keeps caBundle in sync on CRD conversion webhooks
// and Mutating/ValidatingWebhookConfigurations applied DIRECTLY to the target
// cluster (GRM-free / direct-apply topology). Opt-in via --target-label.
// Leader-only, event-driven via a label-scoped target-cluster cache.
//
// Write surface is minimal: patches ONLY caBundle (MergeFrom), never
// creates/deletes, never rewrites clientConfig Service/URL, and only touches
// labeled objects — so it never fights GRM (GRM-owned objects lack the label).
// Stamped CRD names are published on the shared rotation Gate so
// WebhookReconciler's staged-phase promotion waits for target propagation;
// MWC/VWC are not gated (they are not part of conversion-caBundle gating).
type TargetWebhookReconciler struct {
	TargetClient     client.Client
	CertSecretReader client.Reader
	Namespace        string
	CertSecretName   string
	LabelKey         string
	LabelValue       string
	Gate             *rotationgate.Gate
}

func (r *TargetWebhookReconciler) certReader() client.Reader {
	if r.CertSecretReader != nil {
		return r.CertSecretReader
	}
	return r.TargetClient
}

func (r *TargetWebhookReconciler) hasLabel(obj client.Object) bool {
	return obj.GetLabels()[r.LabelKey] == r.LabelValue
}

// ReconcileCRD stamps caBundle on one labeled target CRD's conversion webhook.
func (r *TargetWebhookReconciler) ReconcileCRD(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", crdKind, "name", req.Name)
	gateKey := rotationgate.Key(req.Name)

	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.TargetClient.Get(ctx, req.NamespacedName, crd); err != nil {
		if apierrors.IsNotFound(err) {
			if r.Gate != nil {
				r.Gate.Forget(gateKey)
			}
			return ctrl.Result{}, nil
		}
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("get target CRD %s: %w", req.Name, err)
	}
	if !r.hasLabel(crd) {
		if r.Gate != nil {
			r.Gate.Forget(gateKey)
		}
		return ctrl.Result{}, nil
	}
	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != apiextensionsv1.WebhookConverter ||
		conv.Webhook == nil || conv.Webhook.ClientConfig == nil {
		return ctrl.Result{}, nil
	}
	caCert, err := loadCABundle(ctx, r.certReader(), r.Namespace, r.CertSecretName)
	if err != nil {
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultError).Inc()
		return ctrl.Result{}, err
	}
	if r.Gate != nil {
		r.Gate.Publish(gateKey, []string{crd.Name})
	}
	base := crd.DeepCopy()
	if !stampCRDConversionCABundle(crd, caCert) {
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultConverged).Inc()
		return ctrl.Result{}, nil
	}
	if err := r.TargetClient.Patch(ctx, crd, client.MergeFrom(base)); err != nil {
		metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("patch target CRD %s caBundle: %w", crd.Name, err)
	}
	metrics.TargetCRDStampsTotal.WithLabelValues(metrics.ResultStamped).Inc()
	logger.Info("Stamped target CRD conversion caBundle")
	return ctrl.Result{}, nil
}

// kindMWC / kindVWC label values for TargetStampsTotal (patch-mode MWC/VWC
// reuse that counter). Kept local to avoid exporting from reconciler.go.
const (
	targetKindMWC = "MutatingWebhookConfiguration"
	targetKindVWC = "ValidatingWebhookConfiguration"
)

// ReconcileMWC stamps caBundle on one labeled target MutatingWebhookConfiguration.
func (r *TargetWebhookReconciler) ReconcileMWC(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", targetKindMWC, "name", req.Name)
	mwc := &admissionv1.MutatingWebhookConfiguration{}
	if err := r.TargetClient.Get(ctx, req.NamespacedName, mwc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("get target MWC %s: %w", req.Name, err)
	}
	if !r.hasLabel(mwc) {
		return ctrl.Result{}, nil
	}
	caCert, err := loadCABundle(ctx, r.certReader(), r.Namespace, r.CertSecretName)
	if err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultError).Inc()
		return ctrl.Result{}, err
	}
	base := mwc.DeepCopy()
	if !stampMutatingCABundle(mwc, caCert) {
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultConverged).Inc()
		return ctrl.Result{}, nil
	}
	if err := r.TargetClient.Patch(ctx, mwc, client.MergeFrom(base)); err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("patch target MWC %s caBundle: %w", req.Name, err)
	}
	metrics.TargetStampsTotal.WithLabelValues(targetKindMWC, metrics.ResultStamped).Inc()
	logger.Info("Stamped target MutatingWebhookConfiguration caBundle")
	return ctrl.Result{}, nil
}

// ReconcileVWC stamps caBundle on one labeled target ValidatingWebhookConfiguration.
func (r *TargetWebhookReconciler) ReconcileVWC(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("kind", targetKindVWC, "name", req.Name)
	vwc := &admissionv1.ValidatingWebhookConfiguration{}
	if err := r.TargetClient.Get(ctx, req.NamespacedName, vwc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("get target VWC %s: %w", req.Name, err)
	}
	if !r.hasLabel(vwc) {
		return ctrl.Result{}, nil
	}
	caCert, err := loadCABundle(ctx, r.certReader(), r.Namespace, r.CertSecretName)
	if err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultError).Inc()
		return ctrl.Result{}, err
	}
	base := vwc.DeepCopy()
	if !stampValidatingCABundle(vwc, caCert) {
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultConverged).Inc()
		return ctrl.Result{}, nil
	}
	if err := r.TargetClient.Patch(ctx, vwc, client.MergeFrom(base)); err != nil {
		metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultError).Inc()
		return ctrl.Result{}, fmt.Errorf("patch target VWC %s caBundle: %w", req.Name, err)
	}
	metrics.TargetStampsTotal.WithLabelValues(targetKindVWC, metrics.ResultStamped).Inc()
	logger.Info("Stamped target ValidatingWebhookConfiguration caBundle")
	return ctrl.Result{}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./pkg/controllers/ -run TestTargetReconcileCRDStampsCABundle -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/controllers/target_webhook_reconciler.go pkg/controllers/target_webhook_reconciler_test.go
git commit -m "feat: TargetWebhookReconciler reconcile methods for CRD/MWC/VWC caBundle"
```

---

## Task 5: TargetWebhookReconciler — MWC/VWC + edge-case tests

**Files:**
- Test: `pkg/controllers/target_webhook_reconciler_test.go`

- [ ] **Step 1: Write MWC and VWC stamp tests**

Append to `pkg/controllers/target_webhook_reconciler_test.go` (add `admissionv1 "k8s.io/api/admissionregistration/v1"` to imports):

```go
func tcMWC(ca string) *admissionv1.MutatingWebhookConfiguration {
	m := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-mwc", Labels: map[string]string{targetLabelKey: targetLabelValue}},
		Webhooks:   []admissionv1.MutatingWebhook{{Name: "a.example.com"}, {Name: "b.example.com"}},
	}
	if ca != "" {
		for i := range m.Webhooks {
			m.Webhooks[i].ClientConfig.CABundle = []byte(ca)
		}
	}
	return m
}

func TestTargetReconcileMWCStampsAllWebhooks(t *testing.T) {
	s := testScheme(t)
	mwc := tcMWC("")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(mwc, tcCertSecret()).Build()
	r := newTargetReconciler(c, rotationgate.New())
	if _, err := r.ReconcileMWC(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "tc-mwc"}}); err != nil {
		t.Fatalf("ReconcileMWC: %v", err)
	}
	got := &admissionv1.MutatingWebhookConfiguration{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tc-mwc"}, got); err != nil {
		t.Fatalf("get MWC: %v", err)
	}
	for i, w := range got.Webhooks {
		if string(w.ClientConfig.CABundle) != tcCA {
			t.Errorf("webhook[%d] caBundle = %q, want %q", i, w.ClientConfig.CABundle, tcCA)
		}
	}
}

func TestTargetReconcileMWCConvergedNoPatch(t *testing.T) {
	s := testScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(tcMWC(tcCA), tcCertSecret()).Build()
	c := &countingTargetClient{Client: base}
	r := newTargetReconciler(c, rotationgate.New())
	if _, err := r.ReconcileMWC(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "tc-mwc"}}); err != nil {
		t.Fatalf("ReconcileMWC: %v", err)
	}
	if c.patches != 0 {
		t.Errorf("expected 0 patches on converged MWC, got %d", c.patches)
	}
}
```

- [ ] **Step 2: Write CRD converged / strategy=None / NotFound / label-loss tests**

Append:

```go
func TestTargetReconcileCRDConvergedNoPatch(t *testing.T) {
	s := testScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(tcWebhookCRD(tcCA), tcCertSecret()).Build()
	c := &countingTargetClient{Client: base}
	r := newTargetReconciler(c, rotationgate.New())
	if _, err := r.ReconcileCRD(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tcCRDName}}); err != nil {
		t.Fatalf("ReconcileCRD: %v", err)
	}
	if c.patches != 0 {
		t.Errorf("expected 0 patches on converged CRD, got %d", c.patches)
	}
}

func TestTargetReconcileCRDSkipsNonWebhook(t *testing.T) {
	s := testScheme(t)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: tcCRDName, Labels: map[string]string{targetLabelKey: targetLabelValue}},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Conversion: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}},
	}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(crd, tcCertSecret()).Build()
	c := &countingTargetClient{Client: base}
	gate := rotationgate.New()
	r := newTargetReconciler(c, gate)
	if _, err := r.ReconcileCRD(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tcCRDName}}); err != nil {
		t.Fatalf("ReconcileCRD: %v", err)
	}
	if c.patches != 0 {
		t.Errorf("expected 0 patches for strategy=None, got %d", c.patches)
	}
	if snap := gate.Snapshot(); len(snap) != 0 {
		t.Errorf("expected empty gate for strategy=None, got %v", snap)
	}
}

func TestTargetReconcileCRDNotFoundForgetsGate(t *testing.T) {
	s := testScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(tcCertSecret()).Build()
	gate := rotationgate.New()
	gate.Publish(rotationgate.Key(tcCRDName), []string{tcCRDName})
	r := newTargetReconciler(base, gate)
	if _, err := r.ReconcileCRD(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tcCRDName}}); err != nil {
		t.Fatalf("ReconcileCRD: %v", err)
	}
	if snap := gate.Snapshot(); len(snap) != 0 {
		t.Errorf("expected gate forgotten on NotFound, got %v", snap)
	}
}

func TestTargetReconcileCRDLabelRemovedForgetsGate(t *testing.T) {
	s := testScheme(t)
	crd := tcWebhookCRD("")
	crd.Labels = map[string]string{}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(crd, tcCertSecret()).Build()
	c := &countingTargetClient{Client: base}
	gate := rotationgate.New()
	gate.Publish(rotationgate.Key(tcCRDName), []string{tcCRDName})
	r := newTargetReconciler(c, gate)
	if _, err := r.ReconcileCRD(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tcCRDName}}); err != nil {
		t.Fatalf("ReconcileCRD: %v", err)
	}
	if c.patches != 0 {
		t.Errorf("expected 0 patches for unlabeled CRD, got %d", c.patches)
	}
	if snap := gate.Snapshot(); len(snap) != 0 {
		t.Errorf("expected gate forgotten on label loss, got %v", snap)
	}
}
```

- [ ] **Step 3: Run all target tests**

Run: `go test ./pkg/controllers/ -run TestTargetReconcile -v`

Expected: PASS (all CRD + MWC subtests).

- [ ] **Step 4: Commit**

```bash
git add pkg/controllers/target_webhook_reconciler_test.go
git commit -m "test: MWC stamping + CRD edge cases for TargetWebhookReconciler"
```

---

## Task 6: TargetWebhookReconciler — SetupWithManager (three controllers on target cache) + resync trigger

**Files:**
- Modify: `pkg/controllers/target_webhook_reconciler.go`

- [ ] **Step 1: Read the MR resync-trigger pattern**

Run: `sed -n '606,676p' pkg/controllers/managed_resource_reconciler.go`

Expected: See `managedResourceResyncTrigger` (Runnable + LeaderElectionRunnable) fanning a rotation signal into a `GenericEvent` channel.

- [ ] **Step 2: Add SetupWithManager + resync trigger**

Append to `pkg/controllers/target_webhook_reconciler.go`. Add to imports: `"time"`, `"k8s.io/apimachinery/pkg/labels"`, `"k8s.io/apimachinery/pkg/types"`, `"sigs.k8s.io/controller-runtime/pkg/builder"`, `"sigs.k8s.io/controller-runtime/pkg/cluster"`, `"sigs.k8s.io/controller-runtime/pkg/event"`, `"sigs.k8s.io/controller-runtime/pkg/handler"`, `"sigs.k8s.io/controller-runtime/pkg/manager"`, `"sigs.k8s.io/controller-runtime/pkg/predicate"`, `"sigs.k8s.io/controller-runtime/pkg/reconcile"`, `"sigs.k8s.io/controller-runtime/pkg/source"`, `"github.com/go-logr/logr"`.

```go
// SetupWithManager registers three leader-only controllers (target-crd,
// target-mwc, target-vwc) that watch labeled objects on the TARGET cluster's
// cache. Because those objects live on a different cluster than the manager,
// each watch uses source.Kind against targetCluster.GetCache(). A resync
// trigger (fed by rotationTrigger and firing once at startup) re-lists all
// labeled objects so a fresh caBundle propagates immediately and the initial
// state is reconciled even with an empty event backlog.
func (r *TargetWebhookReconciler) SetupWithManager(mgr ctrl.Manager, targetCluster cluster.Cluster, rotationTrigger <-chan struct{}) error {
	crdCh := make(chan event.GenericEvent, 64)
	mwcCh := make(chan event.GenericEvent, 64)
	vwcCh := make(chan event.GenericEvent, 64)

	trigger := &targetResyncTrigger{
		targetClient:    r.TargetClient,
		labelKey:        r.LabelKey,
		labelValue:      r.LabelValue,
		rotationTrigger: rotationTrigger,
		crdCh:           crdCh,
		mwcCh:           mwcCh,
		vwcCh:           vwcCh,
	}
	if err := mgr.Add(trigger); err != nil {
		return fmt.Errorf("register target resync trigger: %w", err)
	}

	labelPred := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj != nil && obj.GetLabels()[r.LabelKey] == r.LabelValue
	})
	nameMapper := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: obj.GetName()}}}
	})
	tc := targetCluster.GetCache()

	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-crd").
		WatchesRawSource(source.Kind(tc, client.Object(&apiextensionsv1.CustomResourceDefinition{}), &handler.EnqueueRequestForObject{}, labelPred)).
		WatchesRawSource(source.Channel(crdCh, nameMapper)).
		Complete(reconcile.Func(r.ReconcileCRD)); err != nil {
		return fmt.Errorf("build target-crd controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-mwc").
		WatchesRawSource(source.Kind(tc, client.Object(&admissionv1.MutatingWebhookConfiguration{}), &handler.EnqueueRequestForObject{}, labelPred)).
		WatchesRawSource(source.Channel(mwcCh, nameMapper)).
		Complete(reconcile.Func(r.ReconcileMWC)); err != nil {
		return fmt.Errorf("build target-mwc controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("target-vwc").
		WatchesRawSource(source.Kind(tc, client.Object(&admissionv1.ValidatingWebhookConfiguration{}), &handler.EnqueueRequestForObject{}, labelPred)).
		WatchesRawSource(source.Channel(vwcCh, nameMapper)).
		Complete(reconcile.Func(r.ReconcileVWC)); err != nil {
		return fmt.Errorf("build target-vwc controller: %w", err)
	}
	_ = builder.WithPredicates // builder imported for symmetry; predicates applied via source.Kind
	return nil
}

// targetResyncTrigger lists labeled CRD/MWC/VWC on the target cluster and
// emits one GenericEvent per object on the matching channel — once at startup
// and on each rotation signal. Leader-only.
type targetResyncTrigger struct {
	targetClient    client.Client
	labelKey        string
	labelValue      string
	rotationTrigger <-chan struct{}
	crdCh           chan event.GenericEvent
	mwcCh           chan event.GenericEvent
	vwcCh           chan event.GenericEvent
}

func (t *targetResyncTrigger) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("target-resync-trigger")
	t.fanOut(ctx, logger) // initial pass so startup state is reconciled
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.rotationTrigger:
			t.fanOut(ctx, logger)
		}
	}
}

func (t *targetResyncTrigger) fanOut(ctx context.Context, logger logr.Logger) {
	sel := labels.SelectorFromSet(labels.Set{t.labelKey: t.labelValue})
	opt := client.MatchingLabelsSelector{Selector: sel}

	var crds apiextensionsv1.CustomResourceDefinitionList
	if err := t.targetClient.List(ctx, &crds, opt); err != nil {
		logger.Error(err, "target resync: list CRDs")
	} else {
		for i := range crds.Items {
			if !t.send(ctx, t.crdCh, &crds.Items[i]) {
				return
			}
		}
	}
	var mwcs admissionv1.MutatingWebhookConfigurationList
	if err := t.targetClient.List(ctx, &mwcs, opt); err != nil {
		logger.Error(err, "target resync: list MWCs")
	} else {
		for i := range mwcs.Items {
			if !t.send(ctx, t.mwcCh, &mwcs.Items[i]) {
				return
			}
		}
	}
	var vwcs admissionv1.ValidatingWebhookConfigurationList
	if err := t.targetClient.List(ctx, &vwcs, opt); err != nil {
		logger.Error(err, "target resync: list VWCs")
	} else {
		for i := range vwcs.Items {
			if !t.send(ctx, t.vwcCh, &vwcs.Items[i]) {
				return
			}
		}
	}
	logger.V(1).Info("target resync complete", "crds", len(crds.Items), "mwcs", len(mwcs.Items), "vwcs", len(vwcs.Items))
}

func (t *targetResyncTrigger) send(ctx context.Context, ch chan event.GenericEvent, obj client.Object) bool {
	select {
	case ch <- event.GenericEvent{Object: obj}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (t *targetResyncTrigger) NeedLeaderElection() bool { return true }

var (
	_ manager.Runnable               = (*targetResyncTrigger)(nil)
	_ manager.LeaderElectionRunnable = (*targetResyncTrigger)(nil)
)
```

- [ ] **Step 3: Build the package (resolve any import mismatch)**

Run: `go build ./pkg/controllers/`

Expected: PASS. If `builder` is flagged unused, delete its import and the `_ = builder.WithPredicates` line. If `source.Kind`'s generic inference rejects `client.Object(&T{})`, use the concrete pointer directly (`&apiextensionsv1.CustomResourceDefinition{}`) — the generic `source.Kind[object client.Object]` infers from the concrete type.

- [ ] **Step 4: Run package tests (Reconcile* still green)**

Run: `go test ./pkg/controllers/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/controllers/target_webhook_reconciler.go
git commit -m "feat: target-cluster watches (crd/mwc/vwc) + rotation/startup resync trigger"
```

---

## Task 7: WebhookReconciler — optional ConfigMap (cert-only mode) + startup bootstrap

**Files:**
- Modify: `pkg/controllers/reconciler.go`
- Test: `pkg/controllers/reconciler_test.go`

- [ ] **Step 1: Read the current Reconcile head + SetupWithManager**

Run: `sed -n '208,245p' pkg/controllers/reconciler.go && sed -n '1089,1121p' pkg/controllers/reconciler.go`

Expected: See the ConfigMap Get at the top of `Reconcile` and the `For(&ConfigMap{})` in `SetupWithManager`.

- [ ] **Step 2: Write failing test for cert-only mode**

In `pkg/controllers/reconciler_test.go`, add a test that a `WebhookReconciler` with `WebhookConfigName == ""` and `CertSANs` set generates a cert Secret on reconcile. Match the existing reconciler-test client/scheme setup already used in that file (reuse its `newTestScheme`/fake-client helpers — inspect the top of the file first if unsure of names):

```go
func TestReconcileCertOnlyModeGeneratesCert(t *testing.T) {
	s := testScheme(t) // shared with MR tests; includes core+apiextensions
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &WebhookReconciler{
		Client:                c,
		WebhookConfigName:     "", // cert-only mode
		CertSANs:              []string{"webhook.example.com"},
		Namespace:             mrNamespace,
		CertSecretName:        certSecretName,
		CertValidity:          time.Hour,
		CertRotationThreshold: 0.2,
		Scheme:                s,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: mrNamespace, Name: "unused-in-cert-only"},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	sec := &corev1.Secret{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: mrNamespace, Name: certSecretName}, sec); err != nil {
		t.Fatalf("expected cert Secret created in cert-only mode: %v", err)
	}
	if len(sec.Data[tlsCrtKey]) == 0 {
		t.Error("expected tls.crt populated")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./pkg/controllers/ -run TestReconcileCertOnlyModeGeneratesCert -v`

Expected: FAIL — current `Reconcile` returns `IgnoreNotFound` when the ConfigMap is missing, so no cert is created.

- [ ] **Step 4: Make ConfigMap optional in Reconcile**

In `pkg/controllers/reconciler.go`, replace the ConfigMap fetch + parse head of `Reconcile` (lines ~212-241) so that when `WebhookConfigName == ""` it skips ConfigMap I/O and derives services from `CertSANs` only:

```go
	var configs *webhooks.WebhookConfigurations
	var services []certificates.ServiceReference

	if r.WebhookConfigName == "" {
		// Cert-only mode: no ConfigMap. SANs come solely from --cert-sans.
		// Used by pure patch-mode deployments (--target-label without
		// --webhook-config-name). No MWC/VWC are applied to the target here.
		for _, san := range r.CertSANs {
			services = append(services, certificates.ServiceReference{Hostname: san})
		}
		if len(services) == 0 {
			logger.Info("Cert-only mode but no --cert-sans provided; nothing to do")
			return ctrl.Result{}, nil
		}
	} else {
		configMap := &corev1.ConfigMap{}
		if err := r.Client.Get(ctx, req.NamespacedName, configMap); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		logger.Info("Reconciling webhook configuration", "configmap", req.NamespacedName)
		webhooksYAML, ok := configMap.Data["webhooks.yaml"]
		if !ok {
			return ctrl.Result{}, errors.New("webhooks.yaml not found in ConfigMap")
		}
		parsed, err := webhooks.ParseWebhookConfigurations([]byte(webhooksYAML))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to parse webhook configurations: %w", err)
		}
		configs = parsed
		services = webhooks.ExtractServiceReferences(configs)
		for _, san := range r.CertSANs {
			services = append(services, certificates.ServiceReference{Hostname: san})
		}
		if len(services) == 0 {
			logger.Info("No service references found in webhook configurations")
			return ctrl.Result{}, nil
		}
	}
```

Then guard `applyAndUpdateExpiry` against a nil `configs`. In `applyAndUpdateExpiry` (line ~446), wrap the inject+apply so cert-only mode only refreshes the gauge:

```go
func (r *WebhookReconciler) applyAndUpdateExpiry(ctx context.Context, configs *webhooks.WebhookConfigurations, secret *corev1.Secret, logger logr.Logger) error {
	r.updateCertExpiryGauge(secret)
	if configs == nil {
		return nil // cert-only mode: no MWC/VWC to apply to the target
	}
	caCert := secret.Data[tlsCrtKey]
	webhooks.InjectCABundle(configs, caCert)
	if err := r.applyWebhookConfigurations(ctx, configs); err != nil {
		logger.Error(err, "failed to apply webhook configurations")
		return fmt.Errorf("apply webhook configurations: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run the cert-only test**

Run: `go test ./pkg/controllers/ -run TestReconcileCertOnlyModeGeneratesCert -v`

Expected: PASS.

- [ ] **Step 6: Add a leader-only startup bootstrap runnable for cert-only mode**

Cert-only mode has no ConfigMap to watch, so the first reconcile needs a trigger. In `SetupWithManager` (line ~1090), when `WebhookConfigName == ""`, register a `GenericEvent` source + a startup runnable that emits one event. Add to `SetupWithManager`:

```go
func (r *WebhookReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.WebhookConfigName == "" {
		// Cert-only mode: no ConfigMap to anchor on. Drive the first
		// reconcile from a one-shot GenericEvent, then self-drive via
		// RequeueAfter; still watch the cert Secret for rotation triggers.
		bootCh := make(chan event.GenericEvent, 1)
		if err := mgr.Add(&certBootstrapRunnable{ch: bootCh, namespace: r.getNamespace(r.Namespace), name: r.CertSecretName}); err != nil {
			return fmt.Errorf("register cert bootstrap runnable: %w", err)
		}
		secretMapper := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
			if obj.GetName() != r.CertSecretName || obj.GetNamespace() != r.getNamespace(r.Namespace) {
				return nil
			}
			return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: r.getNamespace(r.Namespace), Name: r.CertSecretName}}}
		})
		return ctrl.NewControllerManagedBy(mgr).
			Named("webhook-cert-only").
			WatchesRawSource(source.Channel(bootCh, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
				return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}}
			}))).
			Watches(&corev1.Secret{}, secretMapper).
			Complete(r)
	}
	// ... existing ConfigMap-mode SetupWithManager body unchanged ...
}
```

Add the runnable at the bottom of `reconciler.go`:

```go
// certBootstrapRunnable emits exactly one GenericEvent at startup so the
// cert-only-mode WebhookReconciler runs its first reconcile without a
// ConfigMap to watch. Leader-only. After the first reconcile creates the
// cert Secret, the loop self-drives via RequeueAfter and the Secret watch.
type certBootstrapRunnable struct {
	ch        chan event.GenericEvent
	namespace string
	name      string
}

func (b *certBootstrapRunnable) Start(ctx context.Context) error {
	obj := &corev1.Secret{}
	obj.SetNamespace(b.namespace)
	obj.SetName(b.name)
	select {
	case b.ch <- event.GenericEvent{Object: obj}:
	case <-ctx.Done():
	}
	<-ctx.Done()
	return nil
}

func (b *certBootstrapRunnable) NeedLeaderElection() bool { return true }

var (
	_ manager.Runnable               = (*certBootstrapRunnable)(nil)
	_ manager.LeaderElectionRunnable = (*certBootstrapRunnable)(nil)
)
```

Add to `reconciler.go` imports as needed: `"sigs.k8s.io/controller-runtime/pkg/event"`, `"sigs.k8s.io/controller-runtime/pkg/manager"`, `"sigs.k8s.io/controller-runtime/pkg/source"`.

- [ ] **Step 7: Build + full package test**

Run: `go build ./pkg/controllers/ && go test ./pkg/controllers/`

Expected: PASS (cert-only test + all existing ConfigMap-mode/MR/rotation tests — proves ConfigMap mode is unaffected).

- [ ] **Step 8: Commit**

```bash
git add pkg/controllers/reconciler.go pkg/controllers/reconciler_test.go
git commit -m "feat: cert-only mode (optional ConfigMap) with startup bootstrap"
```

---

## Task 8: Wire --target-label + optional config-name into main.go

**Files:**
- Modify: `cmd/webhook-injector/main.go`

- [ ] **Step 1: Add the flag var + registration**

In `cmd/webhook-injector/main.go`, add `targetLabel string` to the flag `var (...)` block (after `managedResourceLabel`), and register it in `init()` after the `managed-resource-label` flag:

```go
	flag.StringVar(&targetLabel, "target-label", "", "Label selector (key=value) enabling target-cluster patch mode. When set, watches CRDs, MutatingWebhookConfigurations, and ValidatingWebhookConfigurations on the TARGET cluster carrying this label and keeps their caBundle in sync as certs rotate — for GRM-free / direct-apply topologies (no ManagedResource). Only pre-existing labeled objects are stamped; nothing is created/deleted, only caBundle is patched. Independent of --managed-resource-label. When unset, patch mode is not registered.")
```

- [ ] **Step 2: Parse label, replace the required-config-name check with at-least-one-mode + pure-patch --cert-sans validation**

In `main()`, replace the current required check (lines ~122-127) and add parsing after the MR-label parse (after line 133):

```go
	targetLabelKey, targetLabelValue, targetLabelOK := parseLabel(targetLabel)
	if targetLabel != "" && !targetLabelOK {
		setupLog.Error(nil, fmt.Sprintf("--target-label must be in key=value form, got %q", targetLabel))
		os.Exit(1)
	}

	// At least one mode must be active.
	if webhookConfigName == "" && !targetLabelOK {
		setupLog.Error(nil, "at least one of --webhook-config-name or --target-label must be set")
		flag.Usage()
		os.Exit(1)
	}

	// Pure patch mode (no ConfigMap) has no webhook services to derive SANs
	// from — --cert-sans is the only SAN source, so it must be non-empty.
	if webhookConfigName == "" && len(certSANsList) == 0 {
		setupLog.Error(nil, "--cert-sans is required when running patch mode without --webhook-config-name")
		os.Exit(1)
	}
```

(Delete the old `if webhookConfigName == "" { ... os.Exit(1) }` block. Note `certSANsList` is computed at line 135 — move that computation above this validation, or reference `splitAndTrim(certSANs)` inline.)

- [ ] **Step 3: Register WebhookReconciler always, but only construct target reconciler when labeled**

The `WebhookReconciler` construction (lines ~302-321) stays — it now handles both modes (`WebhookConfigName` may be `""`). After the MR reconciler block (after line 346), add the target reconciler + target cluster:

```go
	if targetLabelOK {
		targetCfg, err := getTargetRestConfig()
		if err != nil {
			setupLog.Error(err, "Unable to get target cluster config for patch mode")
			os.Exit(1)
		}
		targetSelector := labels.SelectorFromSet(labels.Set{targetLabelKey: targetLabelValue})
		targetCluster, err := cluster.New(targetCfg, func(o *cluster.Options) {
			o.Scheme = scheme
			o.Cache.SyncPeriod = &syncPeriod
			o.Cache.ByObject = map[client.Object]cache.ByObject{
				&apiextensionsv1.CustomResourceDefinition{}:            {Label: targetSelector},
				&admissionv1.MutatingWebhookConfiguration{}:            {Label: targetSelector},
				&admissionv1.ValidatingWebhookConfiguration{}:          {Label: targetSelector},
			}
		})
		if err != nil {
			setupLog.Error(err, "Unable to create target cluster handle for patch mode")
			os.Exit(1)
		}
		if err := mgr.Add(targetCluster); err != nil {
			setupLog.Error(err, "Unable to add target cluster to manager")
			os.Exit(1)
		}
		tcTrigger := make(chan struct{}, 1)
		rotationTriggers = append(rotationTriggers, tcTrigger)
		tcReconciler := &controllers.TargetWebhookReconciler{
			TargetClient:     targetCluster.GetClient(),
			CertSecretReader: mgr.GetAPIReader(),
			Namespace:        namespace,
			CertSecretName:   certSecretName,
			LabelKey:         targetLabelKey,
			LabelValue:       targetLabelValue,
			Gate:             gate,
		}
		if err := tcReconciler.SetupWithManager(mgr, targetCluster, tcTrigger); err != nil {
			setupLog.Error(err, "Unable to create TargetWebhookReconciler controller")
			os.Exit(1)
		}
		setupLog.Info("TargetWebhookReconciler registered (patch mode, runs on leader only)",
			"label", targetLabelKey+"="+targetLabelValue)
	}
	webhookReconciler.RotationTriggers = rotationTriggers
```

Note: `rotationTriggers` must be declared before the `WebhookReconciler` literal (Task 3 Step 3 placed it there) and assigned to the reconciler *after* all consumers are appended — set `RotationTriggers: rotationTriggers` in the literal is insufficient because the slice grows later, so add the explicit `webhookReconciler.RotationTriggers = rotationTriggers` line above (Go slices are reference types, but appends may reallocate — assign after the final append to be safe).

Add imports to `main.go`: `"sigs.k8s.io/controller-runtime/pkg/cluster"` (others — `labels`, `cache`, `client`, `admissionv1`, `apiextensionsv1` — are already imported).

- [ ] **Step 4: Add target-label to the startup log**

In the `setupLog.Info("Starting webhook-injector", ...)` block, add `"target-label", targetLabel,`.

- [ ] **Step 5: Build the binary**

Run: `go build ./cmd/webhook-injector/`

Expected: PASS.

- [ ] **Step 6: Smoke-check flags and validation**

Run: `go run ./cmd/webhook-injector/ --help 2>&1 | grep target-label && go run ./cmd/webhook-injector/ 2>&1 | grep -i "at least one"`

Expected: help text present; running with **no** flags prints the "at least one of --webhook-config-name or --target-label must be set" error.

- [ ] **Step 7: Commit**

```bash
git add cmd/webhook-injector/main.go
git commit -m "feat: wire --target-label patch mode + make --webhook-config-name optional"
```

---

## Task 9: RBAC — watch on MWC/VWC/CRD + patch on CRD

**Files:**
- Modify: `config/rbac.yaml`

- [ ] **Step 1: Update the target ClusterRole**

In `config/rbac.yaml`, in `webhook-injector-target`, the MWC/VWC rule already has `watch`; ensure the CRD rule adds `patch`:

```yaml
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["get", "list", "watch", "patch"]   # +patch for --target-label patch mode; still no create/delete
```

(The MWC/VWC rule keeps `get,list,watch,create,update,patch` — unchanged; `watch` already present for the target cache.)

- [ ] **Step 2: Verify REUSE + static checks**

Run: `reuse lint && golangci-lint run`

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add config/rbac.yaml
git commit -m "rbac: patch on target CRDs for patch mode (watch already present for MWC/VWC)"
```

---

## Task 10: Full verification pass

**Files:** none

- [ ] **Step 1: Unit tests**

Run: `gmake test`

Expected: PASS across the module.

- [ ] **Step 2: Full check**

Run: `gmake check`

Expected: PASS (golangci-lint, shellcheck, go-licence-detector, REUSE, tests).

- [ ] **Step 3: Backward-compat invariants**

Run: `rg -n "RotationTrigger[^s]" pkg/ cmd/ test/`

Expected: no matches.

Run: `go test ./pkg/controllers/ -run 'TestManagedResource|TestForceRotation|TestIsRotationInFlight' -v`

Expected: PASS — existing MR + rotation-gate behavior unchanged.

- [ ] **Step 4: Build binary**

Run: `gmake build`

Expected: `build/webhook-injector`, exit 0.

---

## Task 11: E2E test — direct-apply CRD + MWC + VWC patch mode

**Files:**
- Modify: `test/e2e/` (match the existing harness — inspect first)

- [ ] **Step 1: Read the e2e harness**

Run: `ls test/e2e/ && rg -n "ForceRotationAnnotation|hostingClient|targetClient|kind-target|--webhook-config-name" test/e2e/ | head -40`

Expected: understand hosting/target client setup, how flags are passed (Skaffold profile / deployment args), and existing assertion/polling helpers.

- [ ] **Step 2: Add the e2e test**

Add a test (matching the harness's style) that:
1. Applies **directly** to the target cluster (no ManagedResource), all labeled with the patch-mode label: a CRD with `conversion.strategy=Webhook` (empty caBundle), a `MutatingWebhookConfiguration`, and a `ValidatingWebhookConfiguration`.
2. Runs webhook-injector with `--target-label=<k>=<v>` and **without** `--webhook-config-name`, with `--cert-sans` set (pure patch mode). Match how other e2e tests configure flags.
3. Polls until all three objects have non-empty `caBundle` equal to the hosting cert Secret's `tls.crt` (proves the Watch stamps within seconds).
4. Force-rotates (`ForceRotationAnnotation` on the cert Secret, imported from the controllers package per the existing e2e pattern).
5. Asserts caBundle on all three shows the **union** then settles to the promoted single cert after the overlap window, and that promotion waited on CRD propagation via the gate.

Keep it behind the same build tag / `make test-e2e` entry as the other e2e tests.

- [ ] **Step 3: Run the e2e suite**

Run: `gmake test-e2e`

Expected: PASS. If kind/docker are unavailable, note it and run `gmake e2e-setup` per `test/e2e/README.md`.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/
git commit -m "test(e2e): direct-apply CRD/MWC/VWC patch mode + rotation"
```

---

## Task 12: Documentation

**Files:**
- Modify: `README.md`, `CLAUDE.md`

- [ ] **Step 1: README — controllers list**

Add after the `ManagedResourceReconciler` bullet:

```markdown
- **TargetWebhookReconciler** (leader only, opt-in via `--target-label`): For GRM-free / direct-apply topologies, watches labeled CRDs, MutatingWebhookConfigurations, and ValidatingWebhookConfigurations on the **target** cluster and keeps their `caBundle` in sync. Patches caBundle only — never creates/deletes, never rewrites `clientConfig`. CRDs participate in the rotation gate.
```

- [ ] **Step 2: README — flag table**

Add a `--target-label` row, and update `--webhook-config-name` from "*required*" to note it is required **unless** `--target-label` is set:

```markdown
| `--webhook-config-name` | ConfigMap (hosting cluster) carrying `webhooks.yaml` MWC/VWC applied to the target. **Required unless `--target-label` is set.** CRDs are not handled here. | unset |
| `--target-label` | Label selector (`key=value`) enabling target-cluster patch mode: watches labeled CRD/MWC/VWC on the **target** cluster and patches only their `caBundle` (GRM-free / direct-apply). Nothing created/deleted. Independent of `--managed-resource-label`. When unset, patch mode is not registered. | unset |
```

- [ ] **Step 3: README — metrics table**

Add a `target_crd_stamps_total{result}` row (patch-mode CRD stamps); note patch-mode MWC/VWC reuse `target_stamps_total`.

- [ ] **Step 4: README — patch-mode subsection**

Add a "Direct-Apply (GRM-free) Patch Mode" section: when a consumer (e.g. `dual-deployment-operator`) applies CRD/MWC/VWC directly to the shoot with no `ManagedResource`, use `--target-label` (optionally without `--webhook-config-name`, with `--cert-sans` supplying SANs). Explain caBundle-only, no admission bootstrap, event-driven Watch, and gate participation.

- [ ] **Step 5: CLAUDE.md**

- Add `TargetWebhookReconciler` to Controllers (leader-only, target cache, three controllers, caBundle-only, gate for CRDs).
- Add `--target-label` to Configuration; note `--webhook-config-name` is now optional (at-least-one-mode) and `--cert-sans` required in pure patch mode.
- Add `target_crd_stamps_total{result}` to Metrics; note MWC/VWC reuse `target_stamps_total`.
- Update the "why patch mode was removed (#11)" history note: a narrow, label-scoped, caBundle-only patch mode (CRD+MWC/VWC Watch, no admission mutator) was reintroduced for the GRM-free topology where the GRM-thrash rationale does not apply.

- [ ] **Step 6: REUSE sanity**

Run: `reuse lint`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "docs: document --target-label patch mode and optional --webhook-config-name"
```

---

## Task 13: Holistic code review (code-reviewer subagent)

**Files:** none (review only — dispatched after all prior tasks land and `gmake check` is green)

- [ ] **Step 1: Confirm the branch is in a reviewable state**

Run: `git status && git log --oneline main..HEAD`

Expected: clean working tree; commits for Tasks 1-12 present.

- [ ] **Step 2: Dispatch the `code-reviewer` subagent for a final holistic review**

Per `AGENTS.md`, review is performed by the configured `code-reviewer` subagent (never `general`). Dispatch it against the full diff from the base branch, not a single commit:

```
task(
  subagent_type="code-reviewer",
  description="Holistic review of target patch mode",
  prompt="Holistically review the entire feature branch feat-13-target-patch-mode against main for SAP-cloud-infrastructure/webhook-injector issue #13. Scope: the full diff `git diff main...HEAD` across all commits, not just the latest.

  Feature summary: opt-in target-cluster patch mode (--target-label) that watches labeled CRD/MWC/VWC on the TARGET cluster and keeps only their caBundle in sync via MergeFrom patch; --webhook-config-name made optional (at-least-one-mode); WebhookReconciler cert-only mode; RotationTrigger→RotationTriggers fan-out; new TargetWebhookReconciler on a label-scoped target cluster.Cluster cache.

  Spec: docs/plans/2026-07-15-target-patch-mode-design.md. Plan: docs/plans/2026-07-15-target-patch-mode.md.

  Review focus (report findings by severity, with file:line and concrete fixes):
  1. Backward compatibility — prove existing ConfigMap mode and --managed-resource-label MR path are byte-for-byte unchanged when --target-label is unset. Flag any behavior drift.
  2. Write-surface minimality — TargetWebhookReconciler must patch ONLY caBundle (MergeFrom base), never create/delete, never mutate clientConfig Service/URL or other fields. Verify.
  3. Concurrency/rotation — RotationTriggers non-blocking fan-out, buffered-1 channels, leader-election gating on the resync trigger, target-cache startup race, gate publish/forget correctness.
  4. RBAC least-privilege — target CRD verbs are get/list/watch/patch only (no create/delete); MWC/VWC unchanged.
  5. Error handling, metrics label cardinality, and idempotency (converged path issues zero patches).
  6. Cert-only mode correctness — SAN source from --cert-sans, bootstrap runnable, nil-configs guard in applyAndUpdateExpiry.
  7. SOLID/security/taste per the reviewer standard.

  Do NOT modify code. Return a prioritized findings list (Critical/High/Medium/Low) plus an overall PASS/FAIL verdict."
)
```

- [ ] **Step 3: Triage and resolve findings**

For each Critical/High finding: fix via a focused follow-up commit (RED-GREEN if it's a behavior change), then re-run `gmake check`. For Medium/Low: fix if cheap, otherwise record as a follow-up note in the PR description. Re-dispatch the `code-reviewer` subagent if Critical/High fixes were substantial.

- [ ] **Step 4: Gate**

The feature is not complete until the `code-reviewer` verdict is PASS (no open Critical/High findings) and `gmake check` is green.

---

## Self-Review Checklist (completed during authoring)

**Spec coverage:** flag (T8), shared helpers (T1), CRD/MWC/VWC reconcile (T4), edge cases (T5), target cache + three watches + resync (T6), rotation fan-out slice (T3), optional ConfigMap / cert-only mode + bootstrap (T7), at-least-one-mode + pure-patch --cert-sans validation (T8), RBAC (T9), metrics (T2), tests (T4/T5/T7/T11), docs (T12), backward-compat invariants (T10), holistic code-reviewer review (T13). ✅

**Placeholder scan:** every code step shows complete code; every command has expected output. "Inspect first" steps (T6.1, T7.1, T11.1) are reads preceding fully-specified edits; T11's e2e body is described concretely against the existing harness. ✅

**Type consistency:** `stampCRDConversionCABundle(crd, caCert) (bool)`, `stampMutatingCABundle(mwc, caCert) (bool)`, `stampValidatingCABundle(vwc, caCert) (bool)` defined in T1, used in T4. `TargetWebhookReconciler` fields (`TargetClient`, `CertSecretReader`, `Namespace`, `CertSecretName`, `LabelKey`, `LabelValue`, `Gate`) consistent T4/T6/T8. `ReconcileCRD`/`ReconcileMWC`/`ReconcileVWC` signatures match `reconcile.Func` and are wired identically in T6. `SetupWithManager(mgr, targetCluster, rotationTrigger)` defined T6, called T8. `RotationTriggers []chan<- struct{}` defined T3, populated T3/T8. `metrics.TargetCRDStampsTotal` + `metrics.Result{Stamped,Converged,Error}` consistent T2/T4. `cluster.New(cfg, opts...)` + `source.Kind(cache, obj, handler, preds...)` match controller-runtime v0.23.1. ✅

**Backward-compat:** ConfigMap-mode path in `Reconcile`/`SetupWithManager` is preserved verbatim in the `else`/non-empty branches (T7); MR path untouched; target cache only built when `targetLabelOK` (T8); every existing flag keeps name/default. ✅
