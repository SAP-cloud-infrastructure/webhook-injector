// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"strings"
	"testing"
	"time"
)

func TestCompletionTracker_ZeroMeansNil(t *testing.T) {
	ct := NewCompletionTracker()
	checker := ct.Checker(15 * time.Minute)
	if err := checker(nil); err != nil {
		t.Errorf("expected nil when no completion recorded (startup grace), got: %v", err)
	}
}

func TestCompletionTracker_RecentCompletionMeansNil(t *testing.T) {
	ct := NewCompletionTracker()
	ct.RecordCompletion()
	checker := ct.Checker(15 * time.Minute)
	if err := checker(nil); err != nil {
		t.Errorf("expected nil when completion was just recorded, got: %v", err)
	}
}

func TestCompletionTracker_StaleCompletionReturnsError(t *testing.T) {
	ct := NewCompletionTracker()
	ct.lastNano.Store(time.Now().Add(-20 * time.Minute).UnixNano())
	checker := ct.Checker(15 * time.Minute)
	err := checker(nil)
	if err == nil {
		t.Fatal("expected error when last completion is older than threshold, got nil")
	}
	if !strings.Contains(err.Error(), "rotation stuck") {
		t.Errorf("expected error to mention \"rotation stuck\", got: %v", err)
	}
}

func TestCompletionTracker_ZeroThresholdDisables(t *testing.T) {
	ct := NewCompletionTracker()
	ct.lastNano.Store(time.Now().Add(-24 * time.Hour).UnixNano())
	checker := ct.Checker(0)
	if err := checker(nil); err != nil {
		t.Errorf("expected nil when threshold=0 (check disabled), got: %v", err)
	}
}

func TestCompletionTracker_RecordIsMonotonic(t *testing.T) {
	ct := NewCompletionTracker()
	ct.RecordCompletion()
	first := ct.lastNano.Load()
	time.Sleep(time.Millisecond)
	ct.RecordCompletion()
	second := ct.lastNano.Load()
	if second <= first {
		t.Errorf("second RecordCompletion should store a later time: first=%d second=%d", first, second)
	}
}
