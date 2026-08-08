package integration_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// deployedPeriapsisCommit names the exact upstream-lineage commit whose
// Periapsis SDK runs inside the currently deployed server. This repository
// starts with fresh history, so that SDK is carried as the byte-exact
// testdata/deployed-sdk-2edaa393 snapshot (go.mod, go.sum, and pkg/periapsis
// archived from commit 2edaa39390a383b386458737194a8e7661cddffa, tree
// 9eead18e9ee89f2905a2faaab0080a110b69ca77) instead of being re-derived from
// local git objects.
const deployedPeriapsisCommit = "2edaa39390a383b386458737194a8e7661cddffa"

const repositoryToolPath = "/tmp/oberth-tools/bin:/usr/local/bin:/usr/bin:/bin"

var repositoryEnvironment = map[string]string{
	"PATH":                repositoryToolPath,
	"OBERTH_TOOLS_DIR":    "/tmp/oberth-tools",
	"HOME":                "/tmp",
	"GOPATH":              "/tmp/oberth-tools/gopath",
	"GOTOOLCHAIN":         "local",
	"GOWORK":              "off",
	"GOENV":               "off",
	"GOMODCACHE":          "/cache/gomod",
	"GOCACHE":             "/cache/gobuild",
	"GIT_TERMINAL_PROMPT": "0",
	"KUBEBUILDER_ASSETS":  "",
	"TRIVY_CACHE_DIR":     "/tmp/oberth-trivy",
	"GOLANGCI_LINT_CACHE": "/tmp/oberth-tools/cache/golangci-lint",
}

func TestReleaseTriggerSelectsEveryRepositoryOwnedSecurityGate(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	releaseSHA := strings.Repeat("b", 40)
	source, err := os.ReadFile(filepath.Join(repoRoot, ".oberth", "periapsis.go"))
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := periapsis.Interpret(string(source), periapsis.NewContext("oberth", "v1.2.3", releaseSHA, "v1.2.3"))
	if err != nil {
		t.Fatalf("interpret release pipeline: %v", err)
	}
	selected, err := periapsis.Select(*pipeline, periapsis.TriggerRelease)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := periapsis.OrderedBurns(selected)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(ordered))
	for _, burn := range ordered {
		if burn.Type != periapsis.Release {
			t.Fatalf("release selection admitted %s burn %q", burn.Type, burn.Name)
		}
		got = append(got, burn.Name)
	}
	want := []string{
		"release-setup", "release-lint", "release-scan", "release-test", "release-chart-test",
		"release-build", "release-publish-r2", "release-publish-images", "release-package-chart", "release-publish-chart",
		"release-verify", "release-finalize",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ordered release burns = %v, want %v", got, want)
	}
	assertGoPythonSetupStep(t, selected, "release-setup")
	assertRepositoryEnvironment(t, selected)
	assertInstallerDownloadCache(t, selected, 6)
	assertStaticReleaseHelperBuilds(t, selected)
	assertReleaseActionHeartbeats(t, selected)
}

