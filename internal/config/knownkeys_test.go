package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// yamlKeysOf returns the yaml key of every field of a struct type.
func yamlKeysOf(t *testing.T, target any) []string {
	t.Helper()

	typ := reflect.TypeOf(target)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("yamlKeysOf needs a struct, got %s", typ.Kind())
	}

	var keys []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			keys = append(keys, name)
		}
	}
	slices.Sort(keys)
	return keys
}

// TestKnownNestedKeys_MatchTheStructTags guards the defect where a new field
// was added to a config struct but not to the accepted key set. Because
// rejectUnknownKeys runs before decode, such a field is rejected outright and
// the feature it belongs to cannot be reached at all.
func TestKnownNestedKeys_MatchTheStructTags(t *testing.T) {
	cases := []struct {
		path   string
		target any
	}{
		{"targets.app-db", DatabaseTarget{}},
		{"targets.upstream-shadow", DatabaseTarget{}},
		{"targets.app-db.inbox", InboxConfig{}},
		{"targets.upstream-shadow.inbox", InboxConfig{}},
		{"targets.wiremock", WireMockTarget{}},
		{"targets.event-trigger", EventTriggerTarget{}},
		{"targets.event-capture", EventCaptureTarget{}},
		{"targets.rabbitmq", RabbitMQTarget{}},
		{"targets.app", AppTarget{}},
		{"targets.debezium-outbox", DebeziumOutboxTarget{}},
		{"defaults", DefaultsConfig{}},
		{"paths", PathsConfig{}},
		{"capture", CaptureConfig{}},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			accepted, ok := knownNestedKeys[tc.path]
			if !ok {
				t.Fatalf("knownNestedKeys has no entry for %q", tc.path)
			}

			acceptedKeys := make([]string, 0, len(accepted))
			for key := range accepted {
				acceptedKeys = append(acceptedKeys, key)
			}
			slices.Sort(acceptedKeys)

			structKeys := yamlKeysOf(t, tc.target)

			if !slices.Equal(acceptedKeys, structKeys) {
				t.Errorf("accepted keys for %q do not match the struct tags\n accepted: %v\n   struct: %v",
					tc.path, acceptedKeys, structKeys)
			}
		})
	}
}

// TestLoad_InboxColumnOverrides_AreAccepted proves an inbox block that names
// every column parses and reaches the decoded configuration.
func TestLoad_InboxColumnOverrides_AreAccepted(t *testing.T) {
	path := writeConfig(t, `version: 1
environment:
  name: local
targets:
  app-db:
    driver: postgres
    dsn: "postgres://citadel:${CITADEL_TEST_PW}@localhost:5432/citadel"
    inbox:
      table: svc_inbox
      messageIdColumn: msg_id
      processedAtColumn: handled_at
      retryCountColumn: attempts
      lastErrorColumn: failure
`)
	t.Setenv("CITADEL_TEST_PW", "pw")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	inbox := cfg.Targets.AppDB.Inbox
	if inbox == nil {
		t.Fatal("app-db inbox is nil")
	}

	checks := map[string][2]string{
		"table":             {inbox.Table, "svc_inbox"},
		"messageIdColumn":   {inbox.MessageIDColumn, "msg_id"},
		"processedAtColumn": {inbox.ProcessedAtColumn, "handled_at"},
		"retryCountColumn":  {inbox.RetryCountColumn, "attempts"},
		"lastErrorColumn":   {inbox.LastErrorColumn, "failure"},
	}
	for name, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
}

// TestLoad_InboxWithoutColumnNames_TakesTheReferenceDefaults covers the other
// half: an inbox block that names nothing still resolves every column.
func TestLoad_InboxWithoutColumnNames_TakesTheReferenceDefaults(t *testing.T) {
	path := writeConfig(t, `version: 1
environment:
  name: local
targets:
  app-db:
    driver: postgres
    dsn: "postgres://citadel:${CITADEL_TEST_PW}@localhost:5432/citadel"
    inbox:
      table: svc_inbox
`)
	t.Setenv("CITADEL_TEST_PW", "pw")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	inbox := cfg.Targets.AppDB.Inbox
	checks := map[string][2]string{
		"messageIdColumn":   {inbox.MessageIDColumn, DefaultInboxMessageIDColumn},
		"processedAtColumn": {inbox.ProcessedAtColumn, DefaultInboxProcessedAtColumn},
		"retryCountColumn":  {inbox.RetryCountColumn, DefaultInboxRetryCountColumn},
		"lastErrorColumn":   {inbox.LastErrorColumn, DefaultInboxLastErrorColumn},
	}
	for name, pair := range checks {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
}
