package diagnostics

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PruneBundles removes bundle directories older than the retention period.
func PruneBundles(bundleDir string, retention time.Duration) error {
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return fmt.Errorf("read bundle directory %s: %w", bundleDir, err)
	}

	cutoff := time.Now().Add(-retention)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat bundle entry %s: %w", entry.Name(), err)
		}
		if info.ModTime().Before(cutoff) {
			dirPath := filepath.Join(bundleDir, entry.Name())
			if err := os.RemoveAll(dirPath); err != nil {
				return fmt.Errorf("remove old bundle %s: %w", dirPath, err)
			}
		}
	}
	return nil
}
