package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"os"
	"path"
	"slices"
	"strings"
)

type requirement struct {
	ID, Component string
	Inspection    struct{ Kind, Name string }
}

func (c *inspection) requirements(p policy) {
	var register struct{ Requirements []requirement }
	body, err := os.ReadFile("docs/plans/requirements.json")
	if err != nil || json.Unmarshal(body, &register) != nil || register.Requirements == nil {
		c.add("docs/plans/requirements.json", 1, "REQ-POLICY", "Restore the requirements array.")
		return
	}
	known, active, covered := map[string]requirement{}, map[string]bool{}, map[string]bool{}
	for _, r := range register.Requirements {
		known[r.ID] = r
	}
	for _, f := range c.files {
		dir := path.Dir(f.name)
		parent := p.supportParent(dir)
		if !p.matches(dir, p.Shared) && !p.matches(parent, p.Shared) {
			active[p.owner(dir)] = true
		}
		c.coverage(f, p, known, covered)
	}
	c.requirementSummary(register.Requirements, active, covered, string(body))
}
func (c *inspection) requirementSummary(requirements []requirement, active, covered map[string]bool, body string) {
	due, tested, defined, proven := 0, 0, 0, 0
	walk := slices.Contains(c.paths, "test/walk/walklib.py")
	steps := strings.Split(os.Getenv("OBERTH_GUARDS_WALK_STEPS"), "\n")
	passed := strings.Split(os.Getenv("OBERTH_GUARDS_WALK_PASSED"), "\n")
	for _, r := range requirements {
		needed := r.Inspection.Kind == "contract_test" && active[r.Component] || r.Inspection.Kind != "contract_test" && walk
		if needed {
			due++
			switch {
			case covered[r.ID]:
				tested++
			case r.Inspection.Kind == "walk_step" && slices.Contains(steps, r.Inspection.Name):
				defined++
				if slices.Contains(passed, r.Inspection.Name) {
					proven++
				}
			default:
				c.add("docs/plans/requirements.json", 1+strings.Count(strings.SplitN(body, r.ID, 2)[0], "\n"), "REQ", r.ID+": "+r.Inspection.Kind+" "+r.Inspection.Name)
			}
		}
	}
	c.notes = append(c.notes, fmt.Sprintf("requirements: %d due; %d covered by tests; %d defined in the walk; %d proven by a candidate walk", due, tested, defined, proven))
}
func (c *inspection) coverage(f source, p policy, known map[string]requirement, covered map[string]bool) {
	tests := map[*ast.CommentGroup]*ast.FuncDecl{}
	for _, decl := range f.node.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			tests[fn.Doc] = fn
		}
	}
	for _, doc := range f.node.Comments {
		fn := tests[doc]
		var ids []string
		for _, line := range doc.List {
			if text, ok := strings.CutPrefix(line.Text, "// Covers:"); ok {
				ids = append(ids, strings.Fields(text)...)
			}
		}
		if len(ids) == 0 {
			continue
		}
		at := f.positions.Position(doc.Pos()).Line
		if len(ids) != 1 {
			c.add(f.name, at, "REQ-COVER", "One test function must claim one id.")
			continue
		}
		r := known[ids[0]]
		if r.Inspection.Kind == "walk_step" {
			c.add(f.name, at, "REQ-WALK", "this requirement uses walk step "+r.Inspection.Name+"; remove the marker")
			continue
		}
		if fn == nil || !f.covers(fn, r, p.owner(path.Dir(f.name))) {
			c.add(f.name, at, "REQ-COVER", ids[0])
			continue
		}
		covered[ids[0]] = r.Inspection.Kind == "contract_test"
	}
}

func (f source) covers(fn *ast.FuncDecl, r requirement, owner string) bool {
	return fn.Name.Name == r.Inspection.Name &&
		fn.Recv == nil && strings.HasSuffix(f.name, "_test.go") && owner == r.Component &&
		f.positions.Position(fn.Doc.End()).Line+1 == f.positions.Position(fn.Pos()).Line
}
