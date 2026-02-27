// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

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
