package capture

import "encoding/json"

// CloudEventsInfo holds CloudEvents validation results for a captured message.
type CloudEventsInfo struct {
	Valid      bool     `json:"valid"`
	Violations []string `json:"violations"`
}

// ValidateCloudEvents checks a captured message body against CloudEvents 1.0.
// It returns a CloudEventsInfo with validity and any violations.
func ValidateCloudEvents(body json.RawMessage) *CloudEventsInfo {
	info := &CloudEventsInfo{Valid: true}

	if len(body) == 0 {
		info.Valid = false
		info.Violations = append(info.Violations, "body is empty")
		return info
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(body, &parsed); err != nil {
		info.Valid = false
		info.Violations = append(info.Violations, "body is not valid JSON object")
		return info
	}

	requireString(parsed, "specversion", info)
	requireString(parsed, "type", info)
	requireString(parsed, "source", info)
	requireString(parsed, "id", info)
	requireString(parsed, "version", info)

	if raw, ok := parsed["specversion"]; ok {
		var sv string
		if json.Unmarshal(raw, &sv) == nil && sv != "1.0" {
			info.Valid = false
			info.Violations = append(info.Violations, "specversion must be \"1.0\"")
		}
	}

	return info
}

func requireString(m map[string]json.RawMessage, attr string, info *CloudEventsInfo) {
	raw, ok := m[attr]
	if !ok {
		info.Valid = false
		info.Violations = append(info.Violations, "missing required attribute: "+attr)
		return
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		info.Valid = false
		info.Violations = append(info.Violations, attr+" must be a string")
		return
	}
	if s == "" {
		info.Valid = false
		info.Violations = append(info.Violations, attr+" must not be empty")
	}
}
