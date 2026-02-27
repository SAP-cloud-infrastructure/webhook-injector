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
