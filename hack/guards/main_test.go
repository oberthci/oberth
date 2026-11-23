package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func write(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T) policy {
	t.Helper()
	catalog, err := os.ReadFile("messages.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	write(t, "hack/guards/messages.json", string(catalog))
	var p policy
	body := "{\"version\":2,\"components\":{\"C1\":[\"component/a/**\"],\"C2\":[\"component/b/**\"]},\"allow\":{\"C1\":[\"component/b\",\"shared\"]},\"shared\":[\"shared\"],\"shared_per_component\":{\"C1\":[\"shared\"]},\"shared_dependencies\":{\"shared\":[]},\"package_allow\":{},\"test_allow\":{},\"test_only\":{},\"packages\":{},\"rules\":[],\"retired\":[\"retired/**\"]}"
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	p.Components["C3"] = []string{"internal/value"}
	p.Packages["shared"] = struct {
		Owner, Kind string
		Consumers   []string
	}{"C1", "shared", nil}
	p.production, p.mains = map[string]bool{}, map[string]bool{}
	write(t, "docs/plans/requirements.json", "{\"requirements\":[]}")
	write(t, "docs/plans/contract.md", "# Contract\n## Behavior\n")
	return p
}
func scan(t *testing.T, p policy, files map[string]string) []string {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	write(t, "docs/plans/dependencies.json", string(body))
	var paths []string
	for name, body := range files {
		if strings.HasPrefix(name, "component/") && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			body = "// Package fixture follows its contract.\n// Spec: docs/plans/contract.md#behavior\n" + body
		}
		write(t, name, body)
		paths = append(paths, name)
	}
	return inspect("example.invalid/unit", []byte(strings.Join(paths, "\x00"))).findings
}
func require(t *testing.T, hits []string, rule string) {
	t.Helper()
	for _, hit := range hits {
		if strings.Contains(hit, "["+rule+"]") {
			return
		}
	}
	t.Fatalf("missing %s: %v", rule, hits)
}
func clean(t *testing.T, hits []string) {
	t.Helper()
	if len(hits) > 0 {
		t.Fatal(hits)
	}
}
func TestDependenciesIncludeInactiveTestAndNestedModuleFiles(t *testing.T) {
	p := fixture(t)
	p.Rules = []rule{{ID: 2, From: []string{"@C2"}, To: []string{"@C1"}, Effect: "deny"}}
	for _, name := range []string{"component/b/win.go", "component/b/a_test.go", "component/b/nested/a.go"} {
		t.Run(name, func(t *testing.T) {
			write(t, "component/b/nested/go.mod", "module example.invalid/nested\n")
			hits := scan(t, p, map[string]string{name: "//go:build windows\n\npackage b\nimport alias \"example.invalid/unit/component/a\"\nvar _=alias.Value\n"})
			require(t, hits, "DEP02")
		})
	}
}
func TestClosedPolicyOverridesAndTests(t *testing.T) {
	p := fixture(t)
	files := map[string]string{"component/a/a.go": "package a\nimport _ \"example.invalid/unit/component/b\"\n"}
	clean(t, scan(t, p, files))
	p.PackageAllow["component/a"] = []string{}
	require(t, scan(t, p, files), "DEP-CLOSED")
	p.TestAllow["component/a"] = []string{"component/b"}
	clean(t, scan(t, p, map[string]string{"component/a/a_test.go": files["component/a/a.go"]}))
	require(t, scan(t, p, files), "DEP-CLOSED")
}
func TestFixturePermissionHasExactPathBoundary(t *testing.T) {
	p := fixture(t)
	p.FixtureOnly = append(p.FixtureOnly, struct{ From, To []string }{[]string{"fixture/disposable/**"}, []string{"component/b"}})
	code := "package fixture\nimport _ \"example.invalid/unit/component/b\"\n"
	clean(t, scan(t, p, map[string]string{"fixture/disposable/a.go": code}))
	require(t, scan(t, p, map[string]string{"fixture/disposable-other/a.go": code}), "DEP-CLOSED")
}
func TestDeniedSharedAndRetiredPaths(t *testing.T) {
	p := fixture(t)
	p.Rules = []rule{{ID: 10, From: []string{"@shared"}, To: []string{"@components"}, Effect: "deny"}}
	require(t, scan(t, p, map[string]string{"shared/a.go": "package shared\nimport _ \"example.invalid/unit/component/b\"\n"}), "DEP10")
	require(t, scan(t, p, map[string]string{"retired/value.go": "package retired\n"}), "DEP13")
	clean(t, scan(t, p, map[string]string{"component/a/allowlist.go": "package a\n"}))
}
func TestSealAndDotImportsAreProductionRestricted(t *testing.T) {
	p := fixture(t)
	p.Components["C4"] = p.Components["C2"]
	delete(p.Components, "C2")
	p.Rules = []rule{{ID: 15, From: []string{"@C1"}, To: []string{"@C4"}, Effect: "allow_if_listed", Symbols: map[string][]string{"*": {"PreparedView"}}}}
	for _, source := range []string{
		"package a\nimport b \"example.invalid/unit/component/b\"\nvar _=b.Seal\n",
		"package a\nimport . \"example.invalid/unit/component/b\"\nvar _=PreparedView\n",
	} {
		require(t, scan(t, p, map[string]string{"component/a/a.go": source}), "DEP15")
		clean(t, scan(t, p, map[string]string{"component/a/a_test.go": source}))
	}
	clean(t, scan(t, p, map[string]string{"component/a/a.go": "package a\nimport b \"example.invalid/unit/component/b\"\nvar _=b.PreparedView\n"}))
}
func TestTraceabilityUsesPackageCommentsAndATXAnchors(t *testing.T) {
	p := fixture(t)
	source := "// Package value has a contract.\n// Spec: docs/plans/contract.md#behavior\npackage value\n"
	clean(t, scan(t, p, map[string]string{"internal/value/doc.go": source}))
	for _, ref := range []string{"docs/plans/contract.md#missing", "docs/plans/../contract.md#behavior", "docs/plans/missing.md#behavior"} {
		require(t, scan(t, p, map[string]string{"internal/value/doc.go": strings.ReplaceAll(source, "docs/plans/contract.md#behavior", ref)}), "TRACE")
	}
	require(t, scan(t, p, map[string]string{"internal/value/doc.go": "package value\n// Spec: docs/plans/contract.md#behavior\nfunc helper(){}\n"}), "TRACE")
}
func TestFencedHeadingsNeverSatisfySpec(t *testing.T) {
	p := fixture(t)
	for _, marker := range []string{strings.Repeat(string(rune(96)), 3), "~~~"} {
		for _, end := range []string{marker, ""} {
			write(t, "docs/plans/contract.md", "# Contract\n"+marker+"\n## Example\n"+end+"\n")
			require(t, scan(t, p, map[string]string{"internal/value/doc.go": "// Package value has a contract.\n// Spec: docs/plans/contract.md#example\npackage value\n"}), "TRACE")
		}
	}
}
func TestRequirementsDirectCommentsAndMissingRegister(t *testing.T) {
	p := fixture(t)
	write(t, "docs/plans/requirements.json", "{\"requirements\":[{\"id\":\"REQ-C1-001\",\"component\":\"C1\",\"inspection\":{\"kind\":\"contract_test\",\"name\":\"TestContract\"}}]}")
	code := "package fixture\nimport \"testing\"\n// Covers: REQ-C1-001\nfunc TestContract(t *testing.T){}\n"
	clean(t, scan(t, p, map[string]string{"component/a/a_test.go": code, "component/a/a.go": "package a\n"}))
	for _, bad := range []string{
		strings.ReplaceAll(code, "\nfunc Test", "\n\nfunc Test"),
		strings.ReplaceAll(code, "TestContract", "helper"),
		strings.ReplaceAll(code, "REQ-C1-001", "REQ-C1-999"),
	} {
		require(t, scan(t, p, map[string]string{"component/a/a_test.go": bad, "component/a/a.go": "package a\n"}), "REQ")
	}
	if err := os.Remove("docs/plans/requirements.json"); err != nil {
		t.Fatal(err)
	}
	require(t, scan(t, p, map[string]string{}), "REQ-POLICY")
	write(t, "docs/plans/requirements.json", "[]")
	require(t, scan(t, p, map[string]string{}), "REQ-POLICY")
}
func TestModulePrefixAndDataFailures(t *testing.T) {
	p := fixture(t)
	clean(t, scan(t, p, map[string]string{"component/a/a.go": "package a\nimport _ \"example.invalid/unitish/component/b\"\n"}))
	require(t, scan(t, p, map[string]string{"component/a/a.go": "package a\nimport _ \"example.invalid/unit\"\n"}), "DEP-CLOSED")
	hits := scan(t, p, map[string]string{"bad.go": "package\n"})
	require(t, hits, "SOURCE")
	write(t, "docs/plans/dependencies.json", "{}")
	require(t, inspect("example.invalid/unit", nil).findings, "DEP-POLICY")
}

