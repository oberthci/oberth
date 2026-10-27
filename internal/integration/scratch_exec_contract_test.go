package integration_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/containerimage"
)

func TestServerImageContainerAndExecContract(t *testing.T) {
	testServerImageContainerAndExecContract(t, "")
}

func testServerImageContainerAndExecContract(t *testing.T, override string) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required to render the chart")
	}
	utility := containerimage.Utility
	if override != "" {
		utility = override
	}
	server := "example.invalid/oberth@sha256:" + strings.Repeat("a", 64)
	args := []string{"template", "oberth", "../../charts/oberth", "--set", "image.ref=" + server}
	if override != "" {
		args = append(args, "--set-string", "utilityImage="+utility)
	}
	output, err := exec.Command(helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("render chart: %v %s", err, output)
	}
	count := 0
	var inspect func(any)
	inspect = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if value["image"] == server {
				count++
				command, ok := value["command"].([]any)
				if !ok || len(command) == 0 || command[0] != "/usr/local/bin/oberth" {
					t.Errorf("server-image container has non-native argv: %v", value)
				}
			}
			if value["name"] == "prepare-caches" && value["image"] != utility {
				t.Errorf("cache init must use shared pinned utility image: %v", value["image"])
			}
			for _, child := range value {
				inspect(child)
			}
		case []any:
			for _, child := range value {
				inspect(child)
			}
		}
	}
	for _, document := range strings.Split(string(output), "\n---") {
		raw, err := yaml.YAMLToJSON([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		var object any
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		inspect(object)
	}
	if count == 0 {
		t.Fatal("no server-image container checked")
	}
	// Check explicit kubectl exec constructions across all first-party Go
	// callers, including installer/TUI/host administration. Dynamic wrappers
	// enforce the same invariant in containerimage.ServerCommand.
	for _, directory := range []string{"../../internal/installer", "../../internal/setuptui", "../../cmd/oberth"} {
		err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != directory {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			check := func(expressions []ast.Expr) {
				serverTarget := false
				for index, expression := range expressions {
					literal, ok := expression.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					if value == "deploy/oberth" || value == "oberth" {
						serverTarget = true
					}
					if serverTarget && value == "--" && index+1 < len(expressions) {
						next, ok := expressions[index+1].(*ast.BasicLit)
						if !ok {
							continue
						}
						command, err := strconv.Unquote(next.Value)
						if err == nil && command != "/usr/local/bin/oberth" {
							t.Errorf("%s executes %q in server image", path, command)
						}
					}
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.CallExpr:
					check(node.Args)
				case *ast.CompositeLit:
					check(node.Elts)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBothGitBackendsGateCIBuilds(t *testing.T) {
	body, err := os.ReadFile("../../.oberth/build.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Spec struct {
			Templates []struct {
				Name string
				DAG  struct {
					Tasks []struct{ Name, Template, Depends string }
				}
				Container struct{ Command, Args []string }
			}
		}
	}
	raw, err := yaml.YAMLToJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	lane := false
	gate := false
	for _, template := range workflow.Spec.Templates {
		if template.Name == "test-gitcache-backends" {
			lane = strings.Join(template.Container.Args, " ") == "hack/test-gitcache-backends.sh"
		}
		for _, task := range template.DAG.Tasks {
			if task.Name == "test-gitcache-backends" {
				gate = task.Template == "test-gitcache-backends" && task.Depends == "lint"
			}
			if strings.HasPrefix(task.Name, "build-") && !strings.Contains(task.Depends, "test-gitcache-backends") {
				t.Errorf("%s bypasses backend parity gate", task.Name)
			}
		}
	}
	if !lane || !gate {
		t.Fatalf("backend CI lane=%v gate=%v", lane, gate)
	}
}

func TestServerImageContainerAndExecContractWithMirror(t *testing.T) {
	testServerImageContainerAndExecContract(t, "registry.internal/busybox@sha256:"+strings.Repeat("b", 64))
}
