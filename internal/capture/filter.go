package capture

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/andersonten/simcitadel/internal/expect"
)

// Filter holds the criteria for querying captured messages.
type Filter struct {
	Exchange          string
	RoutingKey        string
	RoutingKeyPattern string
	EventType         string
	EventTypePattern  string
	Since             time.Time
	RunID             string
	JSONPath          string
	Headers           map[string]string
}

// ParseFilter extracts a Filter from HTTP query parameters.
func ParseFilter(r *http.Request) Filter {
	f := Filter{
		Exchange:          r.URL.Query().Get("exchange"),
		RoutingKey:        r.URL.Query().Get("routingKey"),
		RoutingKeyPattern: r.URL.Query().Get("routingKeyPattern"),
		EventType:         r.URL.Query().Get("eventType"),
		EventTypePattern:  r.URL.Query().Get("eventTypePattern"),
		RunID:             r.URL.Query().Get("runId"),
		JSONPath:          r.URL.Query().Get("jsonPath"),
	}

	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			f.Since = t
		}
	}

	f.Headers = make(map[string]string)
	for key, vals := range r.URL.Query() {
		if strings.HasPrefix(key, "header.") && len(vals) > 0 {
			headerName := strings.TrimPrefix(key, "header.")
			f.Headers[headerName] = vals[0]
		}
	}

	return f
}

// Match returns true if the message matches all non-empty filter criteria.
func (f Filter) Match(msg Message) bool {
	if f.Exchange != "" && msg.Exchange != f.Exchange {
		return false
	}
	if f.RoutingKey != "" && msg.RoutingKey != f.RoutingKey {
		return false
	}
	if f.RoutingKeyPattern != "" && !matchAMQPTopic(f.RoutingKeyPattern, msg.RoutingKey) {
		return false
	}
	if f.EventType != "" && resolveEventType(msg) != f.EventType {
		return false
	}
	if f.EventTypePattern != "" && !matchGlob(f.EventTypePattern, resolveEventType(msg)) {
		return false
	}
	if !f.Since.IsZero() && msg.ReceivedAt.Before(f.Since) {
		return false
	}
	// A message that carries no run ID matches a run-scoped query. The application
	// under test produces the outbound half of the pipeline through its outbox, and
	// it knows nothing about Citadel, so it stamps no run ID. Rejecting an unstamped
	// message hid every event the application produced.
	//
	// This does not weaken the isolation that SDD section 4.3 asks for. Section 4.8
	// holds an exclusive lock, so one run uses an environment at a time, and section
	// 4.9 clears the capture buffer before every scenario, so an unstamped message
	// in the buffer arrived during this run. A message stamped with a different run
	// is still rejected below.
	if f.RunID != "" && msg.RunID != "" && msg.RunID != f.RunID {
		return false
	}
	// A header selector needs the header to be there. SDD section 10.1 makes a
	// header value JSON encoded, so an empty JSON string is a value a message
	// carries, and a map read of an absent key gives the same empty string. A
	// comparison of the value alone therefore matched every message in the buffer
	// for the selector header.X= and turned a mandatory selector of section 8.3
	// into no selector at all.
	for k, v := range f.Headers {
		got, ok := msg.Headers[k]
		if !ok || got != v {
			return false
		}
	}
	if f.JSONPath != "" {
		var body any
		if err := json.Unmarshal(msg.Body, &body); err != nil {
			return false
		}
		values, err := expect.EvalJSONPath(body, f.JSONPath)
		if err != nil || len(values) == 0 {
			return false
		}
	}
	return true
}

// resolveEventType returns the event type for a message. It checks the eventType
// header first, then falls back to the CloudEvents type attribute in the body.
func resolveEventType(msg Message) string {
	if v, ok := msg.Headers["eventType"]; ok && v != "" {
		return v
	}
	if len(msg.Body) == 0 {
		return ""
	}
	var body struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(msg.Body, &body); err == nil {
		return body.Type
	}
	return ""
}

// matchAMQPTopic matches a routing key against an AMQP topic pattern.
// "*" matches exactly one dot-separated word. "#" matches zero or more words.
func matchAMQPTopic(pattern, key string) bool {
	patternParts := strings.Split(pattern, ".")
	keyParts := strings.Split(key, ".")
	return amqpTopicMatch(patternParts, keyParts)
}

func amqpTopicMatch(pattern, key []string) bool {
	for len(pattern) > 0 {
		p := pattern[0]
		pattern = pattern[1:]

		if p == "#" {
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(key); i++ {
				if amqpTopicMatch(pattern, key[i:]) {
					return true
				}
			}
			return false
		}

		if len(key) == 0 {
			return false
		}

		if p != "*" && p != key[0] {
			return false
		}

		key = key[1:]
	}
	return len(key) == 0
}

// matchGlob matches a string against a glob pattern where "*" matches any
// sequence of characters.
func matchGlob(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}

	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]

	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}

	return strings.HasSuffix(s, parts[len(parts)-1])
}
