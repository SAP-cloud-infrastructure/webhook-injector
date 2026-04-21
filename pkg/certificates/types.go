// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
//
// SPDX-License-Identifier: Apache-2.0

package certificates

// ServiceReference represents a Kubernetes service from webhook clientConfig
type ServiceReference struct {
	Name      string
	Namespace string
	// Hostname is used when webhook uses URL instead of service (for remote clusters)
	// Example: "webhook.example.com" from "https://webhook.example.com:9443/mutate"
	Hostname string
}

// CertificateBundle contains all certificate data
type CertificateBundle struct {
	CACert     []byte
	CAKey      []byte
	ServerCert []byte
	ServerKey  []byte
}
