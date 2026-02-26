package webhooks

import (
	"bytes"
	"fmt"
	"io"
	"net/url"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/certificates"
)

// WebhookConfigurations holds parsed webhook configurations
type WebhookConfigurations struct {
	Mutating   []*admissionv1.MutatingWebhookConfiguration
	Validating []*admissionv1.ValidatingWebhookConfiguration
}

// ParseWebhookConfigurations parses multi-document YAML containing webhook configurations
func ParseWebhookConfigurations(data []byte) (*WebhookConfigurations, error) {
	configs := &WebhookConfigurations{
		Mutating:   make([]*admissionv1.MutatingWebhookConfiguration, 0),
		Validating: make([]*admissionv1.ValidatingWebhookConfiguration, 0),
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &runtime.Unknown{}
		err := decoder.Decode(obj)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to decode YAML document: %w", err)
		}

		// Skip empty documents
		if len(obj.Raw) == 0 {
			continue
		}

		// Decode into typed objects
		decoded, _, err := scheme.Codecs.UniversalDeserializer().Decode(obj.Raw, nil, nil)
		if err != nil {
			// Skip objects we can't decode (might be other resource types)
			continue
		}

		switch typed := decoded.(type) {
		case *admissionv1.MutatingWebhookConfiguration:
			configs.Mutating = append(configs.Mutating, typed)
		case *admissionv1.ValidatingWebhookConfiguration:
			configs.Validating = append(configs.Validating, typed)
		}
	}

	return configs, nil
}

// ExtractServiceReferences extracts unique service references and URL hostnames from webhook configurations
func ExtractServiceReferences(configs *WebhookConfigurations) []certificates.ServiceReference {
	// Use map for deduplication
	// Key format: "service:namespace/name" or "hostname:example.com"
	refMap := make(map[string]certificates.ServiceReference)

	// Extract from mutating webhooks
	for _, mwc := range configs.Mutating {
		for _, webhook := range mwc.Webhooks {
			if webhook.ClientConfig.Service != nil {
				key := fmt.Sprintf("service:%s/%s", webhook.ClientConfig.Service.Namespace, webhook.ClientConfig.Service.Name)
				refMap[key] = certificates.ServiceReference{
					Name:      webhook.ClientConfig.Service.Name,
					Namespace: webhook.ClientConfig.Service.Namespace,
				}
			} else if webhook.ClientConfig.URL != nil && *webhook.ClientConfig.URL != "" {
				// Parse URL to extract hostname
				if hostname := extractHostnameFromURL(*webhook.ClientConfig.URL); hostname != "" {
					key := fmt.Sprintf("hostname:%s", hostname)
					refMap[key] = certificates.ServiceReference{
						Hostname: hostname,
					}
				}
			}
		}
	}

	// Extract from validating webhooks
	for _, vwc := range configs.Validating {
		for _, webhook := range vwc.Webhooks {
			if webhook.ClientConfig.Service != nil {
				key := fmt.Sprintf("service:%s/%s", webhook.ClientConfig.Service.Namespace, webhook.ClientConfig.Service.Name)
				refMap[key] = certificates.ServiceReference{
					Name:      webhook.ClientConfig.Service.Name,
					Namespace: webhook.ClientConfig.Service.Namespace,
				}
			} else if webhook.ClientConfig.URL != nil && *webhook.ClientConfig.URL != "" {
				// Parse URL to extract hostname
				if hostname := extractHostnameFromURL(*webhook.ClientConfig.URL); hostname != "" {
					key := fmt.Sprintf("hostname:%s", hostname)
					refMap[key] = certificates.ServiceReference{
						Hostname: hostname,
					}
				}
			}
		}
	}

	// Convert map to slice
	refs := make([]certificates.ServiceReference, 0, len(refMap))
	for _, ref := range refMap {
		refs = append(refs, ref)
	}

	return refs
}

// extractHostnameFromURL extracts the hostname from a webhook URL
// Example: "https://webhook.example.com:9443/mutate" -> "webhook.example.com"
func extractHostnameFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
