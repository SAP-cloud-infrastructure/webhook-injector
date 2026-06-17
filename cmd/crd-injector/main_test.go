// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
