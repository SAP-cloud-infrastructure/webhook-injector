package webhooks

import (
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
)

func TestParseWebhookConfigurations(t *testing.T) {
	yaml := `---
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: test-mutating
webhooks:
  - name: mutate.example.com
    clientConfig:
      service:
        name: webhook-svc
        namespace: default
        path: /mutate
        port: 9443
      caBundle: ""
    admissionReviewVersions: ["v1"]
    sideEffects: None
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: test-validating
webhooks:
  - name: validate.example.com
    clientConfig:
      service:
        name: another-svc
        namespace: test-ns
        path: /validate
      caBundle: ""
    admissionReviewVersions: ["v1"]
    sideEffects: None
`

	configs, err := ParseWebhookConfigurations([]byte(yaml))
	if err != nil {
		t.Fatalf("Failed to parse webhook configurations: %v", err)
	}

	if len(configs.Mutating) != 1 {
		t.Errorf("Expected 1 mutating webhook config, got %d", len(configs.Mutating))
	}
	if len(configs.Validating) != 1 {
		t.Errorf("Expected 1 validating webhook config, got %d", len(configs.Validating))
	}

	// Check mutating webhook
	if configs.Mutating[0].Name != "test-mutating" {
		t.Errorf("Expected mutating config name 'test-mutating', got '%s'", configs.Mutating[0].Name)
	}

	// Check validating webhook
	if configs.Validating[0].Name != "test-validating" {
		t.Errorf("Expected validating config name 'test-validating', got '%s'", configs.Validating[0].Name)
	}
}

func TestExtractServiceReferences(t *testing.T) {
	mutating := &admissionv1.MutatingWebhookConfiguration{
		Webhooks: []admissionv1.MutatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      "webhook-svc",
						Namespace: "default",
					},
				},
			},
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      "another-svc",
						Namespace: "default",
					},
				},
			},
		},
	}

	validating := &admissionv1.ValidatingWebhookConfiguration{
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      "webhook-svc",
						Namespace: "default",
					},
				},
			},
		},
	}

	configs := &WebhookConfigurations{
		Mutating:   []*admissionv1.MutatingWebhookConfiguration{mutating},
		Validating: []*admissionv1.ValidatingWebhookConfiguration{validating},
	}

	refs := ExtractServiceReferences(configs)

	// Should have 2 unique services (webhook-svc and another-svc, both in default)
	if len(refs) != 2 {
		t.Errorf("Expected 2 unique service references, got %d", len(refs))
	}

	// Verify specific services
	foundWebhookSvc := false
	foundAnotherSvc := false
	for _, ref := range refs {
		if ref.Name == "webhook-svc" && ref.Namespace == "default" {
			foundWebhookSvc = true
		}
		if ref.Name == "another-svc" && ref.Namespace == "default" {
			foundAnotherSvc = true
		}
	}

	if !foundWebhookSvc {
		t.Error("Expected to find webhook-svc in default namespace")
	}
	if !foundAnotherSvc {
		t.Error("Expected to find another-svc in default namespace")
	}
}

func TestExtractServiceReferences_WithURLs(t *testing.T) {
	mutating := &admissionv1.MutatingWebhookConfiguration{
		Webhooks: []admissionv1.MutatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					URL: strPtr("https://webhook.example.com:9443/mutate"),
				},
			},
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					URL: strPtr("https://another-webhook.example.com/validate"),
				},
			},
		},
	}

	validating := &admissionv1.ValidatingWebhookConfiguration{
		Webhooks: []admissionv1.ValidatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					URL: strPtr("https://webhook.example.com:9443/mutate"), // Duplicate hostname
				},
			},
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					URL: strPtr("https://third-webhook.test.local:8443/hook"),
				},
			},
		},
	}

	configs := &WebhookConfigurations{
		Mutating:   []*admissionv1.MutatingWebhookConfiguration{mutating},
		Validating: []*admissionv1.ValidatingWebhookConfiguration{validating},
	}

	refs := ExtractServiceReferences(configs)

	// Should have 3 unique hostnames
	if len(refs) != 3 {
		t.Errorf("Expected 3 unique hostname references, got %d", len(refs))
	}

	// Verify specific hostnames
	expectedHostnames := map[string]bool{
		"webhook.example.com":         false,
		"another-webhook.example.com": false,
		"third-webhook.test.local":    false,
	}

	for _, ref := range refs {
		if ref.Hostname == "" {
			t.Error("Expected Hostname to be set for URL-based webhook")
		}
		if _, exists := expectedHostnames[ref.Hostname]; exists {
			expectedHostnames[ref.Hostname] = true
		}
	}

	for hostname, found := range expectedHostnames {
		if !found {
			t.Errorf("Expected to find hostname %s", hostname)
		}
	}
}

func TestExtractServiceReferences_MixedServiceAndURL(t *testing.T) {
	mutating := &admissionv1.MutatingWebhookConfiguration{
		Webhooks: []admissionv1.MutatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					Service: &admissionv1.ServiceReference{
						Name:      "webhook-svc",
						Namespace: "default",
					},
				},
			},
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					URL: strPtr("https://external-webhook.example.com/mutate"),
				},
			},
		},
	}

	configs := &WebhookConfigurations{
		Mutating: []*admissionv1.MutatingWebhookConfiguration{mutating},
	}

	refs := ExtractServiceReferences(configs)

	// Should have 2 references: 1 service + 1 URL
	if len(refs) != 2 {
		t.Errorf("Expected 2 references (1 service + 1 URL), got %d", len(refs))
	}

	foundService := false
	foundURL := false

	for _, ref := range refs {
		if ref.Name == "webhook-svc" && ref.Namespace == "default" && ref.Hostname == "" {
			foundService = true
		}
		if ref.Hostname == "external-webhook.example.com" && ref.Name == "" && ref.Namespace == "" {
			foundURL = true
		}
	}

	if !foundService {
		t.Error("Expected to find service-based reference")
	}
	if !foundURL {
		t.Error("Expected to find URL-based reference")
	}
}

func TestExtractServiceReferences_InvalidURL(t *testing.T) {
	mutating := &admissionv1.MutatingWebhookConfiguration{
		Webhooks: []admissionv1.MutatingWebhook{
			{
				ClientConfig: admissionv1.WebhookClientConfig{
					URL: strPtr("not-a-valid-url"),
				},
			},
		},
	}

	configs := &WebhookConfigurations{
		Mutating: []*admissionv1.MutatingWebhookConfiguration{mutating},
	}

	refs := ExtractServiceReferences(configs)

	// Invalid URLs should be skipped
	if len(refs) != 0 {
		t.Errorf("Expected 0 references for invalid URL, got %d", len(refs))
	}
}

func strPtr(s string) *string {
	return &s
}
