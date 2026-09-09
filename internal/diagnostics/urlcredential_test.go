package diagnostics

import (
	"strings"
	"testing"
)

// TestRedact_CredentialInsideAnAMQPURL_IsRemoved holds the stated purpose of
// SDD section 12.2: a bundle is something a developer pastes into a chat
// window, so a credential must not survive the write.
//
// The key list of section 12.2 covers secret, password, token, authorization
// and dsn. It does not cover amqpUrl, and the reference config of section 5.1
// writes "amqp://${RABBITMQ_USER}:${RABBITMQ_PASSWORD}@host:5672/", so the
// resolved config in run.json carries the real broker password.
func TestRedact_CredentialInsideAnAMQPURL_IsRemoved(t *testing.T) {
	data := map[string]any{
		"targets": map[string]any{
			"rabbitmq": map[string]any{
				"amqpUrl":       "amqp://citadel:s3cr3t@broker.internal:5672/",
				"managementUrl": "http://citadel:s3cr3t@broker.internal:15672",
			},
		},
	}

	got := flatten(Redact(data))
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("the broker password survived redaction: %s", got)
	}
}

// TestRedact_URLWithoutACredential_KeepsTheAddress proves the redaction stays
// useful. An address is the most valuable line in a bundle, so removing a
// credential must not remove the host that a reader needs.
func TestRedact_URLWithoutACredential_KeepsTheAddress(t *testing.T) {
	data := map[string]any{
		"url":     "http://broker.internal:15672/api/queues",
		"amqpUrl": "amqp://broker.internal:5672/",
	}

	got := flatten(Redact(data))
	for _, want := range []string{"broker.internal", "15672", "5672", "/api/queues"} {
		if !strings.Contains(got, want) {
			t.Errorf("redaction removed %q, which carries no credential: %s", want, got)
		}
	}
}

// TestRedact_CredentialInAFreeTextString_IsRemoved covers an error message or a
// log line, where a connection string appears in prose rather than as the whole
// value. Section 12.2 asks for a bearer token to go "wherever it appears", and
// a password in a DSN is no less a credential.
func TestRedact_CredentialInAFreeTextString_IsRemoved(t *testing.T) {
	data := map[string]any{
		"message": `dial amqp amqp://citadel:s3cr3t@broker.internal:5672/: connection refused`,
		"detail":  `pq: password authentication failed for postgres://app:hunter2@db:5432/app`,
	}

	got := flatten(Redact(data))
	for _, secret := range []string{"s3cr3t", "hunter2"} {
		if strings.Contains(got, secret) {
			t.Errorf("the credential %q survived inside a message: %s", secret, got)
		}
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("redaction destroyed the diagnostic text: %s", got)
	}
}

// TestRedact_UserWithoutAPassword_IsKept keeps a URL that names a user and no
// password readable. The user is not the secret.
func TestRedact_UserWithoutAPassword_IsKept(t *testing.T) {
	data := map[string]any{"amqpUrl": "amqp://citadel@broker.internal:5672/"}

	got := flatten(Redact(data))
	if !strings.Contains(got, "citadel@broker.internal") {
		t.Errorf("a URL with no password must stay as it is: %s", got)
	}
}

// TestRedact_SectionTwelveTwoKeys_StillRedactWholly proves the rules that
// section 12.2 names outright keep working.
func TestRedact_SectionTwelveTwoKeys_StillRedactWholly(t *testing.T) {
	data := map[string]any{
		"clientSecret":  "abc",
		"password":      "abc",
		"token":         "abc",
		"authorization": "Bearer abc.def",
		"dsn":           "postgres://app:abc@db:5432/app",
		"note":          "Authorization: Bearer eyJhbGciOi.payload.signature",
	}

	got := flatten(Redact(data))
	if strings.Contains(got, "abc") {
		t.Errorf("a key that section 12.2 names must redact its whole value: %s", got)
	}
	if strings.Contains(got, "eyJhbGciOi.payload.signature") {
		t.Errorf("a bearer token must be replaced wherever it appears: %s", got)
	}
}

// flatten renders a redacted map to one searchable string.
func flatten(data map[string]any) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			for k, item := range val {
				b.WriteString(k)
				b.WriteString("=")
				walk(item)
				b.WriteString(" ")
			}
		case []any:
			for _, item := range val {
				walk(item)
				b.WriteString(" ")
			}
		default:
			b.WriteString(strings.TrimSpace(strings.Join(strings.Fields(toString(val)), " ")))
		}
	}
	walk(data)
	return b.String()
}

func toString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
