package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func workflowFixture(t *testing.T) string {
	t.Helper()
	dir := releaseBuildTree(t)
	for _, path := range []string{"shared"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Makefile", "hack/check.sh", "hack/steps.sh", "hack/lint-tool.sh", "hack/lint-version", "hack/release.sh", "hack/static.sh"} {
		copyFile(t, "../../"+name, filepath.Join(dir, name))
	}
	writeFixtureCommand(t, filepath.Join(dir, "bin/python3"), `if [ "$1" = -c ]; then exec "`+fixtureExecutable(t, "python3")+`" "$@"; fi
exit 0
`)
	writeFixtureCommand(t, filepath.Join(dir, "bin/curl"), `while [ "$#" -gt 0 ]; do
    if [ "$1" = --output ]; then shift; printf signer > "$1"; exit; fi
    shift
done
exit 1
`)
	writeFixtureCommand(t, filepath.Join(dir, "bin/sha256sum"), `read -r hash file
[ "$hash" = ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc ] && [ -s "$file" ]
`)
	writeFixtureCommand(t, filepath.Join(dir, "bin/git"), `case "$*" in
 *status*) printf '%s' "${DIRTY:-}";;
 *--short=12*) printf 000000000001;;
 *refs/tags*) printf '%s' "${TAG_COMMIT:-0000000000000000000000000000000000000000}";;
 *) printf '%s' "${CANDIDATE_COMMIT:-0000000000000000000000000000000000000000}";; esac
`)
	p := filepath.Join(dir, "bin/go")
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "case \"$*\" in", `case "$*" in
  *'version --short') cat hack/lint-version | sed 's/^v//'; exit;;
  'env GOARCH') echo amd64; exit;;
  'test '*) printf '%s' "${OBERTH_TEST_GIT:-}" > "$FIXTURE_ROOT/test-git"; exit;;
  'vet '*|tool\ *) exit;;`, 1))
	if err := os.WriteFile(p, body, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func executeWorkflowTemplate(t *testing.T, wf *wfv1.Workflow, template *wfv1.Template, values map[string]string) ([]byte, error) {
	t.Helper()
	dir := values["FIXTURE_ROOT"]
	container := template.Container
	executable := strings.ReplaceAll(container.Command[0], "/work/shared", dir+"/shared")
	command := exec.CommandContext(t.Context(), executable, append(container.Command[1:], container.Args...)...)
	command.Dir = dir
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command.Env = scriptEnv("PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "BASE_TEST_BINARY="+binary)
	for _, base := range []*wfv1.Template{wf.Spec.TemplateDefaults, template} {
		for _, env := range base.Container.Env {
			if env.Value != "" {
				command.Env = append(command.Env, env.Name+"="+strings.NewReplacer("/cache/", dir+"/cache/", "/work/shared", dir+"/shared").Replace(env.Value))
			}
		}
	}
	for key, value := range values {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.CombinedOutput()
}
func workflowValues(dir string) map[string]string {
	return map[string]string{"FIXTURE_ROOT": dir, "VERSION": "v1.2.3", "BUILD_LOG": filepath.Join(dir, "build.log")}
}
func TestWorkflowGoStepsExecuteTheirWrittenScripts(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range wf.GetTemplateByName("build").DAG.Tasks {
		if task.When != "" || task.Name == "prepare-base" || task.Name == "lint-and-guards-pass" {
			continue
		}
		t.Run(task.Name, func(t *testing.T) {
			dir, target := executeGoStep(t, wf, task)
			if task.Name == "release-bytes-build" {
				assertCompileCaches(t, dir, map[string]int{filepath.Join(dir, "cache", "build"): 6})
				assertCandidateVersion(t, dir)
			}
			if target == "test" {
				assertTestsUseBaseGit(t, dir)
			}
		})
	}
}

// The walk builds its own candidate bytes with the build's command.
func TestWalkByteBuildExecutesItsWrittenScript(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	dir := workflowFixture(t)
	values := workflowValues(dir)
	configureBaseFixture(t, dir)
	for _, name := range []string{"prepare-base", "release-bytes-build"} {
		if output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName(name), values); err != nil {
			t.Fatalf("walk %s failed: %v\n%s", name, err, output)
		}
	}
	assertCompileCaches(t, dir, map[string]int{filepath.Join(dir, "cache", "build"): 6})
	assertCandidateVersion(t, dir)
}

// executeGoStep runs one Go task's written script after its written predecessor.
func executeGoStep(t *testing.T, wf *wfv1.Workflow, task wfv1.DAGTask) (string, string) {
	t.Helper()
	dir := workflowFixture(t)
	values := workflowValues(dir)
	values["TARGET"] = task.Arguments.Parameters[0].Value.String()
	if task.Depends == "prepare-base" {
		configureBaseFixture(t, dir)
		if output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName("prepare-base"), values); err != nil {
			t.Fatal("written base predecessor failed", err, string(output))
		}
	}
	output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName(task.Template), values)
	if err != nil {
		t.Fatalf("workflow script failed: %v\n%s", err, output)
	}
	return dir, values["TARGET"]
}

