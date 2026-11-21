// Command scan extracts source facts for the executable plan inventory.
// Spec: docs/plans/README.md#index
package main

import (
	"encoding/json"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Fact struct {
	Kind   string
	Name   string
	File   string
	Line   int
	Text   string
	Args   []string
	Values []string
	Calls  []string
}

type scanner struct {
	fset   *token.FileSet
	facts  []Fact
	schema []string
	path   string
	src    []byte
}

func main() {
	s := &scanner{fset: token.NewFileSet()}
	filesRaw, err := exec.Command("git", "ls-files", "*.go").Output()
	must(err)
	files := strings.Fields(string(filesRaw))
	sort.Strings(files)
	for _, path := range files {
		if !strings.HasSuffix(path, "_test.go") && !strings.Contains(path, "/testdata/") {
			s.scan(path)
		}
	}
	if len(os.Args) == 2 {
		s.writeRegistry(os.Args[1])
	}
	must(json.NewEncoder(os.Stdout).Encode(s.facts))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func (s *scanner) text(n ast.Node) string {
	if n == nil {
		return ""
	}
	return string(s.src[s.fset.Position(n.Pos()).Offset:s.fset.Position(n.End()).Offset])
}

func (s *scanner) add(kind, name string, n ast.Node) *Fact {
	s.facts = append(s.facts, Fact{Kind: kind, Name: name, File: s.path,
		Line: s.fset.Position(n.Pos()).Line, Text: s.text(n)})
	return &s.facts[len(s.facts)-1]
}

func (s *scanner) scan(path string) {
	s.path = path
	src, err := os.ReadFile(path)
	must(err)
	s.src = src
	file, err := parser.ParseFile(s.fset, path, src, parser.ParseComments)
	must(err)
	for _, decl := range file.Decls {
		s.declaration(decl)
	}
	ast.Inspect(file, s.literal)
}

func (s *scanner) writeRegistry(dir string) {
	program := "package main\nimport (\"encoding/json\";\"os\";\"fmt\")\nconst ToolTimeoutArgument = \"timeout_s\"\nfunc toolOutputSchema(string) map[string]any { return nil }\n" + strings.Join(s.schema, "\n") + "\nfunc main(){ if err := json.NewEncoder(os.Stdout).Encode(toolDefinitions()); err != nil { panic(err) } }\n"
	formatted, err := format.Source([]byte(program))
	must(err)
	must(os.WriteFile(filepath.Join(dir, "registry.go"), formatted, 0600))
}

func (s *scanner) declaration(decl ast.Decl) {
	switch node := decl.(type) {
	case *ast.GenDecl:
		for _, spec := range node.Specs {
			if value, ok := spec.(*ast.ValueSpec); ok {
				s.value(node, value)
			}
		}
	case *ast.FuncDecl:
		if node.Body != nil {
			s.function(node)
		}
	}
}

func (s *scanner) value(gen *ast.GenDecl, value *ast.ValueSpec) {
	for i, name := range value.Names {
		fact := s.add("value", name.Name, value)
		if i < len(value.Values) {
			fact.Values = []string{s.text(value.Values[i])}
		}
		if s.path == "internal/api/mcp.go" && registryValue(name.Name) {
			s.schema = append(s.schema, gen.Tok.String()+" "+s.text(value))
		}
	}
}

func registryValue(name string) bool {
	return name == "IssueReadFields" || name == "maxAssistArgv" || name == "maxAssistExecSeconds"
}

func registryFunction(name string) bool {
	switch name {
	case "toolDefinitions", "agentLoopToolDefinitions", "administrationToolDefinitions",
		"issueReadProperties", "issueWorkStatusProperty", "promotionInputSchema", "withTimeout":
		return true
	default:
		return false
	}
}

func (s *scanner) function(fn *ast.FuncDecl) {
	index := len(s.facts)
	fact := s.add("function", fn.Name.Name, fn)
	for _, param := range fn.Type.Params.List {
		for _, ident := range param.Names {
			fact.Args = append(fact.Args, ident.Name)
		}
	}
	var calls []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		s.statement(fn.Name.Name, n)
		if call, ok := n.(*ast.CallExpr); ok {
			calls = append(calls, s.text(call.Fun))
		}
		return true
	})
	s.facts[index].Calls = calls
	if s.path == "internal/api/mcp.go" && registryFunction(fn.Name.Name) {
		s.schema = append(s.schema, s.text(fn))
	}
}

func (s *scanner) audit(name string, composite *ast.CompositeLit) {
	if !strings.HasSuffix(s.text(composite.Type), "AuditActionSpec") {
		return
	}
	for _, element := range composite.Elts {
		if field, ok := element.(*ast.KeyValueExpr); ok && s.text(field.Key) == "Action" {
			fact := s.add("audit", name, field)
			fact.Values = []string{s.text(field.Value)}
		}
	}
}

func (s *scanner) statement(name string, n ast.Node) {
	switch node := n.(type) {
	case *ast.AssignStmt:
		for i, lhs := range node.Lhs {
			if i < len(node.Rhs) {
				fact := s.add("assignment", name, node)
				fact.Args = []string{s.text(lhs)}
				fact.Values = []string{s.text(node.Rhs[i])}
			}
		}
	case *ast.CompositeLit:
		s.audit(name, node)
	case *ast.CallExpr:
		fact := s.add("call", name, node)
		fact.Values = []string{s.text(node.Fun)}
		for _, arg := range node.Args {
			fact.Args = append(fact.Args, s.text(arg))
		}
	case *ast.SwitchStmt:
		s.switchCases(name, node)
	}
}

func (s *scanner) switchCases(name string, sw *ast.SwitchStmt) {
	for _, item := range sw.Body.List {
		clause, ok := item.(*ast.CaseClause)
		if !ok {
			continue
		}
		fact := s.add("case", name, clause)
		fact.Args = []string{s.text(sw.Tag)}
		for _, value := range clause.List {
			fact.Values = append(fact.Values, s.text(value))
		}
		ast.Inspect(clause, func(child ast.Node) bool {
			if call, ok := child.(*ast.CallExpr); ok {
				fact.Calls = append(fact.Calls, s.text(call.Fun))
			}
			return true
		})
	}
}

func (s *scanner) literal(n ast.Node) bool {
	if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		value, err := strconv.Unquote(lit.Value)
		must(err)
		fact := s.add("string", "", lit)
		fact.Values = []string{value}
	}
	return true
}