func assertReleaseActionHeartbeats(t *testing.T, pipeline periapsis.Pipeline) {
	t.Helper()
	type actionTuple struct {
		burn    string
		step    string
		command string
		args    string
	}
	want := []actionTuple{
		{burn: "release-build", step: "build-reproducible-artifacts", command: "/tmp/oberth-tools/bin/oberth-release-support", args: "heartbeat\x00build-reproducible-artifacts\x00./.oberth/release.sh\x00build"},
		{burn: "release-publish-r2", step: "publish-signed-binaries", command: "/tmp/oberth-tools/bin/oberth-release-support", args: "heartbeat\x00publish-signed-binaries\x00./.oberth/release.sh\x00publish-r2"},
		{burn: "release-publish-images", step: "publish-scan-sign-images", command: "/tmp/oberth-tools/bin/oberth-release-support", args: "heartbeat\x00publish-scan-sign-images\x00./.oberth/release.sh\x00publish-images"},
		{burn: "release-package-chart", step: "package-digest-pinned-chart", command: "/tmp/oberth-tools/bin/oberth-release-support", args: "heartbeat\x00package-digest-pinned-chart\x00./.oberth/release.sh\x00package-chart"},
		{burn: "release-publish-chart", step: "publish-signed-chart", command: "/tmp/oberth-tools/bin/oberth-release-support", args: "heartbeat\x00publish-signed-chart\x00./.oberth/release.sh\x00publish-chart"},
		{burn: "release-verify", step: "verify-public-and-gar-release", command: "/tmp/oberth-tools/bin/oberth-release-support", args: "heartbeat\x00verify-public-and-gar-release\x00./.oberth/release.sh\x00verify"},
		{burn: "release-finalize", step: "advance-stable-aliases", command: "/tmp/oberth-tools/bin/oberth-release-support", args: "heartbeat\x00advance-stable-aliases\x00./.oberth/release.sh\x00finalize"},
	}
	got := make([]actionTuple, 0, len(want))
	for _, burn := range pipeline.Burns {
		for _, step := range burn.Steps {
			if step.Command == "./.oberth/release.sh" {
				if burn.Name != "release-setup" || step.Name != "validate-tag" || !slices.Equal(step.Args, []string{"validate"}) {
					t.Fatalf("%s/%s invokes release.sh outside the one validation step: %v", burn.Name, step.Name, step.Args)
				}
			}
			if step.Command == "/tmp/oberth-tools/bin/oberth-release-support" && len(step.Args) > 0 && step.Args[0] == "heartbeat" {
				if got := step.Env["OBERTH_RUNNER_OWNS_RELEASE_GROUP"]; got != "1" {
					t.Fatalf("%s/%s runner-owned process-group contract = %q, want 1", burn.Name, step.Name, got)
				}
				got = append(got, actionTuple{burn: burn.Name, step: step.Name, command: step.Command, args: strings.Join(step.Args, "\x00")})
			}
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("release heartbeat actions = %#v, want %#v", got, want)
	}
}

func assertStaticReleaseHelperBuilds(t *testing.T, pipeline periapsis.Pipeline) {
	t.Helper()
	want := map[string][]string{
		"build-release-support": {"build", "-trimpath", "-o", "/tmp/oberth-tools/bin/oberth-release-support", "./cmd/oberth-release-support"},
		"build-release-image":   {"build", "-trimpath", "-o", "/tmp/oberth-tools/bin/oberth-release-image", "./cmd/oberth-release-image"},
	}
	matches := make(map[string]int, len(want))
	releaseSetupMatches := 0
	for _, burn := range pipeline.Burns {
		if burn.Name != "release-setup" {
			continue
		}
		releaseSetupMatches++
		for _, step := range burn.Steps {
			arguments, required := want[step.Name]
			if !required {
				continue
			}
			matches[step.Name]++
			if step.Command != "/tmp/oberth-tools/bin/go" || !slices.Equal(step.Args, arguments) {
				t.Fatalf("%s command = %q %v", step.Name, step.Command, step.Args)
			}
			if got := step.Env["CGO_ENABLED"]; got != "0" {
				t.Fatalf("%s CGO_ENABLED = %q, want 0 for the minimal runner", step.Name, got)
			}
		}
	}
	if releaseSetupMatches != 1 {
		t.Fatalf("pipeline contains %d release-setup burns, want 1", releaseSetupMatches)
	}
	for name := range want {
		if matches[name] != 1 {
			t.Fatalf("release-setup contains %d %s steps, want 1", matches[name], name)
		}
	}
}

func assertGoPythonSetupStep(t *testing.T, pipeline periapsis.Pipeline, burnName string) {
	t.Helper()
	for _, burn := range pipeline.Burns {
		if burn.Name != burnName {
			continue
		}
		matches := 0
		for _, step := range burn.Steps {
			if step.Name != "install-go-python" {
				continue
			}
			matches++
			if step.Command != "./.oberth/install-tools.sh" || !slices.Equal(step.Args, []string{"go", "python"}) {
				t.Fatalf("%s install-go-python step = %#v", burnName, step)
			}
		}
		if matches != 1 {
			t.Fatalf("%s contains %d install-go-python steps, want 1", burnName, matches)
		}
		return
	}
	t.Fatalf("selected pipeline lacks %q burn", burnName)
}

func assertRepositoryEnvironment(t *testing.T, pipeline periapsis.Pipeline) {
	t.Helper()
	for _, burn := range pipeline.Burns {
		for _, step := range burn.Steps {
			for name, want := range repositoryEnvironment {
				if got := step.Env[name]; got != want {
					t.Fatalf("%s/%s %s = %q, want %q", burn.Name, step.Name, name, got, want)
				}
			}
			if !strings.Contains(step.Command, "/") {
				t.Fatalf("%s/%s command %q is resolved from the runner's ambient PATH", burn.Name, step.Name, step.Command)
			}
		}
	}
}

func assertInstallerDownloadCache(t *testing.T, pipeline periapsis.Pipeline, want int) {
	t.Helper()
	expected := "/cache/gobuild"
	matches := 0
	for _, burn := range pipeline.Burns {
		for _, step := range burn.Steps {
			if step.Command != "./.oberth/install-tools.sh" {
				continue
			}
			matches++
			if got := step.Env["OBERTH_DOWNLOAD_CACHE"]; got != expected {
				t.Fatalf("%s/%s OBERTH_DOWNLOAD_CACHE = %q, want %q", burn.Name, step.Name, got, expected)
			}
		}
	}
	if matches != want {
		t.Fatalf("pipeline contains %d cached installer steps, want %d", matches, want)
	}
}

func TestRepositoryPipelineStaysCompatibleWithDeployedSDKDuringSelfUpgrade(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	pipeline := readReleaseContractFile(t, filepath.Join(repoRoot, ".oberth", "periapsis.go"))
	for _, required := range []string{
		"predecessorLogProbeSpins = 10_000_000",
		"for spin := 0; spin < predecessorLogProbeSpins; spin++ {",
		"func Pipeline(ctx *oberth.Context) oberth.Pipeline {\n\tbridgeDeployedPredecessorLogProbe()",
		"Remove it as soon as a server containing a068d5a3 is live.",
	} {
		if !strings.Contains(pipeline, required) {
			t.Fatalf("self-upgrade pipeline lacks bounded deployed-predecessor bridge contract %q", required)
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), ".oberth/periapsis.go", pipeline, 0)
	if err != nil {
		t.Fatal(err)
	}
	usesReleaseSelector := false
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "Release" {
			usesReleaseSelector = true
		}
		return true
	})
	if usesReleaseSelector {
		t.Fatal("self-upgrade pipeline calls Release before the deployed SDK exports that builder method")
	}
	assertDeployedSDKInterpretsRepositoryPipeline(t, repoRoot)
}

