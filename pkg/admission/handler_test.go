// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"context"
	"encoding/json"
	"testing"

	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1api "k8s.io/api/admission/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := admissionv1.AddToScheme(s); err != nil {
		t.Fatalf("add admissionv1 to scheme: %v", err)
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatalf("add apiextensionsv1 to scheme: %v", err)
	}
	return s
}

func newMutator(t *testing.T, externalHost string, caBundle []byte) *Mutator {
	t.Helper()
	return &Mutator{
		Decoder:      admission.NewDecoder(newTestScheme(t)),
		ExternalHost: externalHost,
		CABundle: func(_ context.Context) ([]byte, error) {
			return caBundle, nil
		},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// findPatchOp returns the first patch op whose path matches; ok=false if none.
func findPatchOp(patches []jsonpatch.Operation, path string) (jsonpatch.Operation, bool) {
	for _, op := range patches {
		if op.Path == path {
			return op, true
		}
	}
	return jsonpatch.Operation{}, false
}

func decodePatches(t *testing.T, resp admission.Response) []jsonpatch.Operation {
	t.Helper()
	return resp.Patches
}

func TestMutator_MWC_ServiceFormRewrittenToURL(t *testing.T) {
	caBundle := []byte("PEM-CERT")
	mutatePath := "/mutate"
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-mwc"},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name: "wh.example.com",
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Name:      "svc",
					Namespace: "ns",
					Path:      &mutatePath,
				},
			},
		}},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: KindMutatingWebhookConfiguration},
		Operation: admissionv1api.Create,
		Object:    runtime.RawExtension{Raw: mustJSON(t, mwc)},
	}}

	resp := newMutator(t, "ext.example.com:9443", caBundle).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	patches := decodePatches(t, resp)
	caOp, ok := findPatchOp(patches, "/webhooks/0/clientConfig/caBundle")
	if !ok {
		t.Fatalf("missing caBundle patch; got %+v", patches)
	}
	// JSON patches encode []byte as base64 of raw bytes, but we just check it's non-nil.
	if caOp.Value == nil {
		t.Errorf("caBundle patch value nil")
	}

	urlOp, ok := findPatchOp(patches, "/webhooks/0/clientConfig/url")
	if !ok {
		t.Fatalf("missing url patch; got %+v", patches)
	}
	if got, want := urlOp.Value.(string), "https://ext.example.com:9443/mutate"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}

	if _, ok := findPatchOp(patches, "/webhooks/0/clientConfig/service"); !ok {
		// service field removed via "remove" op
		var found bool
		for _, op := range patches {
			if op.Operation == "remove" && op.Path == "/webhooks/0/clientConfig/service" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected /webhooks/0/clientConfig/service to be removed; patches=%+v", patches)
		}
	}
}

func TestMutator_VWC_AlreadyURL_OnlyCABundleChanges(t *testing.T) {
	caBundle := []byte("PEM-CERT")
	existingURL := "https://already.example.com/validate"
	vwc := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-vwc"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name: "wh.example.com",
			ClientConfig: admissionv1.WebhookClientConfig{
				URL: &existingURL,
			},
		}},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: KindValidatingWebhookConfiguration},
		Operation: admissionv1api.Update,
		Object:    runtime.RawExtension{Raw: mustJSON(t, vwc)},
	}}

	resp := newMutator(t, "ext.example.com:9443", caBundle).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	patches := decodePatches(t, resp)

	if _, ok := findPatchOp(patches, "/webhooks/0/clientConfig/caBundle"); !ok {
		t.Fatalf("missing caBundle patch; got %+v", patches)
	}
	if _, ok := findPatchOp(patches, "/webhooks/0/clientConfig/url"); ok {
		t.Errorf("did not expect url to be patched when already URL-form; patches=%+v", patches)
	}
}

