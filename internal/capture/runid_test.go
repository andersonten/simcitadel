package capture

import (
	"encoding/json"
	"testing"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// TestExtractRunID guards the defect where the capture service read the run ID
// from a header named "runId". The event trigger stamps "citadelrun", per SDD
// section 4.3, so every captured message carried an empty run ID and the
// runId filter matched nothing.
func TestExtractRunID_Sources_ReturnTheStampedRunID(t *testing.T) {
	const runID = "0191c3aa-0000-7000-8000-000000000001"

	cases := []struct {
		name    string
		headers map[string]string
		body    string
		want    string
	}{
		{
			name:    "header stamped by the event trigger",
			headers: map[string]string{"citadelrun": runID},
			body:    `{"id":"e1","type":"ArticleSubmitted"}`,
			want:    runID,
		},
		{
			name:    "CloudEvents extension attribute in the body",
			headers: map[string]string{},
			body:    `{"id":"e1","type":"ArticleSubmitted","citadelrun":"` + runID + `"}`,
			want:    runID,
		},
		{
			name:    "header wins over the body",
			headers: map[string]string{"citadelrun": runID},
			body:    `{"id":"e1","citadelrun":"other"}`,
			want:    runID,
		},
		{
			name:    "no run ID anywhere",
			headers: map[string]string{"eventType": "ArticleSubmitted"},
			body:    `{"id":"e1"}`,
			want:    "",
		},
		{
			name:    "body is not JSON",
			headers: map[string]string{},
			body:    `not json at all`,
			want:    "",
		},
		{
			name:    "body run ID is not a string",
			headers: map[string]string{},
			body:    `{"citadelrun":42}`,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRunID(tc.headers, json.RawMessage(tc.body))
			if got != tc.want {
				t.Errorf("extractRunID() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestExtractRunID_TriggerHeaderTable checks the exact header table the event
// trigger builds, after the AMQP table is flattened to strings.
func TestExtractRunID_FlattenedTriggerHeaders_ReturnRunID(t *testing.T) {
	const runID = "0191c3aa-0000-7000-8000-000000000002"

	table := amqpdriver.Table{
		"citadeldepth": int32(1),
		"citadelrun":   runID,
		"eventType":    "ArticleSubmitted",
	}

	headers := flattenHeaders(table)
	if got := extractRunID(headers, json.RawMessage(`{}`)); got != runID {
		t.Errorf("extractRunID() = %q, want %q", got, runID)
	}
}
