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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

var (
	totalRequests      atomic.Int64
	certificateReloads atomic.Int32
	lastRequestTime    atomic.Value
	lastReloadTime     atomic.Value
	replicaName        string
)

type Metrics struct {
	ReplicaName        string    `json:"replica_name"`
	TotalRequests      int64     `json:"total_requests"`
	CertificateReloads int       `json:"certificate_reloads"`
	CertificateSerial  string    `json:"certificate_serial"`
	LastRequestTime    time.Time `json:"last_request_time"`
	LastCertReloadTime time.Time `json:"last_cert_reload_time"`
}

func main() {
	replicaName = os.Getenv("HOSTNAME")
	if replicaName == "" {
		replicaName = "unknown"
	}

	certDir := "/tmp/webhook-certs"
	certFile := fmt.Sprintf("%s/tls.crt", certDir)
	keyFile := fmt.Sprintf("%s/tls.key", certDir)

	// Initialize atomic values
	lastRequestTime.Store(time.Time{})
	lastReloadTime.Store(time.Time{})

	// Create certificate watcher for hot reload
	watcher, err := certwatcher.New(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to create certificate watcher: %v", err)
	}

	// Start certificate watcher
	go func() {
		if err := watcher.Start(context.Background()); err != nil {
			log.Fatalf("Certificate watcher failed: %v", err)
		}
	}()

	// Track certificate reloads
	go trackCertificateReloads(certFile)

	// HTTP handlers
	http.HandleFunc("/validate", handleValidate)
	http.HandleFunc("/metrics", handleMetrics(certFile))
	http.HandleFunc("/healthz", handleHealth)
	http.HandleFunc("/readyz", handleReady)

	// HTTPS server with certificate watcher
	server := &http.Server{
		Addr: ":9443",
		TLSConfig: &tls.Config{
			GetCertificate: watcher.GetCertificate,
		},
	}

	log.Printf("Starting webhook server on :9443 (replica: %s)", replicaName)
	if err := server.ListenAndServeTLS("", ""); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func handleValidate(w http.ResponseWriter, r *http.Request) {
	totalRequests.Add(1)
	lastRequestTime.Store(time.Now())

	var admissionReview admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&admissionReview); err != nil {
		http.Error(w, fmt.Sprintf("failed to decode admission review: %v", err), http.StatusBadRequest)
		return
	}

	log.Printf("Webhook request from %s: %s %s/%s",
		replicaName,
		admissionReview.Request.Operation,
		admissionReview.Request.Namespace,
		admissionReview.Request.Name,
	)

	// Always allow (this is a test webhook)
	response := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: &admissionv1.AdmissionResponse{
			UID:     admissionReview.Request.UID,
			Allowed: true,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleMetrics(certFile string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	// Ready if certificate exists
	if _, err := os.Stat("/tmp/webhook-certs/tls.crt"); err != nil {
		http.Error(w, "certificates not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ready"))
}

func getCertificateSerial(certFile string) string {
	data, err := os.ReadFile(certFile)
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

func trackCertificateReloads(certFile string) {
	var lastSerial string

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		currentSerial := getCertificateSerial(certFile)
		if currentSerial != lastSerial && lastSerial != "" {
			certificateReloads.Add(1)
			lastReloadTime.Store(time.Now())
			log.Printf("Certificate reloaded on %s: %s -> %s", replicaName, lastSerial, currentSerial)
		}
		lastSerial = currentSerial
	}
}
