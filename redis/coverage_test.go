package redis

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// notTested lists wrapper methods that no test calls, each with the reason.
// Keep this list short: a method belongs here only if it cannot be run safely.
var notTested = map[string]string{}

// clientVars are the variable names tests use for a *Client.
var clientVars = map[string]bool{"c": true, "c2": true, "a": true, "b": true, "cc": true}

// TestEveryWrapperIsCalledByATest keeps coverage honest. It scans this
// package's test files, including the integration ones, for calls to
// <client>.<Method>(...) and fails for any Client method that is neither called nor
// listed in notTested with a reason. It proves a test calls each method; the
// integration run (scripts/integration.sh) proves the calls succeed.
func TestEveryWrapperIsCalledByATest(t *testing.T) {
	called := map[string]bool{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && clientVars[id.Name] {
						called[sel.Sel.Name] = true
					}
				}
			}
			return true
		})
	}

	typ := reflect.TypeOf(&Client{})
	var untested []string
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if !called[name] && notTested[name] == "" {
			untested = append(untested, name)
		}
	}
	sort.Strings(untested)
	if len(untested) > 0 {
		t.Fatalf("%d of %d Client methods are called by no test and are not in notTested:\n%s",
			len(untested), typ.NumMethod(), strings.Join(untested, " "))
	}
	for name := range notTested {
		if called[name] {
			t.Errorf("%s is in notTested but a test calls it; remove it from the list", name)
		}
	}
}
