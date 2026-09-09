package diagnostics

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// BundleConfig holds the parameters for writing a diagnostic bundle.
type BundleConfig struct {
	BundleDir string
	RunID     string
	Scenario  string
	Config    map[string]any
	Steps     []map[string]any
	WireMock  *WireMockDiag
	RabbitMQ  *RabbitMQDiag
	Captured  []map[string]any
	Debezium  map[string]any
	AppLogs   string
}

// WireMockDiag holds WireMock diagnostic data.
type WireMockDiag struct {
	Unmatched  any
	NearMisses any
	Journal    any
}

// RabbitMQDiag holds RabbitMQ diagnostic data.
type RabbitMQDiag struct {
	Queues   any
	Topology any
}

// WriteBundle creates the diagnostic bundle directory structure.
// WriteBundle applies secret redaction before writing.
func WriteBundle(bc BundleConfig) error {
	runDir := filepath.Join(bc.BundleDir, bc.RunID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("create bundle directory %s: %w", runDir, err)
	}

	if bc.Config != nil {
		redacted := Redact(bc.Config)
		if err := writeJSON(filepath.Join(runDir, "run.json"), redacted); err != nil {
			return fmt.Errorf("write run.json: %w", err)
		}
	}

	if bc.Scenario != "" {
		if err := os.WriteFile(filepath.Join(runDir, "scenario.yaml"), []byte(bc.Scenario), 0o644); err != nil {
			return fmt.Errorf("write scenario.yaml: %w", err)
		}
	}

	if len(bc.Steps) > 0 {
		redactedSteps := make([]map[string]any, len(bc.Steps))
		for i, s := range bc.Steps {
			redactedSteps[i] = Redact(s)
		}
		if err := writeJSONLines(filepath.Join(runDir, "steps.jsonl"), redactedSteps); err != nil {
			return fmt.Errorf("write steps.jsonl: %w", err)
		}
	}

	if bc.WireMock != nil {
		wmDir := filepath.Join(runDir, "wiremock")
		if err := os.MkdirAll(wmDir, 0o755); err != nil {
			return fmt.Errorf("create wiremock directory: %w", err)
		}
		if bc.WireMock.Unmatched != nil {
			if err := writeJSON(filepath.Join(wmDir, "unmatched.json"), bc.WireMock.Unmatched); err != nil {
				return fmt.Errorf("write wiremock/unmatched.json: %w", err)
			}
		}
		if bc.WireMock.NearMisses != nil {
			if err := writeJSON(filepath.Join(wmDir, "near-misses.json"), bc.WireMock.NearMisses); err != nil {
				return fmt.Errorf("write wiremock/near-misses.json: %w", err)
			}
		}
		if bc.WireMock.Journal != nil {
			if err := writeJSON(filepath.Join(wmDir, "journal.json"), redactValue(bc.WireMock.Journal)); err != nil {
				return fmt.Errorf("write wiremock/journal.json: %w", err)
			}
		}
	}

	if bc.RabbitMQ != nil {
		rmqDir := filepath.Join(runDir, "rabbitmq")
		if err := os.MkdirAll(rmqDir, 0o755); err != nil {
			return fmt.Errorf("create rabbitmq directory: %w", err)
		}
		if bc.RabbitMQ.Queues != nil {
			if err := writeJSON(filepath.Join(rmqDir, "queues.json"), bc.RabbitMQ.Queues); err != nil {
				return fmt.Errorf("write rabbitmq/queues.json: %w", err)
			}
		}
		if bc.RabbitMQ.Topology != nil {
			if err := writeJSON(filepath.Join(rmqDir, "topology.json"), bc.RabbitMQ.Topology); err != nil {
				return fmt.Errorf("write rabbitmq/topology.json: %w", err)
			}
		}
	}

	if len(bc.Captured) > 0 {
		if err := writeJSONLines(filepath.Join(runDir, "captured.jsonl"), bc.Captured); err != nil {
			return fmt.Errorf("write captured.jsonl: %w", err)
		}
	}

	if bc.Debezium != nil {
		dbzDir := filepath.Join(runDir, "debezium")
		if err := os.MkdirAll(dbzDir, 0o755); err != nil {
			return fmt.Errorf("create debezium directory: %w", err)
		}
		if err := writeJSON(filepath.Join(dbzDir, "connectors.json"), bc.Debezium); err != nil {
			return fmt.Errorf("write debezium/connectors.json: %w", err)
		}
	}

	if bc.AppLogs != "" {
		appDir := filepath.Join(runDir, "app")
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			return fmt.Errorf("create app directory: %w", err)
		}
		if err := os.WriteFile(filepath.Join(appDir, "logs.txt"), []byte(bc.AppLogs), 0o644); err != nil {
			return fmt.Errorf("write app/logs.txt: %w", err)
		}
	}

	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON for %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func writeJSONLines(path string, items []map[string]any) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, item := range items {
		if err := enc.Encode(item); err != nil {
			return fmt.Errorf("encode line in %s: %w", filepath.Base(path), err)
		}
	}
	return nil
}
