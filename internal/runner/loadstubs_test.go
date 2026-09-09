package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// importCapture records what the loader sent to the admin API.
type importCapture struct {
	imports []stubImport
	files   map[string]string
}

// newWireMockAdminStub answers the two endpoints the loader uses and records
// every call. The method and the path are both part of each pattern, so a
// loader that sends the wrong verb fails the test rather than passing against a
// permissive stub.
func newWireMockAdminStub(t *testing.T) (*httptest.Server, *importCapture) {
	t.Helper()
	capture := &importCapture{files: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /__admin/mappings/import", func(w http.ResponseWriter, r *http.Request) {
		var body stubImport
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode the import body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		capture.imports = append(capture.imports, body)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("PUT /__admin/files/{name...}", func(w http.ResponseWriter, r *http.Request) {
		content, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the file body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		capture.files[r.PathValue("name")] = string(content)
		w.WriteHeader(http.StatusOK)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, capture
}

// configForStubDir builds a config whose WireMock target is the stub server and
// whose paths.wiremock is dir.
func configForStubDir(adminBase, dir string) *config.CitadelConfig {
	return &config.CitadelConfig{
		Targets: config.TargetsConfig{
			WireMock: &config.WireMockTarget{
				URL:      adminBase,
				AdminURL: adminBase + "/__admin",
			},
		},
		Paths: config.PathsConfig{WireMock: dir},
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const singleMapping = `{
  "scenarioName": "article",
  "requiredScenarioState": "Started",
  "request": {"method": "GET", "url": "/one"},
  "response": {"status": 200}
}`

const wrappedMappings = `{
  "mappings": [
    {"request": {"method": "GET", "url": "/two"}, "response": {"status": 200}},
    {"request": {"method": "GET", "url": "/three"}, "response": {"status": 200}}
  ]
}`

func TestLoadWireMockStubs_MappingFiles_AreImportedInOneCall(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mappings", "one.json"), singleMapping)
	writeFile(t, filepath.Join(dir, "mappings", "more.json"), wrappedMappings)

	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	// One call and not one per file. A per-file import with
	// deleteAllNotInImport would leave only the last file on the server, so the
	// count is the property that matters and not an optimisation.
	if len(capture.imports) != 1 {
		t.Fatalf("import calls = %d, want 1", len(capture.imports))
	}
	if got := len(capture.imports[0].Mappings); got != 3 {
		t.Errorf("imported mappings = %d, want 3 (one single plus two wrapped)", got)
	}
}

func TestLoadWireMockStubs_Import_IsAuthoritative(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mappings", "one.json"), singleMapping)

	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	opts := capture.imports[0].ImportOptions
	// A stub left behind by an earlier run must not survive, or a scenario can
	// pass against a mapping that no file in the repository holds.
	if !opts.DeleteAllNotInImport {
		t.Error("importOptions.deleteAllNotInImport = false, want true")
	}
	if opts.DuplicatePolicy != "OVERWRITE" {
		t.Errorf("importOptions.duplicatePolicy = %q, want OVERWRITE", opts.DuplicatePolicy)
	}
}

func TestLoadWireMockStubs_BodyFiles_AreUploaded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mappings", "one.json"), singleMapping)
	writeFile(t, filepath.Join(dir, "__files", "body.json"), `{"hello":"world"}`)
	writeFile(t, filepath.Join(dir, "__files", "nested", "deep.json"), `{"deep":true}`)

	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	if got := capture.files["body.json"]; got != `{"hello":"world"}` {
		t.Errorf("uploaded body.json = %q", got)
	}
	// A nested file keeps its relative path, because bodyFileName carries that
	// path and a flattened name would not resolve.
	if got := capture.files["nested/deep.json"]; got != `{"deep":true}` {
		t.Errorf("uploaded nested/deep.json = %q, want the nested path preserved", got)
	}
}

func TestLoadWireMockStubs_FilesUnderTheFilesDirectory_AreNotImportedAsMappings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mappings", "one.json"), singleMapping)
	// A response body is JSON and lives in the tree, and it is not a mapping.
	writeFile(t, filepath.Join(dir, "__files", "body.json"), `{"hello":"world"}`)

	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	if got := len(capture.imports[0].Mappings); got != 1 {
		t.Errorf("imported mappings = %d, want 1; a body file must not become a mapping", got)
	}
}

func TestLoadWireMockStubs_NoDirectory_IsNotAnError(t *testing.T) {
	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, filepath.Join(t.TempDir(), "absent"))

	// A scenario that touches no upstream needs no WireMock pack, and it must
	// still run. An absent directory is that case and not a failure.
	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs with no directory: %v", err)
	}
	if len(capture.imports) != 0 {
		t.Errorf("import calls = %d, want 0; an absent pack must not clear the server", len(capture.imports))
	}
}

func TestLoadWireMockStubs_EmptyDirectory_DoesNotClearTheServer(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mappings"), 0o755); err != nil {
		t.Fatalf("create the mappings directory: %v", err)
	}

	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}
	// An empty pack with deleteAllNotInImport would delete a mapping the server
	// loaded from its own file system, which is how a mount and this loader
	// would fight each other.
	if len(capture.imports) != 0 {
		t.Errorf("import calls = %d, want 0 for an empty pack", len(capture.imports))
	}
}

