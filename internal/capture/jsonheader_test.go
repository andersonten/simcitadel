package capture

import (
	"testing"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// SDD section 10.1 sets DEBEZIUM_FORMAT_HEADER=json with schemas disabled, and
// section 10.1 also maps three outbox columns onto headers through
// table.fields.additional.placement. Those two together mean a string header
// arrives JSON encoded, so the value of eventType on the wire is the six
// characters of a quote, the name, and a quote.
//
// The capture service stored that verbatim, and resolveEventType compared it
// against the plain name a scenario writes. It never matched. Every eventType
// selector failed against every event Debezium produced, which is the whole
// outbound half of the platform, and the message a developer saw was
// "countAtLeast: expected at least 1, got 0" with a captured message sitting in
// the buffer that looked correct.

// TestFlattenHeaders_DebeziumJSONEncodedValue_IsDecoded proves the decode.
func TestFlattenHeaders_DebeziumJSONEncodedValue_IsDecoded(t *testing.T) {
	// These are the exact bytes a Debezium sink puts on the wire, recorded from a
	// real run of the compose stack.
	table := amqpdriver.Table{
		"eventType":     `"Manuscript.Created"`,
		"aggregateType": `"public.orchestration-engine.Manuscript.v1"`,
		"aggregateId":   `"d4f2edfb-0cdd-4bc8-89c0-1a1db8e55647"`,
		"id":            `"0913ab57-a093-458b-b42e-82d96d03ecca"`,
	}

	headers := flattenHeaders(table)

	want := map[string]string{
		"eventType":     "Manuscript.Created",
		"aggregateType": "public.orchestration-engine.Manuscript.v1",
		"aggregateId":   "d4f2edfb-0cdd-4bc8-89c0-1a1db8e55647",
		"id":            "0913ab57-a093-458b-b42e-82d96d03ecca",
	}
	for key, expected := range want {
		if headers[key] != expected {
			t.Errorf("header %s is %q, want %q", key, headers[key], expected)
		}
	}
}

// TestFlattenHeaders_PlainValue_IsUnchanged keeps the decode from rewriting a
// header the event trigger set. Those travel as plain AMQP strings, so a value
// that is not a JSON string must survive byte for byte.
func TestFlattenHeaders_PlainValue_IsUnchanged(t *testing.T) {
	table := amqpdriver.Table{
		"citadelrun":  "01a01f2a-90e2-72e2-8413-11956196c034",
		"eventType":   "Manuscript.Created",
		"contentType": "application/json",
		// A number and a boolean are legal AMQP header values, and neither is a
		// JSON string, so both keep the formatting they had.
		"attempt": int32(3),
		"replay":  true,
	}

	headers := flattenHeaders(table)

	want := map[string]string{
		"citadelrun":  "01a01f2a-90e2-72e2-8413-11956196c034",
		"eventType":   "Manuscript.Created",
		"contentType": "application/json",
		"attempt":     "3",
		"replay":      "true",
	}
	for key, expected := range want {
		if headers[key] != expected {
			t.Errorf("header %s is %q, want %q", key, headers[key], expected)
		}
	}
}

// TestFlattenHeaders_ValueThatOnlyLooksQuoted_IsNotMangled draws the line. The
// decode reads a JSON string and nothing else, so a value that starts with a
// quote without being one keeps every byte.
func TestFlattenHeaders_ValueThatOnlyLooksQuoted_IsNotMangled(t *testing.T) {
	cases := map[string]string{
		// An unterminated quote is not a JSON string.
		`"unterminated`: `"unterminated`,
		// A JSON object and a JSON array are not JSON strings, and a scenario that
		// matches on one needs the braces.
		`{"a":1}`: `{"a":1}`,
		`[1,2]`:   `[1,2]`,
		// A quote in the middle is not a wrapper.
		`say "hello"`: `say "hello"`,
		// An empty JSON string decodes to an empty value, which is what it means.
		`""`: ``,
		// An escape inside a JSON string decodes, because that is the encoding.
		`"a\"b"`: `a"b`,
		`"a\nb"`: "a\nb",
	}

	for raw, want := range cases {
		headers := flattenHeaders(amqpdriver.Table{"h": raw})
		if headers["h"] != want {
			t.Errorf("the header value %s decoded to %q, want %q", raw, headers["h"], want)
		}
	}
}

// TestResolveEventType_DebeziumHeader_MatchesTheScenarioSelector is the end of the
// chain, and it is the assertion the reference scenario makes.
func TestResolveEventType_DebeziumHeader_MatchesTheScenarioSelector(t *testing.T) {
	msg := Message{
		Headers: flattenHeaders(amqpdriver.Table{"eventType": `"Manuscript.Created"`}),
		Body:    []byte(`{"specversion":"1.0","type":"Manuscript.Created","source":"a","id":"b","version":"v1"}`),
	}

	if got := resolveEventType(msg); got != "Manuscript.Created" {
		t.Errorf("the event type resolves to %q, and a scenario selector writes Manuscript.Created", got)
	}
}
