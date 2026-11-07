package mongodb

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// notTested lists exported API that no test calls, each with the reason.
var notTested = map[string]string{}

// receiverVars are the variable names tests use for a Client, Database, Collection
// or Bucket.
var receiverVars = map[string]bool{"c": true, "db": true, "coll": true, "bk": true, "cc": true}

// TestEveryExportedAPIIsCalledByATest fails when an exported method or function in
// client.go or gridfs.go is not called by any test. It proves a test calls each
// one; the integration run proves the calls succeed.
func TestEveryExportedAPIIsCalledByATest(t *testing.T) {
	fset := token.NewFileSet()
	want := map[string]bool{}
	for _, file := range []string{"client.go", "gridfs.go"} {
		src, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range src.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.IsExported() {
				want[fd.Name.Name] = true
			}
		}
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
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && (receiverVars[id.Name] || id.Name == "mongodb") {
					called[x.Sel.Name] = true
				}
			case *ast.CallExpr:
				fun := x.Fun
				if ix, ok := fun.(*ast.IndexExpr); ok {
					fun = ix.X
				}
				if id, ok := fun.(*ast.Ident); ok {
					called[id.Name] = true
				}
			case *ast.Ident:
				if x.Obj == nil {
					called[x.Name] = true
				}
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
		t.Fatalf("%d exported names are called by no test:\n%s", len(untested), strings.Join(untested, " "))
	}
}
