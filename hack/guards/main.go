// Command guards parses tracked Go source against approved plan data.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

var codeFences = regexp.MustCompile("(?ms)^```[^\\n]*\\n.*?(?:^```[^\\n]*|\\z)|^~~~[^\\n]*\\n.*?(?:^~~~[^\\n]*|\\z)")
var headingPunctuation = regexp.MustCompile(`[^\pL\pN_\- ]`)
var specReferences = regexp.MustCompile(`(?m)^[ \t]*Spec: (.*)$`)
var planHeadings = regexp.MustCompile("(?m)^#{1,6} (.+)$")

type source struct {
	name      string
	node      *ast.File
	positions *token.FileSet
}
type inspection struct {
	module                                  string
	files                                   []source
	paths, findings, notes, roots, packages []string
	messages                                map[string][2]string
	policy                                  policy
}

func (c *inspection) add(file string, line int, rule, message string) {
	text := c.messages[strings.TrimRight(rule, "0123456789")]
	c.findings = append(c.findings, fmt.Sprintf("%s:%d: [%s] %s %s Detail: %s", file, line, rule, text[0], text[1], message))
}
func inspect(module string, tracked []byte) *inspection {
	c := &inspection{module: module, paths: strings.Split(string(tracked), "\x00")}
	if read("hack/guards/messages.json", &c.messages) != nil {
		panic("restore hack/guards/messages.json")
	}
	if err := read("docs/plans/dependencies.json", &c.policy); err != nil || c.policy.Version != 2 {
		c.add("docs/plans/dependencies.json", 1, "DEP-POLICY", "Restore the version-2 dependency policy.")
		return c
	}
	c.policy.production, c.policy.mains = map[string]bool{}, map[string]bool{}
	for _, name := range c.paths {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		positions := token.NewFileSet()
		node, err := parser.ParseFile(positions, name, nil, parser.ParseComments)
		if err != nil {
			c.add(name, 1, "SOURCE", "invalid Go source; fix parsing errors")
			continue
		}
		c.files = append(c.files, source{name, node, positions})
		c.policy.mains[path.Dir(name)] = c.policy.mains[path.Dir(name)] || node.Name.Name == "main"
		if !strings.HasSuffix(name, "_test.go") {
			for _, spec := range node.Imports {
				raw, _ := strconv.Unquote(spec.Path.Value)
				c.policy.production[strings.TrimPrefix(raw, module+"/")] = true
			}
		}
	}
	c.dependencies(c.policy)
	c.traceability(c.policy)
	c.requirements(c.policy)
	return c
}
func read(name string, value any) error {
	body, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, value)
}
func main() {
	tracked, err := io.ReadAll(os.Stdin)
	if err != nil || len(os.Args) != 2 {
		panic("Provide module path and tracked files on stdin.")
	}
	if os.Args[1] == "--yaml" {
		var documents []any
		decoder := yaml.NewDecoder(bytes.NewReader(tracked))
		for {
			var value any
			if err := decoder.Decode(&value); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				panic("Plugin YAML cannot be parsed.")
			}
			documents = append(documents, value)
		}
		if err := json.NewEncoder(os.Stdout).Encode(documents); err != nil {
			panic("Plugin YAML cannot be encoded.")
		}
		return
	}
	c := inspect(os.Args[1], tracked)
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"Lines": append(c.notes, c.findings...), "Roots": c.roots, "Product": c.policy.ProductCommand, "Packages": c.packages}); err != nil {
		panic(err)
	}
	os.Exit(min(1, len(c.findings)))
}
func (c *inspection) dependencies(p policy) {
	seen, mains := map[string]bool{}, map[string]bool{}
	for _, f := range c.files {
		from := path.Dir(f.name)
		if !seen[from] && p.owner(from) == "" {
			c.add(f.name, 1, "OWNER", from)
		}
		root := c.module + "/" + from
		if f.node.Name.Name == "main" && !strings.HasSuffix(f.name, "_test.go") && !mains[from] {
			if slices.Contains(p.ProductionCommands, from) {
				c.roots = append(c.roots, root)
			} else if p.harness(from) == "" {
				c.add(f.name, 1, "DEP-ROOT", from)
			}
			mains[from] = true
		}
		seen[from] = true
		if p.matches(from, p.Retired) {
			c.add(f.name, 1, "DEP13", from+" is retired; remove this package")
		}
		c.imports(f, p)
	}
}
func (c *inspection) imports(f source, p policy) {
	from := path.Dir(f.name)
	for _, spec := range f.node.Imports {
		raw, _ := strconv.Unquote(spec.Path.Value)
		to, local := strings.CutPrefix(raw+"/", c.module+"/")
		to = path.Clean(to)
		if !local || from == to {
			continue
		}
		before := len(c.findings)
		c.rules(f, spec, p, to)
		if len(c.findings) == before && !p.permitted(from, to, strings.HasSuffix(f.name, "_test.go")) {
			key := "DEP-CLOSED"
			if p.supportParent(to) != "" {
				key = "DEP-TEST"
			}
			c.add(f.name, f.positions.Position(spec.Pos()).Line, key, raw)
		}
	}
}

