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
	t.Setenv("CERT_VALIDITY_DAYS", "60")
	t.Setenv("ENABLE_LEADER_ELECTION", "false")

	cfg := LoadFromEnv()

	if cfg.WebhookConfigName != "test-config" {
		t.Errorf("Expected WebhookConfigName 'test-config', got '%s'", cfg.WebhookConfigName)
	}
	if cfg.CertValidityDays != 60 {
		t.Errorf("Expected CertValidityDays 60, got %d", cfg.CertValidityDays)
	}
	if cfg.EnableLeaderElection {
		t.Error("Expected EnableLeaderElection false, got true")
	}
}

func TestLoadFromEnvDefaults(t *testing.T) {
	cfg := LoadFromEnv()

	if cfg.CertSecretName != "webhook-injector-certs" {
		t.Errorf("Expected default CertSecretName, got '%s'", cfg.CertSecretName)
	}
	if cfg.CertDir != "/tmp/webhook-certs" {
		t.Errorf("Expected default CertDir, got '%s'", cfg.CertDir)
	}
	if cfg.CertValidityDays != 30 {
		t.Errorf("Expected default CertValidityDays 30, got %d", cfg.CertValidityDays)
	}
	if cfg.CertRotationThreshold != 0.8 {
		t.Errorf("Expected default CertRotationThreshold 0.8, got %f", cfg.CertRotationThreshold)
	}
	if cfg.SyncPeriod != 5*time.Minute {
		t.Errorf("Expected default SyncPeriod 5m, got %v", cfg.SyncPeriod)
	}
}