func assertDeployedSDKInterpretsRepositoryPipeline(t *testing.T, repoRoot string) {
	t.Helper()
	absoluteRepoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	fixture := t.TempDir()
	copyDeployedSDKSnapshot(t, filepath.Join("testdata", "deployed-sdk-"+deployedPeriapsisCommit[:8]), fixture)

	compatibilityTest := `package periapsis

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestCandidatePipelineInterprets(t *testing.T) {
	t.Setenv("GOPATH", "/go")
	t.Setenv("KUBEBUILDER_ASSETS", "/seed/tools/envtest")
	t.Setenv("TRIVY_CACHE_DIR", "/seed/trivy")
	t.Setenv("GOLANGCI_LINT_CACHE", "/cache/golangci-lint")
	source, err := os.ReadFile(os.Getenv("OBERTH_CANDIDATE_PERIAPSIS"))
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := Interpret(string(source), NewContext("oberth", "v1.2.3", strings.Repeat("b", 40), "v1.2.3"))
	if err != nil {
		t.Fatalf("deployed SDK cannot interpret candidate pipeline: %v", err)
	}
	selected, err := Select(*pipeline, TriggerRelease)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(selected.Burns), 12; got != want {
		t.Fatalf("deployed SDK selected %d release burns, want %d", got, want)
	}
	wantEnvironment := map[string]string{
		"PATH": "/tmp/oberth-tools/bin:/usr/local/bin:/usr/bin:/bin",
		"OBERTH_TOOLS_DIR": "/tmp/oberth-tools", "HOME": "/tmp",
		"GOPATH": "/tmp/oberth-tools/gopath", "GOTOOLCHAIN": "local", "GOWORK": "off", "GOENV": "off",
		"GOMODCACHE": "/cache/gomod", "GOCACHE": "/cache/gobuild", "GIT_TERMINAL_PROMPT": "0",
		"KUBEBUILDER_ASSETS": "", "TRIVY_CACHE_DIR": "/tmp/oberth-trivy",
		"GOLANGCI_LINT_CACHE": "/tmp/oberth-tools/cache/golangci-lint",
	}
	wantHelperBuilds := map[string][]string{
		"build-release-support": {"build", "-trimpath", "-o", "/tmp/oberth-tools/bin/oberth-release-support", "./cmd/oberth-release-support"},
		"build-release-image": {"build", "-trimpath", "-o", "/tmp/oberth-tools/bin/oberth-release-image", "./cmd/oberth-release-image"},
	}
	type actionTuple struct {
		burn, step, command, args string
	}
	wantHeartbeats := []actionTuple{
		{"release-build", "build-reproducible-artifacts", "/tmp/oberth-tools/bin/oberth-release-support", "heartbeat\x00build-reproducible-artifacts\x00./.oberth/release.sh\x00build"},
		{"release-publish-r2", "publish-signed-binaries", "/tmp/oberth-tools/bin/oberth-release-support", "heartbeat\x00publish-signed-binaries\x00./.oberth/release.sh\x00publish-r2"},
		{"release-publish-images", "publish-scan-sign-images", "/tmp/oberth-tools/bin/oberth-release-support", "heartbeat\x00publish-scan-sign-images\x00./.oberth/release.sh\x00publish-images"},
		{"release-package-chart", "package-digest-pinned-chart", "/tmp/oberth-tools/bin/oberth-release-support", "heartbeat\x00package-digest-pinned-chart\x00./.oberth/release.sh\x00package-chart"},
		{"release-publish-chart", "publish-signed-chart", "/tmp/oberth-tools/bin/oberth-release-support", "heartbeat\x00publish-signed-chart\x00./.oberth/release.sh\x00publish-chart"},
		{"release-verify", "verify-public-and-gar-release", "/tmp/oberth-tools/bin/oberth-release-support", "heartbeat\x00verify-public-and-gar-release\x00./.oberth/release.sh\x00verify"},
		{"release-finalize", "advance-stable-aliases", "/tmp/oberth-tools/bin/oberth-release-support", "heartbeat\x00advance-stable-aliases\x00./.oberth/release.sh\x00finalize"},
	}
	helperMatches := make(map[string]int, len(wantHelperBuilds))
	gotHeartbeats := make([]actionTuple, 0, len(wantHeartbeats))
	releaseSetupMatches := 0
	installerMatches := 0
	for _, burn := range selected.Burns {
		if burn.Name == "release-setup" {
			releaseSetupMatches++
		}
		for _, step := range burn.Steps {
			for name, want := range wantEnvironment {
				if got := step.Env[name]; got != want {
					t.Fatalf("%s/%s %s = %q, want %q", burn.Name, step.Name, name, got, want)
				}
			}
			if !strings.Contains(step.Command, "/") {
				t.Fatalf("%s/%s command %q is resolved from the runner's ambient PATH", burn.Name, step.Name, step.Command)
			}
			if step.Command == "./.oberth/install-tools.sh" {
				installerMatches++
				want := "/cache/gobuild"
				if got := step.Env["OBERTH_DOWNLOAD_CACHE"]; got != want {
					t.Fatalf("%s/%s OBERTH_DOWNLOAD_CACHE = %q, want %q", burn.Name, step.Name, got, want)
				}
			}
			if step.Command == "./.oberth/release.sh" {
				if burn.Name != "release-setup" || step.Name != "validate-tag" || !reflect.DeepEqual(step.Args, []string{"validate"}) {
					t.Fatalf("%s/%s invokes release.sh outside the one validation step: %v", burn.Name, step.Name, step.Args)
				}
			}
			if step.Command == "/tmp/oberth-tools/bin/oberth-release-support" && len(step.Args) > 0 && step.Args[0] == "heartbeat" {
				if got := step.Env["OBERTH_RUNNER_OWNS_RELEASE_GROUP"]; got != "1" {
					t.Fatalf("%s/%s runner-owned process-group contract = %q, want 1", burn.Name, step.Name, got)
				}
				gotHeartbeats = append(gotHeartbeats, actionTuple{burn.Name, step.Name, step.Command, strings.Join(step.Args, "\x00")})
			}
			if burn.Name != "release-setup" {
				continue
			}
			arguments, required := wantHelperBuilds[step.Name]
			if !required {
				continue
			}
			helperMatches[step.Name]++
			if step.Command != "/tmp/oberth-tools/bin/go" || !reflect.DeepEqual(step.Args, arguments) {
				t.Fatalf("%s command = %q %v", step.Name, step.Command, step.Args)
			}
			if got := step.Env["CGO_ENABLED"]; got != "0" {
				t.Fatalf("%s CGO_ENABLED = %q, want 0 for the minimal runner", step.Name, got)
			}
		}
	}
	if releaseSetupMatches != 1 {
		t.Fatalf("deployed SDK selected %d release-setup burns, want 1", releaseSetupMatches)
	}
	if installerMatches != 6 {
		t.Fatalf("deployed SDK selected %d cached release installers, want 6", installerMatches)
	}
	for name := range wantHelperBuilds {
		if helperMatches[name] != 1 {
			t.Fatalf("release-setup contains %d %s steps, want 1", helperMatches[name], name)
		}
	}
	if !reflect.DeepEqual(gotHeartbeats, wantHeartbeats) {
		t.Fatalf("release heartbeat actions = %#v, want %#v", gotHeartbeats, wantHeartbeats)
	}
}

func TestCandidateCIPipelineInterprets(t *testing.T) {
	source, err := os.ReadFile(os.Getenv("OBERTH_CANDIDATE_PERIAPSIS"))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	pipeline, err := Interpret(string(source), NewContext("oberth", "main", sha, ""))
	if err != nil {
		t.Fatalf("deployed SDK cannot interpret candidate CI pipeline: %v", err)
	}
	selected, err := Select(*pipeline, TriggerCI)
	if err != nil {
		t.Fatal(err)
	}
	wantCache := "/cache/gobuild"
	installers := 0
	for _, burn := range selected.Burns {
		for _, step := range burn.Steps {
			if step.Command != "./.oberth/install-tools.sh" {
				continue
			}
			installers++
			if got := step.Env["OBERTH_DOWNLOAD_CACHE"]; got != wantCache {
				t.Fatalf("%s/%s OBERTH_DOWNLOAD_CACHE = %q, want %q", burn.Name, step.Name, got, wantCache)
			}
		}
	}
	if installers != 5 {
		t.Fatalf("deployed SDK selected %d cached CI installers, want 5", installers)
	}
}
`
	testPath := filepath.Join(fixture, "pkg", "periapsis", "candidate_compat_test.go")
	if err := os.WriteFile(testPath, []byte(compatibilityTest), 0o600); err != nil {
		t.Fatal(err)
	}
	candidatePath := filepath.Join(absoluteRepoRoot, ".oberth", "periapsis.go")
	command := exec.Command("go", "test", "-run", "^TestCandidate", "-count=1", "./pkg/periapsis")
	command.Dir = fixture
	command.Env = append(os.Environ(), "GOWORK=off", "OBERTH_CANDIDATE_PERIAPSIS="+candidatePath)
	runReleaseContractCommand(t, command, "interpret candidate pipeline with deployed Periapsis SDK")
}

