// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

package managedresource

import (
	"bytes"
	"testing"
)

func TestIsCompressedKey(t *testing.T) {
	cases := map[string]bool{
		"objects.yaml":    false,
		"objects.yaml.br": true,
		".br":             false, // suffix-only is not a real key
		"":                false,
		"foo.yml.br":      true,
	}
	for in, want := range cases {
		if got := IsCompressedKey(in); got != want {
			t.Errorf("IsCompressedKey(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSplitDocsAndJoin(t *testing.T) {
	in := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: a
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
`)
	docs, err := SplitDocs(in)
	if err != nil {
		t.Fatalf("SplitDocs: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}
	if !bytes.Contains(docs[0], []byte("name: a")) || !bytes.Contains(docs[1], []byte("name: b")) {
		t.Fatalf("docs split incorrectly: %q / %q", docs[0], docs[1])
	}

	// Round-trip: SplitDocs → JoinDocs preserves both docs and survives a
	// second split unchanged in document count.
	joined := JoinDocs(docs)
	roundTrip, err := SplitDocs(joined)
	if err != nil {
		t.Fatalf("SplitDocs(joined): %v", err)
	}
	if len(roundTrip) != 2 {
		t.Fatalf("round-trip lost docs: %d != 2", len(roundTrip))
	}
}

func TestSplitDocsDropsTrailingEmpty(t *testing.T) {
	// A trailing "---\n" with nothing after it is a common artefact of
	// templating tools. Make sure it does not become a third "null" doc.
	in := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: a
---
`)
	docs, err := SplitDocs(in)
	if err != nil {
		t.Fatalf("SplitDocs: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}
}

func TestSplitDocsEmpty(t *testing.T) {
	docs, err := SplitDocs(nil)
	if err != nil {
		t.Fatalf("SplitDocs(nil): %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("expected 0 docs, got %d", len(docs))
	}
}

func TestEncodeDecodePlain(t *testing.T) {
	in := []byte("apiVersion: v1\nkind: ConfigMap\n")
	enc, err := Encode(in, false)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(enc, in) {
		t.Fatalf("plain Encode should be byte-identical, got %q", enc)
	}
	dec, compressed, err := Decode(false, enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if compressed {
		t.Fatalf("plain Decode reported compressed=true")
	}
	if !bytes.Equal(dec, in) {
		t.Fatalf("plain round-trip mismatch")
	}
}

func TestEncodeDecodeBrotli(t *testing.T) {
	in := []byte("apiVersion: v1\nkind: ConfigMap\n" + // small input still round-trips
		"---\napiVersion: v1\nkind: Secret\n")
	enc, err := Encode(in, true)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if bytes.Equal(enc, in) {
		// Not strictly required by the API, but a sanity check: brotli on
		// non-empty input should change the bytes.
		t.Fatalf("brotli Encode produced identical bytes; compression did not run")
	}
	dec, compressed, err := Decode(true, enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !compressed {
		t.Fatalf("brotli Decode reported compressed=false")
	}
	if !bytes.Equal(dec, in) {
		t.Fatalf("brotli round-trip mismatch:\nwant %q\n got %q", in, dec)
	}
}
