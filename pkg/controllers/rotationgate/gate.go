// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Package rotationgate owns the bridge between the publishing reconcilers
// (ManagedResourceReconciler on the hosting cluster, TargetWebhookReconciler
// on the target cluster in patch mode) and WebhookReconciler, which needs
// to gate certificate-rotation promotion on those CRDs' caBundles having
// propagated.
//
// Why a dedicated type rather than a bare channel or a slice on a shared
// struct: publishers and readers are in different packages, run by
// different reconcilers at different cadences, and a typed handle makes
// the contract explicit:
//
//	g := rotationgate.New()
//	wr := WebhookReconciler{..., Gate: g}
//	mr := ManagedResourceReconciler{..., Gate: g}
//	tr := TargetWebhookReconciler{..., Gate: g}
//
// The Gate is keyed by a stable per-publisher string. Two publishers live
// here today, using disjoint key schemes so entries cannot collide:
//
//   - ManagedResourceReconciler publishes each MR under `<namespace>/<name>`
//     (via req.String()); those keys always contain a '/'.
//   - TargetWebhookReconciler publishes each labeled target CRD under its
//     bare CRD name (cluster-scoped resource); CRD names never contain '/'.
//
// Snapshot returns the union of all currently-published CRD names across
// every publisher: sorted, deduplicated, nil-when-empty. Publishers call
// Forget(key) to drop their contribution when the source object is
// deleted, unlabeled, or leaves conversion.strategy=Webhook — otherwise a
// future rotation would wait on a stale gate entry until
// --rotation-gate-timeout.
//
// State is in-memory only by design. A controller restart returns an
// empty Gate; publishers re-populate their entries within one reconcile
// cycle (seconds). Rotations that overlap with that window simply fall
// back to the time-based gate timeout — the alternative (persisting the
// gate set on an apiserver object) would introduce a watch race and a
// stale-entry GC hazard.
package rotationgate

import (
	"sort"
	"sync"
)

// Key uniquely identifies a publisher entry in the Gate. The keyspace is
// shared across publishers; each publisher uses a scheme that cannot
// collide with the others (see the package doc for the current scheme
// assignments). Tests may use any string.
type Key string

// Gate is the shared snapshot of CRD names whose caBundles must show
// the staged-rotation union bundle on the target cluster before
// WebhookReconciler promotes the new serving cert.
//
// Safe for concurrent use by every publisher and the reader. The zero
// value is a usable empty Gate; New() is provided for symmetry with the
// rest of the codebase.
type Gate struct {
	mu           sync.RWMutex
	perPublisher map[Key][]string // canonicalised per-publisher slices; Snapshot unions them.
}

// New returns a fresh empty Gate. Reads (Snapshot) return nil until a
// publisher calls Publish.
func New() *Gate {
	return &Gate{}
}

// Publish atomically replaces the set of CRDs published by the caller
// identified by key. Caller does not need to dedupe or sort — Publish
// canonicalises. An empty or nil slice is the right value when the
// publisher reconciled but observed no CRDs (e.g. an MR that ships only
// ConfigMaps); the entry becomes a no-op contribution to the union but
// stays in the map so later reconciles can re-populate it without a
// Forget round-trip.
//
// Publish copies its input so callers don't need to worry about
// aliasing.
func (g *Gate) Publish(publisher Key, crds []string) {
	canon := canonicalize(crds)
	g.mu.Lock()
	if g.perPublisher == nil {
		g.perPublisher = make(map[Key][]string)
	}
	g.perPublisher[publisher] = canon
	g.mu.Unlock()
}

// Forget drops the given publisher's contribution. Call when the source
// object is deleted, unlabeled, or otherwise no longer contributes to
// the gate. A missing key is a no-op (a publisher that never called
// Publish).
func (g *Gate) Forget(publisher Key) {
	g.mu.Lock()
	delete(g.perPublisher, publisher)
	g.mu.Unlock()
}

// Snapshot returns the union of all currently-published CRD names as a
// freshly-allocated, sorted, deduplicated slice the caller is free to
// retain. Returns nil for an empty gate (no publisher published
// anything, or the map is nil) so callers can treat "no gate" and
// "empty gate" identically.
func (g *Gate) Snapshot() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.perPublisher) == 0 {
		return nil
	}
	// Union without an intermediate set whenever possible — typical case
	// is a single publisher with a handful of CRDs.
	seen := make(map[string]struct{})
	for _, list := range g.perPublisher {
		for _, name := range list {
			seen[name] = struct{}{}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// canonicalize sorts and deduplicates the slice. Returns nil for empty
// input so the in-memory representation of an empty entry is uniformly
// nil rather than alternating with []string{}.
func canonicalize(crds []string) []string {
	if len(crds) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(crds))
	out := make([]string, 0, len(crds))
	for _, c := range crds {
		if c == "" {
			continue
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