// copyDeployedSDKSnapshot places the checked-in deployed-SDK module into a
// writable fixture so the compatibility test file can be added next to it.
func copyDeployedSDKSnapshot(t *testing.T, snapshotRoot, destinationRoot string) {
	t.Helper()
	err := filepath.WalkDir(snapshotRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(snapshotRoot, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(destinationRoot, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, body, 0o600)
	})
	if err != nil {
		t.Fatalf("copy deployed Periapsis SDK snapshot: %v", err)
	}
}

func runReleaseContractCommand(t *testing.T, command *exec.Cmd, context string) {
	t.Helper()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", context, err, output)
	}
}

func TestReleaseContractUsesMinimalRunnerAndImmutablePublication(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	installer := readReleaseContractFile(t, filepath.Join(repoRoot, ".oberth", "install-tools.sh"))
	release := readReleaseContractFile(t, filepath.Join(repoRoot, ".oberth", "release.sh"))
	pipeline := readReleaseContractFile(t, filepath.Join(repoRoot, ".oberth", "periapsis.go"))
	for _, required := range []string{
		`var ReleaseSecrets = []string{"gar-sa-key", "r2-upload-token", "cosign-secret"}`,
		`Args: []string{"zig"}`,
		`Args: []string{"golangci"}`,
		`Args: []string{"trivy"}`,
		`Args: []string{"helm"}`,
		`Args: []string{"cosign"}`,
		`WithEnv("OBERTH_RELEASE_TAG", ctx.Tag)`,
		`WithEnv("OBERTH_RELEASE_SHA", ctx.SHA)`,
	} {
		if !strings.Contains(pipeline, required) {
			t.Errorf("release Periapsis contract lacks %q", required)
		}
	}
	for _, required := range []string{
		"If-None-Match: *",
		"If-Match:",
		"r2-upload-token/token",
		"gar-sa-key/key.json",
		"cosign-secret/cosign.key",
		"oberth-release-image verify",
		"trivy image",
		"helm push",
		"chart-index-state",
		"refusing to overwrite different immutable object",
		"Stable Oberth release is",
		`chart_digest=$(sha256sum "$chart_path" | cut -d ' ' -f 1)`,
	} {
		if !strings.Contains(release, required) {
			t.Errorf("release script lacks %q", required)
		}
	}
	for _, forbidden := range []string{
		"set -x",
		"curl -k",
		"--insecure",
		strings.Join([]string{"docker", "run"}, " "),
		strings.Join([]string{"oberth", "lite"}, "-"),
		"/seed",
		"\n\tchart_digest=sha256:$(sha256sum",
	} {
		if strings.Contains(release, forbidden) || strings.Contains(pipeline, forbidden) {
			t.Errorf("release contract contains forbidden behavior %q", forbidden)
		}
	}
	if !strings.Contains(installer, "cosign-linux-amd64") || !strings.Contains(installer, "ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc") {
		t.Fatal("release tool installer does not bind the pinned cosign binary")
	}
}

