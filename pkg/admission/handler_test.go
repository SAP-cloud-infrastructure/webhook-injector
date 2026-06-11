// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"context"
	"encoding/json"
	"testing"

	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1api "k8s.io/api/admission/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		t.Fatalf("add apiextensionsv1 to scheme: %v", err)
	}
	return s
}

const testExternalHost = "ext.example.com:9443"

func newMutator(t *testing.T, caBundle []byte) *Mutator {
	t.Helper()
	return &Mutator{
		Decoder:      admission.NewDecoder(newTestScheme(t)),
		ExternalHost: testExternalHost,
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

func decodePatches(_ *testing.T, resp admission.Response) []jsonpatch.Operation {
	return resp.Patches
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

	resp := newMutator(t, caBundle).Handle(context.Background(), req)
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

	resp := newMutator(t, caBundle).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patch for CRD without Webhook conversion, got %+v", resp.Patches)
	}
}

func TestMutator_EmptyCABundle_AllowsUnchanged(t *testing.T) {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "things.example.com"},
	}
	req := admission.Request{AdmissionRequest: admissionv1api.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: KindCustomResourceDefinition},
		Operation: admissionv1api.Create,
		Object:    runtime.RawExtension{Raw: mustJSON(t, crd)},
	}}

	resp := newMutator(t, nil).Handle(context.Background(), req)
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

	resp := newMutator(t, []byte("PEM")).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected Allowed, got: %+v", resp)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("expected no patch for unhandled kind, got %+v", resp.Patches)
	}
}

// Compile-time guard: schema registration is required for the decoder.
// (This file would not compile without apiextensionsv1 in newTestScheme.)
