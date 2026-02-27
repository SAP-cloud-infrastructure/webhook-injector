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

package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

type Metrics struct {
	ReplicaName        string    `json:"replica_name"`
	TotalRequests      int64     `json:"total_requests"`
	CertificateReloads int       `json:"certificate_reloads"`
	CertificateSerial  string    `json:"certificate_serial"`
	LastRequestTime    time.Time `json:"last_request_time"`
	LastCertReloadTime time.Time `json:"last_cert_reload_time"`
}

func TestE2E_WebhookHighAvailability(t *testing.T) {
	// Phase 1: Setup
	t.Log("Phase 1: Setting up environment...")
	if err := setupClusters(t); err != nil {
		t.Fatalf("Failed to setup clusters: %v", err)
	}

	// Always teardown on exit
	defer func() {
		t.Log("Tearing down environment...")
		teardownClusters(t)
	}()

	if err := deployWithSkaffold(t); err != nil {
		t.Fatalf("Failed to deploy with Skaffold: %v", err)
	}

	hostingClient := getKubernetesClient(t, "kind-hosting")
	targetClient := getKubernetesClient(t, "kind-target")

	if err := waitForWebhookReady(t, hostingClient, 3); err != nil {
		t.Fatalf("Webhook server not ready: %v", err)
	}

	// Phase 2: Baseline validation
	t.Log("Phase 2: Baseline validation...")
	baselineResults := createConfigMaps(t, targetClient, "baseline", 30)
	assertAllSucceeded(t, baselineResults)

	baselineMetrics := collectMetrics(t, hostingClient)
	assertAllReplicasServing(t, baselineMetrics)

	// Phase 3: Rotation under load
	t.Log("Phase 3: Starting load test with rotation...")
	ctx, cancel := context.WithCancel(context.Background())
	failuresChan := make(chan error, 200)

	go continuousLoad(ctx, targetClient, failuresChan)

	time.Sleep(30 * time.Second) // Let load accumulate

	t.Log("Triggering certificate rotation...")
	if err := triggerRotation(t, hostingClient); err != nil {
		t.Fatalf("Failed to trigger rotation: %v", err)
	}

	time.Sleep(60 * time.Second) // Continue load during rotation

	cancel() // Stop load

	// Phase 4: Assert zero failures
	t.Log("Phase 4: Checking for failures...")
	close(failuresChan)
	var failures []error
	for err := range failuresChan {
		failures = append(failures, err)
	}

	if len(failures) > 0 {
		t.Errorf("Load test had %d failures during rotation:", len(failures))
		for i, err := range failures {
			if i < 5 { // Show first 5 failures
				t.Errorf("  - %v", err)
			}
		}
		if len(failures) > 5 {
			t.Errorf("  ... and %d more", len(failures)-5)
		}
	}

	// Phase 5: Post-rotation verification
	t.Log("Phase 5: Post-rotation verification...")
	postMetrics := collectMetrics(t, hostingClient)
	assertCertificateRotated(t, baselineMetrics, postMetrics)
	assertAllReplicasReloaded(t, postMetrics)

	// Final validation
	finalResults := createConfigMaps(t, targetClient, "final", 30)
	assertAllSucceeded(t, finalResults)

	t.Log("E2E test completed successfully!")
}

func setupClusters(t *testing.T) error {
	cmd := exec.Command("./setup.sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func teardownClusters(t *testing.T) {
	cmd := exec.Command("./teardown.sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run() // Ignore errors during cleanup
}

func deployWithSkaffold(t *testing.T) error {
	cmd := exec.Command("skaffold", "run")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func getKubernetesClient(t *testing.T, contextName string) *kubernetes.Clientset {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		t.Fatalf("Failed to load kubeconfig for context %s: %v", contextName, err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to create clientset: %v", err)
	}

	return clientset
}

func waitForWebhookReady(t *testing.T, client *kubernetes.Clientset, expectedReplicas int) error {
	ctx := context.Background()
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for %d replicas to be ready", expectedReplicas)
		case <-ticker.C:
			deployment, err := client.AppsV1().Deployments("default").Get(ctx, "webhook-server", metav1.GetOptions{})
			if err != nil {
				t.Logf("Waiting for deployment... (%v)", err)
				continue
			}

			if deployment.Status.ReadyReplicas == int32(expectedReplicas) {
				t.Logf("All %d replicas are ready", expectedReplicas)
				return nil
			}

			t.Logf("Waiting for replicas: %d/%d ready", deployment.Status.ReadyReplicas, expectedReplicas)
		}
	}
}

func createConfigMaps(t *testing.T, client *kubernetes.Clientset, prefix string, count int) []error {
	var results []error
	ctx := context.Background()

	for i := 0; i < count; i++ {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", prefix, i),
				Namespace: "default",
			},
			Data: map[string]string{
				"key": "value",
			},
		}

		_, err := client.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
		results = append(results, err)
	}

	return results
}

