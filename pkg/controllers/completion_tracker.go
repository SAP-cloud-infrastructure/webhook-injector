// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// CompletionTracker records the last time WebhookReconciler completed a clean
// reconcile pass (reached "Reconciliation complete"). It is shared between the
// reconciler (which calls RecordCompletion) and the /readyz endpoint (which
// calls Checker) to detect cert rotation that has been stuck for too long.
type CompletionTracker struct {
	lastNano atomic.Int64 // UnixNano of last clean reconcile; 0 = never completed
}

// NewCompletionTracker returns a ready-to-use tracker with no completions recorded.
func NewCompletionTracker() *CompletionTracker {
	return &CompletionTracker{}
}

// RecordCompletion stamps the current time as the last clean reconcile.
// Safe to call concurrently.
func (t *CompletionTracker) RecordCompletion() {
	t.lastNano.Store(time.Now().UnixNano())
}

// Checker returns a healthz.Checker that fails /readyz when no clean reconcile
// has been recorded for longer than threshold.
//
// When lastNano is zero (no completion recorded yet), the checker returns nil:
// the startup window is already covered by certReadyCheck, which blocks readyz
// until cert files exist on disk — which requires at least one successful cert
// write, which in turn requires at least one clean reconcile. Once that first
// pass fires, lastNano is stamped and the stuck check becomes active.
//
// A zero threshold disables the check entirely (always returns nil).
func (t *CompletionTracker) Checker(threshold time.Duration) healthz.Checker {
	return func(_ *http.Request) error {
		if threshold <= 0 {
			return nil
		}
		nano := t.lastNano.Load()
		if nano == 0 {
			return nil
		}
		if elapsed := time.Since(time.Unix(0, nano)); elapsed > threshold {
			return fmt.Errorf("cert rotation stuck: no clean reconcile for %s (threshold %s)",
				elapsed.Round(time.Second), threshold)
		}
		return nil
	}
}
