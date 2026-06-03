// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1api "k8s.io/api/admission/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// TestAdmission_HTTPEndpoint_AppliesPatchAtAdmissionTime mounts the Mutator
// behind controller-runtime's webhook.Admission server (the same wrapper
// main.go uses), POSTs a real AdmissionReview at it, and asserts the
// returned patch contains the caBundle and URL rewrite. This is the closest
// we can stand to an e2e test without spinning up kind: it covers the wire
// format, JSON-patch generation, and the handler/Server interaction that
// the kube-apiserver would drive.
func TestAdmission_HTTPEndpoint_AppliesPatchAtAdmissionTime(t *testing.T) {
	caBundle := []byte("PEM-CERT")
	mutatePath := "/mutate"
	mwc := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "tenant-mwc",
			Labels: map[string]string{"webhook-injector/managed": "true"},
		},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name: "wh.example.com",
			ClientConfig: admissionv1.WebhookClientConfig{
				Service: &admissionv1.ServiceReference{Name: "svc", Namespace: "ns", Path: &mutatePath},
			},
		}},
	}
	rawObj, err := json.Marshal(mwc)
	if err != nil {
		t.Fatalf("marshal mwc: %v", err)
	}

	mutator := newMutator(t, "ext.example.com:9443", caBundle)
	srv := httptest.NewServer(&webhook.Admission{Handler: mutator})
	defer srv.Close()

	review := admissionv1api.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Request: &admissionv1api.AdmissionRequest{
			UID:       "11111111-1111-1111-1111-111111111111",
			Kind:      metav1.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: KindMutatingWebhookConfiguration},
			Resource:  metav1.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"},
			Name:      "tenant-mwc",
			Operation: admissionv1api.Create,
			Object:    runtime.RawExtension{Raw: rawObj},
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal review: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out admissionv1api.AdmissionReview
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Response == nil {
		t.Fatalf("nil response")
	}
	if !out.Response.Allowed {
		t.Fatalf("not allowed: %+v", out.Response.Result)
	}
	if len(out.Response.Patch) == 0 {
		t.Fatalf("expected patch bytes, got none")
	}
	if out.Response.PatchType == nil || *out.Response.PatchType != admissionv1api.PatchTypeJSONPatch {
		t.Fatalf("PatchType = %v, want JSONPatch", out.Response.PatchType)
	}

	// Patch is base64-decoded by the apiserver in real life; here we decode
	// so the assertions read against the JSON-patch document.
	var ops []map[string]any
	if err := json.Unmarshal(out.Response.Patch, &ops); err != nil {
		// On the wire kube-apiserver gets the raw JSON; httptest gives us
		// what the handler wrote, which is already raw JSON for ctrl-runtime.
		// If decoding fails we fall back to base64 decode.
		decoded, derr := base64.StdEncoding.DecodeString(string(out.Response.Patch))
		if derr != nil {
			t.Fatalf("patch is neither JSON nor base64: jsonErr=%v b64Err=%v", err, derr)
		}
		if err := json.Unmarshal(decoded, &ops); err != nil {
			t.Fatalf("decode patch ops: %v", err)
		}
	}

	wantURL := "https://ext.example.com:9443/mutate"
	var sawCA, sawURL bool
	for _, op := range ops {
		path, _ := op["path"].(string)
		switch path {
		case "/webhooks/0/clientConfig/caBundle":
			sawCA = true
		case "/webhooks/0/clientConfig/url":
			if got, _ := op["value"].(string); got != wantURL {
				t.Errorf("url op value = %q, want %q", got, wantURL)
			}
			sawURL = true
		}
	}
	if !sawCA {
		t.Errorf("missing caBundle patch op; ops=%+v", ops)
	}
	if !sawURL {
		t.Errorf("missing url patch op; ops=%+v", ops)
	}
}

// helper to silence unused imports if test build trims one in future refactors.
var _ = context.Background
