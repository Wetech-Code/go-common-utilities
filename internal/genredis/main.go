// Command genredis generates redis/client_gen.go: one plain (value, error)
// method on redis.Client for every command method that go-redis defines, except
// the ones written by hand in redis/client.go.
//
// Commands that exist only on go-redis's single-connection type (Auth, Hello,
// Select, ClientSetName, ...) are not generated because *redis.Client lacks them.
//
// Run it from the redis directory after changing the go-redis version:
//
//	go generate ./redis
//
// It reads the go-redis source in the module cache, so it needs no network. The
// completeness test in redis/client_gen_test.go fails when go-redis gains a
// command that has not been generated yet.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

const goredisPath = "github.com/redis/go-redis/v9"

var builtin = map[string]bool{
	"string": true, "int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"float32": true, "float64": true, "bool": true, "byte": true, "rune": true, "error": true, "any": true,
}

var stdImports = map[string]string{"time": "time", "context": "context", "net": "net", "io": "io"}

// reserved names a parameter must not shadow in the generated bodies.
var reserved = map[string]bool{"c": true, "get": true, "notFound": true, "goredis": true, "time": true, "context": true, "errors": true}

// stubs are go-redis command methods whose body is just panic("not implemented").
// Wrapping them would make Client panic, which this library never does.
var stubs = map[string]bool{"Quit": true, "Sync": true, "JSONDebugMemory": true}

// notes add a caveat to a generated method's documentation.
var notes = map[string]string{
	"JSONArrTrim":  "go-redis sends JSON.ARRTRIM without the start and stop arguments the server requires, so the server rejects this call; use JSONArrTrimWithArgs.",
	"CFScanDump":   "When the dump is finished the server replies with a nil chunk, which is returned as ErrNotFound; treat that as the end of the dump.",
	"HotKeysStart": "go-redis supports HOTKEYS only on a standalone client, not a cluster or pooled one.",
}

// dangerous commands get a warning in their doc comment.
var dangerous = regexp.MustCompile(`^(Flush|Shutdown|ConfigSet|ConfigRewrite|ConfigResetStat|ACL(DelUser|SetUser|Load|Save)|Debug|Keys$|SlaveOf|ReplicaOf|Failover|ClientKill|ClientPause|ClientUnpause|ClientNoEvict|Module|ScriptFlush|ScriptKill|Function(Flush|Delete|Restore|Kill)|Migrate|Restore|Cluster(Failover|Reset|Forget|Meet|DelSlots|FlushSlots|SetSlot|AddSlots|Replicate|SaveConfig|CountFailureReports)|Save|BgSave|BgRewriteAOF|SwapDB|Move|LatencyReset|SlowLogReset|FTDropIndex|JSONDel|JSONForget|TSDel)`)

type param struct {
	name     string
	typ      string
	variadic bool
}

type method struct {
	name    string
	params  []param
	cmdType string
}

type gen struct {
	imports map[string]bool
	skipped []string
}

