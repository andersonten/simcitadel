package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// SDD section 3.4 puts every process's configuration behind one reader, and
// CONTRIBUTING.md states the rule in code terms: "Read configuration through
// internal/config. Do not read an environment variable directly from a package
// under cmd/."
//
// The reader of internal/config tells an absent variable from an empty one and
// names the variable in the message it fails with. os.Getenv reports the same
// empty string for both, so a compose file that carries an unset "${VAR}"
// reaches the service as a deliberate empty value.
func TestEventCapture_Configuration_ReadsNoEnvironmentVariableDirectly(t *testing.T) {
	for _, call := range directEnvReads(t) {
		t.Errorf("%s, and SDD section 3.4 puts that behind internal/config", call)
	}
}

// directEnvReads returns one description per direct environment read in the
// non-test files of this package. It reads the syntax tree rather than the text,
// so a comment or a string literal cannot trip it.
func directEnvReads(t *testing.T) []string {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the package: %v", err)
	}

	var found []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "os" {
					return true
				}
				if sel.Sel.Name != "Getenv" && sel.Sel.Name != "LookupEnv" {
					return true
				}
				found = append(found, fmt.Sprintf("%s reads the environment with os.%s", fset.Position(call.Pos()), sel.Sel.Name))
				return true
			})
		}
	}
	return found
}
