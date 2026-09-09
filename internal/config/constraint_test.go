package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// addressFieldNames lists the configuration fields that carry an address.
// Constraint C3 of SDD sections 1 and 3.4 makes environment identity
// a parameter, so none of these may ever hold a compiled-in value.
var addressFieldNames = map[string]bool{
	"URL":           true,
	"AdminURL":      true,
	"AMQPURL":       true,
	"ManagementURL": true,
	"DSN":           true,
	"TokenURL":      true,
}

// TestConstraintC3_MinimalConfig_LeavesEveryAddressEmpty is the reflected half
// of the C3 check of SDD section 14.3. A config that names no address must load
// with every address field empty. A default here is invisible when it is wrong
// and expensive when it is found.
func TestConstraintC3_MinimalConfig_LeavesEveryAddressEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "citadel.yaml")
	minimal := "version: 1\nenvironment:\n  name: local\n"
	if err := os.WriteFile(path, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load a minimal config: %v", err)
	}

	var found []string
	walkAddressFields(reflect.ValueOf(cfg), "", func(path, value string) {
		if value != "" {
			found = append(found, path+" = "+value)
		}
	})
	if len(found) > 0 {
		t.Errorf("a config that names no address loaded with a compiled-in default: %s",
			strings.Join(found, ", "))
	}
}

// TestWalkAddressFields_FilledAddress_IsReported proves the walker of the C3
// check reports a default instead of passing over it. Without this proof, the
// check above passes whether or not the walker reaches anything.
func TestWalkAddressFields_FilledAddress_IsReported(t *testing.T) {
	cfg := &CitadelConfig{}
	cfg.Targets.RabbitMQ = &RabbitMQTarget{AMQPURL: "amqp://host/"}
	cfg.Targets.AppDB = &DatabaseTarget{DSN: "postgres://host/db"}
	cfg.Auth = map[string]AuthConfig{"app": {TokenURL: "https://idp/token"}}

	seen := map[string]string{}
	walkAddressFields(reflect.ValueOf(cfg), "", func(path, value string) {
		if value != "" {
			seen[path] = value
		}
	})

	want := map[string]string{
		".Targets.RabbitMQ.AMQPURL": "amqp://host/",
		".Targets.AppDB.DSN":        "postgres://host/db",
		".Auth[app].TokenURL":       "https://idp/token",
	}
	for path, value := range want {
		if seen[path] != value {
			t.Errorf("the walker missed %s: got %q, want %q", path, seen[path], value)
		}
	}
}

// walkAddressFields visits every address-bearing string field reachable from v.
func walkAddressFields(v reflect.Value, path string, visit func(path, value string)) {
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface:
		if v.IsNil() {
			return
		}
		walkAddressFields(v.Elem(), path, visit)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			child := path + "." + field.Name
			if field.Type.Kind() == reflect.String && addressFieldNames[field.Name] {
				visit(child, v.Field(i).String())
				continue
			}
			walkAddressFields(v.Field(i), child, visit)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			walkAddressFields(v.MapIndex(key), path+"["+key.String()+"]", visit)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkAddressFields(v.Index(i), path+"["+strconv.Itoa(i)+"]", visit)
		}
	}
}

// addressLiterals are the forms C3 forbids in a string constant. The scan
// covers a host, not a port. A bare port default names where a process listens,
// and the reflected half above already proves that no address field of the
// config carries a default.
var addressLiterals = []string{"localhost", "127.0.0.1"}

// exemption allows one exact literal in one exact file. The pair is exact so
// that an exemption cannot hide a second address literal in the same file.
type exemption struct {
	file    string
	literal string
	reason  string
}

// exemptLiterals holds every address literal that C3 allows. Each entry states
// its reason, because an unexplained exemption grows into a hole.
var exemptLiterals = []exemption{
	{
		file:    filepath.Join("internal", "health", "probe.go"),
		literal: "127.0.0.1",
		reason:  "the container health probe dials its own loopback address",
	},
	{
		file:    filepath.Join("cmd", "citadel", "main.go"),
		literal: "127.0.0.1",
		reason:  "kubectl port-forward binds the local end to loopback, so the readiness dial is local",
	},
	{
		file:    filepath.Join("cmd", "citadel", "main.go"),
		literal: "forwarding %s localhost:%d -> %d\n",
		reason:  "the forward command reports the local end of the tunnel it just opened",
	},
}

func isExempt(file, literal string) bool {
	for _, e := range exemptLiterals {
		if e.file == file && e.literal == literal {
			return true
		}
	}
	return false
}

// TestConstraintC3_NoSourceFileCarriesAnAddressLiteral is the source-scan half
// of the C3 check of SDD section 14.3. It reads every string literal out of the
// syntax tree, so a comment or an identifier that holds one of these words does
// not fail the gate.
func TestConstraintC3_NoSourceFileCarriesAnAddressLiteral(t *testing.T) {
	root := repoRoot(t)

	var problems []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		// The integration suite stands up its own containers and addresses them
		// directly, and it is not part of any shipped binary.
		if strings.HasPrefix(rel, filepath.Join("internal", "integration")) {
			return nil
		}

		for _, literal := range stringLiterals(t, path) {
			if isExempt(rel, literal) {
				continue
			}
			for _, forbidden := range addressLiterals {
				if strings.Contains(literal, forbidden) {
					problems = append(problems, rel+": "+literal)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the source tree: %v", err)
	}

	if len(problems) > 0 {
		t.Errorf("C3 forbids a compiled-in address; found %d:\n%s",
			len(problems), strings.Join(problems, "\n"))
	}
}

// TestConstraintC3_EveryExemption_IsStillInUse keeps the exemption list honest.
// A stale entry widens the blind spot of the scan without anybody noticing, so
// an exemption that no longer matches a literal must be deleted.
func TestConstraintC3_EveryExemption_IsStillInUse(t *testing.T) {
	root := repoRoot(t)
	for _, e := range exemptLiterals {
		path := filepath.Join(root, e.file)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("exempt file %s does not exist, so the reason %q is unreachable", e.file, e.reason)
			continue
		}
		found := false
		for _, literal := range stringLiterals(t, path) {
			if literal == e.literal {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s no longer holds the literal %q, so delete the exemption", e.file, e.literal)
		}
	}
}

// stringLiterals returns every string constant in a Go source file.
func stringLiterals(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, unquoteErr := strconv.Unquote(lit.Value)
		if unquoteErr != nil {
			return true
		}
		out = append(out, value)
		return true
	})
	return out
}

func skipDir(name string) bool {
	return name == ".git" || name == "testdata" || name == "node_modules"
}

// repoRoot returns the module root, found by walking up from the package
// directory to the directory that holds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find go.mod above the package directory")
		}
		dir = parent
	}
}
