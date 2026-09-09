//go:build integration

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// wiremockImage matches the digest pinned in deploy/compose/docker-compose.yml.
// The reset paths are version-specific, so this test is worthless against any
// other tag.
const wiremockImage = "wiremock/wiremock:3.12.1@sha256:c3cf12f602139e3ffb4e042b246887ff6cadf3bcbb32625392d45f12e8f634d4"

// startWireMock starts the pinned WireMock and returns its admin base URL.
func startWireMock(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.Run(ctx, wiremockImage,
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/__admin/health").WithPort("8080/tcp").WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start the WireMock container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate the WireMock container: %v", err)
		}
	})

	endpoint, err := container.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		t.Fatalf("read the WireMock endpoint: %v", err)
	}
	return endpoint + "/__admin"
}

// TestResetWireMock_AgainstThePinnedImage_ClearsTheJournalAndTheScenarios is the
// test that had to exist. Reset runs before every scenario, per SDD section 4.9,
// so a reset that fails makes citadel run exit 3 on every run against the stack
// this repository ships. No unit test could catch it, because the defect was a
// path that this WireMock version does not serve.
func TestResetWireMock_AgainstThePinnedImage_ClearsTheJournalAndTheScenarios(t *testing.T) {
	adminURL := startWireMock(t)
	client := &http.Client{Timeout: 10 * time.Second}
	ctx := context.Background()

	// A stub with two states gives the scenario reset something to undo, and
	// calling it puts a request in the journal for the journal reset to clear.
	stub := `{
		"scenarioName": "Lifecycle",
		"requiredScenarioState": "Started",
		"newScenarioState": "Second",
		"request":  { "method": "GET", "urlPath": "/thing" },
		"response": { "status": 200 }
	}`
	if err := postJSON(ctx, client, adminURL+"/mappings", stub); err != nil {
		t.Fatalf("create the stub: %v", err)
	}

	resp, err := client.Get(strings.TrimSuffix(adminURL, "/__admin") + "/thing")
	if err != nil {
		t.Fatalf("call the stub: %v", err)
	}
	resp.Body.Close()

	if got := journalCount(t, client, adminURL); got != 1 {
		t.Fatalf("the journal holds %d request(s) before the reset, want 1", got)
	}
	if got := scenarioState(t, client, adminURL, "Lifecycle"); got != "Second" {
		t.Fatalf("the scenario is in state %q before the reset, want Second", got)
	}

	if err := resetWireMock(ctx, adminURL, client); err != nil {
		t.Fatalf("reset WireMock against the pinned image: %v", err)
	}

	if got := journalCount(t, client, adminURL); got != 0 {
		t.Errorf("the journal holds %d request(s) after the reset, want 0", got)
	}
	if got := scenarioState(t, client, adminURL, "Lifecycle"); got != "Started" {
		t.Errorf("the scenario is in state %q after the reset, want Started", got)
	}

	// The reset must not remove the stubs. Reset runs before every scenario, and a
	// reset that deleted the mappings would leave the second scenario of a run
	// with no upstream simulation at all.
	if got := mappingCount(t, client, adminURL); got != 1 {
		t.Errorf("the reset left %d mapping(s), want the 1 it started with", got)
	}
}

func postJSON(ctx context.Context, client *http.Client, url, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s returned status %d", url, resp.StatusCode)
	}
	return nil
}

func journalCount(t *testing.T, client *http.Client, adminURL string) int {
	t.Helper()
	var body struct {
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	getJSON(t, client, adminURL+"/requests", &body)
	return body.Meta.Total
}

func mappingCount(t *testing.T, client *http.Client, adminURL string) int {
	t.Helper()
	var body struct {
		Meta struct {
			Total int `json:"total"`
		} `json:"meta"`
	}
	getJSON(t, client, adminURL+"/mappings", &body)
	return body.Meta.Total
}

func scenarioState(t *testing.T, client *http.Client, adminURL, name string) string {
	t.Helper()
	var body struct {
		Scenarios []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"scenarios"`
	}
	getJSON(t, client, adminURL+"/scenarios", &body)
	for _, s := range body.Scenarios {
		if s.Name == name {
			return s.State
		}
	}
	return ""
}

func getJSON(t *testing.T, client *http.Client, url string, out any) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}
