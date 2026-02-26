package webhooks

import (
	"bytes"
	"fmt"
	"io"

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

// ExtractServiceReferences extracts unique service references from webhook configurations
func ExtractServiceReferences(configs *WebhookConfigurations) []certificates.ServiceReference {
	serviceMap := make(map[string]certificates.ServiceReference)

	// Extract from mutating webhooks
	for _, mwc := range configs.Mutating {
		for _, webhook := range mwc.Webhooks {
			if webhook.ClientConfig.Service != nil {
				key := fmt.Sprintf("%s/%s", webhook.ClientConfig.Service.Namespace, webhook.ClientConfig.Service.Name)
				serviceMap[key] = certificates.ServiceReference{
					Name:      webhook.ClientConfig.Service.Name,
					Namespace: webhook.ClientConfig.Service.Namespace,
				}
			}
		}
	}

	// Extract from validating webhooks
	for _, vwc := range configs.Validating {
		for _, webhook := range vwc.Webhooks {
			if webhook.ClientConfig.Service != nil {
				key := fmt.Sprintf("%s/%s", webhook.ClientConfig.Service.Namespace, webhook.ClientConfig.Service.Name)
				serviceMap[key] = certificates.ServiceReference{
					Name:      webhook.ClientConfig.Service.Name,
					Namespace: webhook.ClientConfig.Service.Namespace,
				}
			}
		}
	}

	// Convert map to slice
	services := make([]certificates.ServiceReference, 0, len(serviceMap))
	for _, svc := range serviceMap {
		services = append(services, svc)
	}

	return services
}
