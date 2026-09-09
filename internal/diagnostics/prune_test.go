package diagnostics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneBundles_OldBundle_IsRemoved(t *testing.T) {
	dir := t.TempDir()

	oldDir := filepath.Join(dir, "old-run")
	if err := os.Mkdir(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, past, past); err != nil {
		t.Fatal(err)
	}

	if err := PruneBundles(dir, 24*time.Hour); err != nil {
		t.Fatalf("PruneBundles: %v", err)
	}

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Error("old bundle directory still exists after prune")
	}
}

func TestPruneBundles_RecentBundle_IsKept(t *testing.T) {
	dir := t.TempDir()

	recentDir := filepath.Join(dir, "recent-run")
	if err := os.Mkdir(recentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := PruneBundles(dir, 24*time.Hour); err != nil {
		t.Fatalf("PruneBundles: %v", err)
	}

	if _, err := os.Stat(recentDir); err != nil {
		t.Errorf("recent bundle directory was removed: %v", err)
	}
}
