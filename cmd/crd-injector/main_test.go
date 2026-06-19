// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const validCRD1 = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: foos.example.com
spec:
  group: example.com
  names: {kind: Foo, listKind: FooList, plural: foos, singular: foo}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema: {type: object}
`

const validCRD2 = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: bars.example.com
spec:
  group: example.com
  names: {kind: Bar, listKind: BarList, plural: bars, singular: bar}
  scope: Cluster
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema: {type: object}
`

const validCRD3 = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: bazs.example.com
spec:
  group: example.com
  names: {kind: Baz, listKind: BazList, plural: bazs, singular: baz}
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema: {type: object}
`

// nonCRD is a ConfigMap masquerading inside the YAML stream — anything that is
// not a CRD must be rejected so the binary cannot be misused as a generic
// applier.
const nonCRD = `apiVersion: v1
kind: ConfigMap
metadata:
  name: not-a-crd
data:
  foo: bar
`

func TestParseCRDs(t *testing.T) {
	tests := []struct {
		name      string
		data      map[string]string
		wantNames []string // expected CRD names in the returned order
		wantErr   string   // substring; empty means no error expected
	}{
		{
			name:      "single CRD in single key",
			data:      map[string]string{"crd.yaml": validCRD1},
			wantNames: []string{"foos.example.com"},
		},
		{
			name: "multi-document YAML in one key",
			data: map[string]string{
				"crds.yaml": validCRD1 + "---\n" + validCRD2,
			},
			wantNames: []string{"foos.example.com", "bars.example.com"},
		},
		{
			name: "multiple keys are processed in deterministic (sorted) order",
			data: map[string]string{
				"z-last.yaml":   validCRD3,
				"a-first.yaml":  validCRD1,
				"m-middle.yaml": validCRD2,
			},
			wantNames: []string{
				"foos.example.com", // a-first
				"bars.example.com", // m-middle
				"bazs.example.com", // z-last
			},
		},
		{
			name:      "empty documents are skipped",
			data:      map[string]string{"crd.yaml": "---\n---\n" + validCRD1 + "---\n"},
			wantNames: []string{"foos.example.com"},
		},
		{
			name:      "nil ConfigMap data returns nil",
			data:      nil,
			wantNames: nil,
		},
		{
			name:      "empty ConfigMap data returns nil",
			data:      map[string]string{},
			wantNames: nil,
		},
		{
			name:      "empty value in key is a no-op",
			data:      map[string]string{"empty.yaml": ""},
			wantNames: nil,
		},
		{
			name:    "non-CRD object is rejected",
			data:    map[string]string{"mixed.yaml": validCRD1 + "---\n" + nonCRD},
			wantErr: "expected apiextensions.k8s.io/v1 CustomResourceDefinition",
		},
		{
			name:    "malformed YAML is reported with key + doc index",
			data:    map[string]string{"bad.yaml": "this: is: not: valid: yaml: ["},
			wantErr: "bad.yaml",
		},
		{
			name:    "unknown apiVersion is rejected",
			data:    map[string]string{"crd.yaml": "apiVersion: nope.example.com/v1\nkind: Whatever\nmetadata:\n  name: x\n"},
			wantErr: "crd.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{Data: tc.data}
			got, err := parseCRDs(cm)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error to contain %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(got) != len(tc.wantNames) {
				t.Fatalf("got %d CRDs, want %d (names=%v)", len(got), len(tc.wantNames), namesOf(got))
			}
			for i, want := range tc.wantNames {
				if got[i].Name != want {
					t.Errorf("CRD[%d] = %q, want %q", i, got[i].Name, want)
				}
			}
		})
	}
}

func namesOf(crds []*apiextensionsv1.CustomResourceDefinition) []string {
	out := make([]string, len(crds))
	for i, c := range crds {
		out[i] = c.Name
	}
	return out
}

// TestStripWebhookInjectorFields covers the field-stripping that crd-injector
// performs before server-side applying a CRD that already exists. The three
// fields under spec.conversion.webhook.clientConfig that webhook-injector owns
// at runtime — caBundle, url, service — must be cleared on the in-memory
// object so the subsequent JSON marshal omits them entirely (all three are
// omitempty in their API types). Anything else on the CRD must be left alone.
func TestStripWebhookInjectorFields(t *testing.T) {
	caBundle := []byte("AAAA")
	url := "https://stale.example.com/convert"
	svcPath := "/convert"

	tests := []struct {
		name string
		in   *apiextensionsv1.CustomResourceDefinition
		// jsonShouldNotContain are substrings that must NOT appear in the
		// marshalled JSON of the (possibly stripped) CRD. Empty means we
		// don't care about the JSON contents for this case.
		jsonShouldNotContain []string
	}{
		{
			name: "Webhook strategy with service+caBundle is stripped",
			in: &apiextensionsv1.CustomResourceDefinition{
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Conversion: &apiextensionsv1.CustomResourceConversion{
						Strategy: apiextensionsv1.WebhookConverter,
						Webhook: &apiextensionsv1.WebhookConversion{
							ClientConfig: &apiextensionsv1.WebhookClientConfig{
								CABundle: caBundle,
								Service: &apiextensionsv1.ServiceReference{
									Namespace: "ns",
									Name:      "svc",
									Path:      &svcPath,
								},
							},
						},
					},
				},
			},
			jsonShouldNotContain: []string{`"caBundle"`, `"service"`, `"url"`},
		},
		{
			name: "Webhook strategy with url+caBundle is stripped",
			in: &apiextensionsv1.CustomResourceDefinition{
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Conversion: &apiextensionsv1.CustomResourceConversion{
						Strategy: apiextensionsv1.WebhookConverter,
						Webhook: &apiextensionsv1.WebhookConversion{
							ClientConfig: &apiextensionsv1.WebhookClientConfig{
								CABundle: caBundle,
								URL:      &url,
							},
						},
					},
				},
			},
			jsonShouldNotContain: []string{`"caBundle"`, `"service"`, `"url"`},
		},
		{
			name: "None strategy is left untouched",
			in: &apiextensionsv1.CustomResourceDefinition{
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Conversion: &apiextensionsv1.CustomResourceConversion{
						Strategy: apiextensionsv1.NoneConverter,
					},
				},
			},
		},
		{
			name: "nil Conversion is a no-op",
			in:   &apiextensionsv1.CustomResourceDefinition{},
		},
		{
			name: "Webhook strategy with nil Webhook is a no-op",
			in: &apiextensionsv1.CustomResourceDefinition{
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Conversion: &apiextensionsv1.CustomResourceConversion{
						Strategy: apiextensionsv1.WebhookConverter,
					},
				},
			},
		},
		{
			name: "Webhook strategy with nil ClientConfig is a no-op",
			in: &apiextensionsv1.CustomResourceDefinition{
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{
					Conversion: &apiextensionsv1.CustomResourceConversion{
						Strategy: apiextensionsv1.WebhookConverter,
						Webhook:  &apiextensionsv1.WebhookConversion{},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stripWebhookInjectorFields(tc.in)

			// For Webhook-strategy cases, the in-memory object's three
			// fields must be zeroed.
			if conv := tc.in.Spec.Conversion; conv != nil &&
				conv.Strategy == apiextensionsv1.WebhookConverter &&
				conv.Webhook != nil && conv.Webhook.ClientConfig != nil {
				cc := conv.Webhook.ClientConfig
				if cc.CABundle != nil {
					t.Errorf("CABundle not cleared: got %q", cc.CABundle)
				}
				if cc.URL != nil {
					t.Errorf("URL not cleared: got %q", *cc.URL)
				}
				if cc.Service != nil {
					t.Errorf("Service not cleared: got %+v", *cc.Service)
				}
			}

			if len(tc.jsonShouldNotContain) == 0 {
				return
			}
			body, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, needle := range tc.jsonShouldNotContain {
				if strings.Contains(string(body), needle) {
					t.Errorf("apply body contains %s; full body=%s", needle, body)
				}
			}
		})
	}
}
