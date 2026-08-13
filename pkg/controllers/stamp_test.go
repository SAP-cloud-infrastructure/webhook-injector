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
