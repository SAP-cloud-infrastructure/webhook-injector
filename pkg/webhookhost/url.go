// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Package webhookhost contains small helpers for composing the externally-
// reachable webhook URL used by ManagedResourceReconciler when rewriting
// Service-form conversion clientConfig to URL-form. Dependency-free.
package webhookhost

// BuildExternalURL composes the URL that replaces a Service-form clientConfig
// when --external-host is set. Path semantics: nil pointer or empty string → "/";
// "/foo" → "/foo"; "foo" → "/foo" (leading slash is added).
func BuildExternalURL(host string, svcPath *string) string {
	path := "/"
	if svcPath != nil && *svcPath != "" {
		path = *svcPath
		if path[0] != '/' {
			path = "/" + path
		}
	}
	return "https://" + host + path
}
