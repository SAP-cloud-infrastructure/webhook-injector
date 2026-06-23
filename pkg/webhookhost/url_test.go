// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package webhookhost

import "testing"

func TestBuildExternalURL(t *testing.T) {
	empty := ""
	slashed := "/foo"
	unslashed := "foo"
	deep := "/a/b/c"
	cases := []struct {
		name string
		host string
		path *string
		want string
	}{
		{"nil path", "h:1", nil, "https://h:1/"},
		{"empty path", "h:1", &empty, "https://h:1/"},
		{"slashed path", "h:1", &slashed, "https://h:1/foo"},
		{"unslashed path", "h:1", &unslashed, "https://h:1/foo"},
		{"deep path", "h:1", &deep, "https://h:1/a/b/c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildExternalURL(tc.host, tc.path); got != tc.want {
				t.Errorf("BuildExternalURL(%q, %v) = %q, want %q", tc.host, tc.path, got, tc.want)
			}
		})
	}
}
