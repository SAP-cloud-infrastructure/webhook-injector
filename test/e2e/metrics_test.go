// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scrapeMetrics fetches and parses metrics from the given URL.
func scrapeMetrics(url string) (map[string]*dto.MetricFamily, error) {
	resp, err := http.Get(url) //nolint:noctx,gosec // test helper, URL is constructed from a known local port
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(
		// expfmt.TextParser reads from an io.Reader — wrap the bytes
		nopCloser(body),
	)
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	return families, nil
}

type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func nopCloser(data []byte) io.Reader { return &byteReader{data: data} }

// counterValue returns the value of a counter metric with matching labels.
// Returns 0 if the metric or label set is not found.
func counterValue(families map[string]*dto.MetricFamily, name string, labels map[string]string) float64 {
	fam, ok := families[name]
	if !ok {
		return 0
	}
	for _, m := range fam.GetMetric() {
		if labelsMatch(m.GetLabel(), labels) {
			if c := m.GetCounter(); c != nil {
				return c.GetValue()
			}
		}
	}
	return 0
}

// gaugeValue returns the value of a gauge metric. Returns 0 if not found.
func gaugeValue(families map[string]*dto.MetricFamily, name string) float64 {
	fam, ok := families[name]
	if !ok {
		return 0
	}
	for _, m := range fam.GetMetric() {
		if g := m.GetGauge(); g != nil {
			return g.GetValue()
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	matched := 0
	for _, lp := range got {
		if v, ok := want[lp.GetName()]; ok && v == lp.GetValue() {
			matched++
		}
	}
	return matched == len(want)
}

// startPortForward starts kubectl port-forward in the background and returns
// a cancel func that stops it. It blocks until the port is reachable or timeout.
func startPortForward(t *testing.T, resource, localPort, remotePort string) (metricsURL string, stop func()) {
	t.Helper()
	cmd := exec.Command("kubectl", "--context=kind-hosting",
		"port-forward", resource, fmt.Sprintf("%s:%s", localPort, remotePort))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start port-forward: %v", err)
	}

	url := fmt.Sprintf("http://localhost:%s/metrics", localPort)

	// Wait until reachable (up to 30s)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:noctx,gosec // poll loop, URL is a known localhost port-forward address
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	return url, func() {
		_ = cmd.Process.Kill() //nolint:errcheck // best-effort cleanup of port-forward process
	}
}

// TestMetrics verifies that the webhook-injector Prometheus metrics are
// correctly populated after deployment and after a forced certificate rotation.
func TestMetrics(t *testing.T) {
	kubeconfigPath := "kubeconfig"
	t.Setenv("KUBECONFIG", kubeconfigPath)

	if !*reuseClusters {
		logWithTimestamp(t, "Setting up clusters...")
		if err := runCommand(t, "./setup.sh"); err != nil {
			t.Fatalf("Setup failed: %v", err)
		}
		if err := runCommand(t, "skaffold run"); err != nil {
			t.Fatalf("Skaffold deployment failed: %v", err)
		}
		t.Cleanup(func() {
			runCommand(t, "./teardown.sh") //nolint:errcheck
		})
	} else {
		logWithTimestamp(t, "Reusing existing clusters (skipping setup/teardown)")
	}

	hostingClient := getKubernetesClient(t, "kind-hosting")

	logWithTimestamp(t, "Waiting for webhook deployment ready...")
	waitForDeploymentReady(t, hostingClient, "webhook-server", 3, 5*time.Minute)

	ctx := context.Background()

	// Pick the first ready pod to port-forward against.
	pods, err := hostingClient.CoreV1().Pods("default").List(ctx, metav1.ListOptions{
		LabelSelector: "app=webhook-server",
	})
	if err != nil || len(pods.Items) == 0 {
		t.Fatalf("Failed to list webhook-server pods: %v", err)
	}
	podName := pods.Items[0].Name

	metricsURL, stopPF := startPortForward(t, "pod/"+podName, "19082", "8082")
	defer stopPF()

	// --- 1. Baseline metrics after initial deployment ---

	logWithTimestamp(t, "Scraping baseline metrics...")
	baseline, err := scrapeMetrics(metricsURL)
	if err != nil {
		t.Fatalf("Failed to scrape baseline metrics: %v", err)
	}

	// Initial rotation must have happened (reason=initial)
	initialRotations := counterValue(baseline, "webhook_injector_certificate_rotations_total", map[string]string{"reason": "initial"})
	if initialRotations < 1 {
		t.Errorf("Expected at least 1 initial rotation, got %.0f", initialRotations)
	}

	// Expiry gauge must be set to a future Unix timestamp
	expiryTime := gaugeValue(baseline, "webhook_injector_certificate_expiry_time_seconds")
	switch {
	case expiryTime == 0:
		t.Error("Expected webhook_injector_certificate_expiry_time_seconds to be set")
	case expiryTime < float64(time.Now().Unix()):
		t.Errorf("Certificate expiry time %.0f is in the past", expiryTime)
	default:
		logWithTimestamp(t, "Certificate expires at %s",
			time.Unix(int64(expiryTime), 0).UTC().Format(time.RFC3339))
	}

	// Cert files must have been written (2 files: tls.crt + tls.key)
	filesWritten := counterValue(baseline, "webhook_injector_cert_files_written_total", nil)
	if filesWritten < 2 {
		t.Errorf("Expected at least 2 cert file writes (tls.crt + tls.key), got %.0f", filesWritten)
	}

	logWithTimestamp(t, "Baseline: initial_rotations=%.0f expiry=%s files_written=%.0f",
		initialRotations,
		time.Unix(int64(expiryTime), 0).UTC().Format(time.RFC3339),
		filesWritten)

	// --- 2. Force a rotation and verify counter increments ---

	logWithTimestamp(t, "Triggering forced certificate rotation...")
	if err := triggerRotation(ctx, t, hostingClient); err != nil {
		t.Fatalf("Failed to trigger rotation: %v", err)
	}

	// Wait for rotation to be processed (reconciler must pick up the annotation)
	var afterRotation map[string]*dto.MetricFamily
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		afterRotation, err = scrapeMetrics(metricsURL)
		if err != nil {
			t.Fatalf("Failed to scrape metrics after rotation: %v", err)
		}
		forceRotations := counterValue(afterRotation, "webhook_injector_certificate_rotations_total", map[string]string{"reason": "force_rotation"})
		if forceRotations >= 1 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	forceRotations := counterValue(afterRotation, "webhook_injector_certificate_rotations_total", map[string]string{"reason": "force_rotation"})
	if forceRotations < 1 {
		t.Errorf("Expected at least 1 force_rotation after annotation trigger, got %.0f", forceRotations)
	}

	// Expiry should have advanced (new cert issued)
	newExpiryTime := gaugeValue(afterRotation, "webhook_injector_certificate_expiry_time_seconds")
	if newExpiryTime <= expiryTime {
		t.Errorf("Expected expiry time to advance after rotation (before=%.0f after=%.0f)", expiryTime, newExpiryTime)
	}

	// cert_files_written_total must have increased (new cert written to all replicas' disks)
	newFilesWritten := counterValue(afterRotation, "webhook_injector_cert_files_written_total", nil)
	if newFilesWritten <= filesWritten {
		t.Errorf("Expected cert_files_written_total to increase after rotation (before=%.0f after=%.0f)", filesWritten, newFilesWritten)
	}

	logWithTimestamp(t, "After rotation: force_rotations=%.0f new_expiry=%s files_written=%.0f",
		forceRotations,
		time.Unix(int64(newExpiryTime), 0).UTC().Format(time.RFC3339),
		newFilesWritten)
}
