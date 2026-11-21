package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type remedyText struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
	Help string `json:"help,omitempty"`
}

func TestRemediesThatWork(t *testing.T) {
	root := filepath.Clean("../..")
	texts := remedyGoTexts(t, root)
	texts = append(texts, remedyHelpTexts(t)...)
	texts = append(texts, remedyOutcomeTexts(t)...)
	input, err := json.Marshal(map[string]any{"catalogue": remedyCatalogue(), "texts": texts})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "python3", "testdata/remedies/check.py", root)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatal("message/command contract failed:", err)
	}
}

func remedyGoTexts(t *testing.T, root string) []remedyText {
	t.Helper()
	var result []remedyText
	for _, directory := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && (entry.Name() == "testdata" || entry.Name() == ".cache") {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			result = append(result, remedyFileTexts(t, root, path)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func remedyFileTexts(t *testing.T, root, path string) []remedyText {
	t.Helper()
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatal(err)
	}
	var result []remedyText
	ast.Inspect(file, func(node ast.Node) bool {
		if _, ok := node.(*ast.ImportSpec); ok {
			return false
		}
		if field, ok := node.(*ast.Field); ok && field.Tag != nil {
			result = append(result, remedyTagTexts(t, field, positions.Position(field.Tag.Pos()), relative)...)
			return false
		}
		switch value := node.(type) {
		case *ast.BinaryExpr:
			if value.Op == token.ADD && strings.Contains(remedyExpression(value), "oberth ") {
				result = append(result, remedyText{filepath.ToSlash(relative), positions.Position(value.Pos()).Line, remedyExpression(value), ""})
				return false
			}
		case *ast.BasicLit:
			if value.Kind == token.STRING {
				text, err := strconv.Unquote(value.Value)
				if err != nil {
					t.Fatal(err)
				}
				result = append(result, remedyText{filepath.ToSlash(relative), positions.Position(value.Pos()).Line, text, ""})
			}
		}
		return true
	})
	return result
}

func remedyExpression(node ast.Expr) string {
	switch value := node.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			text, _ := strconv.Unquote(value.Value)
			return text
		}
	case *ast.BinaryExpr:
		if value.Op == token.ADD {
			return remedyExpression(value.X) + remedyExpression(value.Y)
		}
	}
	return "<value>"
}

func remedyHelpTexts(t *testing.T) []remedyText {
	t.Helper()
	var result []remedyText
	origin := remedyFunctionOrigin(t, "commands.go", "help")
	paths := []string{""}
	for path := range remedyCatalogue() {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		var out, diagnostic bytes.Buffer
		cli := application{out: &out, diagnostic: &diagnostic, in: strings.NewReader("")}
		args := append([]string{"help"}, strings.Fields(path)...)
		if cli.run(t.Context(), args) != 0 {
			t.Fatal(path, diagnostic.String())
		}
		result = append(result, remedyText{"cmd/oberth/commands.go", origin, out.String(), "help:" + path})
	}
	return result
}

// Raw names come from the same declarations as findCommand; their JSON tail
// uses the real generic tool syntax, with the tool-name argument consumed.
type remedySyntax struct {
	commandSyntax
	StdinJSON     bool            `json:"stdin_json"`
	PositionFlags map[string]bool `json:"position_flags,omitempty"`
	RequiredFlags []string        `json:"required_flags,omitempty"`
}

func remedyCatalogue() map[string]remedySyntax {
	custom := map[string]bool{}
	for _, row := range flowCommands() {
		custom[row.name] = true
	}
	for _, row := range installationCommands() {
		custom[row.name] = true
	}
	rows := map[string]remedySyntax{}
	for name, syntax := range commandCatalogue() {
		rows[name] = remedySyntax{commandSyntax: syntax}
	}
	for _, row := range commands() {
		if row.path != "" && !custom[row.path] {
			syntax := rows[row.path]
			syntax = remedyServiceSyntax(row, syntax)
			rows[row.path] = syntax
		}
		syntax := rows["tool"]
		syntax.Args[0]--
		syntax.Args[1]--
		syntax.Summary = row.definition.Summary
		rows["tool "+row.definition.Name] = syntax
	}
	return rows
}

func remedyFunctionOrigin(t *testing.T, path, name string) int {
	t.Helper()
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok && function.Name.Name == name {
			return positions.Position(function.Pos()).Line
		}
	}
	t.Fatal("display function is missing", name)
	return 0
}

func remedyTagTexts(t *testing.T, field *ast.Field, position token.Position, file string) []remedyText {
	t.Helper()
	text, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		t.Fatal(err)
	}
	tag := reflect.StructTag(text)
	var result []remedyText
	if name := strings.Split(tag.Get("json"), ",")[0]; name != "" && name != "-" {
		result = append(result, remedyText{filepath.ToSlash(file), position.Line, name, "field:" + name})
	}
	for _, key := range []string{"summary", "desc"} {
		if value := tag.Get(key); value != "" {
			result = append(result, remedyText{filepath.ToSlash(file), position.Line, value, ""})
		}
	}
	return result
}

func remedyServiceSyntax(row command, syntax remedySyntax) remedySyntax {
	syntax.StdinJSON = true
	syntax.PositionFlags = map[string]bool{}
	for _, field := range helpFields(row) {
		if field.sensitive {
			continue
		}
		if field.position > 0 {
			syntax.PositionFlags["--"+field.flag] = field.required
		} else if field.required {
			syntax.RequiredFlags = append(syntax.RequiredFlags, "--"+field.flag)
		}
	}
	return syntax
}
