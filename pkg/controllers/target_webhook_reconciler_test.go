// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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