func TestLoadWireMockStubs_InvalidJSON_NamesTheFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mappings", "broken.json"), `{"request": `)

	server, _ := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	err := LoadWireMockStubs(context.Background(), cfg, server.Client())
	if err == nil {
		t.Fatal("LoadWireMockStubs returned nil for a malformed mapping file")
	}
	if !strings.Contains(err.Error(), "broken.json") {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestLoadWireMockStubs_NoWireMockTarget_DoesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mappings", "one.json"), singleMapping)

	cfg := &config.CitadelConfig{Paths: config.PathsConfig{WireMock: dir}}

	// A config with no WireMock target cannot load stubs, and that is not a
	// failure: the run simply has no mock to load them into.
	if err := LoadWireMockStubs(context.Background(), cfg, http.DefaultClient); err != nil {
		t.Fatalf("LoadWireMockStubs with no target: %v", err)
	}
}

func TestLoadWireMockStubs_BodyFile_IsResolvedIntoBody(t *testing.T) {
	dir := t.TempDir()

	webhookBody := `{"delivery":"amqp","exchange":"test","routingKey":"test.key","event":{"type":"Test"}}`
	writeFile(t, filepath.Join(dir, "webhooks", "hook.json"), webhookBody)

	stub := `{
  "request": {"method": "POST", "url": "/graphql"},
  "response": {"status": 200},
  "postServeActions": [{
    "name": "webhook",
    "parameters": {
      "method": "POST",
      "url": "http://event-trigger:8080/publish",
      "headers": {"Content-Type": "application/json"},
      "bodyFile": "webhooks/hook.json"
    }
  }]
}`
	writeFile(t, filepath.Join(dir, "mappings", "with-webhook.json"), stub)

	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	if len(capture.imports) != 1 {
		t.Fatalf("import calls = %d, want 1", len(capture.imports))
	}

	imported := capture.imports[0].Mappings[0]
	var mapping map[string]json.RawMessage
	if err := json.Unmarshal(imported, &mapping); err != nil {
		t.Fatalf("unmarshal the imported mapping: %v", err)
	}

	var actions []map[string]json.RawMessage
	if err := json.Unmarshal(mapping["postServeActions"], &actions); err != nil {
		t.Fatalf("unmarshal postServeActions: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(actions[0]["parameters"], &params); err != nil {
		t.Fatalf("unmarshal parameters: %v", err)
	}

	if _, exists := params["bodyFile"]; exists {
		t.Error("bodyFile still present after resolution; it must be removed")
	}

	var body string
	if err := json.Unmarshal(params["body"], &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}

	if body != webhookBody {
		t.Errorf("resolved body = %q, want %q", body, webhookBody)
	}
}

func TestLoadWireMockStubs_BodyFile_MissingFile_ReportsEnvironmentError(t *testing.T) {
	dir := t.TempDir()

	stub := `{
  "request": {"method": "POST", "url": "/graphql"},
  "response": {"status": 200},
  "postServeActions": [{
    "name": "webhook",
    "parameters": {
      "method": "POST",
      "url": "http://event-trigger:8080/publish",
      "bodyFile": "webhooks/missing.json"
    }
  }]
}`
	writeFile(t, filepath.Join(dir, "mappings", "with-webhook.json"), stub)

	server, _ := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	err := LoadWireMockStubs(context.Background(), cfg, server.Client())
	if err == nil {
		t.Fatal("LoadWireMockStubs returned nil for a missing bodyFile")
	}
	if !strings.Contains(err.Error(), "missing.json") {
		t.Errorf("error does not name the missing file: %v", err)
	}
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("ClassifyError = %d, want ExitEnvironment (%d)", got, ExitEnvironment)
	}
}

func TestLoadWireMockStubs_ServeEventListeners_BodyFile_IsResolved(t *testing.T) {
	dir := t.TempDir()

	webhookBody := `{"delivery":"amqp","exchange":"x","routingKey":"y","event":{"type":"Z"}}`
	writeFile(t, filepath.Join(dir, "webhooks", "listener.json"), webhookBody)

	stub := `{
  "request": {"method": "POST", "url": "/graphql"},
  "response": {"status": 200},
  "serveEventListeners": [{
    "name": "webhook",
    "parameters": {
      "method": "POST",
      "url": "http://event-trigger:8080/publish",
      "bodyFile": "webhooks/listener.json"
    }
  }]
}`
	writeFile(t, filepath.Join(dir, "mappings", "listener.json"), stub)

	server, capture := newWireMockAdminStub(t)
	cfg := configForStubDir(server.URL, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, server.Client()); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	imported := capture.imports[0].Mappings[0]
	var mapping map[string]json.RawMessage
	if err := json.Unmarshal(imported, &mapping); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var actions []map[string]json.RawMessage
	if err := json.Unmarshal(mapping["serveEventListeners"], &actions); err != nil {
		t.Fatalf("unmarshal serveEventListeners: %v", err)
	}

	var params map[string]json.RawMessage
	if err := json.Unmarshal(actions[0]["parameters"], &params); err != nil {
		t.Fatalf("unmarshal parameters: %v", err)
	}

	var body string
	if err := json.Unmarshal(params["body"], &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body != webhookBody {
		t.Errorf("resolved body = %q, want %q", body, webhookBody)
	}
}

func TestLoadWireMockStubs_ServerRejectsTheImport_ReportsAnEnvironmentError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "mappings", "one.json"), singleMapping)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /__admin/mappings/import", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cfg := configForStubDir(server.URL, dir)
	err := LoadWireMockStubs(context.Background(), cfg, server.Client())
	if err == nil {
		t.Fatal("LoadWireMockStubs returned nil for a 500 from the server")
	}
	// The stubs are part of the environment, so a failure to load them is an
	// environment error and exits 2 rather than reporting a scenario failure.
	if got := ClassifyError(err); got != ExitEnvironment {
		t.Errorf("ClassifyError = %d, want ExitEnvironment (%d)", got, ExitEnvironment)
	}
}
