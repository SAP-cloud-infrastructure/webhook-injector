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

package config

import (
	"testing"
	"time"
)

func TestLoadFromEnv(t *testing.T) {
	// Set test environment variables
	t.Setenv("WEBHOOK_CONFIG_NAME", "test-config")
	t.Setenv("WEBHOOK_CONFIG_NAMESPACE", "test-ns")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv failed: %v", err)
	}

	if cfg.WebhookConfigName != "test-config" {
		t.Errorf("Expected WebhookConfigName 'test-config', got '%s'", cfg.WebhookConfigName)
	}
}

func TestLoadFromEnvDefaults(t *testing.T) {
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv failed: %v", err)
	}

	if cfg.CertSecretName != "webhook-injector-certs" {
		t.Errorf("Expected default CertSecretName, got '%s'", cfg.CertSecretName)
	}
	if cfg.CertDir != "/tmp/webhook-certs" {
		t.Errorf("Expected default CertDir, got '%s'", cfg.CertDir)
	}
	if cfg.CertValidity != 720*time.Hour {
		t.Errorf("Expected default CertValidity 720h, got %v", cfg.CertValidity)
	}
	if cfg.CertRotationThreshold != 0.8 {
		t.Errorf("Expected default CertRotationThreshold 0.8, got %f", cfg.CertRotationThreshold)
	}
	if cfg.SyncPeriod != 5*time.Minute {
		t.Errorf("Expected default SyncPeriod 5m, got %v", cfg.SyncPeriod)
	}
}

func TestLoadFromEnv_CertValidity_Duration(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		expected time.Duration
		wantErr  bool
	}{
		{
			name:     "seconds",
			envValue: "60s",
			expected: 60 * time.Second,
			wantErr:  false,
		},
		{
			name:     "minutes",
			envValue: "5m",
			expected: 5 * time.Minute,
			wantErr:  false,
		},
		{
			name:     "hours",
			envValue: "720h",
			expected: 720 * time.Hour,
			wantErr:  false,
		},
		{
			name:     "invalid duration",
			envValue: "invalid",
			expected: 0,
			wantErr:  true,
		},
		{
			name:     "default when empty",
			envValue: "",
			expected: 720 * time.Hour, // 30 days default
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envValue != "" {
				t.Setenv("CERT_VALIDITY", tt.envValue)
			}

			cfg, err := LoadFromEnv()
			if tt.wantErr {
				if err == nil {
					t.Error("Expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if cfg.CertValidity != tt.expected {
				t.Errorf("Expected CertValidity %v, got %v", tt.expected, cfg.CertValidity)
			}
		})
	}
}
