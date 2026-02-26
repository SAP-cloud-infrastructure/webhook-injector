package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadFromEnv(t *testing.T) {
	// Set test environment variables
	os.Setenv("WEBHOOK_CONFIG_NAME", "test-config")
	os.Setenv("CERT_VALIDITY_DAYS", "60")
	os.Setenv("ENABLE_LEADER_ELECTION", "false")
	defer func() {
		os.Unsetenv("WEBHOOK_CONFIG_NAME")
		os.Unsetenv("CERT_VALIDITY_DAYS")
		os.Unsetenv("ENABLE_LEADER_ELECTION")
	}()

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
