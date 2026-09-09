package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// ExecutePublish publishes a CloudEvents message via the event trigger service.
// When the step carries an as: field, ExecutePublish returns a MessageRef binding.
func ExecutePublish(ctx context.Context, step *scenario.Step, rc *RunContext) (*scenario.MessageRef, error) {
	if rc.Config.Targets.EventTrigger == nil {
		return nil, fmt.Errorf("publish: event-trigger target is not configured")
	}
	triggerURL := rc.Config.Targets.EventTrigger.URL

	data, err := resolvePublishData(step, rc)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"exchange":   step.Exchange,
		"routingKey": step.RoutingKey,
		"event":      data,
	}
	if step.Delivery != "" {
		payload["delivery"] = step.Delivery
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("publish marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, triggerURL+"/publish", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("publish create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Citadel-Run", rc.RunID)

	resp, err := rc.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("publish request: %w", err)
	}
	defer resp.Body.Close()

	var triggerResp struct {
		EventID string `json:"eventId"`
	}
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("publish: event-trigger returned status %d", resp.StatusCode)
	}

	_ = json.Unmarshal(respBody, &triggerResp)

	eventMap, _ := data.(map[string]any)
	headers := make(map[string]string)
	if eventMap != nil {
		for k, v := range eventMap {
			if s, ok := v.(string); ok && k != "data" {
				headers[k] = s
			}
		}
	}

	ref := &scenario.MessageRef{
		ID:         triggerResp.EventID,
		Exchange:   step.Exchange,
		RoutingKey: step.RoutingKey,
		Body:       data,
		Headers:    headers,
		SentAt:     time.Now().UTC().Format(time.RFC3339),
	}
	return ref, nil
}

func resolvePublishData(step *scenario.Step, rc *RunContext) (any, error) {
	if step.DataFile != "" {
		path := rc.Config.PayloadPath(step.DataFile)
		rendered, err := readRenderedContent(path, rc)
		if err != nil {
			return nil, fmt.Errorf("publish data file: %w", err)
		}
		var data any
		if err := json.Unmarshal([]byte(rendered), &data); err != nil {
			return nil, fmt.Errorf("publish parse data file %s: %w", path, err)
		}
		return data, nil
	}
	return step.Data, nil
}
