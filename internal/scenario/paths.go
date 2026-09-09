package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ExpandPaths turns the command line arguments of SDD section 4.1 into a list
// of scenario files. It keeps a file path as it is, and it replaces a directory
// path with every *.yaml and *.yml file directly inside that directory, in
// lexical order, so a run is reproducible. ExpandPaths returns an error for a
// path that does not exist and for a directory that holds no scenario file.
func ExpandPaths(paths []string) ([]string, error) {
	var expanded []string
	seen := make(map[string]bool)

	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("read scenario path %s: %w", path, err)
		}

		if !info.IsDir() {
			if !seen[path] {
				seen[path] = true
				expanded = append(expanded, path)
			}
			continue
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, fmt.Errorf("read scenario directory %s: %w", path, err)
		}

		var found []string
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			switch filepath.Ext(entry.Name()) {
			case ".yaml", ".yml":
				found = append(found, filepath.Join(path, entry.Name()))
			}
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("read scenario directory %s: no *.yaml file found", path)
		}

		sort.Strings(found)
		for _, f := range found {
			if !seen[f] {
				seen[f] = true
				expanded = append(expanded, f)
			}
		}
	}

	return expanded, nil
}
