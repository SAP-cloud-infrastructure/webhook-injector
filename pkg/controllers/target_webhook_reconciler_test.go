// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/metrics"
)

const (
	targetLabelKey   = "webhook-injector/target"
	targetLabelValue = "metal-operator"
	tcCRDName        = "servers.metal.ironcore.dev"
	tcCA             = "target-ca-bundle-bytes"
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

// TestTargetReconcileMWCPatchDoesNotClobberList asserts the MWC caBundle
// patch targets only per-webhook clientConfig.caBundle (strategic-merge
// shape: $setElementOrder + name-keyed elements) and never emits a bare
// "webhooks" array that would replace the whole list and null out
// operator-owned fields like admissionReviewVersions/sideEffects.
func TestTargetReconcileMWCPatchDoesNotClobberList(t *testing.T) {
	s := testScheme(t)
	mwc := tcMWC("")
	var captured string
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(mwc, tcCertSecret()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			if err != nil {
				t.Fatalf("patch.Data: %v", err)
			}
			captured = string(data)
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	r := newTargetReconciler(c, rotationgate.New())
	if _, err := r.ReconcileMWC(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "tc-mwc"}}); err != nil {
		t.Fatalf("ReconcileMWC: %v", err)
	}
	if captured == "" {
		t.Fatal("no patch captured")
	}
	// Strategic-merge patch keys webhook entries by name and preserves order;
	// it must NOT null out fields the reconciler didn't touch.
	if !strings.Contains(captured, "$setElementOrder/webhooks") {
		t.Errorf("expected strategic-merge patch ($setElementOrder/webhooks), got: %s", captured)
	}
	if strings.Contains(captured, `"admissionReviewVersions":null`) || strings.Contains(captured, `"sideEffects":null`) {
		t.Errorf("patch nulls out operator-owned fields (list replacement): %s", captured)
	}
}

// TestTargetReconcileCRDStrategyChangeForgetsGate asserts that when a
// previously-published labeled CRD switches conversion.strategy away from
// Webhook, its rotation-gate entry is forgotten — otherwise a future
// rotation waits on a stale gate entry until --rotation-gate-timeout.
func TestTargetReconcileCRDStrategyChangeForgetsGate(t *testing.T) {
	s := testScheme(t)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: tcCRDName, Labels: map[string]string{targetLabelKey: targetLabelValue}},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Conversion: &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}},
	}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(crd, tcCertSecret()).Build()
	c := &countingTargetClient{Client: base}
	gate := rotationgate.New()
	// Simulate a prior pass that published this CRD while it was Webhook-conversion.
	gate.Publish(rotationgate.Key(tcCRDName), []string{tcCRDName})
	r := newTargetReconciler(c, gate)
	if _, err := r.ReconcileCRD(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: tcCRDName}}); err != nil {
		t.Fatalf("ReconcileCRD: %v", err)
	}
	if c.patches != 0 {
		t.Errorf("expected 0 patches for strategy=None, got %d", c.patches)
	}
	if snap := gate.Snapshot(); len(snap) != 0 {
		t.Errorf("expected gate forgotten when CRD left Webhook conversion, got %v", snap)
	}
}

// --- ReconcileVWC coverage -------------------------------------------------

func tcVWC(ca string) *admissionv1.ValidatingWebhookConfiguration {
	v := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tc-vwc", Labels: map[string]string{targetLabelKey: targetLabelValue}},
		Webhooks:   []admissionv1.ValidatingWebhook{{Name: "a.example.com"}, {Name: "b.example.com"}},
	}
	if ca != "" {
		for i := range v.Webhooks {
			v.Webhooks[i].ClientConfig.CABundle = []byte(ca)
		}
	}
	return v
}

func TestTargetReconcileVWCStampsAllWebhooks(t *testing.T) {
	s := testScheme(t)
	vwc := tcVWC("")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(vwc, tcCertSecret()).Build()
	r := newTargetReconciler(c, rotationgate.New())
	if _, err := r.ReconcileVWC(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "tc-vwc"}}); err != nil {
		t.Fatalf("ReconcileVWC: %v", err)
	}
	got := &admissionv1.ValidatingWebhookConfiguration{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tc-vwc"}, got); err != nil {
		t.Fatalf("get VWC: %v", err)
	}
	if len(got.Webhooks) != 2 {
		t.Fatalf("expected 2 webhooks preserved, got %d", len(got.Webhooks))
	}
	for i, w := range got.Webhooks {
		if string(w.ClientConfig.CABundle) != tcCA {
			t.Errorf("webhook[%d] caBundle = %q, want %q", i, w.ClientConfig.CABundle, tcCA)
		}
	}
}

