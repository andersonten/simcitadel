package diagnostics

import (
	"testing"
)

func TestRedact_SecretKey_IsRedacted(t *testing.T) {
	keys := []string{"secret", "password", "token", "authorization", "dsn"}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			data := map[string]any{key: "sensitive-value"}
			result := Redact(data)
			if result[key] != "«redacted»" {
				t.Errorf("key %q: got %v, want «redacted»", key, result[key])
			}
		})
	}
}

func TestRedact_NonSecretKey_IsPreserved(t *testing.T) {
	data := map[string]any{
		"host": "localhost",
		"port": 5672,
	}
	result := Redact(data)
	if result["host"] != "localhost" {
		t.Errorf("host: got %v, want localhost", result["host"])
	}
	if result["port"] != 5672 {
		t.Errorf("port: got %v, want 5672", result["port"])
	}
}

func TestRedact_NestedMap_RedactsDeep(t *testing.T) {
	data := map[string]any{
		"database": map[string]any{
			"host":     "db.example.com",
			"password": "hunter2",
		},
	}
	result := Redact(data)
	nested, ok := result["database"].(map[string]any)
	if !ok {
		t.Fatal("database value is not a map")
	}
	if nested["host"] != "db.example.com" {
		t.Errorf("nested host: got %v, want db.example.com", nested["host"])
	}
	if nested["password"] != "«redacted»" {
		t.Errorf("nested password: got %v, want «redacted»", nested["password"])
	}
}

func TestRedact_BearerToken_IsRedacted(t *testing.T) {
	data := map[string]any{
		"header": "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
	}
	result := Redact(data)
	want := "Bearer «redacted»"
	if result["header"] != want {
		t.Errorf("header: got %v, want %v", result["header"], want)
	}
}

func TestRedactString_BearerToken_IsReplaced(t *testing.T) {
	input := "Authorization: Bearer abc123.def456"
	got := RedactString(input)
	want := "Authorization: Bearer «redacted»"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactString_NoToken_IsUnchanged(t *testing.T) {
	input := "just a normal string"
	got := RedactString(input)
	if got != input {
		t.Errorf("got %q, want %q", got, input)
	}
}
