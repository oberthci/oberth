package periapsis

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
)

const maxReleaseSecrets = 32
const maxSecretStoreSecrets = 32
const maxSecretStorePathBytes = 512

// SecretStoreDeclaration names one OpenBao KV path a repository requests for
// its release Jobs and the directory name its keys are delivered under.
type SecretStoreDeclaration struct {
	Name string
	Path string
}

// DeclaredReleaseSecrets returns the literal administrator-approved Secret
// names requested by a repository for release Jobs. The declaration is parsed,
// never evaluated in the Oberth process; yaegi remains confined to the Job.
//
// A release-capable repository declares exactly one top-level value:
//
//	var ReleaseSecrets = []string{"r2-upload-token", "cosign-secret"}
//
// An explicit empty slice is valid. Computed values are rejected so the
// server-side admission decision and the reviewed source cannot diverge.
func DeclaredReleaseSecrets(source string) ([]string, error) {
	if err := validateSource(source); err != nil {
		return nil, err
	}
	file, err := parser.ParseFile(token.NewFileSet(), ".oberth/periapsis.go", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse Periapsis release Secret declaration: %w", err)
	}
	var declaration *ast.ValueSpec
	for _, item := range file.Decls {
		general, ok := item.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, raw := range general.Specs {
			specification := raw.(*ast.ValueSpec)
			for _, name := range specification.Names {
				if name.Name != "ReleaseSecrets" {
					continue
				}
				if declaration != nil {
					return nil, fmt.Errorf("periapsis source must declare ReleaseSecrets exactly once")
				}
				declaration = specification
			}
		}
	}
	if declaration == nil {
		return nil, fmt.Errorf("periapsis source must declare release credentials as var ReleaseSecrets = []string{...}")
	}
	if len(declaration.Names) != 1 || declaration.Names[0].Name != "ReleaseSecrets" || len(declaration.Values) != 1 {
		return nil, fmt.Errorf("ReleaseSecrets must be a standalone []string literal")
	}
	literal, ok := declaration.Values[0].(*ast.CompositeLit)
	if !ok || !isStringSlice(literal.Type) {
		return nil, fmt.Errorf("ReleaseSecrets must be a []string literal")
	}
	if declaration.Type != nil && !isStringSlice(declaration.Type) {
		return nil, fmt.Errorf("ReleaseSecrets must have type []string")
	}
	if len(literal.Elts) > maxReleaseSecrets {
		return nil, fmt.Errorf("ReleaseSecrets declares %d names, maximum is %d", len(literal.Elts), maxReleaseSecrets)
	}
	result := make([]string, 0, len(literal.Elts))
	seen := make(map[string]struct{}, len(literal.Elts))
	for index, element := range literal.Elts {
		value, ok := element.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return nil, fmt.Errorf("ReleaseSecrets entry %d must be a string literal", index)
		}
		name, err := strconv.Unquote(value.Value)
		if err != nil || strings.TrimSpace(name) != name || name == "" || strings.ContainsAny(name, "\x00\r\n") {
			return nil, fmt.Errorf("ReleaseSecrets entry %d is invalid", index)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("ReleaseSecrets contains duplicate name %q", name)
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}

func isStringSlice(expression ast.Expr) bool {
	slice, ok := expression.(*ast.ArrayType)
	if !ok || slice.Len != nil {
		return false
	}
	element, ok := slice.Elt.(*ast.Ident)
	return ok && element.Name == "string"
}

// DeclaredSecretStoreSecrets returns the literal store-sourced release secret
// requests of a repository, sorted by delivery name. Like ReleaseSecrets, the
// declaration is parsed and never evaluated in the Oberth process.
//
// The declaration is optional. A release-capable repository that wants
// store-sourced credentials declares at most one top-level value:
//
//	var SecretStoreSecrets = map[string]string{
//	        "r2-token": "ci/data/r2-upload", // delivery name -> OpenBao KV path
//	}
//
// An absent declaration means zero secret store secrets. Computed keys or values are
// rejected so the server-side admission decision and the reviewed source
// cannot diverge.
func DeclaredSecretStoreSecrets(source string) ([]SecretStoreDeclaration, error) {
	if err := validateSource(source); err != nil {
		return nil, err
	}
	file, err := parser.ParseFile(token.NewFileSet(), ".oberth/periapsis.go", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse Periapsis secret store secret declaration: %w", err)
	}
	var declaration *ast.ValueSpec
	for _, item := range file.Decls {
		general, ok := item.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, raw := range general.Specs {
			specification := raw.(*ast.ValueSpec)
			for _, name := range specification.Names {
				if name.Name != "SecretStoreSecrets" {
					continue
				}
				if declaration != nil {
					return nil, fmt.Errorf("periapsis source must declare SecretStoreSecrets at most once")
				}
				declaration = specification
			}
		}
	}
	if declaration == nil {
		return nil, nil
	}
	if len(declaration.Names) != 1 || declaration.Names[0].Name != "SecretStoreSecrets" || len(declaration.Values) != 1 {
		return nil, fmt.Errorf("SecretStoreSecrets must be a standalone map[string]string literal")
	}
	literal, ok := declaration.Values[0].(*ast.CompositeLit)
	if !ok || !isStringStringMap(literal.Type) {
		return nil, fmt.Errorf("SecretStoreSecrets must be a map[string]string literal")
	}
	if declaration.Type != nil && !isStringStringMap(declaration.Type) {
		return nil, fmt.Errorf("SecretStoreSecrets must have type map[string]string")
	}
	if len(literal.Elts) > maxSecretStoreSecrets {
		return nil, fmt.Errorf("SecretStoreSecrets declares %d entries, maximum is %d", len(literal.Elts), maxSecretStoreSecrets)
	}
	result := make([]SecretStoreDeclaration, 0, len(literal.Elts))
	seenNames := make(map[string]struct{}, len(literal.Elts))
	seenPaths := make(map[string]struct{}, len(literal.Elts))
	for index, element := range literal.Elts {
		entry, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("SecretStoreSecrets entry %d must be a key/value pair", index)
		}
		name, err := storeLiteralString(entry.Key)
		if err != nil {
			return nil, fmt.Errorf("SecretStoreSecrets entry %d name: %w", index, err)
		}
		path, err := storeLiteralString(entry.Value)
		if err != nil {
			return nil, fmt.Errorf("SecretStoreSecrets entry %q path: %w", name, err)
		}
		if err := ValidateSecretStorePath(path); err != nil {
			return nil, fmt.Errorf("SecretStoreSecrets entry %q: %w", name, err)
		}
		if _, duplicate := seenNames[name]; duplicate {
			return nil, fmt.Errorf("SecretStoreSecrets contains duplicate name %q", name)
		}
		if _, duplicate := seenPaths[path]; duplicate {
			return nil, fmt.Errorf("SecretStoreSecrets contains duplicate path %q", path)
		}
		seenNames[name] = struct{}{}
		seenPaths[path] = struct{}{}
		result = append(result, SecretStoreDeclaration{Name: name, Path: path})
	}
	slices.SortFunc(result, func(left, right SecretStoreDeclaration) int {
		return strings.Compare(left.Name, right.Name)
	})
	return result, nil
}

