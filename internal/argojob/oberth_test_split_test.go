package argojob

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func oberthTestPackages(t *testing.T) []string {
	t.Helper()
	command := exec.Command("go", "list", "./...")
	command.Dir = repositoryRoot(t)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list repository packages: %v\n%s", err, output)
	}
	return strings.Fields(string(output))
}

func TestOberthRaceGroupsCoverEveryPackageExactlyOnce(t *testing.T) {
	command := exec.Command("python3", "-I", "-S", "-B", ".oberth/test-packages.py", "--list")
	command.Dir = repositoryRoot(t)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list race groups: %v\n%s", err, output)
	}
	var groups map[string][]string
	if err := json.Unmarshal(output, &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 4 {
		t.Fatalf("race groups = %v, want store, service, gitcache and rest", groups)
	}
	owners := map[string]string{}
	for _, name := range []string{"store", "service", "gitcache", "rest"} {
		if len(groups[name]) == 0 {
			t.Fatalf("empty race group %s", name)
		}
		for _, pkg := range groups[name] {
			if owner, found := owners[pkg]; found {
				t.Fatalf("package %s appears in both %s and %s", pkg, owner, name)
			}
			owners[pkg] = name
		}
	}
	var actual []string
	for pkg := range owners {
		actual = append(actual, pkg)
	}
	want := oberthTestPackages(t)
	sort.Strings(actual)
	sort.Strings(want)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("race package union = %v, want go list ./... = %v", actual, want)
	}
	for _, name := range []string{"store", "service", "gitcache"} {
		want := []string{"github.com/oberthci/oberth/internal/" + name}
		if !reflect.DeepEqual(groups[name], want) {
			t.Errorf("%s group = %v, want %v", name, groups[name], want)
		}
	}
}

func TestOberthRaceGroupLauncherPreservesChecks(t *testing.T) {
	root := t.TempDir()
	packages := oberthTestPackages(t)
	// A newly listed package must join the remainder without a pipeline edit.
	packages = append(packages, "github.com/oberthci/oberth/internal/future-package")
	packageFile := filepath.Join(root, "packages")
	if err := os.WriteFile(packageFile, []byte(strings.Join(packages, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goPath := filepath.Join(root, "go")
	fakeGo := `#!/usr/bin/python3
import json
import os
import sys
if sys.argv[1:] == ["list", "./..."]:
    with open(os.environ["TEST_PACKAGE_LIST"]) as source:
        sys.stdout.write(source.read())
elif sys.argv[1:2] == ["test"]:
    with open(os.environ["TEST_GO_ARGS"], "w") as output:
        json.dump(sys.argv[1:], output)
else:
    raise SystemExit("unexpected Go invocation")
`
	if err := os.WriteFile(goPath, []byte(fakeGo), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"store", "service", "gitcache", "rest"} {
		t.Run(group, func(t *testing.T) {
			argsFile := filepath.Join(root, group+".json")
			command := exec.Command("python3", "-I", "-S", "-B", ".oberth/test-packages.py", group)
			command.Dir = repositoryRoot(t)
			command.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_PACKAGE_LIST="+packageFile, "TEST_GO_ARGS="+argsFile)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("launch %s: %v\n%s", group, err, output)
			}
			output, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			var actual []string
			if err := json.Unmarshal(output, &actual); err != nil {
				t.Fatal(err)
			}
			want := []string{"test", "-race", "-v", "-count=1", "-p=2", "-timeout=20m"}
			for _, pkg := range packages {
				owner := "rest"
				for _, dedicated := range []string{"store", "service", "gitcache"} {
					if pkg == "github.com/oberthci/oberth/internal/"+dedicated {
						owner = dedicated
					}
				}
				if owner == group {
					want = append(want, pkg)
				}
			}
			if !reflect.DeepEqual(actual, want) {
				t.Fatalf("Go invocation = %v, want %v", actual, want)
			}
		})
	}
}

func TestOberthSplitBurnsUseTheRaceGroupLauncher(t *testing.T) {
	built, err := buildOberthPipelineForIsolation(t, periapsis.TriggerCI, loadPipeline(t, "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"store", "service", "gitcache", "rest"} {
		found := false
		for _, template := range built.Spec.Templates {
			if template.Name != "test-"+group {
				continue
			}
			found = true
			container := template.Container
			wantCommand := []string{"/usr/bin/python3"}
			wantArgs := []string{"-I", "-S", "-B", ".oberth/test-packages.py", group}
			if group == "gitcache" {
				// Retain the static-Git package contract before the dedicated
				// race suite, which must not also run in the rest group.
				wantCommand = []string{"/bin/sh"}
				wantArgs = []string{"hack/test-gitcache.sh"}
				script, err := os.ReadFile(filepath.Join(repositoryRoot(t), "hack/test-gitcache.sh"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(script), "/bin/sh hack/test-install-static-git-packages.sh\nexec python3 -I -S -B .oberth/test-packages.py gitcache\n") {
					t.Fatal("Git-cache gate must retain its package contract and race group launcher")
				}
			}
			if container == nil || !reflect.DeepEqual(usageOriginalCommand(container.Command), wantCommand) || !reflect.DeepEqual(container.Args, wantArgs) {
				t.Fatalf("%s does not invoke its race group", template.Name)
			}
			var cgo, procs string
			for _, variable := range container.Env {
				if variable.Name == "CGO_ENABLED" {
					cgo = variable.Value
				}
				if variable.Name == "GOMAXPROCS" {
					procs = variable.Value
				}
			}
			count, err := strconv.ParseInt(procs, 10, 64)
			if cgo != "1" || err != nil || count < 1 || count*1000 > container.Resources.Limits.Cpu().MilliValue() {
				t.Fatalf("%s needs CGO and processor concurrency bounded by its CPU limit", template.Name)
			}
		}
		if !found {
			t.Errorf("missing race burn test-%s", group)
		}
	}
}
