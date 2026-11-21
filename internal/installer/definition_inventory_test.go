package installer

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	definitions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type submissionPackage struct {
	ImportPath, Dir, Export string
	GoFiles                 []string
}

// Inspect typed mutation references, including calls and captured method values.
// Read-only kinds and nested API structs do not imply submission coverage.
func TestSubmittedWorkflowKindsHavePayloads(t *testing.T) {
	t.Parallel()
	packages := submissionPackages(t)
	files := token.NewFileSet()
	lookup := func(path string) (io.ReadCloser, error) { return os.Open(packages[path].Export) }
	checker := types.Config{Importer: importer.ForCompiler(files, "gc", lookup)}
	kinds := map[string]bool{"WorkflowTaskResult": true} // The pinned executor submits this from its sidecar.
	for path, pkg := range packages {
		if !strings.HasPrefix(path, "github.com/oberthci/oberth/") {
			continue
		}
		syntax := submissionSyntax(t, files, pkg)
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
		if _, err := checker.Check(path, files, syntax, info); err != nil {
			t.Fatal(err)
		}
		for _, file := range syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if ok {
					collectSubmittedKinds(selector, info, kinds)
				}
				return true
			})
		}
	}
	names := make([]string, 0, len(kinds))
	for kind := range kinds {
		names = append(names, kind)
	}
	slices.Sort(names)
	t.Log("submitted workflow kinds:", names)
	if len(kinds) < 3 {
		t.Fatal("submission inventory missed product call sites", names)
	}
	checkUntypedSubmissionInventory(t, files, &checker)
	for _, kind := range names {
		requireSubmissionPayload(t, kind)
	}
}

func collectSubmittedKinds(selector *ast.SelectorExpr, info *types.Info, kinds map[string]bool) {
	if !slices.Contains([]string{"Create", "Update", "UpdateStatus", "Patch", "Apply", "ApplyStatus"}, selector.Sel.Name) {
		return
	}
	signature, ok := info.TypeOf(selector).(*types.Signature)
	if !ok {
		return
	}
	for _, tuple := range []*types.Tuple{signature.Params(), signature.Results()} {
		for i := 0; i < tuple.Len(); i++ {
			typ := tuple.At(i).Type()
			if pointer, ok := typ.(*types.Pointer); ok {
				typ = pointer.Elem()
			}
			named, ok := types.Unalias(typ).(*types.Named)
			if ok && named.Obj().Pkg() != nil {
				collectSubmissionType(named, kinds)
			}
		}
	}
}

func submissionPackages(t *testing.T) map[string]submissionPackage {
	t.Helper()
	command := exec.CommandContext(t.Context(), "go", "list", "-p=4", "-buildvcs=false", "-deps", "-export", "-json", "./cmd/...")
	command.Dir = "../.."
	var errors bytes.Buffer
	command.Stderr = &errors
	raw, err := command.Output()
	if err != nil {
		t.Fatalf("product package inventory: %v: %s", err, &errors)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	result := map[string]submissionPackage{}
	for decoder.More() {
		var pkg submissionPackage
		if err := decoder.Decode(&pkg); err != nil {
			t.Fatal(err)
		}
		result[pkg.ImportPath] = pkg
	}
	return result
}

func submissionSyntax(t *testing.T, files *token.FileSet, pkg submissionPackage) []*ast.File {
	t.Helper()
	var syntax []*ast.File
	for _, name := range pkg.GoFiles {
		file, err := parser.ParseFile(files, filepath.Join(pkg.Dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		syntax = append(syntax, file)
	}
	return syntax
}

func requireSubmissionPayload(t *testing.T, kind string) {
	t.Helper()
	if definitionPayload(nil, kind) != "schema missing" {
		t.Errorf("submitted %s has no payload in the compatibility check", kind)
	}
	keep := true
	schema := &definitions.CustomResourceValidation{OpenAPIV3Schema: &definitions.JSONSchemaProps{Type: "object", XPreserveUnknownFields: &keep}}
	if detail := definitionPayload(schema, kind); detail != "" {
		t.Errorf("submitted %s payload cannot be read and checked: %s", kind, detail)
	}
}

// Generic object writes cannot establish kind coverage. Keep product submissions typed.
func collectSubmissionType(named *types.Named, kinds map[string]bool) {
	switch named.Obj().Pkg().Path() {
	case "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1":
		kinds[named.Obj().Name()] = true
	case "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured", "k8s.io/apimachinery/pkg/runtime":
		kinds["untyped Kubernetes write: "+named.Obj().Name()] = true
	}
}
