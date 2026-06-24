// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Package rotationgate owns the bridge between ManagedResourceReconciler
// (which knows which CRDs it has stamped) and WebhookReconciler (which
// needs to gate certificate-rotation promotion on those CRDs' caBundles
// having propagated to the target cluster).
//
// Why a dedicated type rather than a bare channel or a slice on a shared
// struct: the read and write sides are in different packages run by
// different reconcilers at different cadences, and a typed handle makes
// the contract explicit:
//
//	g := rotationgate.New()
//	wr := WebhookReconciler{..., Gate: g}
//	mr := ManagedResourceReconciler{..., Gate: g}
//
// The Gate is keyed by ManagedResource (a `key` derived from namespace +
// name), so each MR independently publishes the set of CRDs it observed
// on its most recent reconcile, and the global view (Snapshot) is the
// union across every currently-known MR. When an MR is deleted, the
// reconciler calls Forget(key) to drop its contribution. Snapshot
// canonicalises the union: sorted, deduplicated, nil-when-empty.
//
// State is in-memory only by design. A controller restart returns an
// empty Gate; the MR reconciler re-populates each MR's entry within
// one reconcile cycle (seconds). Rotations that overlap with that
// window simply fall back to the time-based gate timeout — the
// alternative (persisting the gate set on an apiserver object) would
// introduce a watch race and a stale-entry GC hazard.
package rotationgate

import (
	"sort"
	"sync"
)

// Key uniquely identifies a ManagedResource for gate publishing. Tests
// can use any string; production callers pass `<namespace>/<name>`.
type Key string

// Gate is the shared snapshot of CRD names whose caBundles must show
// the staged-rotation union bundle on the target cluster before
// WebhookReconciler promotes the new serving cert.
//
// Safe for concurrent use by both reconcilers. The zero value is a
// usable empty Gate; New() is provided for symmetry with the rest of
// the codebase.
type Gate struct {
	mu    sync.RWMutex
	perMR map[Key][]string // canonicalised per-MR slices; Snapshot unions them.
}

// New returns a fresh empty Gate. Reads (Snapshot) return nil; the MR
// reconciler will call Publish once it stamps any CRD.
func New() *Gate {
	return &Gate{}
}

// Publish atomically replaces the set of CRDs published by the given
// MR. Caller does not need to dedupe or sort — Publish canonicalises.
// An empty or nil slice is the right value when an MR was reconciled
// but observed no CRDs (e.g. it ships only ConfigMaps); the entry
// becomes a no-op contribution to the union but stays in the map so
// later reconciles can re-populate it without a Forget round-trip.
//
// Publish copies its input so callers don't need to worry about
// aliasing.
func (g *Gate) Publish(mr Key, crds []string) {
	canon := canonicalize(crds)
	g.mu.Lock()
	if g.perMR == nil {
		g.perMR = make(map[Key][]string)
	}
	g.perMR[mr] = canon
	g.mu.Unlock()
}

// Forget drops the given MR's contribution. Call on MR deletion. A
// missing key is a no-op (an MR that was deleted before it was ever
// reconciled).
func (g *Gate) Forget(mr Key) {
	g.mu.Lock()
	delete(g.perMR, mr)
	g.mu.Unlock()
}

// Snapshot returns the union of all currently-published CRD names as a
// freshly-allocated, sorted, deduplicated slice the caller is free to
// retain. Returns nil for an empty gate (no MRs published anything, or
// the perMR map is nil) so callers can treat "no gate" and "empty gate"
// identically.
func (g *Gate) Snapshot() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.perMR) == 0 {
		return nil
	}
	// Union without an intermediate set whenever possible — typical case
	// is a single MR with a handful of CRDs.
	seen := make(map[string]struct{})
	for _, list := range g.perMR {
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
