package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/andersonten/simcitadel/internal/config"
)

// stubImport is the body of POST /__admin/mappings/import.
type stubImport struct {
	Mappings      []json.RawMessage `json:"mappings"`
	ImportOptions importOptions     `json:"importOptions"`
}

// importOptions controls what the import does to the stubs already on the
// server.
type importOptions struct {
	// DuplicatePolicy is OVERWRITE, so a mapping that carries an id replaces the
	// one already registered under it rather than failing the import.
	DuplicatePolicy string `json:"duplicatePolicy"`

	// DeleteAllNotInImport makes the pack on disk authoritative. A mapping that
	// no file holds is removed, so a stub a previous run left behind cannot
	// answer a request and make a scenario pass against nothing.
	DeleteAllNotInImport bool `json:"deleteAllNotInImport"`
}

// filesDirName is the directory WireMock resolves bodyFileName against.
const filesDirName = "__files"

// webhooksDirName holds webhook body files that bodyFile references resolve
// against. readMappings skips it, because a webhook body is not a stub.
const webhooksDirName = "webhooks"

// environmentError marks a failure to load the pack as a problem with the
// environment. The stubs are part of the environment, so the run exits 2 and
// does not report a scenario failure for a mock that could not be prepared.
func environmentError(err error) error {
	return &EnvironmentError{Msg: err.Error()}
}

// LoadWireMockStubs uploads the WireMock pack under paths.wiremock to the
// server named by targets.wiremock.
//
// The pack is loaded over the admin API rather than mounted, so one mechanism
// serves a local stack and a cluster. A mount cannot reach a cluster without a
// ConfigMap, a ConfigMap caps the pack at 1 MiB, and a stub change behind either
// needs a pod restart. The CLI already holds the admin URL and already forwards
// the port per SDD section 5.1, so it needs nothing new to reach the server.
//
// LoadWireMockStubs runs once per run, after the preflight and before the first
// scenario. It does not run per scenario: the reset of section 4.9 returns every
// scenario to its start state and leaves the mappings in place, which the
// integration test proves against the pinned image.
//
// An absent or empty pack loads nothing and is not an error. A scenario that
// touches no upstream needs no pack, and sending an empty import would delete
// mappings the server loaded from its own file system.
func LoadWireMockStubs(ctx context.Context, cfg *config.CitadelConfig, httpClient *http.Client) error {
	if cfg.Targets.WireMock == nil || cfg.Targets.WireMock.AdminURL == "" {
		return nil
	}
	// The directory resolves against the config file and not the working
	// directory. Reading paths.wiremock raw made a run from the repository root
	// look for a pack that was beside the config, find nothing, and load nothing
	// while reporting success.
	dir := cfg.WireMockDir()
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		// A missing directory is the "no pack" case, and a consuming repository
		// that keeps no stubs must still run. It is logged rather than passed
		// over in silence: an empty mock answers every upstream call with a 404,
		// and the path it looked in is the first thing worth knowing.
		slog.Info("no wiremock pack to load", "path", dir)
		return nil
	}

	mappings, err := readMappings(dir)
	if err != nil {
		return err
	}

	mappings, err = resolveWebhookBodyFiles(dir, mappings)
	if err != nil {
		return err
	}

	bodyFiles, err := readBodyFiles(dir)
	if err != nil {
		return err
	}

	if len(mappings) == 0 && len(bodyFiles) == 0 {
		slog.Info("the wiremock pack holds no mapping", "path", dir)
		return nil
	}

	adminBase := cfg.Targets.WireMock.AdminBase()

	// The bodies upload first. A mapping that names a bodyFileName is servable
	// the moment it is registered, so the file it points at has to be there
	// already.
	for name, content := range bodyFiles {
		if err := putBodyFile(ctx, httpClient, adminBase, name, content); err != nil {
			return err
		}
	}

	if len(mappings) > 0 {
		if err := importMappings(ctx, httpClient, adminBase, mappings); err != nil {
			return err
		}
	}

	slog.Info("wiremock pack loaded",
		"path", dir,
		"mappings", len(mappings),
		"bodyFiles", len(bodyFiles))
	return nil
}

// readMappings reads every mapping in the pack.
//
// A file holds either one mapping or a {"mappings": [...]} wrapper, and WireMock
// accepts both on disk, so both are read here. Every mapping from every file
// goes into one import, because an import per file with an authoritative
// deleteAllNotInImport would leave only the last file registered.
func readMappings(dir string) ([]json.RawMessage, error) {
	var mappings []json.RawMessage

	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// A response body and a webhook body are both JSON files, and
			// neither is a mapping.
			if entry.Name() == filesDirName || entry.Name() == webhooksDirName {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read the wiremock stub file %s: %w", path, err)
		}
		found, err := parseMappingFile(data)
		if err != nil {
			return fmt.Errorf("parse the wiremock stub file %s: %w", path, err)
		}
		mappings = append(mappings, found...)
		return nil
	})
	if err != nil {
		return nil, environmentError(err)
	}
	return mappings, nil
}

