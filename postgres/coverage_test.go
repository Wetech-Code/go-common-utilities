package postgres

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// notTested lists exported API that no test calls, each with the reason. Keep it
// empty unless something genuinely cannot be run safely.
var notTested = map[string]string{}

// clientVars are the variable names tests use for a *Client.
var clientVars = map[string]bool{"c": true, "pg": true}

// TestEveryExportedAPIIsCalledByATest fails when an exported method, function or
// type method in client.go is not called by any test (unit or integration). It
// proves a test calls each one; the integration run proves the calls succeed.
func TestEveryExportedAPIIsCalledByATest(t *testing.T) {
	fset := token.NewFileSet()

	// Everything exported in client.go that is callable.
	src, err := parser.ParseFile(fset, "client.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, d := range src.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || !fd.Name.IsExported() {
			continue
		}
		want[fd.Name.Name] = true
	}

	called := map[string]bool{}
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") || e.Name() == "coverage_test.go" {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && (clientVars[id.Name] || id.Name == "postgres" || id.Name == "l") {
					called[sel.Sel.Name] = true
				}
			}
			// Same-package unit tests call functions directly, e.g. IsUniqueViolation(err).
			if call, ok := n.(*ast.CallExpr); ok {
				fun := call.Fun
				if ix, ok := fun.(*ast.IndexExpr); ok {
					fun = ix.X
				}
				if id, ok := fun.(*ast.Ident); ok {
					called[id.Name] = true
				}
			}
			if id, ok := n.(*ast.Ident); ok && id.Obj == nil {
				called[id.Name] = true // function values such as is: IsUniqueViolation
			}
			return true
		})
	}

	var untested []string
	for name := range want {
		if !called[name] && notTested[name] == "" {
			untested = append(untested, name)
		}
	}
	sort.Strings(untested)
	if len(untested) > 0 {
		t.Fatalf("%d exported names in client.go are called by no test:\n%s", len(untested), strings.Join(untested, " "))
	}
}
