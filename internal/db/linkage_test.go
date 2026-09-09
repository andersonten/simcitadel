package db_test

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// driverPackages holds every package whose init registers a database driver.
// A single constant here covered pgx alone, so the arrival of the SQL Server
// driver of SDD section 10.2 passed the guard below without a look. Each new
// driver must join this list, because a driver linked into a sidecar grows an
// image that CONTRIBUTING.md builds FROM scratch.
var driverPackages = []string{
	"github.com/jackc/pgx/v5/stdlib",
	"github.com/microsoft/go-mssqldb",
}

// modulePath is the module prefix. A full package path lets go list run from
// any directory inside the module.
const modulePath = "github.com/andersonten/simcitadel"

// linkedDrivers reports every driver package in the dependency graph of pkg.
func linkedDrivers(t *testing.T, pkg string) []string {
	t.Helper()

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("go list -deps %s: %v: %s", pkg, err, exitErr.Stderr)
		}
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}

	graph := map[string]bool{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		graph[strings.TrimSpace(line)] = true
	}

	var found []string
	for _, driver := range driverPackages {
		if graph[driver] {
			found = append(found, driver)
		}
	}
	return found
}

// TestBinaryLinksTheDriver asserts that the citadel binary's dependency graph
// contains every driver package. The binary opens whichever target the config
// names, so a driver that no package imports fails at run time only.
//
// TestDriverIsRegistered in the db package cannot catch the original defect,
// because a test in package db links the blank import by construction. The
// defect was a driver registered in a package that nothing imports, which only
// a check over the real binary's dependency graph detects.
func TestBinaryLinksTheDriver_CitadelDependencyGraphContainsEveryDriver(t *testing.T) {
	linked := linkedDrivers(t, modulePath+"/cmd/citadel")
	for _, driver := range driverPackages {
		if !slices.Contains(linked, driver) {
			t.Errorf("the citadel binary does not link %s, so sql.Open for that driver fails at run time with \"unknown driver\"", driver)
		}
	}
}

// TestServiceBinariesDoNotLinkTheDriver keeps the service images small.
// Neither service touches a database, so neither needs a driver.
func TestServiceBinariesDoNotLinkAnyDriver(t *testing.T) {
	for _, name := range []string{"event-trigger", "event-capture"} {
		t.Run(name, func(t *testing.T) {
			if linked := linkedDrivers(t, modulePath+"/cmd/"+name); len(linked) > 0 {
				t.Errorf("%s links %s but does not use a database", name, strings.Join(linked, ", "))
			}
		})
	}
}