func TestConventionSupportInheritsParentPermissionsAndRestrictsConsumers(t *testing.T) {
	p := fixture(t)
	p.PackageAllow["component/b"] = []string{"shared"}
	p.Components["C3"] = []string{"."}
	if p.supportParent("fake") != "." || p.owner("fake") != "C3" {
		t.Fatal("root package support has no parent owner")
	}
	for _, suffix := range []string{"fake", "contracttest", "steptest", "catalogtest"} {
		helper := "component/b/" + suffix
		if p.owner(helper) != "C2" || p.supportParent(helper+"/nested") != "" {
			t.Fatal("test support has the wrong scope", helper)
		}
		for _, tc := range []struct {
			from, to   string
			test, want bool
		}{
			{helper, "component/b", false, true},
			{helper, "shared", false, true},
			{helper, "component/b/contracttest", false, true},
			{"component/a", helper, true, true},
			{"component/b", helper, true, true},
			{"component/a/fake", helper, false, true},
			{"component/a", helper, false, false},
			{"unknown", helper, true, false},
			{helper, "component/a", false, false},
		} {
			if p.permitted(tc.from, tc.to, tc.test) != tc.want {
				t.Fatal("test support permission", helper, tc)
			}
		}
	}
	code := "package a\nimport _ \"example.invalid/unit/component/b/fake\"\n"
	clean(t, scan(t, p, map[string]string{"component/a/a_test.go": code}))
	require(t, scan(t, p, map[string]string{"component/a/a.go": code}), "DEP-TEST")
	require(t, scan(t, p, map[string]string{"component/b/fake/a.go": "package fake\nimport _ \"example.invalid/unit/component/a\"\n"}), "DEP-CLOSED")
}
func TestDeclaredHarnessPolicyBoundaries(t *testing.T) {
	p := fixture(t)
	p.Harness.Trees = []string{"tools/**"}
	p.TestAllow["tools/check"] = []string{"shared"}
	if !p.permitted("tools/check", "tools/library", false) || !p.permitted("tools/check", "shared", false) ||
		p.permitted("component/a", "tools/library", true) || p.permitted("tools/check", "component/b", false) {
		t.Fatal("harness policy boundaries failed")
	}
}
func TestRequirementsWaitForImplementationAndBecomeDue(t *testing.T) {
	p := fixture(t)
	write(t, "docs/plans/requirements.json", "{\"requirements\":[{\"id\":\"REQ-C1-001\",\"component\":\"C1\",\"inspection\":{\"kind\":\"contract_test\",\"name\":\"TestContract\"}}]}")
	require(t, scan(t, p, map[string]string{"component/a/a_test.go": "package a\n"}), "REQ")
	var metadata policy
	if err := json.Unmarshal([]byte("{\"components\":{\"C1\":[\"component/a/**\"]},\"packages\":{\"component/a/leaf\":{\"owner\":\"C1\",\"kind\":\"runtime\"}}}"), &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Version = 2
	require(t, scan(t, metadata, map[string]string{"component/a/leaf/leaf.go": "package leaf\n"}), "REQ")
	metadata.Shared = []string{"component/a/leaf"}
	clean(t, scan(t, metadata, map[string]string{"component/a/leaf/leaf.go": "package leaf\n"}))
	require(t, scan(t, metadata, map[string]string{"component/a/leaf/implementation/value.go": "package implementation\n"}), "REQ")
	require(t, scan(t, p, map[string]string{"component/a/a.go": "package a\n"}), "REQ")
	code := "package a\nimport \"testing\"\n// Covers: REQ-C1-001\nfunc TestContract(t *testing.T){}\n"
	clean(t, scan(t, p, map[string]string{"component/a/a.go": "package a\n", "component/a/a_test.go": code}))
}
func TestPackageOwnershipMustBeUnique(t *testing.T) {
	p := fixture(t)
	require(t, scan(t, p, map[string]string{"unlisted/value.go": "package value\n"}), "OWNER")
	p.Components["C4"] = p.Components["C1"]
	require(t, scan(t, p, map[string]string{"component/a/a.go": "package a\n"}), "OWNER")
}

func TestCoverageRequiresCurrentSingleInspectionAndWalkTable(t *testing.T) {
	p := fixture(t)
	write(t, "docs/plans/requirements.json", "{\"requirements\":[{\"id\":\"REQ-C1-001\",\"component\":\"C1\",\"inspection\":{\"kind\":\"contract_test\",\"name\":\"TestContract\"}}]}")
	code := "package a\nimport \"testing\"\n// Covers: REQ-C1-001\nfunc TestContract(t *testing.T){}\n"
	for _, bad := range []string{strings.ReplaceAll(code, "REQ-C1-001", "REQ-C1-999"), strings.ReplaceAll(code, "REQ-C1-001", "REQ-C1-001 REQ-C1-001"), strings.ReplaceAll(code, "TestContract", "TestOther"), strings.ReplaceAll(code, "func TestContract(t *testing.T){}", "var Value = 1")} {
		require(t, scan(t, p, map[string]string{"component/a/a_test.go": bad}), "REQ-COVER")
	}
	write(t, "docs/plans/requirements.json", "{\"requirements\":[{\"id\":\"REQ-C1-001\",\"component\":\"C1\",\"inspection\":{\"kind\":\"walk_step\",\"name\":\"push\"}}]}")
	require(t, scan(t, p, map[string]string{"component/a/a_test.go": code}), "REQ-WALK")
	require(t, scan(t, p, map[string]string{"component/a/a_test.go": code, "test/walk/walklib.py": "STEPS = ()"}), "REQ")
	t.Setenv("OBERTH_GUARDS_WALK_STEPS", "push")
	clean(t, scan(t, p, map[string]string{"test/walk/walklib.py": "STEPS = ()"}))
}

func TestSuffixSupportNeedsOnlyTestImportsAndNoMain(t *testing.T) {
	for _, tc := range []struct {
		name, consumer, body, want string
	}{
		{"test helper", "component/a/a_test.go", "package helper\nimport _ `example.invalid/unit/component/b`\n", ""},
		{"ordinary latest", "component/a/a.go", "package latest\n", ""},
		{"ordinary helper cannot use fakes", "component/a/a.go", "package helper\nimport _ `example.invalid/unit/component/b/fake`\n", "DEP-TEST"},
		{"main cannot use fakes", "component/a/a_test.go", "package main\nimport _ `example.invalid/unit/component/b/fake`\nfunc main(){}\n", "DEP-TEST"},
		{"inactive main cannot use fakes", "component/a/a_test.go", "//go:build ignore\n\npackage main\nimport _ `example.invalid/unit/component/b/fake`\n", "DEP-TEST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			helper := "component/b/runtest"
			if tc.name == "ordinary latest" {
				helper = "component/b/latest"
			}
			p.Allow["C1"] = append(p.Allow["C1"], helper)
			hits := scan(t, p, map[string]string{
				tc.consumer:      "package a\nimport _ \"example.invalid/unit/" + helper + "\"\n",
				helper + "/a.go": tc.body,
			})
			if tc.want == "" {
				clean(t, hits)
			} else {
				require(t, hits, tc.want)
			}
		})
	}
}

