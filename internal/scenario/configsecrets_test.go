package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersonten/simcitadel/internal/config"
)

// writeConfig writes a citadel.yaml and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "citadel.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestValidateConfigSecrets_LoadedConfig guards the defect where the check ran
// after ${VAR} interpolation. At that point every correct reference already
// held its value, so a config that followed SDD section 5.3 was reported as
// holding a literal secret.
func TestValidateConfigSecrets_LoadedConfig_JudgesTheFileAsWritten(t *testing.T) {
	t.Setenv("CITADEL_TEST_DB_PASSWORD", "s3cr3t-value")
	t.Setenv("CITADEL_TEST_CLIENT_SECRET", "another-s3cr3t")

	cases := []struct {
		name        string
		body        string
		wantErrOn   string
		wantNoError bool
	}{
		{
			name: "dsn uses a variable reference",
			body: `version: 1
environment:
  name: local
targets:
  app-db:
    driver: postgres
    dsn: "postgres://citadel:${CITADEL_TEST_DB_PASSWORD}@localhost:5432/citadel"
`,
			wantNoError: true,
		},
		{
			name: "dsn holds a literal password",
			body: `version: 1
environment:
  name: local
targets:
  app-db:
    driver: postgres
    dsn: "postgres://citadel:hunter2@localhost:5432/citadel"
`,
			wantErrOn: "app-db",
		},
		{
			name: "client secret uses a variable reference",
			body: `version: 1
environment:
  name: local
auth:
  default:
    tokenUrl: "https://auth.example.com/token"
    clientId: "citadel"
    clientSecret: "${CITADEL_TEST_CLIENT_SECRET}"
`,
			wantNoError: true,
		},
		{
			name: "client secret holds a literal value",
			body: `version: 1
environment:
  name: local
auth:
  default:
    tokenUrl: "https://auth.example.com/token"
    clientId: "citadel"
    clientSecret: "hunter2"
`,
			wantErrOn: "clientSecret",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tc.body))
			if err != nil {
				t.Fatalf("config.Load returned error: %v", err)
			}

			errs := ValidateConfigSecrets(cfg)

			if tc.wantNoError {
				if len(errs) > 0 {
					t.Fatalf("ValidateConfigSecrets reported %d error(s) for a config that uses a ${VAR} reference: %v", len(errs), errs)
				}
				return
			}

			found := false
			for _, e := range errs {
				if strings.Contains(e.Error(), tc.wantErrOn) {
					found = true
				}
			}
			if !found {
				t.Fatalf("ValidateConfigSecrets did not report %q; errors: %v", tc.wantErrOn, errs)
			}
		})
	}
}

// TestValidateConfigSecrets_LoadedConfig_DoesNotLeakTheSecret confirms an
// error message names the field and not the value.
func TestValidateConfigSecrets_LiteralSecret_IsNotEchoedInTheError(t *testing.T) {
	const literal = "hunter2-do-not-echo"

	cfg, err := config.Load(writeConfig(t, `version: 1
environment:
  name: local
targets:
  app-db:
    driver: postgres
    dsn: "postgres://citadel:`+literal+`@localhost:5432/citadel"
`))
	if err != nil {
		t.Fatalf("config.Load returned error: %v", err)
	}

	for _, e := range ValidateConfigSecrets(cfg) {
		if strings.Contains(e.Error(), literal) {
			t.Errorf("error message echoes the secret: %v", e)
		}
	}
}
