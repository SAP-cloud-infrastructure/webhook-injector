// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company
// SPDX-License-Identifier: Apache-2.0

// Package managedresource contains helpers for decoding and re-encoding the
// resource Secrets referenced by Gardener ManagedResource objects.
//
// Gardener's gardener-resource-manager packs the manifests for a
// ManagedResource into one or more Secrets. Each key in a Secret holds a
// multi-document YAML blob; keys whose name ends in ".br" are additionally
// Brotli-compressed. webhook-injector needs to read these blobs, mutate the
// embedded CustomResourceDefinitions (stamp caBundle, optionally rewrite
// Service-form conversion webhooks to URL-form), and write them back —
// preserving the unstamped documents byte-for-byte so user-authored
// formatting (comments, key ordering, whitespace) is not churned by a
// lossy YAML round-trip.
package managedresource

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// brotliSuffix marks a Secret key whose value is Brotli-compressed. Gardener's
// resource-manager auto-detects the suffix on read and emits with it on write
// once a payload crosses its compression threshold.
const brotliSuffix = ".br"

// IsCompressedKey reports whether a Secret data key carries the brotli suffix
// used by gardener-resource-manager.
func IsCompressedKey(key string) bool {
	return len(key) > len(brotliSuffix) && key[len(key)-len(brotliSuffix):] == brotliSuffix
}

// SplitDocs splits a multi-document YAML payload into one byte slice per
// document, preserving the original bytes of each document so unstamped
// documents survive an unmodified Encode round-trip without YAML reformatting.
//
// A trailing empty document (e.g. a stray "---\n") is dropped — those are
// produced by editors and tools that always terminate with a separator and
// would otherwise become an empty third "document" that re-marshals as "null".
func SplitDocs(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var out [][]byte
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("split multi-doc YAML: %w", err)
		}
		// utilyaml.NewYAMLReader strips the leading "---" but leaves trailing
		// whitespace alone. Trim trailing whitespace-only content so a doc
		// that was nothing but separator+blank lines collapses to empty.
		trimmed := bytes.TrimSpace(doc)
		if len(trimmed) == 0 {
			continue
		}
		// Keep the original (pre-trim) bytes so we round-trip cleanly. Most
		// docs have a trailing newline; we ensure that below in JoinDocs.
		out = append(out, doc)
	}
	return out, nil
}

// JoinDocs concatenates the byte slices produced by SplitDocs (or replaced
// downstream with freshly marshalled docs) back into a multi-document YAML
// payload separated by "---\n". Each document is normalised to end in a
// newline so the separator lands on its own line.
func JoinDocs(docs [][]byte) []byte {
	if len(docs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for i, d := range docs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		buf.Write(d)
		if len(d) == 0 || d[len(d)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// Decode returns the plaintext multi-document YAML payload for a Secret data
// value. If keyHasBrotliSuffix is true the input is Brotli-decompressed
// first; otherwise it is returned as-is (copied so callers can mutate
// safely). compressed is reported back so a matching Encode call can
// preserve the original on-disk representation.
func Decode(keyHasBrotliSuffix bool, data []byte) (plain []byte, compressed bool, err error) {
	if !keyHasBrotliSuffix {
		out := make([]byte, len(data))
		copy(out, data)
		return out, false, nil
	}
	reader := brotli.NewReader(bytes.NewReader(data))
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		return nil, false, fmt.Errorf("brotli decompress: %w", err)
	}
	return buf.Bytes(), true, nil
}

// Encode is the inverse of Decode: it returns the bytes to write back into
// secret.Data[key]. When compressed is true the payload is Brotli-compressed
// at the default level (matching gardener-resource-manager's own choice —
// quality is not stored on the wire, so any standards-compliant brotli
// decoder, including GRM's, can read it).
func Encode(plain []byte, compressed bool) ([]byte, error) {
	if !compressed {
		out := make([]byte, len(plain))
		copy(out, plain)
		return out, nil
	}
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write(plain); err != nil {
		return nil, fmt.Errorf("brotli compress: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("brotli close: %w", err)
	}
	return buf.Bytes(), nil
}
