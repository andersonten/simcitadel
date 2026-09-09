//go:build integration

package runner

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests below run against the WireMock digest that deploy/compose pins. The
// import endpoint, the file endpoint and the reset semantics are all
// version-specific, so a unit test with a stub server cannot prove any of them.

// startWireMockRoot starts the pinned WireMock and returns its root URL.
// startWireMock returns the admin URL, and configForStubDir appends the /__admin
// segment itself, so the root is what this file needs.
func startWireMockRoot(t *testing.T) string {
	t.Helper()
	return strings.TrimSuffix(startWireMock(t), "/__admin")
}

// packDir writes a WireMock pack and returns its root.
func packDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

// get returns the status and body of a GET against the WireMock base URL.
func get(t *testing.T, base, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("build the request for %s: %v", path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	return resp.StatusCode, strings.TrimSpace(string(body[:n]))
}

const helloMapping = `{
  "request": {"method": "GET", "url": "/hello"},
  "response": {"status": 200, "body": "hello"}
}`

func TestLoadWireMockStubs_AgainstThePinnedImage_ServesTheStub(t *testing.T) {
	base := startWireMockRoot(t)
	dir := packDir(t, map[string]string{"mappings/hello.json": helloMapping})
	cfg := configForStubDir(base, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, &http.Client{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	// The proof is a served request and not a 200 from the admin API. An import
	// the server accepts and does not register would pass a weaker assertion.
	status, body := get(t, base, "/hello")
	if status != http.StatusOK || body != "hello" {
		t.Fatalf("GET /hello = %d %q, want 200 \"hello\"", status, body)
	}
}

func TestLoadWireMockStubs_AgainstThePinnedImage_BodyFileResolves(t *testing.T) {
	base := startWireMockRoot(t)
	dir := packDir(t, map[string]string{
		"mappings/body.json": `{
			"request": {"method": "GET", "url": "/from-file"},
			"response": {"status": 200, "bodyFileName": "nested/payload.json"}
		}`,
		"__files/nested/payload.json": `{"loaded":"from a file"}`,
	})
	cfg := configForStubDir(base, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, &http.Client{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	// This is the case a ConfigMap mount handles for free and an admin API load
	// has to earn. A bodyFileName that does not resolve returns a 500, so a
	// served body proves the upload landed where WireMock looks for it.
	status, body := get(t, base, "/from-file")
	if status != http.StatusOK {
		t.Fatalf("GET /from-file = %d, want 200; the body file did not resolve", status)
	}
	if !strings.Contains(body, "from a file") {
		t.Errorf("GET /from-file body = %q, want the uploaded file content", body)
	}
}

func TestLoadWireMockStubs_AgainstThePinnedImage_SurvivesTheReset(t *testing.T) {
	base := startWireMockRoot(t)
	dir := packDir(t, map[string]string{
		"mappings/stateful.json": `{
			"scenarioName": "article",
			"requiredScenarioState": "Started",
			"request": {"method": "GET", "url": "/stateful"},
			"response": {"status": 200, "body": "start state"}
		}`,
	})
	cfg := configForStubDir(base, dir)

	if err := LoadWireMockStubs(context.Background(), cfg, &http.Client{Timeout: 30 * time.Second}); err != nil {
		t.Fatalf("LoadWireMockStubs: %v", err)
	}

	// The two calls of SDD section 4.9 step 1 and step 2. The loader runs once
	// per run and the reset runs once per scenario, so the whole design depends
	// on the reset leaving the mappings alone.
	for _, call := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/scenarios/reset"},
		{http.MethodDelete, "/requests"},
	} {
		req, err := http.NewRequestWithContext(context.Background(), call.method, cfg.Targets.WireMock.AdminBase()+call.path, nil)
		if err != nil {
			t.Fatalf("build %s %s: %v", call.method, call.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", call.method, call.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d, want 200", call.method, call.path, resp.StatusCode)
		}
	}

	status, body := get(t, base, "/stateful")
	if status != http.StatusOK || body != "start state" {
		t.Fatalf("after the reset, GET /stateful = %d %q, want 200 \"start state\"", status, body)
	}
}

func TestLoadWireMockStubs_AgainstThePinnedImage_RemovesAStubNoFileHolds(t *testing.T) {
	base := startWireMockRoot(t)
	client := &http.Client{Timeout: 30 * time.Second}

	// A previous run leaves two stubs behind.
	first := packDir(t, map[string]string{
		"mappings/hello.json": helloMapping,
		"mappings/stale.json": `{
			"request": {"method": "GET", "url": "/stale"},
			"response": {"status": 200, "body": "stale"}
		}`,
	})
	if err := LoadWireMockStubs(context.Background(), configForStubDir(base, first), client); err != nil {
		t.Fatalf("the first load: %v", err)
	}
	if status, _ := get(t, base, "/stale"); status != http.StatusOK {
		t.Fatalf("GET /stale before the second load = %d, want 200", status)
	}

	// Someone deletes the stale stub file and runs again.
	second := packDir(t, map[string]string{"mappings/hello.json": helloMapping})
	if err := LoadWireMockStubs(context.Background(), configForStubDir(base, second), client); err != nil {
		t.Fatalf("the second load: %v", err)
	}

	// The deleted file must take its stub with it. A stub that outlives the file
	// that declared it can answer a request and make a scenario pass against a
	// mapping nobody can find in the repository.
	if status, _ := get(t, base, "/stale"); status != http.StatusNotFound {
		t.Errorf("GET /stale after the second load = %d, want 404", status)
	}
	if status, _ := get(t, base, "/hello"); status != http.StatusOK {
		t.Errorf("GET /hello after the second load = %d, want 200", status)
	}
	if got := mappingCount(t, client, configForStubDir(base, second).Targets.WireMock.AdminBase()); got != 1 {
		t.Errorf("registered mappings = %d, want 1", got)
	}
}