// ValidateSecretStorePath enforces the syntax shared by repository declarations, the
// administrator allowlist, and the fetching client: a clean, relative,
// slash-separated KV API path with a conservative character set. Vault system
// backends are rejected outright; a release secret read never targets auth,
// sys, or identity endpoints.
func ValidateSecretStorePath(path string) error {
	if path == "" || len(path) > maxSecretStorePathBytes {
		return fmt.Errorf("secret store path must contain 1-%d bytes", maxSecretStorePathBytes)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("secret store path %q must use clean non-empty segments", path)
		}
		for _, character := range []byte(segment) {
			valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
				character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.'
			if !valid {
				return fmt.Errorf("secret store path %q may use only ASCII letters, digits, dots, dashes, underscores, and slashes", path)
			}
		}
	}
	first := path
	if index := strings.IndexByte(path, '/'); index >= 0 {
		first = path[:index]
	}
	switch first {
	case "auth", "sys", "identity", "cubbyhole":
		return fmt.Errorf("secret store path %q targets the reserved %q backend; only KV secret paths are allowed", path, first)
	}
	return nil
}

func storeLiteralString(expression ast.Expr) (string, error) {
	value, ok := expression.(*ast.BasicLit)
	if !ok || value.Kind != token.STRING {
		return "", fmt.Errorf("must be a string literal")
	}
	text, err := strconv.Unquote(value.Value)
	if err != nil || strings.TrimSpace(text) != text || text == "" || strings.ContainsAny(text, "\x00\r\n") {
		return "", fmt.Errorf("is invalid")
	}
	return text, nil
}

func isStringStringMap(expression ast.Expr) bool {
	mapType, ok := expression.(*ast.MapType)
	if !ok {
		return false
	}
	key, ok := mapType.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	value, ok := mapType.Value.(*ast.Ident)
	return ok && value.Name == "string"
}
