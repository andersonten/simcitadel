package db

import "testing"

// This file holds the SQL Server driver rules of SDD section 10.2.

// TestNormalizeDriver_SQLServer_IsAccepted holds SDD section 10.2: "SQL Server
// exists behind the cdc-sqlserver profile for teams whose *application*
// database is SQL Server". A team of that kind sets targets.app-db.driver to
// sqlserver, so every database path of Citadel must open the connection:
// sql/exec, sql/query, inbox/assert, the truncate of the reset in section 4.9,
// the SELECT 1 preflight of section 4.10, and three doctor checks of section
// 12.3.
func TestNormalizeDriver_SQLServer_IsAccepted(t *testing.T) {
	for _, name := range []string{"sqlserver", "mssql"} {
		driver, err := NormalizeDriver(name)
		if err != nil {
			t.Errorf("NormalizeDriver(%q) returned %v, so a SQL Server application database cannot be a target", name, err)
			continue
		}
		if driver != sqlServerDriver {
			t.Errorf("NormalizeDriver(%q) = %q, want %q", name, driver, sqlServerDriver)
		}
	}
}