// parseMappingFile returns the mappings one file holds.
func parseMappingFile(data []byte) ([]json.RawMessage, error) {
	var wrapper struct {
		Mappings []json.RawMessage `json:"mappings"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, err
	}
	if len(wrapper.Mappings) > 0 {
		return wrapper.Mappings, nil
	}

	// No mappings key, so the file is one mapping. It still has to be a JSON
	// object: a file that parses as an array or a number is not a mapping, and
	// sending it would fail the import with a message about the whole batch
	// rather than about this file.
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	if len(object) == 0 {
		return nil, nil
	}
	return []json.RawMessage{json.RawMessage(data)}, nil
}

// readBodyFiles reads the __files directory of the pack, keyed by the path
// relative to it. WireMock resolves bodyFileName against that directory, so a
// nested file keeps its relative path or the name does not resolve.
func readBodyFiles(dir string) (map[string]string, error) {
	root := filepath.Join(dir, filesDirName)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	files := map[string]string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read the wiremock body file %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("resolve the wiremock body file %s: %w", path, err)
		}
		// The admin API takes a URL path, so the separator is a slash on every
		// platform.
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		return nil, environmentError(err)
	}
	return files, nil
}

// importMappings sends the whole pack in one authoritative import.
func importMappings(ctx context.Context, httpClient *http.Client, adminBase string, mappings []json.RawMessage) error {
	body, err := json.Marshal(stubImport{
		Mappings: mappings,
		ImportOptions: importOptions{
			DuplicatePolicy:      "OVERWRITE",
			DeleteAllNotInImport: true,
		},
	})
	if err != nil {
		return fmt.Errorf("encode the wiremock import: %w", err)
	}

	url := adminBase + "/mappings/import"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return environmentError(fmt.Errorf("build the wiremock import request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return environmentError(fmt.Errorf("import wiremock mappings: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return environmentError(fmt.Errorf(
			"import wiremock mappings: POST %s returned status %d: %s",
			url, resp.StatusCode, strings.TrimSpace(string(detail))))
	}
	return nil
}

// resolveWebhookBodyFiles walks every mapping and replaces a custom "bodyFile"
// field inside postServeActions and serveEventListeners parameters with the
// stringified content of the named file. The developer writes clean JSON in a
// separate file and never touches an escaped string.
//
// The field name is "bodyFile" rather than "bodyFileName" to avoid collision
// with WireMock 4.x native support. When WireMock 4.x is stable, this
// pre-processing becomes a no-op and the field migrates to the native one with
// no change to the developer's file layout.
func resolveWebhookBodyFiles(dir string, mappings []json.RawMessage) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(mappings))
	for _, raw := range mappings {
		resolved, err := resolveBodyFileInMapping(dir, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

// resolveBodyFileInMapping processes one mapping. It returns the input
// unchanged when no bodyFile field exists, so the common path allocates nothing.
func resolveBodyFileInMapping(dir string, raw json.RawMessage) (json.RawMessage, error) {
	var mapping map[string]json.RawMessage
	if err := json.Unmarshal(raw, &mapping); err != nil {
		return raw, nil
	}

	changed := false

	for _, key := range []string{"postServeActions", "serveEventListeners"} {
		actions, ok := mapping[key]
		if !ok {
			continue
		}
		resolved, didChange, err := resolveBodyFileInActions(dir, actions)
		if err != nil {
			return nil, err
		}
		if didChange {
			mapping[key] = resolved
			changed = true
		}
	}

	if !changed {
		return raw, nil
	}
	result, err := json.Marshal(mapping)
	if err != nil {
		return nil, fmt.Errorf("re-encode the mapping after bodyFile resolution: %w", err)
	}
	return result, nil
}

// resolveBodyFileInActions processes a postServeActions or serveEventListeners
// array and resolves any bodyFile references.
func resolveBodyFileInActions(dir string, raw json.RawMessage) (json.RawMessage, bool, error) {
	var actions []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &actions); err != nil {
		return raw, false, nil
	}

	changed := false
	for i, action := range actions {
		paramsRaw, ok := action["parameters"]
		if !ok {
			continue
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal(paramsRaw, &params); err != nil {
			continue
		}
		bodyFileRaw, ok := params["bodyFile"]
		if !ok {
			continue
		}

		var bodyFilePath string
		if err := json.Unmarshal(bodyFileRaw, &bodyFilePath); err != nil {
			return nil, false, fmt.Errorf("parse the bodyFile path: %w", err)
		}

		content, err := os.ReadFile(filepath.Join(dir, bodyFilePath))
		if err != nil {
			return nil, false, environmentError(
				fmt.Errorf("read the webhook body file %s: %w", bodyFilePath, err))
		}

		// Compact the file content into a single JSON string value so WireMock
		// receives it as the "body" parameter and applies its Handlebars engine.
		compacted := new(bytes.Buffer)
		if err := json.Compact(compacted, content); err != nil {
			// The file is not JSON. Pass it through as a raw string.
			escaped, _ := json.Marshal(string(content))
			params["body"] = escaped
		} else {
			escaped, _ := json.Marshal(compacted.String())
			params["body"] = escaped
		}
		delete(params, "bodyFile")

		newParams, err := json.Marshal(params)
		if err != nil {
			return nil, false, fmt.Errorf("re-encode webhook parameters: %w", err)
		}
		actions[i]["parameters"] = newParams
		changed = true
	}

	if !changed {
		return raw, false, nil
	}
	result, err := json.Marshal(actions)
	if err != nil {
		return nil, false, fmt.Errorf("re-encode actions after bodyFile resolution: %w", err)
	}
	return result, true, nil
}

// putBodyFile uploads one response body.
func putBodyFile(ctx context.Context, httpClient *http.Client, adminBase, name, content string) error {
	url := adminBase + "/files/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(content))
	if err != nil {
		return environmentError(fmt.Errorf("build the wiremock file request for %s: %w", name, err))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return environmentError(fmt.Errorf("upload the wiremock body file %s: %w", name, err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return environmentError(fmt.Errorf(
			"upload the wiremock body file %s: PUT %s returned status %d: %s",
			name, url, resp.StatusCode, strings.TrimSpace(string(detail))))
	}
	return nil
}