func TestTargetReconcileVWCConvergedNoPatch(t *testing.T) {
	s := testScheme(t)
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(tcVWC(tcCA), tcCertSecret()).Build()
	c := &countingTargetClient{Client: base}
	r := newTargetReconciler(c, rotationgate.New())
	if _, err := r.ReconcileVWC(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "tc-vwc"}}); err != nil {
		t.Fatalf("ReconcileVWC: %v", err)
	}
	if c.patches != 0 {
		t.Errorf("expected 0 patches on converged VWC, got %d", c.patches)
	}
}

// TestTargetReconcileVWCPatchDoesNotClobberList asserts the VWC caBundle
// patch targets only per-webhook clientConfig.caBundle (strategic-merge
// shape: $setElementOrder + name-keyed elements) and never emits a bare
// "webhooks" array that would replace the whole list and null out
// operator-owned fields like admissionReviewVersions/sideEffects. Same
// invariant as the MWC test.
func TestTargetReconcileVWCPatchDoesNotClobberList(t *testing.T) {
	s := testScheme(t)
	var captured string
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(tcVWC(""), tcCertSecret()).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			if err != nil {
				t.Fatalf("patch.Data: %v", err)
			}
			captured = string(data)
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	r := newTargetReconciler(c, rotationgate.New())
	if _, err := r.ReconcileVWC(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "tc-vwc"}}); err != nil {
		t.Fatalf("ReconcileVWC: %v", err)
	}
	if captured == "" {
		t.Fatal("no patch captured")
	}
	if !strings.Contains(captured, "$setElementOrder/webhooks") {
		t.Errorf("expected strategic-merge patch ($setElementOrder/webhooks), got: %s", captured)
	}
	if strings.Contains(captured, `"admissionReviewVersions":null`) || strings.Contains(captured, `"sideEffects":null`) {
		t.Errorf("patch nulls out operator-owned fields (list replacement): %s", captured)
	}
}

// --- targetResyncTrigger coverage -----------------------------------------

// tcTrigger builds a targetResyncTrigger wired to buffered channels large
// enough for typical tests (avoids blocking sends in the trigger). Named
// returns document which channel each output is.
func tcTrigger(c client.Client, rot <-chan struct{}, buf int) (tr *targetResyncTrigger, crdCh, mwcCh, vwcCh chan event.GenericEvent) {
	crdCh = make(chan event.GenericEvent, buf)
	mwcCh = make(chan event.GenericEvent, buf)
	vwcCh = make(chan event.GenericEvent, buf)
	tr = &targetResyncTrigger{
		targetClient:    c,
		labelKey:        targetLabelKey,
		labelValue:      targetLabelValue,
		rotationTrigger: rot,
		crdCh:           crdCh,
		mwcCh:           mwcCh,
		vwcCh:           vwcCh,
	}
	return tr, crdCh, mwcCh, vwcCh
}

// drainOne pulls exactly one event off ch or fails the test on timeout.
// Returns the event's object name so callers can assert per-kind routing.
func drainOne(t *testing.T, ch <-chan event.GenericEvent, timeout time.Duration) string {
	t.Helper()
	select {
	case ev := <-ch:
		return ev.Object.GetName()
	case <-time.After(timeout):
		t.Fatalf("timeout draining channel")
		return ""
	}
}

// TestTargetResyncTriggerRoutesPerKind asserts fanOut sends each labeled
// object to the channel matching its kind and skips unlabeled objects.
func TestTargetResyncTriggerRoutesPerKind(t *testing.T) {
	s := testScheme(t)
	crdLabeled := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "crd-l", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	crdUnlabeled := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "crd-u"}}
	mwcLabeled := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "mwc-l", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	vwcLabeled := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "vwc-l", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(crdLabeled, crdUnlabeled, mwcLabeled, vwcLabeled).Build()
	tr, crdCh, mwcCh, vwcCh := tcTrigger(c, nil, 8)

	tr.fanOut(context.Background(), logr.Discard())

	if got := drainOne(t, crdCh, time.Second); got != "crd-l" {
		t.Errorf("crd channel: got %q, want crd-l", got)
	}
	if got := drainOne(t, mwcCh, time.Second); got != "mwc-l" {
		t.Errorf("mwc channel: got %q, want mwc-l", got)
	}
	if got := drainOne(t, vwcCh, time.Second); got != "vwc-l" {
		t.Errorf("vwc channel: got %q, want vwc-l", got)
	}
	// Unlabeled CRD must not leak onto crdCh.
	select {
	case ev := <-crdCh:
		t.Errorf("unexpected extra event on crdCh: %s", ev.Object.GetName())
	default:
	}
}

