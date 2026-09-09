package capture

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// conformingBody returns a CloudEvents 1.0 envelope with the required version
// attribute.
const conformingBody = `{"specversion":"1.0","id":"e-1","type":"ArticleSubmitted",` +
	`"source":"SRV-TEST","version":"v1"}`

// TestCapturedHandler_NonJSONBody_ServesEveryMessage holds SDD section 8.2 and
// section 4.11. A body that is not JSON is the most malformed event an
// application can publish. The query response must still parse, and it must
// still hold that message and every other message in the buffer.
func TestCapturedHandler_NonJSONBody_ServesEveryMessage(t *testing.T) {
	const malformed = "<xml>not an event</xml>"

	buf := NewBuffer(10)
	buf.Add(Message{
		ReceivedAt: time.Now(),
		Exchange:   "public.orchestration-engine.Manuscript.v1",
		RoutingKey: "public.orchestration-engine.Manuscript.v1",
		Headers:    map[string]string{},
		RunID:      "run-nonjson",
		Body:       json.RawMessage(malformed),
	})
	buf.Add(Message{
		ReceivedAt: time.Now(),
		Exchange:   "public.orchestration-engine.Article.v1",
		RoutingKey: "public.orchestration-engine.Article.v1",
		Headers:    map[string]string{},
		RunID:      "run-nonjson",
		Body:       json.RawMessage(conformingBody),
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewServer(buf, func() map[string]string { return nil }, logger).Handler())
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/captured")
	if err != nil {
		t.Fatalf("query the capture service: %v", err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read the query response: %v", err)
	}
	t.Logf("GET /captured returned status %d and body %q", resp.StatusCode, string(raw))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /captured returned status %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// The runner decodes the response into a list of generic objects, so the
	// test asserts the wire shape that the runner sees.
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("the query response is not valid JSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("the query response holds %d entries, want 2", len(entries))
	}

	bad, good := entries[0], entries[1]

	if got := bad["bodyEncoding"]; got != BodyEncodingText {
		t.Errorf("the malformed entry reports bodyEncoding %v, want %q", got, BodyEncodingText)
	}
	if got, ok := bad["body"].(string); !ok || got != malformed {
		t.Errorf("the malformed entry holds body %v, want the verbatim bytes %q", bad["body"], malformed)
	}
	verdict, ok := bad["cloudEvents"].(map[string]any)
	if !ok {
		t.Fatalf("the malformed entry holds cloudEvents %v, want an object", bad["cloudEvents"])
	}
	if valid, _ := verdict["valid"].(bool); valid {
		t.Error("the malformed entry reports cloudEvents.valid true, want false")
	}

	if got := good["bodyEncoding"]; got != BodyEncodingJSON {
		t.Errorf("the well-formed entry reports bodyEncoding %v, want %q", got, BodyEncodingJSON)
	}
	envelope, ok := good["body"].(map[string]any)
	if !ok {
		t.Fatalf("the well-formed entry holds body %v, want a JSON object", good["body"])
	}
	if envelope["id"] != "e-1" {
		t.Errorf("the well-formed entry holds id %v, want %q", envelope["id"], "e-1")
	}
}

// TestMessage_BodyOfEachKind_MarshalsAndRoundTrips holds the query contract of
// SDD section 8.2. A consumer must tell a JSON body from a body that is not
// JSON, and no byte of the payload is lost.
func TestMessage_BodyOfEachKind_MarshalsAndRoundTrips(t *testing.T) {
	binary := []byte{0xff, 0xfe, 0x00, 0x01}

	cases := []struct {
		name         string
		body         json.RawMessage
		wantEncoding string
		wantWire     any
	}{
		{
			name:         "a JSON object stays JSON",
			body:         json.RawMessage(`{"id":"e-1"}`),
			wantEncoding: BodyEncodingJSON,
			wantWire:     map[string]any{"id": "e-1"},
		},
		{
			name:         "a JSON string stays JSON",
			body:         json.RawMessage(`"plain"`),
			wantEncoding: BodyEncodingJSON,
			wantWire:     "plain",
		},
		{
			name:         "text that is not JSON becomes a JSON string",
			body:         json.RawMessage("<xml>not an event</xml>"),
			wantEncoding: BodyEncodingText,
			wantWire:     "<xml>not an event</xml>",
		},
		{
			name:         "an empty body becomes an empty JSON string",
			body:         json.RawMessage(""),
			wantEncoding: BodyEncodingText,
			wantWire:     "",
		},
		{
			name:         "bytes that are not UTF-8 become base64",
			body:         json.RawMessage(binary),
			wantEncoding: BodyEncodingBase64,
			wantWire:     base64.StdEncoding.EncodeToString(binary),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(Message{Exchange: "x", Body: tc.body})
			if err != nil {
				t.Fatalf("marshal the message: %v", err)
			}
			t.Logf("wire form: %s", string(raw))

			var entry map[string]any
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatalf("the wire form is not valid JSON: %v", err)
			}
			if got := entry["bodyEncoding"]; got != tc.wantEncoding {
				t.Errorf("bodyEncoding = %v, want %q", got, tc.wantEncoding)
			}
			switch want := tc.wantWire.(type) {
			case string:
				if got, ok := entry["body"].(string); !ok || got != want {
					t.Errorf("body = %v, want the string %q", entry["body"], want)
				}
			case map[string]any:
				got, ok := entry["body"].(map[string]any)
				if !ok {
					t.Fatalf("body = %v, want a JSON object", entry["body"])
				}
				for k, v := range want {
					if got[k] != v {
						t.Errorf("body[%q] = %v, want %v", k, got[k], v)
					}
				}
			}

			var back Message
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal the message: %v", err)
			}
			if string(back.Body) != string(tc.body) {
				t.Errorf("the round trip returns body %q, want %q", string(back.Body), string(tc.body))
			}
		})
	}
}

