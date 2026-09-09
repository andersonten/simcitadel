package db

import (
	"database/sql"
	"slices"
	"testing"
)

func TestNormalizeDriver_ConfigNames_MapToRegisteredDriver(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"postgres as written in citadel.yaml", "postgres", pgxDriver},
		{"postgresql spelling", "postgresql", pgxDriver},
		{"pgx passthrough", "pgx", pgxDriver},
		{"mixed case", "PostgreS", pgxDriver},
		{"surrounding space", "  postgres  ", pgxDriver},
		{"sqlserver as written in citadel.yaml", "sqlserver", sqlServerDriver},
		{"mssql spelling", "mssql", sqlServerDriver},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDriver(tc.input)
			if err != nil {
				t.Fatalf("NormalizeDriver(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeDriver(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestNormalizeDriver_UnsupportedName_ReturnsError keeps every name that
// Citadel does not support behind an error. SDD section 10.2 names Postgres and
// SQL Server, so "sqlserver" left this list when the SQL Server driver arrived.
func TestNormalizeDriver_UnsupportedName_ReturnsError(t *testing.T) {
	cases := []string{"", "mysql", "sqlite", "oracle"}
	for _, input := range cases {
		if _, err := NormalizeDriver(input); err == nil {
			t.Errorf("NormalizeDriver(%q) returned no error, want one", input)
		}
	}
}

// TestDialectOf_SupportedDrivers_MapToTheirPlaceholderSyntax holds the dialect
// difference that SDD section 10.2 creates. A statement that Citadel builds
// carries $1 for Postgres and @p1 for SQL Server.
func TestDialectOf_SupportedDrivers_MapToTheirPlaceholderSyntax(t *testing.T) {
	cases := []struct {
		driver      string
		dialect     Dialect
		placeholder string
	}{
		{"postgres", DialectPostgres, "$1"},
		{"pgx", DialectPostgres, "$1"},
		{"sqlserver", DialectSQLServer, "@p1"},
		{"mssql", DialectSQLServer, "@p1"},
	}

	for _, tc := range cases {
		got, err := DialectOf(tc.driver)
		if err != nil {
			t.Fatalf("DialectOf(%q) returned error: %v", tc.driver, err)
		}
		if got != tc.dialect {
			t.Errorf("DialectOf(%q) = %q, want %q", tc.driver, got, tc.dialect)
		}
		if marker := got.Placeholder(1); marker != tc.placeholder {
			t.Errorf("Dialect(%q).Placeholder(1) = %q, want %q", got, marker, tc.placeholder)
		}
	}
}

// TestDriverIsRegistered_SQLServerPresentInSQLDrivers holds SDD section 10.2.
// A team whose application database is SQL Server needs the driver in the
// binary, and a blank import alone is not evidence that it registers.
func TestDriverIsRegistered_SQLServerPresentInSQLDrivers(t *testing.T) {
	drivers := sql.Drivers()
	if !slices.Contains(drivers, sqlServerDriver) {
		t.Fatalf("driver %q is not registered; sql.Drivers() = %v", sqlServerDriver, drivers)
	}
}

// TestDriverIsRegistered guards the defect where no driver was imported
// anywhere in the module, so every sql.Open call failed at runtime with
// "unknown driver".
func TestDriverIsRegistered_PgxPresentInSQLDrivers(t *testing.T) {
	drivers := sql.Drivers()
	if !slices.Contains(drivers, pgxDriver) {
		t.Fatalf("driver %q is not registered; sql.Drivers() = %v", pgxDriver, drivers)
	}
}

// TestOpen_UnsupportedDriver_FailsBeforeDial verifies that an unsupported
// driver name is rejected without a network attempt.
func TestOpen_UnsupportedDriver_ReturnsError(t *testing.T) {
	if _, err := Open(t.Context(), "mysql", "whatever"); err == nil {
		t.Fatal("Open with an unsupported driver returned no error, want one")
	}
}
