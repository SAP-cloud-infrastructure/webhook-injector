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