func assertAllSucceeded(t *testing.T, results []error) {
	for i, err := range results {
		if err != nil {
			t.Errorf("ConfigMap creation %d failed: %v", i, err)
		}
	}
}

func collectMetrics(t *testing.T, client *kubernetes.Clientset) map[string]Metrics {
	ctx := context.Background()
	pods, err := client.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=webhook-server",
	})
	if err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
		Timeout: 5 * time.Second,
	}

	metricsMap := make(map[string]Metrics)
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		// Port-forward to pod
		localPort := 19443
		portForwardCmd := exec.Command("kubectl", "--context", "kind-hosting",
			"port-forward", pod.Name, fmt.Sprintf("%d:9443", localPort), "-n", "default")

		if err := portForwardCmd.Start(); err != nil {
			t.Logf("Failed to port-forward to %s: %v", pod.Name, err)
			continue
		}

		time.Sleep(2 * time.Second) // Wait for port-forward to establish

		// Fetch metrics
		resp, err := httpClient.Get(fmt.Sprintf("https://localhost:%d/metrics", localPort))
		if err != nil {
			t.Logf("Failed to fetch metrics from %s: %v", pod.Name, err)
			portForwardCmd.Process.Kill()
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		portForwardCmd.Process.Kill()

		var metrics Metrics
		if err := json.Unmarshal(body, &metrics); err != nil {
			t.Logf("Failed to parse metrics from %s: %v", pod.Name, err)
			continue
		}

		metricsMap[metrics.ReplicaName] = metrics
		t.Logf("Metrics from %s: requests=%d, reloads=%d, serial=%s",
			metrics.ReplicaName, metrics.TotalRequests, metrics.CertificateReloads, metrics.CertificateSerial)
	}

	return metricsMap
}

func assertAllReplicasServing(t *testing.T, metrics map[string]Metrics) {
	if len(metrics) != 3 {
		t.Errorf("Expected metrics from 3 replicas, got %d", len(metrics))
	}

	for name, m := range metrics {
		if m.TotalRequests == 0 {
			t.Errorf("Replica %s served no requests", name)
		}
	}
}

func continuousLoad(ctx context.Context, client *kubernetes.Clientset, failures chan<- error) {
	i := 0
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("load-%d", i),
					Namespace: "default",
				},
				Data: map[string]string{
					"key": "value",
				},
			}

			_, err := client.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{})
			if err != nil {
				failures <- err
			}
			i++
		}
	}
}

func triggerRotation(t *testing.T, client *kubernetes.Clientset) error {
	ctx := context.Background()
	secret, err := client.CoreV1().Secrets("default").Get(ctx, "webhook-injector-certs", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations["webhook-injector.sap.com/force-rotation"] = "true"

	_, err = client.CoreV1().Secrets("default").Update(ctx, secret, metav1.UpdateOptions{})
	return err
}

func assertCertificateRotated(t *testing.T, before, after map[string]Metrics) {
	for name, beforeMetrics := range before {
		afterMetrics, exists := after[name]
		if !exists {
			t.Errorf("Replica %s not found in post-rotation metrics", name)
			continue
		}

		if beforeMetrics.CertificateSerial == afterMetrics.CertificateSerial {
			t.Errorf("Replica %s certificate not rotated (serial unchanged: %s)",
				name, beforeMetrics.CertificateSerial)
		}
	}
}

func assertAllReplicasReloaded(t *testing.T, metrics map[string]Metrics) {
	for name, m := range metrics {
		if m.CertificateReloads == 0 {
			t.Errorf("Replica %s did not reload certificate", name)
		}
	}
}
