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
