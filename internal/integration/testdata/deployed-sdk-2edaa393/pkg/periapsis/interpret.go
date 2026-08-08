package periapsis

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
)

const (
	interpretTimeout = 5 * time.Second
	maxSourceBytes   = 1 << 20
)

// Interpret evaluates a //go:build ignore pipeline using only the Oberth SDK.
// It is intended to run in the short-lived Job process, never in the server.
func Interpret(source string, pipelineContext *Context) (*Pipeline, error) {
	return interpretWithTimeout(source, pipelineContext, interpretTimeout)
}

func interpretWithTimeout(source string, pipelineContext *Context, timeout time.Duration) (pipeline *Pipeline, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			pipeline = nil
			err = fmt.Errorf("periapsis evaluation panicked: %v", recovered)
		}
	}()
	if pipelineContext == nil {
		return nil, fmt.Errorf("periapsis context is required")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("interpret timeout must be greater than zero")
	}
	if err := validateSource(source); err != nil {
		return nil, err
	}

	interpreter := interp.New(interp.Options{
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
		Args:   []string{},
		Env:    []string{},
	})
	if err := interpreter.Use(sdkExports()); err != nil {
		return nil, fmt.Errorf("export Periapsis SDK: %w", err)
	}
	if err := interpreter.Use(interp.Exports{
		"oberthruntime/oberthruntime": {
			"Invocation": reflect.ValueOf(pipelineContext),
		},
	}); err != nil {
		return nil, fmt.Errorf("export invocation context: %w", err)
	}

	evalContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := interpreter.EvalWithContext(evalContext, source); err != nil {
		return nil, fmt.Errorf("interpret Periapsis source: %w", err)
	}
	if _, err := interpreter.EvalWithContext(evalContext, `import oberthruntime "oberthruntime"`); err != nil {
		return nil, fmt.Errorf("bind invocation context: %w", err)
	}
	value, err := interpreter.EvalWithContext(evalContext, "Pipeline(oberthruntime.Invocation)")
	if err != nil {
		return nil, fmt.Errorf("invoke Pipeline: %w", err)
	}
	pipelineValue, ok := value.Interface().(Pipeline)
	if !ok {
		return nil, fmt.Errorf("pipeline returned %T, expected oberth.Pipeline", value.Interface())
	}
	if err := Validate(pipelineValue); err != nil {
		return nil, fmt.Errorf("validate pipeline: %w", err)
	}
	return &pipelineValue, nil
}

func validateSource(source string) error {
	if len(source) > maxSourceBytes {
		return fmt.Errorf(".oberth/periapsis.go exceeds %d bytes", maxSourceBytes)
	}
	file, err := parser.ParseFile(token.NewFileSet(), ".oberth/periapsis.go", source, parser.SkipObjectResolution|parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse Periapsis source: %w", err)
	}
	if file.Name == nil || file.Name.Name != "main" {
		return fmt.Errorf("periapsis source package must be main")
	}
	if !hasIgnoreBuildConstraint(file) {
		return fmt.Errorf("periapsis source must begin with %q", "//go:build ignore")
	}
	foundSDK := false
	for _, importSpec := range file.Imports {
		path, err := strconv.Unquote(importSpec.Path.Value)
		if err != nil {
			return fmt.Errorf("parse import path: %w", err)
		}
		if path != "oberth" {
			return fmt.Errorf("import %q is not allowed; Periapsis source may import only %q", path, "oberth")
		}
		if importSpec.Name != nil {
			return fmt.Errorf("import %q must not use an alias", path)
		}
		foundSDK = true
	}
	if !foundSDK {
		return fmt.Errorf("periapsis source must import %q", "oberth")
	}
	return validatePipelineDeclaration(file)
}

func hasIgnoreBuildConstraint(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.End() > file.Package {
			continue
		}
		for _, comment := range group.List {
			if strings.TrimSpace(comment.Text) == "//go:build ignore" {
				return true
			}
		}
	}
	return false
}

func validatePipelineDeclaration(file *ast.File) error {
	declarations := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "Pipeline" {
			continue
		}
		declarations++
		if function.Recv != nil || function.Type.TypeParams != nil || !singleField(function.Type.Params, pointerSelector("oberth", "Context")) || !singleField(function.Type.Results, selector("oberth", "Pipeline")) {
			return fmt.Errorf("pipeline must have signature func Pipeline(ctx *oberth.Context) oberth.Pipeline")
		}
	}
	if declarations != 1 {
		return fmt.Errorf("periapsis source must declare exactly one Pipeline function")
	}
	return nil
}

func singleField(fields *ast.FieldList, match func(ast.Expr) bool) bool {
	if fields == nil || len(fields.List) != 1 {
		return false
	}
	field := fields.List[0]
	return len(field.Names) <= 1 && match(field.Type)
}

func pointerSelector(packageName, typeName string) func(ast.Expr) bool {
	return func(expression ast.Expr) bool {
		pointer, ok := expression.(*ast.StarExpr)
		return ok && selector(packageName, typeName)(pointer.X)
	}
}

func selector(packageName, typeName string) func(ast.Expr) bool {
	return func(expression ast.Expr) bool {
		selection, ok := expression.(*ast.SelectorExpr)
		if !ok || selection.Sel.Name != typeName {
			return false
		}
		identifier, ok := selection.X.(*ast.Ident)
		return ok && identifier.Name == packageName
	}
}

func sdkExports() interp.Exports {
	return interp.Exports{
		"oberth/oberth": {
			"New":             reflect.ValueOf(New),
			"NewContext":      reflect.ValueOf(NewContext),
			"Retrograde":      reflect.ValueOf(Retrograde),
			"Prograde":        reflect.ValueOf(Prograde),
			"Escaped":         reflect.ValueOf(Escaped),
			"Second":          reflect.ValueOf(Second),
			"Minute":          reflect.ValueOf(Minute),
			"Hour":            reflect.ValueOf(Hour),
			"Pipeline":        reflect.ValueOf((*Pipeline)(nil)),
			"Burn":            reflect.ValueOf((*Burn)(nil)),
			"Step":            reflect.ValueOf((*Step)(nil)),
			"BurnType":        reflect.ValueOf((*BurnType)(nil)),
			"Context":         reflect.ValueOf((*Context)(nil)),
			"GoSDK":           reflect.ValueOf((*GoSDK)(nil)),
			"LintSDK":         reflect.ValueOf((*LintSDK)(nil)),
			"ScanSDK":         reflect.ValueOf((*ScanSDK)(nil)),
			"PipelineBuilder": reflect.ValueOf((*PipelineBuilder)(nil)),
		},
	}
}
