// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers/rotationgate"
)

const (
	tcLabelKey   = "webhook-injector/target"
	tcLabelValue = "metal-operator"
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
		ObjectMeta: metav1.ObjectMeta{Name: tcCRDName, Labels: map[string]string{tcLabelKey: tcLabelValue}},
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
		LabelKey: tcLabelKey, LabelValue: tcLabelValue, Gate: gate,
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
		ObjectMeta: metav1.ObjectMeta{Name: "tc-mwc", Labels: map[string]string{tcLabelKey: tcLabelValue}},
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
		ObjectMeta: metav1.ObjectMeta{Name: tcCRDName, Labels: map[string]string{tcLabelKey: tcLabelValue}},
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
		ObjectMeta: metav1.ObjectMeta{Name: tcCRDName, Labels: map[string]string{tcLabelKey: tcLabelValue}},
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