func TestReleaseScriptsArePOSIXShell(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	for _, name := range []string{"install-tools.sh", "release.sh"} {
		path := filepath.Join(repoRoot, ".oberth", name)
		command := exec.Command("sh", "-n", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("sh -n %s: %v\n%s", name, err, output)
		}
	}
}

func TestReleaseValidationBindsTagToTheExactCheckedOutCommit(t *testing.T) {
	releaseScript, err := filepath.Abs(filepath.Join("..", "..", ".oberth", "release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := t.TempDir()
	runGit(t, fixture, nil, "init", "--initial-branch=main")
	runGit(t, fixture, nil, "config", "user.name", "Oberth release contract")
	runGit(t, fixture, nil, "config", "user.email", "release@example.invalid")
	if err := os.WriteFile(filepath.Join(fixture, "source.txt"), []byte("exact source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture, nil, "add", "source.txt")
	runGit(t, fixture, nil, "commit", "-m", "fixture")
	runGit(t, fixture, nil, "tag", "v1.2.3")
	commit := strings.TrimSpace(runGit(t, fixture, nil, "rev-parse", "HEAD"))
	runReleaseValidation := func(tag, sha string, wantSuccess bool) {
		t.Helper()
		command := exec.Command("sh", releaseScript, "validate")
		command.Dir = fixture
		command.Env = append(os.Environ(), "OBERTH_RELEASE_TAG="+tag, "OBERTH_RELEASE_SHA="+sha)
		output, err := command.CombinedOutput()
		if wantSuccess && err != nil {
			t.Fatalf("release validation failed: %v\n%s", err, output)
		}
		if !wantSuccess && err == nil {
			t.Fatalf("release validation accepted tag=%q sha=%q", tag, sha)
		}
	}
	runReleaseValidation("v1.2.3", commit, true)
	runReleaseValidation("release-1.2.3", commit, false)
	runReleaseValidation("v1.2.3", strings.Repeat("a", 40), false)
}

func readReleaseContractFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
