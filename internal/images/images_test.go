package images

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestDigestAndPrivateCloneIsolation(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	original := []byte("immutable prepared guest image")
	os.WriteFile(source, original, 0400)
	sum := sha256.Sum256(original)
	digest := hex.EncodeToString(sum[:])
	if Verify(source, digest) != nil || Verify(source, "wrong") == nil {
		t.Fatal("digest verification")
	}
	first, second := filepath.Join(dir, "first.raw"), filepath.Join(dir, "second.raw")
	if Clone(source, first, 100) != nil || Clone(source, second, 100) != nil {
		t.Fatal("clone")
	}
	if Clone(source, first, 100) == nil {
		t.Fatal("overwrote executed disk")
	}
	if Clone(source, filepath.Join(dir, "too-small"), 1) == nil {
		t.Fatal("disk budget ignored")
	}
	os.WriteFile(first, []byte("executed writes"), 0600)
	for _, p := range []string{source, second} {
		b, _ := os.ReadFile(p)
		if string(b) != string(original) {
			t.Fatal("writable source or cross-VM sharing")
		}
	}
}
