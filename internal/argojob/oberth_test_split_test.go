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

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestOberthTestNodeCacheAndFailures(t *testing.T) {
	command := exec.Command("python3", "-I", "-S", "-B", "../../hack/test-test-node.py")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("test Node launcher: %v\n%s", err, output)
	}
}

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
	packages := append(oberthTestPackages(t), "github.com/oberthci/oberth/internal/future-package")
	packageFile := filepath.Join(root, "packages")
	if err := os.WriteFile(packageFile, []byte(strings.Join(packages, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(root, "upstream")
	if err := os.Mkdir(upstream, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstream, "go.sum"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeGo := `#!/usr/bin/python3
import json
import os
import sys
args = sys.argv[1:]
if args == ["list", "./..."]:
    with open(os.environ["TEST_PACKAGE_LIST"]) as source:
        sys.stdout.write(source.read())
elif args == ["list", "-m", "-json", "github.com/argoproj/argo-workflows/v4"]:
    print(json.dumps({"Dir": os.environ["TEST_UPSTREAM_MODULE"]}))
elif args[:2] == ["mod", "edit"]:
    assert args[2].startswith("-replace=github.com/oberthci/oberth=")
    with open("edit-args.json", "w") as output:
        json.dump(args, output)
elif args[:1] == ["test"] and args[-1] == ".":
    assert os.path.realpath(os.environ["GOPATH"]) == os.path.join(os.getcwd(), "gopath")
    assert os.path.isfile("go.sum") and os.path.isfile("edit-args.json")
    with open("race_test.go") as source:
        proof = source.read()
    assert "TestReviewerStopRacePreservesGenuineFailure" in proof
    assert "TestReviewerShutdownEscalationKeepsCompletedStop" in proof
    with open(os.environ["TEST_GO_PROOF_ARGS"], "w") as output:
        json.dump(args, output)
    print(json.dumps(dict(Action="pass", Package="github.com/oberthci/oberth/hack/argo-shutdown-races")) if "-json" in args else "ISOLATED_PROOF_OUTPUT")
    if os.path.exists(os.path.join(os.environ["TEST_UPSTREAM_MODULE"], "fail-proof")):
        raise SystemExit(37)
elif args[:1] == ["test"]:
    with open(os.environ["TEST_GO_ARGS"], "w") as output:
        json.dump(args, output)
    print("ROOT_GROUP_OUTPUT")
else:
    raise SystemExit("unexpected Go invocation")
`
	if err := os.WriteFile(filepath.Join(root, "go"), []byte(fakeGo), 0o700); err != nil {
		t.Fatal(err)
	}
	env := func(argsFile, proofFile string) []string {
		return append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_PACKAGE_LIST="+packageFile, "TEST_GO_ARGS="+argsFile, "TEST_GO_PROOF_ARGS="+proofFile, "TEST_UPSTREAM_MODULE="+upstream)
	}
	for _, name := range []string{"service", "gitcache", "rest", "rest-json"} {
		group, jsonEvents := strings.TrimSuffix(name, "-json"), strings.HasSuffix(name, "-json")
		t.Run(name, func(t *testing.T) {
			argsFile, proofFile := filepath.Join(root, name+".json"), filepath.Join(root, name+"-proof.json")
			command := exec.Command("python3", "-I", "-S", "-B", ".oberth/test-packages.py", group)
			if jsonEvents {
				command.Args = append(command.Args, "--json")
			}
			command.Dir = repositoryRoot(t)
			command.Env = env(argsFile, proofFile)
			var stdout, stderr strings.Builder
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err := command.Run(); err != nil {
				t.Fatalf("launch %s: %v\n%s", group, err, stderr.String())
			}
			wantOutput := "ROOT_GROUP_OUTPUT\n"
			if jsonEvents {
				wantOutput = "{\"Action\": \"pass\", \"Package\": \"github.com/oberthci/oberth/hack/argo-shutdown-races\"}\n" + wantOutput
			}
			if stdout.String() != wantOutput {
				t.Fatal("proof output contaminated package events", stdout.String())
			}
			output, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			var actual []string
			if err = json.Unmarshal(output, &actual); err != nil {
				t.Fatal(err)
			}
			timeout := "-timeout=20m"
			switch group {
			case "service":
				timeout = "-timeout=60m"
			case "rest":
				timeout = "-timeout=55m"
			}
			want := []string{"test", "-race", "-gcflags=modernc.org/sqlite/...=-race=false", "-gcflags=modernc.org/libc/...=-race=false", "-v", "-count=1", "-parallel=2", "-p=2", timeout}
			if jsonEvents {
				want[4] = "-json"
			}
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
				t.Fatalf("Go invocation=%v, want %v", actual, want)
			}
			proof, err := os.ReadFile(proofFile)
			if group != "rest" {
				if !os.IsNotExist(err) {
					t.Fatal("unrelated race group ran the upstream proof", err)
				}
				return
			}
			if err != nil {
				t.Fatal("required Argo proof did not run", err)
			}
			var proofArgs []string
			if err = json.Unmarshal(proof, &proofArgs); err != nil {
				t.Fatal(err)
			}
			proofWant := []string{"test", "-mod=mod", "-race", "-gcflags=modernc.org/sqlite/...=-race=false", "-gcflags=modernc.org/libc/...=-race=false", "-count=1", "-p=2", "-timeout=3m", "-v", "."}
			if jsonEvents {
				proofWant[8] = "-json"
			}
			if !reflect.DeepEqual(proofArgs, proofWant) || (!jsonEvents && !strings.Contains(stderr.String(), "ISOLATED_PROOF_OUTPUT")) || (jsonEvents && stderr.Len() != 0) {
				t.Fatal("upstream race checks/output lost", proofArgs, stderr.String())
			}
		})
	}
	t.Run("upstream proof failure prevents package success", func(t *testing.T) {
		argsFile, proofFile := filepath.Join(root, "failed-root.json"), filepath.Join(root, "failed-proof.json")
		command := exec.Command("python3", "-I", "-S", "-B", ".oberth/test-packages.py", "rest", "--json")
		command.Dir = repositoryRoot(t)
		command.Env = env(argsFile, proofFile)
		if err := os.WriteFile(filepath.Join(upstream, "fail-proof"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr strings.Builder
		command.Stdout, command.Stderr = &stdout, &stderr
		if err := command.Run(); err == nil {
			t.Fatal("failed proof did not fail rest group", stdout.String(), stderr.String())
		}
		var event map[string]string
		for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatal("proof failure contaminated JSON events", err)
			}
		}
		if event["Action"] != "fail" || event["Package"] != "github.com/oberthci/oberth/hack/argo-shutdown-races" {
			t.Fatal("proof failure lost package identity", stdout.String())
		}
		if _, err := os.Stat(proofFile); err != nil {
			t.Fatal("failing proof was not executed", err)
		}
		if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
			t.Fatal("package group continued after failed proof", err)
		}
	})
}

func TestOberthRaceBudgetsPreserveKnownBounds(t *testing.T) {
	var workflow wfv1.Workflow
	if err := yaml.Unmarshal(loadPipeline(t, "build.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	// Store's successful whole-command wall maximum (2635.148s) requires
	// 66 minutes. Service and rest retain their successful whole-group maxima
	// (2068s from ff85317f and 2172.592s): max(1.5 * observed, observed + 120),
	// rounded up to minutes. Censored memory samples cannot reduce old bounds.
	want := map[string]int64{"test-store": 3960, "test-service": 3120, "test-rest": 3300}
	for _, template := range workflow.Spec.Templates {
		deadline, ok := want[template.Name]
		if !ok {
			continue
		}
		delete(want, template.Name)
		if template.ActiveDeadlineSeconds == nil || template.ActiveDeadlineSeconds.IntValue() < int(deadline) {
			t.Errorf("%s deadline = %v, need at least %ds", template.Name, template.ActiveDeadlineSeconds, deadline)
		}
		if template.Container == nil {
			t.Fatalf("%s has no container", template.Name)
		}
		// The old service bound was 4Gi; the old rest bound was 6208Mi.
		// An OOM at 2054Mi supplies no evidence to shrink either allocation.
		floor := resource.MustParse("4Gi")
		if template.Name == "test-rest" {
			floor = resource.MustParse("6208Mi")
		}
		if template.Container.Resources.Limits.Memory().Cmp(floor) < 0 {
			t.Errorf("%s memory limit = %s, below retained floor %s", template.Name, template.Container.Resources.Limits.Memory(), floor.String())
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing race templates: %v", want)
	}
	for _, template := range workflow.Spec.Templates {
		if template.Name == "setup" {
			for _, row := range template.Steps {
				for _, step := range row.Steps {
					if step.Template == "resource-guard" {
						return
					}
				}
			}
		}
	}
	t.Fatal("resource guard must run during setup")
}

func TestOberthSplitStagesUseTheRaceGroupLauncher(t *testing.T) {
	built, err := buildOberthPipelineForIsolation(t, pipeline.TriggerCI, loadPipeline(t, "build.yaml"))
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
			if seconds, measured := map[string]int32{"store": 3960, "service": 3120, "rest": 3300}[group]; measured {
				if template.ActiveDeadlineSeconds == nil || template.ActiveDeadlineSeconds.IntVal < seconds {
					t.Fatalf("%s step deadline = %v, need at least %d seconds", group, template.ActiveDeadlineSeconds, seconds)
				}
			}
			container := template.Container
			wantCommand := []string{"/usr/bin/python3"}
			wantArgs := []string{"-I", "-S", "-B", ".oberth/test-packages.py", group}
			if group == "rest" {
				wantCommand = []string{"/bin/sh"}
				wantArgs = append([]string{"hack/with-test-node.sh", "/bin/sh", "hack/bootstrap-acceptance/rest.sh", "/usr/bin/python3"}, wantArgs...)
			}
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
			if container == nil || !reflect.DeepEqual(container.Command, wantCommand) || !reflect.DeepEqual(container.Args, wantArgs) {
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

func TestOberthStorePartitionRunnerControls(t *testing.T) {
	command := exec.Command("python3", "-I", "-S", "-B", ".oberth/test_store_runner.py")
	command.Dir = repositoryRoot(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("store runner controls: %v\n%s", err, output)
	}
}
