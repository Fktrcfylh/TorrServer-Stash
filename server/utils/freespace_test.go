package utils

import (
	"path/filepath"
	"testing"
)

func TestFreeSpace(t *testing.T) {
	free, ok := FreeSpace(t.TempDir())
	if !ok || free == 0 {
		t.Fatalf("FreeSpace(temp dir) = %d, %v, want > 0, true", free, ok)
	}
}

func TestFreeSpaceMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "dir")
	if free, ok := FreeSpace(missing); ok || free != 0 {
		t.Fatalf("FreeSpace(missing) = %d, %v, want 0, false", free, ok)
	}
}