func main() {
	out := flag.String("out", "client_gen.go", "output file")
	hand := flag.String("hand", "client.go", "hand-written file whose methods are not regenerated")
	flag.Parse()

	dir := goList("{{.Dir}}")
	version := goList("{{.Version}}")

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	check(err)
	pkg := pkgs["redis"]
	if pkg == nil {
		check(fmt.Errorf("package redis not found in %s", dir))
	}

	handWritten := handMethods(*hand)
	g := &gen{imports: map[string]bool{"context": true}}

	results := map[string][]string{} // Cmd type -> result types
	var methods []method
	seen := map[string]bool{}
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			recv := recvName(fd.Recv.List[0].Type)
			switch {
			case fd.Name.Name == "Result" && strings.HasSuffix(recv, "Cmd") && fd.Type.Results != nil:
				var rs []string
				ok := true
				for _, fl := range fd.Type.Results.List {
					t, good := g.typ(fl.Type, false)
					ok = ok && good
					n := len(fl.Names)
					if n == 0 {
						n = 1
					}
					for i := 0; i < n; i++ {
						rs = append(rs, t)
					}
				}
				if ok {
					results[recv] = rs
				}
			case (recv == "cmdable" || recv == "Client") && ast.IsExported(fd.Name.Name):
				ret := cmdReturn(fd)
				if ret == "" || seen[fd.Name.Name] {
					continue
				}
				m, ok := g.toMethod(fd, ret)
				if !ok {
					g.skipped = append(g.skipped, fd.Name.Name)
					continue
				}
				seen[fd.Name.Name] = true
				methods = append(methods, m)
			}
		}
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].name < methods[j].name })

	var body bytes.Buffer
	count := 0
	for _, m := range methods {
		if handWritten[m.name] || stubs[m.name] {
			continue
		}
		rs, ok := results[m.cmdType]
		if !ok {
			g.skipped = append(g.skipped, m.name+" (no Result for "+m.cmdType+")")
			continue
		}
		g.emit(&body, m, rs)
		count++
	}

	var src bytes.Buffer
	fmt.Fprintf(&src, "// Code generated by internal/genredis from %s %s; DO NOT EDIT.\n\n", goredisPath, version)
	src.WriteString("package redis\n\nimport (\n")
	var imps []string
	for p := range g.imports {
		imps = append(imps, p)
	}
	sort.Strings(imps)
	for _, p := range imps {
		fmt.Fprintf(&src, "\t%q\n", p)
	}
	fmt.Fprintf(&src, "\n\tgoredis %q\n)\n\n", goredisPath)
	src.Write(body.Bytes())

	formatted, err := format.Source(src.Bytes())
	check(err)
	check(os.WriteFile(*out, formatted, 0o644))
	fmt.Fprintf(os.Stderr, "genredis: wrote %d methods to %s (go-redis %s)\n", count, *out, version)
	if len(g.skipped) > 0 {
		sort.Strings(g.skipped)
		fmt.Fprintf(os.Stderr, "genredis: skipped %d without a plain result: %s\n", len(g.skipped), strings.Join(g.skipped, ", "))
	}
}

func (g *gen) emit(b *bytes.Buffer, m method, rs []string) {
	var sig, call []string
	sig = append(sig, "ctx context.Context")
	for i, p := range m.params {
		if i == 0 {
			continue // ctx
		}
		t := p.typ
		if p.variadic {
			t = "..." + t
		}
		sig = append(sig, p.name+" "+t)
	}
	call = append(call, "ctx")
	for i, p := range m.params {
		if i == 0 {
			continue
		}
		if p.variadic {
			call = append(call, p.name+"...")
		} else {
			call = append(call, p.name)
		}
	}
	vals, last := rs[:len(rs)-1], rs[len(rs)-1]
	if last != "error" {
		return
	}
	fmt.Fprintf(b, "// %s calls the go-redis command %s and returns its result.", m.name, m.name)
	if len(vals) > 0 {
		b.WriteString(" A nil reply is returned as ErrNotFound.")
	}
	if n := notes[m.name]; n != "" {
		b.WriteString("\n//\n// Note: " + n)
	}
	if dangerous.MatchString(m.name) {
		b.WriteString("\n//\n// Dangerous: this command can delete data, change server settings or disrupt other clients.")
	}
	b.WriteString("\n")
	callExpr := fmt.Sprintf("c.Native.%s(%s)", m.name, strings.Join(call, ", "))
	switch len(vals) {
	case 0:
		fmt.Fprintf(b, "func (c *Client) %s(%s) error {\n\treturn notFound(%s.Err())\n}\n\n", m.name, strings.Join(sig, ", "), callExpr)
	case 1:
		fmt.Fprintf(b, "func (c *Client) %s(%s) (%s, error) {\n\treturn get(%s.Result())\n}\n\n", m.name, strings.Join(sig, ", "), vals[0], callExpr)
	default:
		var names []string
		for i := range vals {
			names = append(names, fmt.Sprintf("r%d", i))
		}
		fmt.Fprintf(b, "func (c *Client) %s(%s) (%s, error) {\n\t%s, err := %s.Result()\n\treturn %s, notFound(err)\n}\n\n",
			m.name, strings.Join(sig, ", "), strings.Join(vals, ", "),
			strings.Join(names, ", "), callExpr, strings.Join(names, ", "))
	}
}

