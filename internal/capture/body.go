package capture

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"
)

// BodyEncoding values tell a consumer how to read the body field of a captured
// entry.
const (
	// BodyEncodingJSON marks a body that is valid JSON. The body field holds
	// the payload itself.
	BodyEncodingJSON = "json"

	// BodyEncodingText marks a body that is not JSON but is valid UTF-8. The
	// body field holds the payload as a JSON string.
	BodyEncodingText = "text"

	// BodyEncodingBase64 marks a body that is neither JSON nor valid UTF-8.
	// The body field holds the standard base64 form of the payload bytes.
	BodyEncodingBase64 = "base64"
)

// wireMessage is the JSON form of the captured entry that SDD section 8.2
// defines, plus the bodyEncoding discriminator.
//
// The entry needs the discriminator because the service captures the AMQP body
// verbatim, and an application publishes a body that is not JSON. A
// json.RawMessage that holds such a body fails to marshal, and one failure
// empties the whole query response. The malformed event is then the one event
// the conformance check of SDD section 4.11 cannot see.
type wireMessage struct {
	Seq          int64             `json:"seq"`
	ReceivedAt   time.Time         `json:"receivedAt"`
	Exchange     string            `json:"exchange"`
	RoutingKey   string            `json:"routingKey"`
	Headers      map[string]string `json:"headers"`
	RunID        string            `json:"runId"`
	CloudEvents  *CloudEventsInfo  `json:"cloudEvents"`
	BodyEncoding string            `json:"bodyEncoding"`
	Body         json.RawMessage   `json:"body"`
}

// MarshalJSON writes the captured entry of SDD section 8.2. It keeps a JSON
// body as JSON, and it writes any other body as a JSON string. The
// bodyEncoding field tells the two apart. No byte of the payload is lost,
// because a developer needs the bytes to debug the event.
func (m Message) MarshalJSON() ([]byte, error) {
	encoding, body, err := encodeBody(m.Body)
	if err != nil {
		return nil, err
	}
	return json.Marshal(wireMessage{
		Seq:          m.Seq,
		ReceivedAt:   m.ReceivedAt,
		Exchange:     m.Exchange,
		RoutingKey:   m.RoutingKey,
		Headers:      m.Headers,
		RunID:        m.RunID,
		CloudEvents:  m.CloudEvents,
		BodyEncoding: encoding,
		Body:         body,
	})
}

// UnmarshalJSON reads a captured entry back into a Message. It restores the
// verbatim payload bytes, so a filter that reads the body gets the same bytes
// the broker delivered.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w wireMessage
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("decode captured entry: %w", err)
	}
	body, err := decodeBody(w.BodyEncoding, w.Body)
	if err != nil {
		return err
	}
	*m = Message{
		Seq:         w.Seq,
		ReceivedAt:  w.ReceivedAt,
		Exchange:    w.Exchange,
		RoutingKey:  w.RoutingKey,
		Headers:     w.Headers,
		RunID:       w.RunID,
		CloudEvents: w.CloudEvents,
		Body:        body,
	}
	return nil
}

// encodeBody returns the encoding of a captured body and its JSON form.
func encodeBody(raw json.RawMessage) (string, json.RawMessage, error) {
	if json.Valid(raw) {
		return BodyEncodingJSON, raw, nil
	}

	encoding, text := BodyEncodingBase64, base64.StdEncoding.EncodeToString(raw)
	if utf8.Valid(raw) {
		encoding, text = BodyEncodingText, string(raw)
	}

	encoded, err := json.Marshal(text)
	if err != nil {
		return "", nil, fmt.Errorf("encode captured body as %s: %w", encoding, err)
	}
	return encoding, encoded, nil
}

// decodeBody returns the verbatim payload bytes of a captured entry.
func decodeBody(encoding string, raw json.RawMessage) (json.RawMessage, error) {
	if encoding != BodyEncodingText && encoding != BodyEncodingBase64 {
		return raw, nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("decode captured body of encoding %s: %w", encoding, err)
	}
	if encoding == BodyEncodingText {
		return json.RawMessage(text), nil
	}

	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("decode captured body of encoding %s: %w", encoding, err)
	}
	return decoded, nil
}