func TestWalkMarkerExplainsTheRequiredProofWithoutAWalk(t *testing.T) {
	p := fixture(t)
	write(t, "docs/plans/requirements.json", `{"requirements":[{"id":"REQ-C1-001","component":"C1","inspection":{"kind":"walk_step","name":"push"}}]}`)
	hits := scan(t, p, map[string]string{"component/a/a_test.go": "package a\nimport \"testing\"\n// Covers: REQ-C1-001\nfunc TestPush(t *testing.T){}\n"})
	require(t, hits, "REQ-WALK")
	if !strings.Contains(strings.Join(hits, "\n"), "this requirement uses walk step push; remove the marker") {
		t.Fatal(hits)
	}
}

func TestASTReportFindsMainAfterExternalTestsAndExcludesHarness(t *testing.T) {
	p := fixture(t)
	p.ProductionCommands = []string{"cmd/oberth", "cmd/absent"}
	p.Components["C1"] = append(p.Components["C1"], "cmd/oberth")
	p.Harness.Trees = []string{"tools/**"}
	clean(t, scan(t, p, map[string]string{
		"cmd/oberth/a_test.go": "package main_test\n",
		"cmd/oberth/main.go":   "// Command fixture follows its contract.\n// Spec: docs/plans/contract.md#behavior\npackage main\nfunc main(){}\n",
		"tools/main.go":        "package main\nfunc main(){}\n",
	}))
	c := inspect("example.invalid/unit", []byte("cmd/oberth/a_test.go\x00tools/main.go\x00cmd/oberth/main.go\x00"))
	if strings.Join(c.roots, ",") != "example.invalid/unit/cmd/oberth" {
		t.Fatal(c.roots)
	}
}

