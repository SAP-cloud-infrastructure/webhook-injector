package main

import (
	"testing"

	"github.com/sap-cloud-infrastructure/webhook-injector/pkg/controllers"
)

// TestCertificateReloaderDoesNotNeedLeaderElection verifies that CertificateReloader
// implements LeaderElectionRunnable and returns false (runs on all replicas)
func TestCertificateReloaderDoesNotNeedLeaderElection(t *testing.T) {
	reloader := &controllers.CertificateReloader{}

	// CertificateReloader should implement LeaderElectionRunnable
	if !reloader.NeedLeaderElection() {
		// Good - does not need leader election, runs on all replicas
		return
	}

	t.Error("CertificateReloader.NeedLeaderElection() should return false (should run on all replicas)")
}

// TestWebhookReconcilerNeedsLeaderElection verifies that WebhookReconciler
// either doesn't implement LeaderElectionRunnable (defaults to true) or returns true
func TestWebhookReconcilerNeedsLeaderElection(t *testing.T) {
	reconciler := &controllers.WebhookReconciler{}

	// WebhookReconciler should either not implement the interface (defaults to needing LE)
	// or implement it and return true

	// Try to call NeedLeaderElection - if it exists and returns false, that's wrong
	type leaderElectionRunnable interface {
		NeedLeaderElection() bool
	}

	if ler, ok := interface{}(reconciler).(leaderElectionRunnable); ok {
		if !ler.NeedLeaderElection() {
			t.Error("WebhookReconciler.NeedLeaderElection() should return true (should run only on leader)")
		}
	}
	// If interface not implemented, that's fine - defaults to needing leader election
}
