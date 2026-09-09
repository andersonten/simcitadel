package config

import (
	"strings"
	"testing"
	"time"
)

// This file locks down the defaults block and the key discipline of SDD
// section 5.1. These tests began as a proof that a config which omits defaults
// behaved differently from one that writes the documented values, and that an
// unknown key inside a list passed unnoticed.

const minimalConfig = `version: 1
environment:
  name: local
targets:
  wiremock:
    url: http://127.0.0.1:9090
    adminUrl: http://127.0.0.1:9090/__admin
`

// TestLoad_NoDefaultsBlock_AppliesEveryDocumentedDefault holds SDD section 5.1.
// An omitted block must behave exactly like one that writes every value.
func TestLoad_NoDefaultsBlock_AppliesEveryDocumentedDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"awaitTimeout", cfg.Defaults.AwaitTimeout.Duration(), 30 * time.Second},
		{"awaitInterval", cfg.Defaults.AwaitInterval.Duration(), 250 * time.Millisecond},
		{"quietPeriod", cfg.Defaults.QuietPeriod.Duration(), 5 * time.Second},
		{"appReadyTimeout", cfg.Defaults.AppReadyTimeout.Duration(), 60 * time.Second},
		{"resetSettle", cfg.Defaults.ResetSettle.Duration(), 250 * time.Millisecond},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if cfg.Defaults.PublishBudget != 100 {
		t.Errorf("publishBudget = %d, want 100", cfg.Defaults.PublishBudget)
	}
}

// TestLoad_NoPathsBlock_AppliesEveryDocumentedDirectory covers the paths half
// of the same section.
func TestLoad_NoPathsBlock_AppliesEveryDocumentedDirectory(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	cases := []struct{ name, got, want string }{
		{"scenarios", cfg.Paths.Scenarios, "./scenarios"},
		{"payloads", cfg.Paths.Payloads, "./payloads"},
		{"sql", cfg.Paths.SQL, "./sql"},
		{"queries", cfg.Paths.Queries, "./queries"},
		{"wiremock", cfg.Paths.WireMock, "./wiremock"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("paths.%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestLoad_WrittenDefaults_WinOverTheBuiltInValues keeps the defaults
// overridable, so the test above cannot pass by pinning the values.
func TestLoad_WrittenDefaults_WinOverTheBuiltInValues(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig+`defaults:
  awaitInterval: 900ms
  publishBudget: 7
paths:
  payloads: ./fixtures
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Defaults.AwaitInterval.Duration(); got != 900*time.Millisecond {
		t.Errorf("awaitInterval = %s, want 900ms", got)
	}
	if cfg.Defaults.PublishBudget != 7 {
		t.Errorf("publishBudget = %d, want 7", cfg.Defaults.PublishBudget)
	}
	if cfg.Paths.Payloads != "./fixtures" {
		t.Errorf("paths.payloads = %q, want ./fixtures", cfg.Paths.Payloads)
	}
	// An unwritten sibling still takes its default.
	if got := cfg.Defaults.AwaitTimeout.Duration(); got != 30*time.Second {
		t.Errorf("awaitTimeout = %s, want 30s", got)
	}
}

// TestLoad_UnknownKeyInsideAList_IsRejected holds the key discipline of SDD
// section 5.1 for list elements. A typo inside capture.exchanges is as silent
// as a typo anywhere else, and just as wrong.
func TestLoad_UnknownKeyInsideAList_IsRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "capture exchange",
			body: minimalConfig + `capture:
  exchanges:
    - { name: public.a.v1, declair: own }
`,
			want: "capture.exchanges[0]",
		},
		{
			name: "kubernetes forward",
			body: minimalConfig + `kubernetes:
  context: c
  namespace: n
  forwards:
    - { service: citadel-wiremock, localPort: 9090, remotePort: 8080, protocol: tcp }
`,
			want: "kubernetes.forwards[0]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("an unknown key inside a list was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err.Error(), tc.want)
			}
		})
	}
}

// TestLoad_KnownKeysInsideAList_AreAccepted keeps the check above from
// rejecting the documented shape.
func TestLoad_KnownKeysInsideAList_AreAccepted(t *testing.T) {
	_, err := Load(writeConfig(t, minimalConfig+`capture:
  exchanges:
    - { name: public.orchestration-engine.Article.v1, declare: own }
  ignore:
    - private.orchestration-engine.Heartbeat.v1
kubernetes:
  context: my-cluster
  namespace: default
  forwards:
    - { service: citadel-wiremock, localPort: 9090, remotePort: 8080 }
`))
	if err != nil {
		t.Fatalf("the documented list shape was rejected: %v", err)
	}
}