func (c *inspection) rules(f source, spec *ast.ImportSpec, p policy, to string) {
	raw, _ := strconv.Unquote(spec.Path.Value)
	from := path.Dir(f.name)
	granted := strings.HasSuffix(f.name, "_test.go") && p.matches(to, p.TestAllow[from])
	for _, rule := range p.Rules {
		switch { // only an explicit test-only grant excepts a test file from a deny rule
		case !p.matches(from, rule.From) || !p.matches(to, rule.To):
		case rule.Effect == "deny" && !granted:
			c.add(f.name, f.positions.Position(spec.Pos()).Line, fmt.Sprintf("DEP%02d", rule.ID), p.owner(from)+" -> "+p.owner(to)+": "+raw)
			return
		case rule.Effect == "allow_if_listed" && len(rule.Symbols) > 0:
			c.symbols(f, spec, rule, p)
		}
	}
}
func (c *inspection) symbols(f source, spec *ast.ImportSpec, r rule, p policy) {
	if strings.HasSuffix(f.name, "_test.go") {
		return
	}
	raw, _ := strconv.Unquote(spec.Path.Value)
	alias := path.Base(raw)
	if spec.Name != nil {
		alias = spec.Name.Name
	}
	if alias == "." {
		c.add(f.name, f.positions.Position(spec.Pos()).Line, fmt.Sprintf("DEP%02d", r.ID), "dot import hides restricted symbols; use an explicit alias")
		return
	}
	allowed := append(r.Symbols["*"], r.Symbols[p.owner(path.Dir(f.name))]...)
	ast.Inspect(f.node, func(n ast.Node) bool {
		selector, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, _ := selector.X.(*ast.Ident)
		name := selector.Sel.Name
		if base != nil && base.Name == alias && !slices.Contains(allowed, name) && !slices.Contains(allowed, path.Base(raw)+"."+name) {
			c.add(f.name, f.positions.Position(selector.Pos()).Line, fmt.Sprintf("DEP%02d", r.ID), alias+"."+name+" is forbidden here; use a permitted symbol")
		}
		return true
	})
}
func (c *inspection) traceability(p policy) {
	packages := map[string]string{}
	documented := map[string]bool{}
	for _, f := range c.files {
		dir := path.Dir(f.name)
		if slices.Contains([]string{"harness", "fixture"}, p.owner(dir)) || strings.HasSuffix(f.name, "_test.go") {
			continue
		}
		packages[dir] = f.name
		for _, match := range specReferences.FindAllStringSubmatch(f.node.Doc.Text(), -1) {
			ref := strings.TrimSpace(match[1])
			documented[dir] = true
			if !c.validSpec(ref) {
				c.add(f.name, f.positions.Position(f.node.Doc.Pos()).Line, "TRACE", ref+" is missing; fix the plan file or heading anchor")
			}
		}
	}
	for dir, file := range packages {
		if p.owner(dir) != "" && p.supportParent(dir) == "" && p.Packages[dir].Kind != "test_only" {
			c.packages = append(c.packages, path.Join(c.module, dir))
		}
		if !documented[dir] {
			c.add(file, 1, "TRACE", dir+" lacks package documentation; add a package-comment Spec: docs/plans/<file>.md#<anchor>")
		}
	}
}
func (c *inspection) validSpec(ref string) bool {
	file, anchor, ok := strings.Cut(ref, "#")
	if !ok || anchor == "" || !strings.HasPrefix(file, "docs/plans/") || path.Clean(file) != file || !strings.HasSuffix(file, ".md") {
		return false
	}
	body, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(planHeadings.FindAllStringSubmatch(codeFences.ReplaceAllString(string(body), ""), -1), func(heading []string) bool {
		title := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(heading[1]), "#"))
		return strings.ReplaceAll(headingPunctuation.ReplaceAllString(strings.ToLower(title), ""), " ", "-") == anchor
	})
}
