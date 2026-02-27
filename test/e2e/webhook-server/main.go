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
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
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
	scheme             = runtime.NewScheme()
	setupLog           = ctrl.Log.WithName("setup")
	totalRequests      atomic.Int64
	certificateReloads atomic.Int32
	lastRequestTime    atomic.Value
	lastReloadTime     atomic.Value
	replicaName        string
	certFile           string
)

func init() {
	// We don't actually need to register schemes since we're just validating ConfigMaps
	// but controller-runtime requires a scheme
	_ = corev1.AddToScheme(scheme)
	_ = admissionv1.AddToScheme(scheme)
}

type Metrics struct {
	ReplicaName        string    `json:"replica_name"`
	TotalRequests      int64     `json:"total_requests"`
	CertificateReloads int       `json:"certificate_reloads"`
	CertificateSerial  string    `json:"certificate_serial"`
	LastRequestTime    time.Time `json:"last_request_time"`
	LastCertReloadTime time.Time `json:"last_cert_reload_time"`
}

// ConfigMapValidator validates ConfigMap admissions (always allows)
type ConfigMapValidator struct {
	decoder *admission.Decoder
}

func (v *ConfigMapValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	totalRequests.Add(1)
	lastRequestTime.Store(time.Now())

	setupLog.Info("Webhook request received",
		"replica", replicaName,
		"operation", req.Operation,
		"namespace", req.Namespace,
		"name", req.Name,
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
	var metricsAddr string
	var probeAddr string
	var certDir string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the probe endpoint binds to.")
	flag.StringVar(&certDir, "cert-dir", "/tmp/webhook-certs", "Directory containing TLS certificates.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	replicaName = os.Getenv("HOSTNAME")
	if replicaName == "" {
		replicaName = "unknown"
	}

	certFile = fmt.Sprintf("%s/tls.crt", certDir)
	keyFile := fmt.Sprintf("%s/tls.key", certDir)

	// Initialize atomic values
	lastRequestTime.Store(time.Time{})
	lastReloadTime.Store(time.Time{})

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

	// Track certificate reloads
	go trackCertificateReloads(certFile)

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
			BindAddress: "0", // Disable default metrics server, we use custom endpoint on webhook server
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

	// Register custom metrics endpoint on webhook server (port 9443)
	mgr.GetWebhookServer().Register("/metrics", http.HandlerFunc(handleMetrics))

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
		"replica", replicaName,
		"port", 9443,
		"certDir", certDir,
	)

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	lastReq := lastRequestTime.Load().(time.Time)
	lastReload := lastReloadTime.Load().(time.Time)

	metrics := Metrics{
		ReplicaName:        replicaName,
		TotalRequests:      totalRequests.Load(),
		CertificateReloads: int(certificateReloads.Load()),
		CertificateSerial:  getCertificateSerial(certFile),
		LastRequestTime:    lastReq,
		LastCertReloadTime: lastReload,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

func getCertificateSerial(certPath string) string {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return "error-reading-cert"
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return "error-decoding-pem"
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "error-parsing-cert"
	}

	return cert.SerialNumber.String()
}

func trackCertificateReloads(certPath string) {
	var lastSerial string

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		currentSerial := getCertificateSerial(certPath)
		if currentSerial != lastSerial && lastSerial != "" {
			certificateReloads.Add(1)
			lastReloadTime.Store(time.Now())
			setupLog.Info("Certificate reloaded",
				"replica", replicaName,
				"oldSerial", lastSerial,
				"newSerial", currentSerial,
			)
		}
		lastSerial = currentSerial
	}
}