func TestMutator_CRD_WithWebhookConversion(t *testing.T) {
	caBundle := []byte("PEM-CERT")
	convertPath := "/convert"
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "things.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "things", Singular: "thing", Kind: "Thing", ListKind: "ThingList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Conversion: &apiextensionsv1.CustomResourceConversion{
				Strategy: apiextensionsv1.WebhookConverter,
				Webhook: &apiextensionsv1.WebhookConversion{
					ConversionReviewVersions: []string{"v1"},
					ClientConfig: &apiextensionsv1.WebhookClientConfig{
						Service: &apiextensionsv1.ServiceReference{
							Name: "conv", Namespace: "ns", Path: &convertPath,
						},
					},
				},
			},
		},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: KindCustomResourceDefinition},
		Operation: admissionv1api.Create,
		Object:    runtime.RawExtension{Raw: mustJSON(t, crd)},
	}}

	resp := newMutator(t, "ext.example.com:9443", caBundle).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	patches := decodePatches(t, resp)

	if _, ok := findPatchOp(patches, "/spec/conversion/webhook/clientConfig/caBundle"); !ok {
		t.Fatalf("missing caBundle patch; got %+v", patches)
	}
	urlOp, ok := findPatchOp(patches, "/spec/conversion/webhook/clientConfig/url")
	if !ok {
		t.Fatalf("missing url patch; got %+v", patches)
	}
	if got, want := urlOp.Value.(string), "https://ext.example.com:9443/convert"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

func TestMutator_CRD_NoWebhookConversion_NoOp(t *testing.T) {
	caBundle := []byte("PEM-CERT")
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "things.example.com"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "things", Singular: "thing", Kind: "Thing", ListKind: "ThingList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			// No Conversion / strategy != Webhook.
		},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: KindCustomResourceDefinition},
		Operation: admissionv1api.Create,
		Object:    runtime.RawExtension{Raw: mustJSON(t, crd)},
	}}

	resp := newMutator(t, "ext.example.com:9443", caBundle).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patch for CRD without Webhook conversion, got %+v", resp.Patches)
	}
}

func TestMutator_EmptyExternalHost_OnlyCABundleChanges(t *testing.T) {
	caBundle := []byte("PEM-CERT")
	mutatePath := "/mutate"
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-mwc"},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name: "wh.example.com",
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{
					Name: "svc", Namespace: "ns", Path: &mutatePath,
				},
			},
		}},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: KindMutatingWebhookConfiguration},
		Operation: admissionv1api.Create,
		Object:    runtime.RawExtension{Raw: mustJSON(t, mwc)},
	}}

	resp := newMutator(t, "", caBundle).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	patches := decodePatches(t, resp)

	if _, ok := findPatchOp(patches, "/webhooks/0/clientConfig/caBundle"); !ok {
		t.Fatalf("missing caBundle patch; got %+v", patches)
	}
	if _, ok := findPatchOp(patches, "/webhooks/0/clientConfig/url"); ok {
		t.Errorf("did not expect url patch when ExternalHost is empty; patches=%+v", patches)
	}
}

func TestMutator_EmptyCABundle_AllowsUnchanged(t *testing.T) {
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-mwc"},
		Webhooks:   []admissionv1.MutatingWebhook{{Name: "wh.example.com"}},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: KindMutatingWebhookConfiguration},
		Operation: admissionv1api.Create,
		Object:    runtime.RawExtension{Raw: mustJSON(t, mwc)},
	}}

	resp := newMutator(t, "ext.example.com:9443", nil).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patch when caBundle empty, got %+v", resp.Patches)
	}
}

func TestMutator_UnhandledKind(t *testing.T) {
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
		Operation: admissionv1api.Create,
		Object:    runtime.RawExtension{Raw: []byte(`{}`)},
	}}

	resp := newMutator(t, "ext.example.com:9443", []byte("PEM")).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patch for unhandled kind, got %+v", resp.Patches)
	}
}

func TestMutator_SelfMWC_SkipByName(t *testing.T) {
	caBundle := []byte("PEM-CERT")
	mutatePath := "/mutate"
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook-injector-mutator"},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name: "wh.example.com",
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{Name: "svc", Namespace: "ns", Path: &mutatePath},
			},
		}},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: KindMutatingWebhookConfiguration},
		Name:      "webhook-injector-mutator",
		Operation: admissionv1api.Update,
		Object:    runtime.RawExtension{Raw: mustJSON(t, mwc)},
	}}

	m := newMutator(t, "ext.example.com:9443", caBundle)
	m.SelfMWCName = "webhook-injector-mutator"
	resp := m.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patches for self MWC, got %+v", resp.Patches)
	}
}

// Compile-time guard: schema registration is required for the decoder.
// (This file would not compile without admissionv1 + apiextensionsv1 in newTestScheme.)
