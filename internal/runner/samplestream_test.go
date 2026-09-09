package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The sample stream of SDD section 4.6 is the only description of the JSON output
// that a consumer reads before writing code against it. It listed five event types
// and the emitter produced six, so check.fail was undocumented, and check.fail is
// the event that explains a run which ends non-zero with every step passing.
//
// A sample is documentation, and documentation drifts. This compares the two lists
// rather than trusting either, so a new event type fails this test until the sample
// carries it.

// emitterTypePattern finds the type literal of each event the emitter writes.
var emitterTypePattern = regexp.MustCompile(`"type":\s*"([a-z]+\.[a-z]+)"`)

// sampleBlockPattern finds the fenced JSON block of section 4.6. The block is the
// one that follows the --output json paragraph, and it is the only json fence in
// the section.
var sampleBlockPattern = regexp.MustCompile("(?s)### 4\\.6 Machine-readable output.*?```json\n(.*?)```")

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestSampleStream_EveryEmittedEventType_IsDocumented is the drift guard.
func TestSampleStream_EveryEmittedEventType_IsDocumented(t *testing.T) {
	emitted := emittedEventTypes(t)
	documented := documentedEventTypes(t)

	for _, eventType := range emitted {
		if !documented[eventType] {
			t.Errorf("the emitter writes %q and the sample stream of SDD section 4.6 does not show it, "+
				"so a consumer of --output json has no way to know it exists", eventType)
		}
	}
}

// TestSampleStream_EveryDocumentedEventType_IsEmitted closes the other direction.
// A sample that shows an event the emitter never writes sends a consumer looking
// for something that never arrives.
func TestSampleStream_EveryDocumentedEventType_IsEmitted(t *testing.T) {
	emitted := make(map[string]bool)
	for _, eventType := range emittedEventTypes(t) {
		emitted[eventType] = true
	}

	for eventType := range documentedEventTypes(t) {
		if !emitted[eventType] {
			t.Errorf("the sample stream of SDD section 4.6 shows %q and the emitter never writes it", eventType)
		}
	}
}

// TestSampleStream_EveryLine_IsValidJSON keeps the sample usable. It is
// newline-delimited JSON, and a consumer that pastes a line into a decoder to
// build a type against it must get a parse.
func TestSampleStream_EveryLine_IsValidJSON(t *testing.T) {
	for _, line := range sampleLines(t) {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Errorf("a sample line does not parse as JSON: %v\n%s", err, line)
			continue
		}
		for _, required := range []string{"ts", "runId", "type"} {
			if _, ok := parsed[required]; !ok {
				t.Errorf("a sample line carries no %q field: %s", required, line)
			}
		}
	}
}

// emittedEventTypes reads the type literals out of the emitter.
func emittedEventTypes(t *testing.T) []string {
	t.Helper()
	source := repoFile(t, filepath.Join("internal", "runner", "output.go"))

	seen := make(map[string]bool)
	for _, match := range emitterTypePattern.FindAllStringSubmatch(source, -1) {
		seen[match[1]] = true
	}
	if len(seen) == 0 {
		t.Fatal("no event type was found in the emitter, so this test proves nothing")
	}

	types := make([]string, 0, len(seen))
	for eventType := range seen {
		types = append(types, eventType)
	}
	sort.Strings(types)
	return types
}

// documentedEventTypes reads the type field of every line of the sample stream.
func documentedEventTypes(t *testing.T) map[string]bool {
	t.Helper()

	documented := make(map[string]bool)
	for _, line := range sampleLines(t) {
		var parsed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		if parsed.Type != "" {
			documented[parsed.Type] = true
		}
	}
	if len(documented) == 0 {
		t.Fatal("no event type was found in the sample stream, so this test proves nothing")
	}
	return documented
}

// sampleLines returns the non-empty lines of the section 4.6 sample block.
func sampleLines(t *testing.T) []string {
	t.Helper()
	sdd := repoFile(t, filepath.Join("docs", "sdd.md"))

	match := sampleBlockPattern.FindStringSubmatch(sdd)
	if match == nil {
		t.Fatal("the sample stream block of SDD section 4.6 was not found, so this test proves nothing")
	}

	var lines []string
	for _, line := range strings.Split(match[1], "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
