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
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
)

var (
	testRuntime      = flag.Duration("test.runtime", 1*time.Minute, "How long to run the load test")
	testConcurrency  = flag.Int("test.concurrency", 5, "Number of concurrent clients")
	rotationInterval = flag.Duration("test.rotation-interval", 10*time.Second, "How often to trigger cert rotation")
	reuseClusters    = flag.Bool("test.reuse-clusters", false, "Skip cluster setup/teardown")
)

type FailureRecord struct {
	Timestamp time.Time
	ClientID  int    // -1 for rotation errors
	Operation string // "get", "update", "rotation"
	Error     error
}

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}

// logWithTimestamp logs a message with timestamp prefix
func logWithTimestamp(t *testing.T, format string, args ...any) {
	t.Helper()
	timestamp := time.Now().Format("15:04:05.000")
	t.Logf("[%s] "+format, append([]any{timestamp}, args...)...)
}

func runCommand(t *testing.T, command string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = "."
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Inherit PATH from parent process
	cmd.Env = os.Environ()
	return cmd.Run()
}

func getKubernetesClient(t *testing.T, contextName string) *kubernetes.Clientset {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("KUBECONFIG environment variable not set")
	}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		t.Fatalf("Failed to load kubeconfig for context %s: %v", contextName, err)
	}

	// Disable rate limiter for load testing
	config.QPS = -1   // disable
	config.Burst = -1 // disable

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to create clientset: %v", err)
	}

	return clientset
}

func waitForDeploymentReady(t *testing.T, client *kubernetes.Clientset, name string, replicas int, timeout time.Duration) {
	ctx := context.Background()
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("Timeout waiting for deployment %s to have %d ready replicas", name, replicas)
		case <-ticker.C:
			deployment, err := client.AppsV1().Deployments("default").Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				logWithTimestamp(t, "Waiting for deployment %s... (%v)", name, err)
				continue
			}

			if deployment.Status.ReadyReplicas == int32(replicas) {
				logWithTimestamp(t, "Deployment %s has %d/%d ready replicas", name, replicas, replicas)
				return
			}

			logWithTimestamp(t, "Waiting for deployment %s: %d/%d ready", name, deployment.Status.ReadyReplicas, replicas)
		}
	}
}

func runLoadClient(ctx context.Context, clientID int, client *kubernetes.Clientset,
	failures chan<- FailureRecord, totalRequests *atomic.Int64) {

	cmName := fmt.Sprintf("load-test-cm-%d", clientID)
	reqCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		default:
			reqCount++
			totalRequests.Add(1)
			ctx2, cancel := context.WithTimeout(context.Background(), 1*time.Second) // Timeout for individual request
			cm, err := client.CoreV1().ConfigMaps("default").Get(ctx2, cmName, metav1.GetOptions{})
			cancel()
			if err != nil {
				failures <- FailureRecord{
					Timestamp: time.Now(),
					ClientID:  clientID,
					Operation: "get",
					Error:     err,
				}
				continue
			}

			cm.Data["counter"] = strconv.Itoa(reqCount)
			ctx2, cancel = context.WithTimeout(context.Background(), 1*time.Second) // Timeout for individual request
			_, err = client.CoreV1().ConfigMaps("default").Update(ctx2, cm, metav1.UpdateOptions{})
			cancel()
			if err != nil {
				failures <- FailureRecord{
					Timestamp: time.Now(),
					ClientID:  clientID,
					Operation: "update",
					Error:     err,
				}
			}
		}
	}
}

func runRotationTrigger(ctx context.Context, t *testing.T, client *kubernetes.Clientset,
	interval time.Duration, failures chan<- FailureRecord) {

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := triggerRotation(ctx, t, client); err != nil {
				select {
				case <-ctx.Done():
				default:
					failures <- FailureRecord{
						Timestamp: time.Now(),
						ClientID:  -1,
						Operation: "rotation",
						Error:     err,
					}
				}
			}
		}
	}
}

func triggerRotation(ctx context.Context, t *testing.T, client *kubernetes.Clientset) error {
	logWithTimestamp(t, "Triggering certificate rotation")
	secret, err := client.CoreV1().Secrets("default").Get(ctx, "webhook-injector-certs", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get secret: %w", err)
	}

	if secret.Annotations == nil {
		secret.Annotations = make(map[string]string)
	}
	secret.Annotations[controllers.ForceRotationAnnotation] = "true"

	_, err = client.CoreV1().Secrets("default").Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to annotate secret: %w", err)
	}

	return nil
}