func TestDeclaredTestOnlyConsumersStayConfinedToTests(t *testing.T) {
	p := fixture(t)
	p.Packages["shared"] = struct {
		Owner, Kind string
		Consumers   []string
	}{"C2", "test_only", []string{"C1"}}
	code := `package a
import _ "example.invalid/unit/shared"
`
	clean(t, scan(t, p, map[string]string{"component/a/a_test.go": code}))
	require(t, scan(t, p, map[string]string{"component/a/a.go": code}), "DEP-CLOSED")
	require(t, scan(t, p, map[string]string{"component/b/b_test.go": code}), "DEP-CLOSED")
}

func TestAllComponentMessageCatalogues(t *testing.T) {
	owners := map[string][]source{}
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "../.." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		node, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		owner := filepath.Dir(path)
		owners[owner] = append(owners[owner], source{name: path, node: node})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) < 4 {
		t.Fatal("product catalogue discovery is incomplete")
	}
	for _, files := range owners {
		parameters := productParameters(files)
		for _, f := range files {
			inspectProductMessages(t, f, parameters)
		}
	}
}
func productParameters(files []source) map[string]map[int]int {
	result := map[string]map[int]int{}
	for _, f := range files {
		ast.Inspect(f.node, func(node ast.Node) bool {
			fn, ok := node.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				return true
			}
			limits := map[int]int{}
			index := 0
			for _, field := range fn.Type.Params.List {
				for _, name := range field.Names {
					limits[index] = map[string]int{"reason": 80, "remedy": 120}[name.Name]
					index++
				}
			}
			result[fn.Name.Name] = limits
			return false
		})
	}

	return result
}
func inspectProductMessages(t *testing.T, f source, parameters map[string]map[int]int) {
	t.Helper()
	ast.Inspect(f.node, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.CallExpr:
			productArguments(t, v.Args, productCallLimits(v, parameters), f.name)
		case *ast.ValueSpec:
			limits := map[int]int{}
			for i, name := range v.Names {
				limits[i] = productMessageLimit(name.Name)
			}
			productArguments(t, v.Values, limits, f.name)
		case *ast.CompositeLit:
			if strings.HasSuffix(f.name, "api/messages.go") {
				productArguments(t, v.Elts, map[int]int{1: 80, 2: 120}, f.name)
			}

		case *ast.KeyValueExpr:
			if key, ok := v.Key.(*ast.Ident); ok && key.Name == "DisplayReason" {
				productLiteral(t, v.Value, 40, f.name)
			}
		case *ast.AssignStmt:
			productStatusAssignment(t, v, f.name)
		case *ast.FuncDecl:
			productMethodReturns(t, v, f.name)
		}
		return true
	})
}
func productCallLimits(call *ast.CallExpr, parameters map[string]map[int]int) map[int]int {
	if name, ok := call.Fun.(*ast.Ident); ok {
		return parameters[name.Name]
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "New" {
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "publicerr" {
			return map[int]int{1: 80, 2: 120}
		}
	}
	return nil
}
func productMessageLimit(name string) int {
	if strings.HasSuffix(name, "StatusReason") {
		return 40
	}
	if strings.HasSuffix(name, "Reason") {
		return 80
	}
	if strings.HasSuffix(name, "Remedy") {
		return 120
	}
	return 0
}
func productStatusAssignment(t *testing.T, v *ast.AssignStmt, path string) {
	t.Helper()
	for i, lhs := range v.Lhs {
		if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "DisplayReason" && i < len(v.Rhs) {
			productLiteral(t, v.Rhs[i], 40, path)
		}
	}
}
func productMethodReturns(t *testing.T, fn *ast.FuncDecl, path string) {
	t.Helper()
	limit := productMessageLimit(fn.Name.Name)
	if limit == 0 {
		return
	}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if ret, ok := node.(*ast.ReturnStmt); ok {
			for _, value := range ret.Results {
				productLiteral(t, value, limit, path)
			}
		}
		return true
	})
}
func productLiteral(t *testing.T, expr ast.Expr, limit int, path string) {
	t.Helper()
	if limit == 0 {
		return
	}
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		value, _ := strconv.Unquote(lit.Value)
		if !productLength(value, limit) {
			t.Errorf("%s: message exceeds %d characters", path, limit)
		}
	} else if limit == 40 && !reasonFormatter(expr) {
		switch expr.(type) {
		case *ast.BinaryExpr, *ast.CallExpr:
			t.Errorf("%s: status reasons must come from a literal catalogue", path)
		}
	}
}

func productLength(value string, limit int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit
}
func TestProductLiteralLengthBoundary(t *testing.T) {
	for _, limit := range []int{40, 80, 120} {
		if !productLength(strings.Repeat("r", limit), limit) || productLength(strings.Repeat("r", limit+1), limit) {
			t.Fatal("message length boundary changed", limit)
		}
	}
}

func productArguments(t *testing.T, args []ast.Expr, limits map[int]int, path string) {
	t.Helper()
	for i, limit := range limits {
		if i < len(args) {
			productLiteral(t, args[i], limit, path)
		}
	}
}
