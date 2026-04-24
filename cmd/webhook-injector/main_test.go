// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCertReadyCheck(t *testing.T) {
	t.Run("returns error when cert dir does not exist", func(t *testing.T) {
		checker := certReadyCheck("/nonexistent/path")
		err := checker(nil)
		if err == nil {
			t.Fatal("expected error for nonexistent cert dir, got nil")
		}
	})

	t.Run("returns error when cert files are missing", func(t *testing.T) {
		dir := t.TempDir()
		checker := certReadyCheck(dir)
		err := checker(nil)
		if err == nil {
			t.Fatal("expected error for missing cert files, got nil")
		}
	})

	t.Run("returns error when only tls.crt exists", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("cert"), 0600); err != nil {
			t.Fatal(err)
		}
		checker := certReadyCheck(dir)
		err := checker(nil)
		if err == nil {
			t.Fatal("expected error when tls.key missing, got nil")
		}
	})

	t.Run("returns error when only tls.key exists", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("key"), 0600); err != nil {
			t.Fatal(err)
		}
		checker := certReadyCheck(dir)
		err := checker(nil)
		if err == nil {
			t.Fatal("expected error when tls.crt missing, got nil")
		}
	})

	t.Run("returns nil when both cert files exist", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("cert"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tls.key"), []byte("key"), 0600); err != nil {
			t.Fatal(err)
		}
		checker := certReadyCheck(dir)
		err := checker(&http.Request{})
		if err != nil {
			t.Fatalf("expected nil for existing cert files, got %v", err)
		}
	})
}
