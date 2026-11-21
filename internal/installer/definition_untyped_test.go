package installer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func checkUntypedSubmissionInventory(t *testing.T, files *token.FileSet, checker *types.Config) {
	t.Helper()
	for _, invocation := range []string{
		`client.Resource(schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflowtasksets"}).Namespace("ci").Create(ctx, object, meta.CreateOptions{})`,
		`client.Resource(resource).Namespace("ci").UpdateStatus(ctx, object, meta.UpdateOptions{})`,
		`port.Apply(ctx, object)`,
		`invoke := client.Resource(resource).Namespace("ci").Create; invoke(ctx, object, meta.CreateOptions{})`,
		`invoke := client.Resource(resource).Namespace("ci").UpdateStatus; invoke(ctx, object, meta.UpdateOptions{})`,
		`_ = client.Resource(resource).Namespace("ci").Update`,
		`_ = client.Resource(resource).Namespace("ci").Patch`,
		`_ = client.Resource(resource).Namespace("ci").Apply`,
		`_ = client.Resource(resource).Namespace("ci").ApplyStatus`,
		`invoke := port.Apply; invoke(ctx, object)`,
	} {
		source := `package probe
import (
 "context"
 "k8s.io/client-go/dynamic"
 meta "k8s.io/apimachinery/pkg/apis/meta/v1"
 "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
 "k8s.io/apimachinery/pkg/runtime/schema"
)
type writer interface { Apply(context.Context, *unstructured.Unstructured) error }
func write(ctx context.Context, client dynamic.Interface, resource schema.GroupVersionResource, object *unstructured.Unstructured, port writer) {
 _ = meta.CreateOptions{}
 ` + invocation + `
}`
		file, err := parser.ParseFile(files, "probe.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
		if _, err := checker.Check("probe", files, []*ast.File{file}, info); err != nil {
			t.Fatal(err)
		}
		kinds := map[string]bool{}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				collectSubmittedKinds(selector, info, kinds)
			}
			return true
		})
		if len(kinds) != 1 {
			t.Fatal("untyped write escaped inventory", invocation, kinds)
		}
		for kind := range kinds {
			if !strings.HasPrefix(kind, "untyped Kubernetes write:") || definitionPayload(nil, kind) == "schema missing" {
				t.Fatal("untyped write gained unchecked coverage", kind)
			}
		}
	}
}
