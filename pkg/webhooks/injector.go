// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package webhooks

// InjectCABundle injects the CA bundle into all webhook configurations
func InjectCABundle(configs *WebhookConfigurations, caCert []byte) {
	// Store raw PEM bytes - Kubernetes handles base64 encoding during serialization

	// Inject into mutating webhooks
	for _, mwc := range configs.Mutating {
		for i := range mwc.Webhooks {
			mwc.Webhooks[i].ClientConfig.CABundle = caCert
		}
	}

	// Inject into validating webhooks
	for _, vwc := range configs.Validating {
		for i := range vwc.Webhooks {
			vwc.Webhooks[i].ClientConfig.CABundle = caCert
		}
	}
}
