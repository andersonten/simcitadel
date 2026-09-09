//go:build integration

package steps

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/andersonten/simcitadel/internal/config"
	"github.com/andersonten/simcitadel/internal/scenario"
)

// This file proves the journal match of SDD section 6.5 against the WireMock
// image that deploy/compose/docker-compose.yml pins. The unit tests of
// journal_test.go answer with a fake that follows the same rules, so this test
// is the authority for those rules.
//
// Run it with:
//
//	go test -tags=integration ./internal/steps/...
const (
	// wiremockImage matches the digest pinned in deploy/compose/docker-compose.yml.
	wiremockImage = "wiremock/wiremock:3.12.1@sha256:c3cf12f602139e3ffb4e042b246887ff6cadf3bcbb32625392d45f12e8f634d4"

	// journalPollInterval is how often a condition is re-checked.
	journalPollInterval = 50 * time.Millisecond

	// journalPollTimeout bounds how long a condition can take to become true.
	journalPollTimeout = 20 * time.Second
)

// startWireMock starts a WireMock container and returns its base address.
func startWireMock(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.Run(ctx, wiremockImage,
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/__admin/health").WithPort("8080/tcp").WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start WireMock container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate WireMock container: %v", err)
		}
	})

	endpoint, err := container.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		t.Fatalf("read WireMock address: %v", err)
	}
	return endpoint
}

// callWireMock sends one request that WireMock records in its journal.
func callWireMock(t *testing.T, client *http.Client, method, baseURL, path string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("create %s %s: %v", method, path, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send %s %s: %v", method, path, err)
	}
	_ = resp.Body.Close()
}

// journalCount returns the number of journal entries that match a query.
func journalCount(t *testing.T, rc *RunContext, q *scenario.JournalQuery) int {
	t.Helper()
	result, err := ExecuteProbe(context.Background(), &scenario.Step{
		Name:    "journal count",
		Type:    "probe",
		Journal: q,
	}, rc)
	if err != nil {
		t.Fatalf("probe journal: %v", err)
	}
	matches, ok := result.Response.([]any)
	if !ok {
		t.Fatalf("probe journal answered %T, want a list of matches", result.Response)
	}
	return len(matches)
}

// awaitJournalCount polls until the whole journal holds the wanted number of
// entries. WireMock records a request as it serves it, and a poll with a
// timeout replaces a fixed sleep.
func awaitJournalCount(t *testing.T, rc *RunContext, want int) {
	t.Helper()
	deadline := time.Now().Add(journalPollTimeout)
	for {
		got := journalCount(t, rc, &scenario.JournalQuery{})
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("journal holds %d entries after %s, want %d", got, journalPollTimeout, want)
		}
		time.Sleep(journalPollInterval)
	}
}

// The journal probe asks WireMock itself which requests match, so the match
// rules are WireMock's rules. This test records the rules that
// journal_test.go relies on.
func TestProbeJournal_AgainstWireMock_MatchesMethodAndExactPath(t *testing.T) {
	baseURL := startWireMock(t)
	client := &http.Client{Timeout: 10 * time.Second}

	rc := &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				WireMock: &config.WireMockTarget{URL: baseURL, AdminURL: baseURL + "/__admin"},
			},
		},
		RunID:      "run-journal-container",
		HTTPClient: client,
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}

	callWireMock(t, client, http.MethodPost, baseURL, "/graphql")
	callWireMock(t, client, http.MethodPost, baseURL, "/graphql?trace=1")
	callWireMock(t, client, http.MethodGet, baseURL, "/graphql")
	callWireMock(t, client, http.MethodPost, baseURL, "/graphql/extra")
	awaitJournalCount(t, rc, 4)

	cases := []struct {
		name  string
		query *scenario.JournalQuery
		want  int
	}{
		{"method and exact path", &scenario.JournalQuery{Method: "POST", URLPath: "/graphql"}, 2},
		{"another method", &scenario.JournalQuery{Method: "PUT", URLPath: "/graphql"}, 0},
		{"another path", &scenario.JournalQuery{Method: "POST", URLPath: "/rest/articles"}, 0},
		{"a longer path", &scenario.JournalQuery{Method: "POST", URLPath: "/graphql/extra"}, 1},
		{"method alone", &scenario.JournalQuery{Method: "POST"}, 3},
		{"path alone", &scenario.JournalQuery{URLPath: "/graphql"}, 3},
		{"lower case method", &scenario.JournalQuery{Method: "post", URLPath: "/graphql"}, 2},
		{"no filter", &scenario.JournalQuery{}, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := journalCount(t, rc, c.query); got != c.want {
				t.Errorf("matches for %+v: got %d, want %d", c.query, got, c.want)
			}
		})
	}
}

// count and countAtLeast read the filtered set, so an expectation on the
// journal passes only when the app called that one operation often enough.
func TestProbeJournal_AgainstWireMock_CountReadsTheFilteredSet(t *testing.T) {
	baseURL := startWireMock(t)
	client := &http.Client{Timeout: 10 * time.Second}

	rc := &RunContext{
		Config: &config.CitadelConfig{
			Targets: config.TargetsConfig{
				WireMock: &config.WireMockTarget{URL: baseURL, AdminURL: baseURL + "/__admin"},
			},
		},
		RunID:      "run-journal-container-count",
		HTTPClient: client,
		Variables:  map[string]any{},
		Bindings:   map[string]any{},
	}

	callWireMock(t, client, http.MethodGet, baseURL, "/favicon.ico")
	callWireMock(t, client, http.MethodGet, baseURL, "/rest/articles")
	callWireMock(t, client, http.MethodPost, baseURL, "/graphql")
	awaitJournalCount(t, rc, 3)

	query := func() *scenario.JournalQuery {
		return &scenario.JournalQuery{Method: "POST", URLPath: "/graphql"}
	}
	cases := []struct {
		name string
		exp  *scenario.Expect
		pass bool
	}{
		{"count one match", &scenario.Expect{Count: intOf(1)}, true},
		{"count the whole journal", &scenario.Expect{Count: intOf(3)}, false},
		{"countAtLeast one match", &scenario.Expect{CountAtLeast: intOf(1)}, true},
		{"countAtLeast two matches", &scenario.Expect{CountAtLeast: intOf(2)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := Execute(context.Background(), &scenario.Step{
				Name:    fmt.Sprintf("journal %s", c.name),
				Type:    "probe",
				Journal: query(),
				Expect:  c.exp,
			}, rc)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if result.Pass != c.pass {
				t.Errorf("pass: got %t, want %t (%s)", result.Pass, c.pass, result.Expect.Message)
			}
		})
	}
}