// TestBuffer_Add_MessageWithoutVerdict_StoresTheCloudEventsVerdict holds SDD
// section 8.2. The stored entry presents cloudEvents as the validity verdict,
// so every entry carries one.
func TestBuffer_Add_MessageWithoutVerdict_StoresTheCloudEventsVerdict(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", Body: json.RawMessage("<xml>not an event</xml>")})
	buf.Add(Message{Exchange: "a", Body: json.RawMessage(conformingBody)})

	messages := buf.Query(Filter{})
	if len(messages) != 2 {
		t.Fatalf("the buffer holds %d messages, want 2", len(messages))
	}

	if messages[0].CloudEvents == nil {
		t.Fatal("the entry of the malformed message carries no cloudEvents verdict")
	}
	if messages[0].CloudEvents.Valid {
		t.Errorf("the malformed message reports valid true, want false: %v", messages[0].CloudEvents)
	}
	if messages[1].CloudEvents == nil || !messages[1].CloudEvents.Valid {
		t.Errorf("the conforming message reports %v, want a valid verdict", messages[1].CloudEvents)
	}
}

// TestBuffer_Query_NonJSONBody_BodyFiltersDoNotMatch holds SDD section 8.2. A
// filter that reads the body of a message that is not JSON must not match, and
// it must not stop the query from returning the other messages.
func TestBuffer_Query_NonJSONBody_BodyFiltersDoNotMatch(t *testing.T) {
	buf := NewBuffer(10)
	buf.Add(Message{Exchange: "a", Headers: map[string]string{}, Body: json.RawMessage("<xml>not an event</xml>")})
	buf.Add(Message{Exchange: "a", Headers: map[string]string{}, Body: json.RawMessage(conformingBody)})

	cases := []struct {
		name   string
		filter Filter
		want   int
	}{
		{"no filter returns both", Filter{}, 2},
		{"a jsonPath filter skips the malformed body", Filter{JSONPath: "$.id"}, 1},
		{"an eventType filter skips the malformed body", Filter{EventType: "ArticleSubmitted"}, 1},
		{"an eventType pattern skips the malformed body", Filter{EventTypePattern: "Article*"}, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(buf.Query(tc.filter)); got != tc.want {
				t.Errorf("Query returned %d messages, want %d", got, tc.want)
			}
			if got := buf.Count(tc.filter); got != tc.want {
				t.Errorf("Count returned %d, want %d", got, tc.want)
			}
		})
	}
}
