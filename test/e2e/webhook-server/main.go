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

package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	scheme       = runtime.NewScheme()
	setupLog     = ctrl.Log.WithName("setup")
	probeAddr    string
	certDir      string
	webhookDelay time.Duration
	randGen      *rand.Rand
)

func init() {
	// We don't actually need to register schemes since we're just validating ConfigMaps
	// but controller-runtime requires a scheme
	_ = corev1.AddToScheme(scheme)
	_ = admissionv1.AddToScheme(scheme)
}

// ConfigMapValidator validates ConfigMap admissions (always allows)
type ConfigMapValidator struct {
	decoder *admission.Decoder
}

func (v *ConfigMapValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	// Add jittered delay: ±20% of configured delay
	jitter := 1.0 + (randGen.Float64()*0.4 - 0.2) // Range: 0.8 to 1.2
	delay := time.Duration(float64(webhookDelay) * jitter)
	time.Sleep(delay)

	// Log the incoming request
	log := ctrl.LoggerFrom(ctx)
	log.Info("Processed webhook request",
		"operation", req.Operation,
		"namespace", req.Namespace,
		"duration", delay.String(),
		"name", req.Name,
		"kind", req.Kind.Kind,
		"uid", req.UID,
	)

	// Always allow (this is a test webhook)
	return admission.Allowed("")
}

// InjectDecoder injects the decoder (required by admission.Handler interface)
func (v *ConfigMapValidator) InjectDecoder(d *admission.Decoder) error {
	v.decoder = d
	return nil
}

func main() {
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the probe endpoint binds to.")
	flag.StringVar(&certDir, "cert-dir", "/tmp/webhook-certs", "Directory containing TLS certificates.")
	flag.DurationVar(&webhookDelay, "webhook-delay", 100*time.Millisecond, "Average webhook processing delay with ±20% jitter")

	opts := zap.Options{
		Development: false, // Reduce log verbosity for integration tests
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Initialize random generator for jitter (modern Go 1.20+ approach)
	randGen = rand.New(rand.NewSource(time.Now().UnixNano()))

	certFile := certDir + "/tls.crt"
	keyFile := certDir + "/tls.key"

	// Wait for certificates to be created by webhook-injector
	setupLog.Info("Waiting for certificates to be generated...")
	for {
		if _, err := os.Stat(certFile); err == nil {
			if _, err := os.Stat(keyFile); err == nil {
				setupLog.Info("Certificates found, starting manager...")
				break
			}
		}
		time.Sleep(1 * time.Second)
	}

	// Create a minimal REST config - we don't actually need to connect to a cluster
	// since this is just a test webhook server
	restConfig := &rest.Config{
		Host: "http://localhost:8080", // Dummy config, not actually used
	}

	// Create manager with webhook server
	// The webhook server automatically uses certwatcher for hot reload (controller-runtime v0.17+)
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:     9443,
			CertDir:  certDir,
			CertName: "tls.crt",
			KeyName:  "tls.key",
		}),
		Metrics: metricsserver.Options{
			BindAddress: "0", // Disable default metrics server
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         false, // No need for leader election in test webhook
	})
	if err != nil {
		setupLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	// Register webhook handler
	mgr.GetWebhookServer().Register("/validate", &webhook.Admission{
		Handler: &ConfigMapValidator{},
	})

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		// Ready if certificate exists
		if _, err := os.Stat(certFile); err != nil {
			return fmt.Errorf("certificates not ready: %w", err)
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "Unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting webhook server",
		"port", 9443,
		"certDir", certDir,
	)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
