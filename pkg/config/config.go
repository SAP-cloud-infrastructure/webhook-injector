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
	"os"
	"strconv"
	"time"
)

// Config holds all configuration for webhook-injector
type Config struct {
	// Webhook ConfigMap settings
	WebhookConfigName      string
	WebhookConfigNamespace string

	// Certificate Secret settings
	CertSecretName      string
	CertSecretNamespace string
	CertDir             string

	// Certificate generation settings
	CertValidityDays      int
	CertRotationThreshold float64

	// Target cluster settings
	Kubeconfig string

	// Leader election settings
	EnableLeaderElection    bool
	LeaderElectionID        string
	LeaderElectionNamespace string

	// Controller settings
	SyncPeriod time.Duration

	// Server addresses
	MetricsAddr string
	HealthAddr  string

	// Logging
	LogLevel string
}

// LoadFromEnv loads configuration from environment variables with defaults
func LoadFromEnv() *Config {
	return &Config{
		WebhookConfigName:       getEnv("WEBHOOK_CONFIG_NAME", ""),
		WebhookConfigNamespace:  getEnv("WEBHOOK_CONFIG_NAMESPACE", ""),
		CertSecretName:          getEnv("CERT_SECRET_NAME", "webhook-injector-certs"),
		CertSecretNamespace:     getEnv("CERT_SECRET_NAMESPACE", ""),
		CertDir:                 getEnv("CERT_DIR", "/tmp/webhook-certs"),
		CertValidityDays:        getEnvInt("CERT_VALIDITY_DAYS", 30),
		CertRotationThreshold:   getEnvFloat("CERT_ROTATION_THRESHOLD", 0.8),
		Kubeconfig:              getEnv("KUBECONFIG", ""),
		EnableLeaderElection:    getEnvBool("ENABLE_LEADER_ELECTION", true),
		LeaderElectionID:        getEnv("LEADER_ELECTION_ID", "webhook-injector-leader"),
		LeaderElectionNamespace: getEnv("LEADER_ELECTION_NAMESPACE", ""),
		SyncPeriod:              getEnvDuration("SYNC_PERIOD", 5*time.Minute),
		MetricsAddr:             getEnv("METRICS_ADDR", ":8080"),
		HealthAddr:              getEnv("HEALTH_ADDR", ":8081"),
		LogLevel:                getEnv("LOG_LEVEL", "info"),
	}
}

// Validate checks if required configuration is present
func (c *Config) Validate() error {
	if c.WebhookConfigName == "" {
		return ErrMissingWebhookConfigName
	}
	return nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}

var ErrMissingWebhookConfigName = &ConfigError{"WEBHOOK_CONFIG_NAME is required"}

type ConfigError struct {
	message string
}

func (e *ConfigError) Error() string {
	return e.message
}
