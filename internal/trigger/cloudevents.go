package trigger

import "fmt"

// ValidateCloudEvents checks that the event object in a publish request
// contains the required CloudEvents 1.0 attributes.
// Required: specversion (must be "1.0"), id, type, source, version.
// Optional: subject, time, data.
func ValidateCloudEvents(event map[string]any) error {
	if err := requireStringAttr(event, "specversion"); err != nil {
		return err
	}
	sv, _ := event["specversion"].(string)
	if sv != "1.0" {
		return fmt.Errorf("cloud event attribute %q must be %q", "specversion", "1.0")
	}

	if err := requireStringAttr(event, "id"); err != nil {
		return err
	}

	if err := requireStringAttr(event, "type"); err != nil {
		return err
	}

	if err := requireStringAttr(event, "source"); err != nil {
		return err
	}

	if err := requireStringAttr(event, "version"); err != nil {
		return err
	}

	return nil
}

func requireStringAttr(event map[string]any, name string) error {
	v, ok := event[name]
	if !ok {
		return fmt.Errorf("cloud event missing required attribute %q", name)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return fmt.Errorf("cloud event attribute %q must be a non-empty string", name)
	}
	return nil
}
