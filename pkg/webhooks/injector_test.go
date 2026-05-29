// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package webhooks

import (
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInjectCABundle(t *testing.T) {
	caCert := []byte("test-ca-certificate")

	mutating := &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-mutating"},
		Webhooks: []admissionv1.MutatingWebhook{
			{
				Name: "webhook1",
				ClientConfig: admissionv1.WebhookClientConfig{
					CABundle: nil,
				},
			},
			{
				Name: "webhook2",
				ClientConfig: admissionv1.WebhookClientConfig{
					CABundle: []byte("old-bundle"),
				},
			},
		},
	}

	validating := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "test-validating"},
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				Name: "webhook1",
				ClientConfig: admissionv1.WebhookClientConfig{
					CABundle: nil,
				},
			},
		},
	}

	configs := &WebhookConfigurations{
		Mutating:   []*admissionv1.MutatingWebhookConfiguration{mutating},
		Validating: []*admissionv1.ValidatingWebhookConfiguration{validating},
	}

	InjectCABundle(configs, caCert)

	// Check mutating webhooks - verify raw PEM bytes are stored
	for i, webhook := range configs.Mutating[0].Webhooks {
		if string(webhook.ClientConfig.CABundle) != string(caCert) {
			t.Errorf("Mutating webhook %d: expected CA bundle '%s', got '%s'", i, string(caCert), string(webhook.ClientConfig.CABundle))
		}
	}

	// Check validating webhooks - verify raw PEM bytes are stored
	for i, webhook := range configs.Validating[0].Webhooks {
		if string(webhook.ClientConfig.CABundle) != string(caCert) {
			t.Errorf("Validating webhook %d: expected CA bundle '%s', got '%s'", i, string(caCert), string(webhook.ClientConfig.CABundle))
		}
	}
}

func TestBuildURL(t *testing.T) {
	tests := []struct {
		name string
		host string
		path string
		want string
	}{
		{"host with port and path", "example.com:9443", "/mutate", "https://example.com:9443/mutate"},
		{"empty path defaults to slash", "example.com:9443", "", "https://example.com:9443/"},
		{"ip host", "172.18.0.3:9443", "/validate", "https://172.18.0.3:9443/validate"},
		{"host without port", "example.com", "/x", "https://example.com/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildURL(tt.host, tt.path); got != tt.want {
				t.Errorf("BuildURL(%q, %q) = %q, want %q", tt.host, tt.path, got, tt.want)
			}
		})
	}
}

func TestExtractPathFromURL(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{"empty url", "", "/"},
		{"path only", "https://h:1/mutate", "/mutate"},
		{"no path", "https://h:1", "/"},
		{"with query", "https://h:1/mutate?timeout=5s", "/mutate?timeout=5s"},
		{"invalid url", "::not-a-url", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractPathFromURL(tt.rawURL); got != tt.want {
				t.Errorf("ExtractPathFromURL(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
		})
	}
}