// The pipeline's Git cache tests run the Git the release ships, not the build image's.
func assertTestsUseBaseGit(t *testing.T, dir string) {
	t.Helper()
	exported := filepath.Join(dir, "shared/git/git")
	body, err := os.ReadFile(exported)
	if err != nil || !bytes.Equal(body, fixtureGit(runtime.GOARCH)) {
		t.Fatal("test step did not export the checked base Git", err)
	}
	found, err := os.ReadFile(filepath.Join(dir, "test-git"))
	if err != nil || strings.TrimSpace(string(found)) != exported {
		t.Fatal("tests do not name the base Git as their test Git", err, string(found))
	}
}
func assertCompileCaches(t *testing.T, dir string, want map[string]int) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, line := range strings.Split(string(body), "\n") {
		cache, args, _ := strings.Cut(line, "|")
		if strings.HasSuffix(args, "./cmd/oberth") || strings.HasSuffix(args, "./cmd/oberth-source-plugin") {
			got[cache]++
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("wrong compilation passes or caches", got, want)
	}
}
func TestReleaseWorkflowExecutesTestedHeadGate(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	template := wf.GetTemplateByName("release-bytes-build")
	if !strings.HasPrefix(template.Container.Args[0], "bash hack/release.sh check\n") {
		t.Fatal("tested-head check is not first")
	}
	assertEmptyReleaseFacts(t, wf)
	for _, tc := range []struct {
		name    string
		allowed bool
	}{{"valid", true}, {"status", false}, {"default", true}, {"tested", false}, {"tag", false}} {
		which := tc.name
		t.Run(which, func(t *testing.T) {
			dir := workflowFixture(t)
			values := workflowValues(dir)
			commit := strings.Repeat("0", 40)
			values["RELEASE_BUILD_STATUS"], values["RELEASE_DEFAULT_COMMIT"], values["RELEASE_TESTED_COMMIT"] = "passed", commit, commit
			switch which {
			case "status":
				values["RELEASE_BUILD_STATUS"] = ""
			case "default":
				values["RELEASE_DEFAULT_COMMIT"] = ""
			case "tested":
				values["RELEASE_TESTED_COMMIT"] = ""
			case "tag":
				values["TAG_COMMIT"] = strings.Repeat("1", 40)
			}
			if tc.allowed {
				configureBaseFixture(t, dir)
				if output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName("prepare-base"), values); err != nil {
					t.Fatal("written base predecessor failed", err, string(output))
				}
			}
			output, err := executeWorkflowTemplate(t, wf, template, values)
			if (err == nil) != tc.allowed {
				t.Fatal("wrong tested-head behavior", which, err, string(output))
			}
			if tc.allowed {
				assertCompileCaches(t, dir, map[string]int{filepath.Join(dir, "cache", "build-a"): 6, filepath.Join(dir, "cache", "build-b"): 6})
			}
		})
	}
}
func TestWalkKeyUsesRunClaimAndPublicCloudInit(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	key := wf.GetTemplateByName("key")
	if len(key.Outputs.Parameters) != 1 || key.Outputs.Parameters[0].Name != "cloud-init" || key.Outputs.Parameters[0].ValueFrom.Path != "/work/shared/walk-key/cloud-init" || len(key.Outputs.Artifacts) != 0 {
		t.Fatal("private key is exported")
	}
	assertRunKeyMounts(t, wf)
	manifest := wf.GetTemplateByName("vm").Resource.Manifest
	if !strings.Contains(manifest, `userDataBase64: "{{=sprig.b64enc(inputs.parameters['cloud-init'])}}"`) {
		t.Fatal("public cloud-init is not safely encoded")
	}
	dir := t.TempDir()
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	args := append(slices.Clone(key.Container.Command[1:]), key.Container.Args...)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "/work/src", source)
	}
	command := exec.CommandContext(t.Context(), key.Container.Command[0], args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	assertKeyCloudInit(t, filepath.Join(dir, "walk-key"))
	if info, err := os.Stat(filepath.Join(dir, "walk-key/id_ed25519")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private-key mode", err)
	}
}

func assertEmptyReleaseFacts(t *testing.T, wf *wfv1.Workflow) {
	t.Helper()
	for _, name := range []string{"release-tested-commit", "release-default-commit", "release-build-status"} {
		for _, p := range wf.Spec.Arguments.Parameters {
			if p.Name == name && p.Value.String() != "" {
				t.Fatal("authored trusted-fact default", name)
			}
		}
	}
}

