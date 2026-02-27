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

	if ler, ok := any(reconciler).(leaderElectionRunnable); ok {
		if !ler.NeedLeaderElection() {
			t.Error("WebhookReconciler.NeedLeaderElection() should return true (should run only on leader)")
		}
	}
	// If interface not implemented, that's fine - defaults to needing leader election
}
