package argoworkflow

import (
	"crypto/sha256"
	"fmt"
	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// These controller decisions are unexported. Behavioral rows exercise our small
// adaptations; this guard requires a review when the pinned implementation changes.
func TestPinnedNativePlanningDecisions(t *testing.T) {
	t.Parallel()
	directory := controllertest.Module(t.Context(), t)
	for _, tc := range []struct{ file, function, digest string }{
		{"operator.go", "mergedTemplateDefaultsInto", "b57b4842aea61c03c407609b080db05c67309c4315fc92e38e6a7c6c42f59a30"},
		{"operator.go", "generateNodeName", "d72a1713568deab00ce600705848c9df7544242e9a9a781093b12a49e993bc6b"},
		{"operator.go", "processItem", "0fc3deb10579d3ee3d989071fbc9b9c3383e1d059e6968e1d2ee36e34af411c1"},
		{"dag.go", "executeDAGTask", "a0dbd94642f902fd19e0d53104fa790aef779a5a93415d8ec5172ee11d5d8186"},
		{"dag.go", "executeDAG", "4c41895a0786db49b2befc3fc062f201ae2d1919e9a0092c5801b91d4f7bb338"},
		{"steps.go", "executeStepGroup", "ded458163e6350859ea87dea640c43172ff246400ea0fe6c0935bce18100d415"},
	} {
		t.Run(tc.function, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(directory, "workflow/controller", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			fs := token.NewFileSet()
			file, err := parser.ParseFile(fs, tc.file, raw, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != tc.function {
					continue
				}
				actual := fmt.Sprintf("%x", sha256.Sum256(raw[fs.Position(fn.Pos()).Offset:fs.Position(fn.End()).Offset]))
				if actual != tc.digest {
					t.Fatalf("review the native decision and its behavioral rows: %s %s", tc.function, actual)
				}
				return
			}
			t.Fatal("native decision is missing")
		})
	}
}
