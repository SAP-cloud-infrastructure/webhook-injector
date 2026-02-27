// Copyright 2026 SAP SE or an SAP affiliate company
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
