// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package rotationgate

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestNewIsEmpty(t *testing.T) {
	g := New()
	if got := g.Snapshot(); got != nil {
		t.Fatalf("New().Snapshot() = %v, want nil", got)
	}

	var zero Gate
	if got := zero.Snapshot(); got != nil {
		t.Fatalf("(zero Gate).Snapshot() = %v, want nil", got)
	}
}

func TestPublishCanonicalizesInput(t *testing.T) {
	g := New()
	g.Publish("ns/mr", []string{"c", "a", "b", "a", "", "c"})

	want := []string{"a", "b", "c"}
	got := g.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %v, want %v", got, want)
	}
}

func TestPublishOverwritesEntry(t *testing.T) {
	g := New()
	g.Publish("ns/mr", []string{"a", "b"})
	if got := g.Snapshot(); len(got) != 2 {
		t.Fatalf("precondition: Snapshot() = %v, want 2 entries", got)
	}

	// Re-publish with empty: entry remains in map but contributes nothing.
	g.Publish("ns/mr", nil)
	if got := g.Snapshot(); got != nil {
		t.Fatalf("after Publish(nil): Snapshot() = %v, want nil", got)
	}

	// Re-populate the same key with a different set.
	g.Publish("ns/mr", []string{"x"})
	if got := g.Snapshot(); !reflect.DeepEqual(got, []string{"x"}) {
		t.Fatalf("after re-Publish: Snapshot() = %v, want [x]", got)
	}
}

func TestSnapshotUnionsAcrossMRs(t *testing.T) {
	g := New()
	g.Publish("ns/a", []string{"crd1", "crd2"})
	g.Publish("ns/b", []string{"crd2", "crd3"})
	g.Publish("ns/c", []string{"crd4"})

	want := []string{"crd1", "crd2", "crd3", "crd4"}
	got := g.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() union = %v, want %v", got, want)
	}
}

func TestForgetDropsEntry(t *testing.T) {
	g := New()
	g.Publish("ns/a", []string{"crd1", "crd2"})
	g.Publish("ns/b", []string{"crd3"})

	g.Forget("ns/a")
	if got := g.Snapshot(); !reflect.DeepEqual(got, []string{"crd3"}) {
		t.Fatalf("after Forget(ns/a): Snapshot() = %v, want [crd3]", got)
	}

	g.Forget("ns/b")
	if got := g.Snapshot(); got != nil {
		t.Fatalf("after Forget(ns/b): Snapshot() = %v, want nil", got)
	}

	// Forget on a missing key is a no-op (no panic).
	g.Forget("ns/nope")
}

func TestSnapshotIsACopy(t *testing.T) {
	g := New()
	g.Publish("ns/mr", []string{"a", "b", "c"})

	snap := g.Snapshot()
	// Mutate the returned slice in place and grow it.
	snap[0] = "ZZZ"
	snap = append(snap, "extra")
	_ = snap

	want := []string{"a", "b", "c"}
	got := g.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after mutating prior snapshot: Snapshot() = %v, want %v", got, want)
	}
}

func TestPublishDoesNotAliasInput(t *testing.T) {
	g := New()
	input := []string{"a", "b", "c"}
	g.Publish("ns/mr", input)

	// Mutate the original slice after handing it to Publish.
	input[0] = "ZZZ"
	input[1] = "YYY"

	want := []string{"a", "b", "c"}
	got := g.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after mutating input slice: Snapshot() = %v, want %v", got, want)
	}
}

func TestConcurrentReadersWriters(t *testing.T) {
	g := New()

	const writers = 8
	const readers = 8
	const duration = 100 * time.Millisecond

	var wg sync.WaitGroup
	stop := make(chan struct{})

	keys := []Key{"a", "b", "c", "d"}
	payloads := [][]string{
		{"alpha", "beta", "gamma"},
		{"gamma", "alpha", "alpha"},
		nil,
		{},
		{"", "beta"},
		{"delta"},
	}

	for i := range writers {
		wg.Go(func() {
			key := keys[i%len(keys)]
			j := 0
			for {
				select {
				case <-stop:
					return
				default:
					if j%17 == 0 {
						g.Forget(key)
					} else {
						g.Publish(key, payloads[j%len(payloads)])
					}
					j++
				}
			}
		})
	}

	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					snap := g.Snapshot()
					// Iterate to give the race detector something to chew on.
					for _, s := range snap {
						_ = s
					}
				}
			}
		})
	}

	time.Sleep(duration)
	close(stop)
	wg.Wait()
}