func assertKeyCloudInit(t *testing.T, dir string) {
	t.Helper()
	cloud, err := os.ReadFile(filepath.Join(dir, "cloud-init"))
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		SSHPwauth bool `json:"ssh_pwauth"`
		Users     []struct {
			LockPasswd bool     `json:"lock_passwd"`
			Keys       []string `json:"ssh_authorized_keys"`
		} `json:"users"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(cloud, []byte("#cloud-config\n")), &data); err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, "id_ed25519.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if data.SSHPwauth || len(data.Users) != 1 || !data.Users[0].LockPasswd || len(data.Users[0].Keys) != 1 || data.Users[0].Keys[0] != strings.TrimSpace(string(pub)) {
		t.Fatal("VM does not require this run's key")
	}
}

func TestReleaseCommandHasNoFixedDeadline(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	signalContext, fixed := false, false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if owner.Name == "signal" && selector.Sel.Name == "NotifyContext" {
			signalContext = true
		}
		if owner.Name == "context" && (selector.Sel.Name == "WithTimeout" || selector.Sel.Name == "WithDeadline") {
			fixed = true
		}
		return true
	})
	if !signalContext || fixed {
		t.Fatal("release command adds a deadline instead of using its signal context")
	}
}
func fixtureExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func assertCandidateVersion(t *testing.T, dir string) {
	t.Helper()
	if got := candidateBuildVersion(t, dir); got != "v0.0.0" {
		t.Fatal("invalid candidate version", got)
	}
}
func candidateBuildVersion(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	_, tail, ok := strings.Cut(string(body), "main.version=")
	if !ok {
		t.Fatal("candidate version was not passed to compilation")
	}
	return strings.Fields(tail)[0]
}

func TestWalkWorkflowRunsWrittenCommandAndCollectsFailure(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"0", "7", "missing-candidate"} {
		t.Run(status, func(t *testing.T) {
			source, run := t.TempDir(), t.TempDir()
			prepareWalkScriptFixture(t, source, run)
			if status == "missing-candidate" {
				if err := os.Remove(filepath.Join(run, "work/dist/oberth.tgz")); err != nil {
					t.Fatal(err)
				}
			}
			container := wf.GetTemplateByName("walk").Container
			command := exec.CommandContext(t.Context(), container.Command[0], append(container.Command[1:], container.Args...)...)
			command.Dir = source
			command.Env = append(os.Environ(), "PATH="+filepath.Join(run, "bin")+":"+os.Getenv("PATH"), "WALK_VM_ADDRESS=192.0.2.1", "WALK_SHARED="+filepath.Join(run, "work"), "SSH_CALLS="+filepath.Join(run, "calls"), "REMOTE_EVIDENCE="+filepath.Join(run, "remote"), "WALK_EXIT="+status)
			output, err := command.CombinedOutput()
			if (err == nil) != (status == "0") {
				t.Fatal("walk status lost", err, string(output))
			}
			if _, err := os.Stat(filepath.Join(run, "work/result/summary.json")); status != "missing-candidate" && err != nil {
				t.Fatal("walk evidence lost", err)
			}
			if _, err := os.Stat(filepath.Join(run, "work/walk-key/id_ed25519")); !os.IsNotExist(err) {
				t.Fatal("private key retained", err)
			}
		})
	}
}
func TestOnlyGoTasksReserveHeavyResources(t *testing.T) {
	for _, name := range []string{"build", "walk"} {
		wf, err := argoworkflow.Decode(ownWorkflow(t, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(wf.Spec.TemplateDefaults.Container.Resources.Requests) != 0 || len(wf.Spec.TemplateDefaults.Container.Resources.Limits) != 0 {
			t.Fatal("trivial tasks inherit heavy reservations", name)
		}
	}
	wf, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"key", "walk"} {
		if wf.GetTemplateByName(name).Container.Resources.Requests.Cpu().MilliValue() > 100 {
			t.Fatal("trivial task over-reserves CPU", name)
		}
	}
}

func prepareWalkScriptFixture(t *testing.T, source, run string) {
	t.Helper()
	for _, path := range []string{filepath.Join(source, "hack"), filepath.Join(run, "bin"), filepath.Join(run, "work/walk-key"), filepath.Join(run, "work/dist"), filepath.Join(run, "remote/result")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, "../../hack/walk-vm.sh", filepath.Join(source, "hack/walk-vm.sh"))
	for _, path := range []string{"work/walk-key/id_ed25519", "work/dist/oberth.tgz", "remote/result/summary.json"} {
		if err := os.WriteFile(filepath.Join(run, path), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureCommand(t, filepath.Join(run, "bin/ssh"), `printf '%s\n' "$*" >> "$SSH_CALLS"
case "$*" in
 *'tar -cf - -C "$HOME/evidence" result'*) exec tar -cf - -C "$REMOTE_EVIDENCE" result;;
 *'tar -xf -'*) cat >/dev/null;;
 *'test/walk/walk" --mode fresh'*) exit "$WALK_EXIT";;
esac
`)
}

func assertRunKeyMounts(t *testing.T, wf *wfv1.Workflow) {
	t.Helper()
	if len(wf.Spec.VolumeClaimTemplates) != 1 || wf.Spec.VolumeClaimTemplates[0].Name != "work" {
		t.Fatal("key must use the run's own claim")
	}
	if wf.GetTemplateByName("key").Container.WorkingDir != "/work/shared" {
		t.Fatal("key is generated outside the run claim")
	}
	for _, name := range []string{"key", "walk"} {
		found := false
		for _, mount := range wf.GetTemplateByName(name).Container.VolumeMounts {
			if mount.Name == "work" && mount.MountPath == "/work/shared" && !mount.ReadOnly {
				found = true
			}
		}
		if !found {
			t.Fatal("step does not use its run claim", name)
		}
	}
}
