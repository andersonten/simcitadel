package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/andersonten/simcitadel/internal/scenario"
)

// ExecuteSetState sets a WireMock scenario to the specified state.
func ExecuteSetState(ctx context.Context, step *scenario.Step, rc *RunContext) error {
	if rc.Config.Targets.WireMock == nil {
		return fmt.Errorf("wiremock set-state: wiremock target is not configured")
	}
	adminURL := rc.Config.Targets.WireMock.AdminBase()

	body := map[string]string{"state": step.State}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("wiremock set-state marshal body: %w", err)
	}

	reqURL := adminURL + "/scenarios/" + step.ScenarioName + "/state"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("wiremock set-state create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := rc.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("wiremock set-state request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("wiremock set-state: returned status %d", resp.StatusCode)
	}

	return nil
}