// TestTargetResyncTriggerCRDListErrorDoesNotStarveMWCVWC is the head-of-line
// blocking regression test for #2c. It uses an interceptor to make CRD List
// fail while MWC/VWC List succeed, and asserts fanOut still delivers
// MWC/VWC events on their channels within a short timeout. Under the
// old sequential implementation, a CRD list failure would log and continue,
// but a wedged CRD *send* (buffered channel full + slow consumer) would
// block the whole pass — the goroutine rewrite makes each kind independent,
// so this test primarily exercises the "one kind fails, others proceed"
// contract that the new implementation guarantees.
func TestTargetResyncTriggerCRDListErrorDoesNotStarveMWCVWC(t *testing.T) {
	s := testScheme(t)
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "mwc-x", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "vwc-x", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	base := fake.NewClientBuilder().WithScheme(s).WithObjects(mwc, vwc).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*apiextensionsv1.CustomResourceDefinitionList); ok {
				return errors.New("simulated CRD list failure")
			}
			return cl.List(ctx, list, opts...)
		},
	})

	// Snapshot the error counter before the pass so a shared registry
	// doesn't leak counts across tests.
	errBefore := readCounter(t, fanOutKindCRD, metrics.FanOutError)

	tr, _, mwcCh, vwcCh := tcTrigger(c, nil, 8)
	tr.fanOut(context.Background(), logr.Discard())

	if got := drainOne(t, mwcCh, time.Second); got != "mwc-x" {
		t.Errorf("mwc channel did not receive event despite CRD list failure: got %q", got)
	}
	if got := drainOne(t, vwcCh, time.Second); got != "vwc-x" {
		t.Errorf("vwc channel did not receive event despite CRD list failure: got %q", got)
	}
	errAfter := readCounter(t, fanOutKindCRD, metrics.FanOutError)
	if errAfter-errBefore != 1 {
		t.Errorf("RotationFanOutEventsTotal{kind=TargetCRD,outcome=error} delta = %v, want 1", errAfter-errBefore)
	}
}

// TestTargetResyncTriggerCRDSendBlockedDoesNotStarveMWCVWC is the true
// head-of-line-blocking regression test for #2c: it saturates crdCh
// BEFORE fanOut runs, so the CRD goroutine blocks on send (not on List).
// Under the pre-fix sequential implementation, that would freeze the
// entire pass — MWC/VWC would never be enumerated. The per-kind
// goroutine rewrite makes the three fan-outs independent, so MWC/VWC
// events must arrive while the CRD goroutine is still parked on its
// blocked send. fanOut itself only returns when ctx is cancelled (the
// blocked CRD send abandons via the send()'s ctx.Done() arm), so this
// test drives fanOut in a goroutine and cancels the context at the end.
func TestTargetResyncTriggerCRDSendBlockedDoesNotStarveMWCVWC(t *testing.T) {
	s := testScheme(t)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "crd-blocked", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "mwc-live", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "vwc-live", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(crd, mwc, vwc).Build()

	// crdCh is size 1 and pre-filled: the CRD goroutine's first send()
	// blocks. mwcCh/vwcCh have room so their goroutines can complete.
	crdCh := make(chan event.GenericEvent, 1)
	mwcCh := make(chan event.GenericEvent, 4)
	vwcCh := make(chan event.GenericEvent, 4)
	crdCh <- event.GenericEvent{Object: &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "prefill"}}}

	tr := &targetResyncTrigger{
		targetClient: c,
		labelKey:     targetLabelKey,
		labelValue:   targetLabelValue,
		crdCh:        crdCh,
		mwcCh:        mwcCh,
		vwcCh:        vwcCh,
	}

	ctx, cancel := context.WithCancel(context.Background())
	fanOutDone := make(chan struct{})
	go func() {
		tr.fanOut(ctx, logr.Discard())
		close(fanOutDone)
	}()

	// While the CRD goroutine is parked on its blocked send, MWC and VWC
	// events must still arrive. Under sequential HOL blocking these
	// would never fire.
	if got := drainOne(t, mwcCh, 2*time.Second); got != "mwc-live" {
		t.Errorf("mwc did not arrive while CRD send blocked: got %q", got)
	}
	if got := drainOne(t, vwcCh, 2*time.Second); got != "vwc-live" {
		t.Errorf("vwc did not arrive while CRD send blocked: got %q", got)
	}

	// Cancel context so the blocked CRD goroutine unwinds via send()'s
	// ctx.Done() arm. Assert fanOut returns cleanly (no goroutine leak).
	cancel()
	select {
	case <-fanOutDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fanOut did not return after ctx cancel while CRD send was blocked")
	}
}
func readCounter(t *testing.T, kind, outcome string) float64 {
	t.Helper()
	m, err := metrics.RotationFanOutEventsTotal.GetMetricWithLabelValues(kind, outcome)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q,%q): %v", kind, outcome, err)
	}
	var pb dto.Metric
	if err := m.Write(&pb); err != nil {
		t.Fatalf("counter Write: %v", err)
	}
	if pb.Counter == nil {
		return 0
	}
	return pb.Counter.GetValue()
}

