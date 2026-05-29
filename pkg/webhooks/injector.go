// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package webhooks

import "net/url"

// BuildURL constructs a webhook clientConfig.URL from a host (or host:port) and a path.
// The path is preserved as-is from the existing webhook entry; if empty, "/" is used.
func BuildURL(host, path string) string {
	if path == "" {
		path = "/"
	}
	return "https://" + host + path
}

// ExtractPathFromURL returns the path component (including any query/fragment) of a
// webhook clientConfig.URL, or "/" if the URL cannot be parsed or has no path.
func ExtractPathFromURL(rawURL string) string {
	if rawURL == "" {
		return "/"
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Path == "" {
		return "/"
	}
	if parsed.RawQuery != "" {
		return parsed.Path + "?" + parsed.RawQuery
	}
	return parsed.Path
}

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
