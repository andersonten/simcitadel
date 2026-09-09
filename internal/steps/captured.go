package steps

import (
	"fmt"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// capturedQueryFromStep turns the selectors of an event/captured step into the
// capture query of SDD section 8.2. Every selector the step writes goes into the
// query, because one left behind widens the query and the assertion then reports
// on something the author did not write.
//
// A selector arrives either on the step or under captured:, because validate
// accepts both shapes, so both are read here and a step level selector wins over
// the same selector under captured:. This is the rule buildNotCapturedParams
// applies to the negative form of the same assertion.
func capturedQueryFromStep(step *scenario.Step) *scenario.CapturedQuery {
	base := step.Captured
	if base == nil {
		base = &scenario.CapturedQuery{}
	}

	query := &scenario.CapturedQuery{
		Exchange:          firstNonEmpty(step.Exchange, base.Exchange),
		RoutingKey:        firstNonEmpty(step.RoutingKey, base.RoutingKey),
		RoutingKeyPattern: base.RoutingKeyPattern,
		EventType:         firstNonEmpty(step.EventType, base.EventType),
		EventTypePattern:  firstNonEmpty(step.EventTypePattern, base.EventTypePattern),
		RunID:             base.RunID,
		JSONPath:          base.JSONPath,
	}

	if len(base.Headers)+len(step.HeaderFilter) > 0 {
		query.Headers = make(map[string]string, len(base.Headers)+len(step.HeaderFilter))
		for k, v := range base.Headers {
			query.Headers[k] = v
		}
		for k, v := range step.HeaderFilter {
			query.Headers[k] = v
		}
	}
	return query
}

// capturedBinding returns the message an event/captured step binds. SDD section
// 6.4 makes the binding of a receiving step the matched message, with
// receivedAt in place of sentAt. More than one match binds the first, and
// validate reports the assertion as ambiguous.
//
// capturedBinding returns nil when the probe matched nothing, so a step that
// asserts count: 0 binds nothing rather than an empty message.
func capturedBinding(pr *ProbeResult) *scenario.MessageRef {
	if pr == nil {
		return nil
	}
	matches, ok := pr.Response.([]any)
	if !ok || len(matches) == 0 {
		return nil
	}
	entry, ok := matches[0].(map[string]any)
	if !ok {
		return nil
	}

	ref := &scenario.MessageRef{
		Exchange:   stringField(entry, "exchange"),
		RoutingKey: stringField(entry, "routingKey"),
		Body:       entry["body"],
		Headers:    stringMap(entry["headers"]),
		ReceivedAt: stringField(entry, "receivedAt"),
	}

	// The capture entry of SDD section 8.2 holds the CloudEvents id inside the
	// envelope under body. A capture service that reports its own id wins.
	ref.ID = stringField(entry, "id")
	if ref.ID == "" {
		if body, isMap := entry["body"].(map[string]any); isMap {
			ref.ID = stringField(body, "id")
		}
	}
	return ref
}

// stringField returns one field of a decoded JSON object as a string.
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// stringMap converts the headers of a capture entry to the map shape a template
// reads. A header value that is not a string is formatted, because a header
// carries an integer as well, for example citadeldepth.
func stringMap(v any) map[string]string {
	source, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(source))
	for k, item := range source {
		if s, isString := item.(string); isString {
			out[k] = s
			continue
		}
		out[k] = fmt.Sprintf("%v", item)
	}
	return out
}