func (g *gen) toMethod(fd *ast.FuncDecl, ret string) (method, bool) {
	m := method{name: fd.Name.Name, cmdType: ret}
	idx := 0
	for _, f := range fd.Type.Params.List {
		variadic := false
		te := f.Type
		if el, ok := te.(*ast.Ellipsis); ok {
			variadic, te = true, el.Elt
		}
		t, ok := g.typ(te, false)
		if !ok {
			return m, false
		}
		names := f.Names
		n := len(names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("a%d", idx)
			if len(names) > 0 && names[i].Name != "_" {
				name = names[i].Name
			}
			if idx == 0 {
				name = "ctx"
			}
			if reserved[name] {
				name += "_"
			}
			m.params = append(m.params, param{name: name, typ: t, variadic: variadic})
			idx++
		}
	}
	if len(m.params) == 0 || m.params[0].typ != "context.Context" {
		return m, false
	}
	m.params[0].name = "ctx"
	return m, true
}

// typ renders a type expression with go-redis types qualified as goredis.X. The
// bool is false for anything the generator does not support (unexported types,
// channels, structs, unknown packages).
func (g *gen) typ(e ast.Expr, _ bool) (string, bool) {
	switch t := e.(type) {
	case *ast.Ident:
		switch {
		case builtin[t.Name]:
			return t.Name, true
		case ast.IsExported(t.Name):
			return "goredis." + t.Name, true
		}
		return "", false
	case *ast.StarExpr:
		s, ok := g.typ(t.X, false)
		return "*" + s, ok
	case *ast.ArrayType:
		s, ok := g.typ(t.Elt, false)
		if t.Len == nil {
			return "[]" + s, ok
		}
		lit, isLit := t.Len.(*ast.BasicLit)
		if !isLit {
			return "", false
		}
		return "[" + lit.Value + "]" + s, ok
	case *ast.MapType:
		k, ok1 := g.typ(t.Key, false)
		v, ok2 := g.typ(t.Value, false)
		return "map[" + k + "]" + v, ok1 && ok2
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		path, known := stdImports[x.Name]
		if !known {
			return "", false
		}
		g.imports[path] = true
		return x.Name + "." + t.Sel.Name, true
	case *ast.InterfaceType:
		if t.Methods == nil || len(t.Methods.List) == 0 {
			return "any", true
		}
		return "", false
	case *ast.Ellipsis:
		s, ok := g.typ(t.Elt, false)
		return "..." + s, ok
	}
	return "", false
}

// cmdReturn returns the *XCmd type name a command method returns, or "".
func cmdReturn(fd *ast.FuncDecl) string {
	if fd.Type.Results == nil || len(fd.Type.Results.List) != 1 {
		return ""
	}
	star, ok := fd.Type.Results.List[0].Type.(*ast.StarExpr)
	if !ok {
		return ""
	}
	id, ok := star.X.(*ast.Ident)
	if !ok || !strings.HasSuffix(id.Name, "Cmd") {
		return ""
	}
	return id.Name
}

func recvName(e ast.Expr) string {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func handMethods(path string) map[string]bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	check(err)
	out := map[string]bool{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && recvName(fd.Recv.List[0].Type) == "Client" {
			out[fd.Name.Name] = true
		}
	}
	return out
}

func goList(format string) string {
	b, err := exec.Command("go", "list", "-m", "-f", format, goredisPath).Output()
	check(err)
	return strings.TrimSpace(string(b))
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "genredis:", err)
		os.Exit(1)
	}
}
