package webhooks

import (
	"encoding/base64"
)

// InjectCABundle injects the CA bundle into all webhook configurations
func InjectCABundle(configs *WebhookConfigurations, caCert []byte) {
	caBundle := base64.StdEncoding.EncodeToString(caCert)

	// Inject into mutating webhooks
	for _, mwc := range configs.Mutating {
		for i := range mwc.Webhooks {
			mwc.Webhooks[i].ClientConfig.CABundle = []byte(caBundle)
		}
	}

	// Inject into validating webhooks
	for _, vwc := range configs.Validating {
		for i := range vwc.Webhooks {
			vwc.Webhooks[i].ClientConfig.CABundle = []byte(caBundle)
		}
	}
}