func TestWebhookLoadAndRotation(t *testing.T) {
	// Set KUBECONFIG to use dedicated test kubeconfig
	kubeconfigPath := filepath.Join(".", "kubeconfig")
	t.Setenv("KUBECONFIG", kubeconfigPath)

	// Conditional setup
	if !*reuseClusters {
		logWithTimestamp(t, "Setting up clusters...")
		if err := runCommand(t, "./setup.sh"); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}

		// Deploy with skaffold
		logWithTimestamp(t, "Deploying with skaffold...")
		if err := runCommand(t, "skaffold run"); err != nil {
			t.Fatalf("Skaffold deployment failed: %v", err)
		}

		t.Cleanup(func() {
			logWithTimestamp(t, "Tearing down clusters...")
			runCommand(t, "./teardown.sh") // Ignore errors during cleanup
		})
	} else {
		logWithTimestamp(t, "Reusing existing clusters (skipping setup/teardown)")
	}

	// Get Kubernetes clients
	hostingClient := getKubernetesClient(t, "kind-hosting")
	targetClient := getKubernetesClient(t, "kind-target")

	// Wait for webhook server to be ready
	logWithTimestamp(t, "Waiting for webhook deployment ready...")
	waitForDeploymentReady(t, hostingClient, "webhook-server", 3, 5*time.Minute)

	// Create load test ConfigMaps (delete existing ones first)
	ctx := context.Background()
	logWithTimestamp(t, "Creating %d load test ConfigMaps...", *testConcurrency)
	for i := range *testConcurrency {
		cmName := fmt.Sprintf("load-test-cm-%d", i)

		// Delete if exists (ignore errors)
		_ = targetClient.CoreV1().ConfigMaps("default").Delete(ctx, cmName, metav1.DeleteOptions{})

		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName,
				Namespace: "default",
			},
			Data: map[string]string{"counter": "0"},
		}
		if _, err := targetClient.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			t.Fatalf("Failed to create load test ConfigMap %d: %v", i, err)
		}
	}

	// Run load test with timeout
	logWithTimestamp(t, "Starting load test (runtime=%s, concurrency=%d, rotation=%s)",
		*testRuntime, *testConcurrency, *rotationInterval)

	loadCtx, cancel := context.WithTimeout(ctx, *testRuntime)
	defer cancel()

	failures := make(chan FailureRecord, 1000)
	var totalRequests atomic.Int64
	var wg sync.WaitGroup

	// Start load clients
	for i := range *testConcurrency {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runLoadClient(loadCtx, id, targetClient, failures, &totalRequests)
		}(i)
	}

	// Start rotation trigger
	wg.Go(func() {
		runRotationTrigger(loadCtx, t, hostingClient, *rotationInterval, failures)
	})

	// Wait for test duration
	<-loadCtx.Done()
	logWithTimestamp(t, "Load test completed")

	// Wait for all goroutines to exit before closing channel
	wg.Wait()
	close(failures)
	var collectedFailures []FailureRecord
	for f := range failures {
		collectedFailures = append(collectedFailures, f)
	}

	// Report results
	total := totalRequests.Load()
	failureCount := len(collectedFailures)
	failureRate := 0.0
	if total > 0 {
		failureRate = float64(failureCount) / float64(total) * 100
	}

	t.Logf("\n=== Load Test Results ===")
	t.Logf("Duration: %s", *testRuntime)
	t.Logf("Total requests: %d", total)
	t.Logf("Failures: %d (%.2f%%)", failureCount, failureRate)

	if failureCount > 0 {
		t.Logf("\nFailed requests:")
		for _, f := range collectedFailures {
			clientStr := fmt.Sprintf("client-%d", f.ClientID)
			if f.ClientID == -1 {
				clientStr = "rotation"
			}
			logWithTimestamp(t, "%s %s: %v",
				clientStr,
				f.Operation,
				f.Error)
		}
		t.Fatalf("Load test had %d failures", failureCount)
	}
}
