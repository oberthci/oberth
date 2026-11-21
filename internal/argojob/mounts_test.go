package argojob_test

import (
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Product preparation adds helper mounts. The controller projection test checks
// native mounts from the actual controller's generated Pods.
func TestProductMountsStayInsideAdmissionRoots(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "granted"} {
		t.Run(kind, func(t *testing.T) { checkProductMounts(t, kind) })
	}
}

func checkProductMounts(t *testing.T, kind string) {
	t.Helper()
	var prepared *admissioncontract.PreparedAdmission
	if kind == "granted" {
		var err error
		prepared, err = prepareSecretWorkflow(t, "forge/org/repo")
		if err != nil {
			t.Fatal(err)
		}
	} else {
		prepared, _, _ = fixtureKind(t, kind, nativebinding.Preparation{Assist: true, HoldTTL: time.Minute, HelperImage: "example.test/helper:1"})
	}
	raw, err := json.Marshal(prepared.View().Workflow)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err = json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	mounts := productMountPaths(t, document)
	if len(mounts) < 2 {
		t.Fatal("helper preparation added no mounts")
	}
	checkReservedProductMounts(t, mounts)
}

func checkReservedProductMounts(t *testing.T, mounts []string) {
	t.Helper()
	for _, mount := range mounts {
		cleaned := path.Clean(mount)
		if strings.HasPrefix(cleaned, "/var/run/") {
			cleaned = strings.TrimPrefix(cleaned, "/var")
		}
		contained := false
		for _, root := range append(argoworkflow.ReservedMountRoots(), "/oberth-hold") {
			if cleaned == root || strings.HasPrefix(cleaned, root+"/") {
				contained = true
				break
			}
		}
		if !contained {
			t.Errorf("product mount %q is outside delivery roots", mount)
		}
	}
}

// Walk all template/container kinds and static Pod patches without a second
// inventory of the mounts preparation adds.
func productMountPaths(t *testing.T, value any) []string {
	t.Helper()
	var paths []string
	switch v := value.(type) {
	case []any:
		for _, child := range v {
			paths = append(paths, productMountPaths(t, child)...)
		}
	case map[string]any:
		for key, child := range v {
			if key == "mountPath" {
				paths = append(paths, child.(string))
			}
			if key == "podSpecPatch" {
				paths = append(paths, productPatchMounts(t, child.(string))...)
			}
			paths = append(paths, productMountPaths(t, child)...)
		}
	}
	return paths
}

func productPatchMounts(t *testing.T, patch string) []string {
	t.Helper()
	if !strings.Contains(patch, "mountPath") {
		return nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(patch), &decoded); err != nil {
		t.Fatal("mount expression needs a concrete Pod fixture", err)
	}
	return productMountPaths(t, decoded)
}