// TestTargetResyncTriggerStartupPassAndRotation asserts that Start performs
// an initial fanOut immediately (so startup state is reconciled even with
// an empty event backlog) AND that each rotation-trigger signal drives an
// additional fanOut pass, AND that Start returns cleanly on ctx cancel.
func TestTargetResyncTriggerStartupPassAndRotation(t *testing.T) {
	s := testScheme(t)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "crd-startup", Labels: map[string]string{targetLabelKey: targetLabelValue}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(crd).Build()
	rot := make(chan struct{}, 1)
	tr, crdCh, _, _ := tcTrigger(c, rot, 8)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	var startErr error
	go func() {
		defer wg.Done()
		startErr = tr.Start(ctx)
	}()

	// Initial pass: exactly one CRD event within a short deadline.
	if got := drainOne(t, crdCh, 2*time.Second); got != "crd-startup" {
		t.Errorf("initial pass: got %q, want crd-startup", got)
	}

	// Rotation pass: another CRD event.
	rot <- struct{}{}
	if got := drainOne(t, crdCh, 2*time.Second); got != "crd-startup" {
		t.Errorf("rotation pass: got %q, want crd-startup", got)
	}

	// Clean shutdown on ctx cancel.
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("targetResyncTrigger.Start did not return after ctx cancel")
	}
	if startErr != nil {
		t.Errorf("Start returned error: %v", startErr)
	}
}

// TestLeaderOnlyClusterIsNotHasCache is the regression test for PR #14
// finding #1. LeaderOnlyCluster's whole reason for existing is that it
// implements manager.Runnable + LeaderElectionRunnable but DELIBERATELY
// does NOT expose GetCache(). controller-runtime v0.23.1's
// runnables.Add type-switch (pkg/manager/runnable_group.go) matches
//
//	case hasCache: // Runnable + GetCache() cache.Cache
//
// BEFORE it checks LeaderElectionRunnable. A future maintainer adding
// an innocuous convenience passthrough like
//
//	func (l *LeaderOnlyCluster) GetCache() cache.Cache { return l.Cluster.GetCache() }
//
// would silently reinstate the original bug: the target cluster lands
// back in the manager's Caches runnable group, every replica (leader
// and followers) blocks on WaitForCacheSync unconditionally, readyz
// stays red, no CrashLoopBackOff. Nothing else fails to compile and
// no other test breaks — the failure is quiet and would reach a
// cluster before anyone noticed.
//
// This test pins the invariant: LeaderOnlyCluster must NOT satisfy
// the hasCache interface, AND must require leader election.
func TestLeaderOnlyClusterIsNotHasCache(t *testing.T) {
	var r manager.Runnable = &LeaderOnlyCluster{}
	if _, ok := r.(interface{ GetCache() cache.Cache }); ok {
		t.Fatal("LeaderOnlyCluster must NOT expose GetCache(): it would land in " +
			"the manager's Caches runnable group and block followers on cache sync")
	}
	ler, ok := r.(manager.LeaderElectionRunnable)
	if !ok {
		t.Fatal("LeaderOnlyCluster must implement manager.LeaderElectionRunnable")
	}
	if !ler.NeedLeaderElection() {
		t.Fatal("LeaderOnlyCluster.NeedLeaderElection() must return true; " +
			"otherwise it lands in the manager's Others group and starts on followers")
	}
}
