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
