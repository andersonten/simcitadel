//go:build integration

package integration

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/andersonten/simcitadel/internal/config"
	citadeldb "github.com/andersonten/simcitadel/internal/db"
	"github.com/andersonten/simcitadel/internal/runner"
)

// postgresImage matches the digest pinned in deploy/compose/docker-compose.yml.
const postgresImage = "postgres:15@sha256:5f72c7b5bd616308ccfd2e74d6be16fb06364e5eecbb815fe9dc6ab9761d2111"

// startPostgres starts a Postgres container and returns its DSN.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("citadel"),
		tcpostgres.WithUsername("citadel"),
		tcpostgres.WithPassword("citadel"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort("5432/tcp").WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("start Postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate Postgres container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("read Postgres connection string: %v", err)
	}
	return dsn
}

// TestOpenDatabases_ConfiguredTarget_IsConnectedAndUsable guards the defect
// where no SQL driver was registered anywhere in the module, so every
// sql.Open call failed with "unknown driver".
func TestOpenDatabases_ConfiguredTarget_IsConnectedAndUsable(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()

	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "integration"},
		Targets: config.TargetsConfig{
			AppDB: &config.DatabaseTarget{Driver: "postgres", DSN: dsn},
		},
	}

	databases, closeAll, err := runner.OpenDatabases(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenDatabases returned error: %v", err)
	}
	defer closeAll()

	handle, ok := databases["app-db"]
	if !ok || handle == nil {
		t.Fatalf("app-db is not present in the connection map; keys=%v", keysOf(databases))
	}

	var got int
	if err := handle.QueryRowContext(ctx, "SELECT 1").Scan(&got); err != nil {
		t.Fatalf("query the connected database: %v", err)
	}
	if got != 1 {
		t.Errorf("SELECT 1 returned %d, want 1", got)
	}
}

// TestOpenDatabases_UnreachableTarget_IsAnEnvironmentFailure proves a database
// that does not answer produces exit code 3 rather than a silent skip.
func TestOpenDatabases_UnreachableTarget_IsAnEnvironmentFailure(t *testing.T) {
	cfg := &config.CitadelConfig{
		Version:     1,
		Environment: config.EnvironmentConfig{Name: "integration"},
		Targets: config.TargetsConfig{
			AppDB: &config.DatabaseTarget{
				Driver: "postgres",
				// Port 1 never listens.
				DSN: "postgres://citadel:citadel@127.0.0.1:1/citadel?sslmode=disable&connect_timeout=2",
			},
		},
	}

	_, closeAll, err := runner.OpenDatabases(context.Background(), cfg)
	defer closeAll()

	if err == nil {
		t.Fatal("OpenDatabases against an unreachable database returned no error, want one")
	}
	if got := runner.ClassifyError(err); got != runner.ExitEnvironment {
		t.Errorf("ClassifyError = %d, want %d (error: %v)", got, runner.ExitEnvironment, err)
	}
}

// TestResetTruncate_EmptiesTheNamedTables covers the truncation half of the
// reset sequence of SDD section 4.9.
func TestResetTruncate_EmptiesTheNamedTables(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()

	handle, err := citadeldb.Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if _, err := handle.ExecContext(ctx,
		`CREATE TABLE inbox (message_id text primary key, processed_at timestamptz, retry_count int, last_error text)`,
	); err != nil {
		t.Fatalf("create inbox table: %v", err)
	}
	if _, err := handle.ExecContext(ctx,
		`INSERT INTO inbox (message_id, retry_count) VALUES ('m1', 0), ('m2', 1)`,
	); err != nil {
		t.Fatalf("seed inbox table: %v", err)
	}

	if err := runner.ResetTruncate(ctx, map[string]*sql.DB{"app-db": handle}, []string{"inbox"}); err != nil {
		t.Fatalf("ResetTruncate returned error: %v", err)
	}

	var count int
	if err := handle.QueryRowContext(ctx, "SELECT count(*) FROM inbox").Scan(&count); err != nil {
		t.Fatalf("count inbox rows: %v", err)
	}
	if count != 0 {
		t.Errorf("inbox holds %d rows after truncation, want 0", count)
	}
}

// TestResetTruncate_WithoutAppDB_IsAnEnvironmentFailure guards the defect
// where a missing connection made truncation a silent no-op, so a run
// proceeded against stale rows.
func TestResetTruncate_WithoutAppDB_IsAnEnvironmentFailure(t *testing.T) {
	err := runner.ResetTruncate(context.Background(), nil, []string{"inbox"})
	if err == nil {
		t.Fatal("ResetTruncate without a connected app-db returned no error, want one")
	}
	if got := runner.ClassifyError(err); got != runner.ExitEnvironment {
		t.Errorf("ClassifyError = %d, want %d (error: %v)", got, runner.ExitEnvironment, err)
	}
}

// TestResetTruncate_RejectsANonIdentifierTableName proves a TRUNCATE target is
// checked, because it cannot be a bound parameter.
func TestResetTruncate_NonIdentifierTableName_IsRejected(t *testing.T) {
	dsn := startPostgres(t)
	ctx := context.Background()

	handle, err := citadeldb.Open(ctx, "postgres", dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	err = runner.ResetTruncate(ctx, map[string]*sql.DB{"app-db": handle}, []string{"inbox; DROP TABLE inbox"})
	if err == nil {
		t.Fatal("ResetTruncate accepted a table name that is not an identifier, want an error")
	}
	if got := runner.ClassifyError(err); got != runner.ExitInvalidInput {
		t.Errorf("ClassifyError = %d, want %d (error: %v)", got, runner.ExitInvalidInput, err)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
