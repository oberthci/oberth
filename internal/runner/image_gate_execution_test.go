package runner

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerImageGateExecution(t *testing.T) {
	t.Run("clean valid gate", func(t *testing.T) {
		fixture := newRunnerImageGateFixture(t)
		result := fixture.run(t, "clean")
		if result.err != nil {
			t.Fatalf("gate failed: %v\n%s", result.err, result.output)
		}

		state := fixture.readState(t)
		if state.Status != "passed" || state.ExitCode != 0 {
			t.Fatalf("state = status %q exit %d, want passed exit 0", state.Status, state.ExitCode)
		}
		receiptSHA := sha256File(t, fixture.receiptPath)
		if state.ReceiptSHA256 != receiptSHA {
			t.Fatalf("state receipt SHA = %q, want %q", state.ReceiptSHA256, receiptSHA)
		}
		for _, path := range []string{fixture.ociPath, fixture.receiptPath, fixture.scanPath} {
			if info, err := os.Stat(path); err != nil || info.Size() == 0 {
				t.Fatalf("published artifact %q is missing or empty: %v", path, err)
			}
		}
		if targets := fixture.dockerTargets(t); strings.Join(targets, ",") != "runner,runner" {
			t.Fatalf("docker targets = %v, want exactly two minimal runner builds", targets)
		}
		invocations := fixture.dockerInvocations(t)
		if len(invocations) != 2 {
			t.Fatalf("docker invocation count = %d, want 2", len(invocations))
		}
		for index, invocation := range invocations {
			exactNoCache := false
			for _, argument := range strings.Fields(invocation) {
				if argument == "--no-cache" {
					exactNoCache = true
				}
				if strings.HasPrefix(argument, "--no-cache=") || strings.HasPrefix(argument, "--no-cache-filter") {
					t.Fatalf("runner reproducibility build %d contains a cache-bypass override: %q", index+1, invocation)
				}
			}
			if !exactNoCache {
				t.Fatalf("runner reproducibility build %d lacks the exact --no-cache argument: %q", index+1, invocation)
			}
			if strings.Contains(invocation, "BUILD_CACHE_NAMESPACE=") {
				t.Fatalf("minimal runner build %d retains fat-tool cache namespace: %q", index+1, invocation)
			}
		}
	})

	t.Run("dirty source is refused before Docker", func(t *testing.T) {
		fixture := newRunnerImageGateFixture(t)
		writeRunnerImageGateFile(t, filepath.Join(fixture.repo, "untracked.txt"), []byte("dirty\n"), 0o644)

		result := fixture.run(t, "clean")
		if result.err == nil {
			t.Fatal("gate unexpectedly accepted a dirty source tree")
		}
		if !strings.Contains(result.output, "refusing to authorize an image from a dirty source tree") {
			t.Fatalf("gate did not report the dirty-tree refusal:\n%s", result.output)
		}
		if targets := fixture.dockerTargets(t); len(targets) != 0 {
			t.Fatalf("dirty-tree gate invoked Docker targets %v", targets)
		}
		state := fixture.readState(t)
		if state.Status != "failed" || state.ExitCode == 0 || state.ReceiptSHA256 != "" {
			t.Fatalf("failed state = %+v", state)
		}
	})

	t.Run("expected Go target with empty packages fails", func(t *testing.T) {
		fixture := newRunnerImageGateFixture(t)
		result := fixture.run(t, "empty-expected-target")
		if result.err == nil {
			t.Fatal("gate unexpectedly accepted an expected Go target with no packages")
		}
		const omitted = "scan omitted usr/local/bin/oberth-runner"
		if !strings.Contains(result.output, omitted) {
			t.Fatalf("gate did not report %q:\n%s", omitted, result.output)
		}
		state := fixture.readState(t)
		if state.Status != "failed" || state.ExitCode == 0 || state.ReceiptSHA256 != "" {
			t.Fatalf("failed state = %+v", state)
		}
		for _, path := range []string{fixture.ociPath, fixture.receiptPath, fixture.scanPath} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("failed gate published %q: %v", path, err)
			}
		}
	})

	for _, test := range []struct {
		name string
		mode string
		want string
	}{
		{name: "Python OS package is refused", mode: "python-os-package", want: "forbidden baked package found: python3"},
		{name: "Python language inventory is refused", mode: "python-language-package", want: "forbidden Python package inventory found"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunnerImageGateFixture(t)
			result := fixture.run(t, test.mode)
			if result.err == nil {
				t.Fatalf("gate unexpectedly accepted %s", test.mode)
			}
			if !strings.Contains(result.output, test.want) {
				t.Fatalf("gate did not report %q:\n%s", test.want, result.output)
			}
			state := fixture.readState(t)
			if state.Status != "failed" || state.ExitCode == 0 || state.ReceiptSHA256 != "" {
				t.Fatalf("failed state = %+v", state)
			}
		})
	}

	t.Run("divergent repeated build is refused", func(t *testing.T) {
		fixture := newRunnerImageGateFixture(t)
		result := fixture.run(t, "divergent-rebuild")
		if result.err == nil {
			t.Fatal("gate unexpectedly accepted divergent repeated builds")
		}
		const mismatch = "runner image gate: repeated build produced a different manifest digest"
		if !strings.Contains(result.output, mismatch) {
			t.Fatalf("gate did not report %q:\n%s", mismatch, result.output)
		}
		state := fixture.readState(t)
		if state.Status != "failed" || state.ExitCode == 0 || state.ReceiptSHA256 != "" {
			t.Fatalf("failed state = %+v", state)
		}
		for _, path := range []string{fixture.ociPath, fixture.receiptPath, fixture.scanPath} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("failed gate published %q: %v", path, err)
			}
		}
	})

	t.Run("newer failure supersedes an older passing receipt", func(t *testing.T) {
		fixture := newRunnerImageGateFixture(t)
		first := fixture.run(t, "clean")
		if first.err != nil {
			t.Fatalf("initial gate failed: %v\n%s", first.err, first.output)
		}
		oldReceiptSHA := sha256File(t, fixture.receiptPath)
		passedState := fixture.readState(t)
		if passedState.Status != "passed" || passedState.ReceiptSHA256 != oldReceiptSHA {
			t.Fatalf("initial state = %+v, receipt SHA = %s", passedState, oldReceiptSHA)
		}

		second := fixture.run(t, "empty-expected-target")
		if second.err == nil {
			t.Fatal("newer invalid verification unexpectedly passed")
		}
		if got := sha256File(t, fixture.receiptPath); got != oldReceiptSHA {
			t.Fatalf("failed attempt changed older receipt SHA from %s to %s", oldReceiptSHA, got)
		}
		failedState := fixture.readState(t)
		if failedState.Status != "failed" || failedState.ExitCode == 0 || failedState.ReceiptSHA256 != "" {
			t.Fatalf("newer failed state = %+v", failedState)
		}
		authoritative := failedState.Status == "passed" && failedState.ExitCode == 0 && failedState.ReceiptSHA256 == oldReceiptSHA
		if authoritative {
			t.Fatal("older passing receipt remained authoritative after a newer failed attempt")
		}
	})
}

type runnerImageGateFixture struct {
	repo                 string
	gatePath             string
	binDir               string
	verifyTmpDir         string
	dockerLogPath        string
	dockerInvocationPath string
	ociFixture           string
	divergentOCIFixture  string
	trivyStub            string
	cleanScan            string
	emptyScan            string
	pythonOSScan         string
	pythonLanguageScan   string
	databaseDir          string
	configDigest         string
	evidenceDir          string
	ociPath              string
	receiptPath          string
	scanPath             string
	statePath            string
}

type runnerImageGateResult struct {
	output string
	err    error
}

type runnerImageGateState struct {
	Status        string `json:"status"`
	ExitCode      int    `json:"exitCode"`
	ReceiptSHA256 string `json:"receiptSHA256"`
}

func newRunnerImageGateFixture(t *testing.T) *runnerImageGateFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &runnerImageGateFixture{
		repo:                 filepath.Join(root, "repo"),
		binDir:               filepath.Join(root, "bin"),
		verifyTmpDir:         filepath.Join(root, "verify-tmp"),
		dockerLogPath:        filepath.Join(root, "docker-targets.log"),
		dockerInvocationPath: filepath.Join(root, "docker-invocations.log"),
		ociFixture:           filepath.Join(root, "fixtures", "runner.oci"),
		divergentOCIFixture:  filepath.Join(root, "fixtures", "runner-divergent.oci"),
		trivyStub:            filepath.Join(root, "fixtures", "trivy"),
		cleanScan:            filepath.Join(root, "fixtures", "scan-clean.json"),
		emptyScan:            filepath.Join(root, "fixtures", "scan-empty.json"),
		pythonOSScan:         filepath.Join(root, "fixtures", "scan-python-os.json"),
		pythonLanguageScan:   filepath.Join(root, "fixtures", "scan-python-language.json"),
		databaseDir:          filepath.Join(root, "fixtures", "trivy-db"),
		evidenceDir:          filepath.Join(root, "evidence"),
	}
	fixture.gatePath = filepath.Join(fixture.repo, "hack", "verify-runner-image.sh")
	fixture.ociPath = filepath.Join(fixture.evidenceDir, "runner.oci")
	fixture.receiptPath = filepath.Join(fixture.evidenceDir, "receipt.json")
	fixture.scanPath = filepath.Join(fixture.evidenceDir, "scan.json")
	fixture.statePath = filepath.Join(fixture.evidenceDir, "state.json")

	for _, dir := range []string{
		fixture.repo,
		fixture.binDir,
		fixture.verifyTmpDir,
		filepath.Dir(fixture.ociFixture),
		fixture.databaseDir,
		fixture.evidenceDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(fixture.verifyTmpDir, 0o700); err != nil {
		t.Fatal(err)
	}

	gate, err := os.ReadFile("../../hack/verify-runner-image.sh")
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerImageGateFile(t, fixture.gatePath, gate, 0o755)
	for path, contents := range map[string]string{
		"Dockerfile.runner":              "FROM scratch AS runner\n",
		"Dockerfile.runner.dockerignore": "",
		"go.mod":                         "module example.test/runner-gate\n\ngo 1.26\n",
		"go.sum":                         "",
		"cmd/oberth-runner/main.go":      "package main\nfunc main() {}\n",
		"internal/runner/runner.go":      "package runner\n",
		"pkg/periapsis/periapsis.go":     "package periapsis\n",
	} {
		writeRunnerImageGateFile(t, filepath.Join(fixture.repo, path), []byte(contents), 0o644)
	}

	commitTime := time.Date(2026, time.August, 5, 0, 0, 0, 0, time.UTC)
	fixture.configDigest = writeRunnerImageGateOCI(t, fixture.ociFixture, commitTime, "runner image gate fixture layer\n")
	writeRunnerImageGateOCI(t, fixture.divergentOCIFixture, commitTime, "divergent runner image gate fixture layer\n")
	writeRunnerImageGateScan(t, fixture.cleanScan, fixture.configDigest, "clean")
	writeRunnerImageGateScan(t, fixture.emptyScan, fixture.configDigest, "empty-expected-target")
	writeRunnerImageGateScan(t, fixture.pythonOSScan, fixture.configDigest, "python-os-package")
	writeRunnerImageGateScan(t, fixture.pythonLanguageScan, fixture.configDigest, "python-language-package")
	writeRunnerImageGateTrivyStub(t, fixture.trivyStub)
	writeRunnerImageGateDockerStub(t, filepath.Join(fixture.binDir, "docker"))
	writeRunnerImageGateFile(t, filepath.Join(fixture.databaseDir, "trivy.db"), []byte("fixture database\n"), 0o644)
	metadata := map[string]string{
		"UpdatedAt":  time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		"NextUpdate": time.Now().UTC().Add(12 * time.Hour).Format(time.RFC3339),
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerImageGateFile(t, filepath.Join(fixture.databaseDir, "metadata.json"), metadataJSON, 0o644)

	fixture.git(t, "init", "--quiet", "--initial-branch=main")
	fixture.git(t, "add", ".")
	commitDate := fmt.Sprintf("@%d +0000", commitTime.Unix())
	fixture.gitWithEnv(t, []string{"GIT_AUTHOR_DATE=" + commitDate, "GIT_COMMITTER_DATE=" + commitDate},
		"-c", "user.name=Runner Gate Test", "-c", "user.email=runner-gate@example.invalid",
		"commit", "--quiet", "-m", "fixture")
	return fixture
}

func (fixture *runnerImageGateFixture) run(t *testing.T, scanMode string) runnerImageGateResult {
	t.Helper()
	command := exec.Command(fixture.gatePath, fixture.ociPath, fixture.receiptPath, fixture.scanPath, fixture.statePath)
	command.Dir = fixture.repo
	command.Env = []string{
		"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv(runnerTestJQDirectoryEnv) + ":/usr/bin:/bin",
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"RUNNER_VERIFY_TMPDIR=" + fixture.verifyTmpDir,
		"RUNNER_TEST_DOCKER_LOG=" + fixture.dockerLogPath,
		"RUNNER_TEST_DOCKER_INVOCATIONS=" + fixture.dockerInvocationPath,
		"RUNNER_TEST_OCI=" + fixture.ociFixture,
		"RUNNER_TEST_OCI_DIVERGENT=" + fixture.divergentOCIFixture,
		"RUNNER_TEST_TRIVY=" + fixture.trivyStub,
		"RUNNER_TEST_SCAN_CLEAN=" + fixture.cleanScan,
		"RUNNER_TEST_SCAN_EMPTY=" + fixture.emptyScan,
		"RUNNER_TEST_SCAN_PYTHON_OS=" + fixture.pythonOSScan,
		"RUNNER_TEST_SCAN_PYTHON_LANGUAGE=" + fixture.pythonLanguageScan,
		"RUNNER_TEST_SCAN_MODE=" + scanMode,
		"RUNNER_TEST_TRIVY_DB=" + fixture.databaseDir,
	}
	output, err := command.CombinedOutput()
	return runnerImageGateResult{output: string(output), err: err}
}

func (fixture *runnerImageGateFixture) readState(t *testing.T) runnerImageGateState {
	t.Helper()
	raw, err := os.ReadFile(fixture.statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state runnerImageGateState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (fixture *runnerImageGateFixture) dockerTargets(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(fixture.dockerLogPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func (fixture *runnerImageGateFixture) dockerInvocations(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(fixture.dockerInvocationPath)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (fixture *runnerImageGateFixture) git(t *testing.T, args ...string) {
	t.Helper()
	fixture.gitWithEnv(t, nil, args...)
}

func (fixture *runnerImageGateFixture) gitWithEnv(t *testing.T, extraEnv []string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = fixture.repo
	command.Env = append([]string{
		"PATH=/usr/bin:/bin",
		"LANG=C",
		"LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}, extraEnv...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func writeRunnerImageGateDockerStub(t *testing.T, path string) {
	t.Helper()
	const stub = `#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$RUNNER_TEST_DOCKER_INVOCATIONS"
target=
output=
while test "$#" -gt 0; do
  case "$1" in
    --target)
      target=$2
      shift 2
      ;;
    --output)
      output=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
printf '%s\n' "$target" >>"$RUNNER_TEST_DOCKER_LOG"
case "$target" in
  runner)
    destination=${output#type=oci,dest=}
    destination=${destination%,rewrite-timestamp=true}
	fixture=$RUNNER_TEST_OCI
	if test "$RUNNER_TEST_SCAN_MODE" = "divergent-rebuild" &&
	   test "$(awk '$0 == "runner" { count++ } END { print count+0 }' "$RUNNER_TEST_DOCKER_LOG")" -eq 2; then
	  fixture=$RUNNER_TEST_OCI_DIVERGENT
	fi
	cp "$fixture" "$destination"
    ;;
  *)
    printf 'unexpected docker target: %s\n' "$target" >&2
    exit 2
    ;;
esac
`
	writeRunnerImageGateFile(t, path, []byte(stub), 0o755)
}

func writeRunnerImageGateTrivyStub(t *testing.T, path string) {
	t.Helper()
	const stub = `#!/bin/sh
set -eu
if test "${1:-}" = "--version"; then
  printf '%s\n' 'Version: 0.0.0-test'
  exit 0
fi
output=
cache=
while test "$#" -gt 0; do
  case "$1" in
    --output)
      output=$2
      shift 2
      ;;
    --cache-dir)
      cache=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
test -n "$output"
test -n "$cache"
mkdir -p "$cache/db"
cp "$RUNNER_TEST_TRIVY_DB/trivy.db" "$cache/db/trivy.db"
cp "$RUNNER_TEST_TRIVY_DB/metadata.json" "$cache/db/metadata.json"
scan=$RUNNER_TEST_SCAN_CLEAN
case "$RUNNER_TEST_SCAN_MODE" in
  empty-expected-target) scan=$RUNNER_TEST_SCAN_EMPTY ;;
  python-os-package) scan=$RUNNER_TEST_SCAN_PYTHON_OS ;;
  python-language-package) scan=$RUNNER_TEST_SCAN_PYTHON_LANGUAGE ;;
esac
cp "$scan" "$output"
`
	writeRunnerImageGateFile(t, path, []byte(stub), 0o755)
}

func writeRunnerImageGateScan(t *testing.T, path, configDigest, mode string) {
	t.Helper()
	packages := make([]map[string]string, 20)
	for index := range packages {
		packages[index] = map[string]string{"Name": fmt.Sprintf("alpine-package-%03d", index)}
	}
	if mode == "python-os-package" {
		packages = append(packages, map[string]string{"Name": "python3"})
	}
	results := []map[string]any{{
		"Target":   "runner-alpine",
		"Class":    "os-pkgs",
		"Type":     "alpine",
		"Packages": packages,
	}}
	for _, target := range []string{"usr/local/bin/oberth-runner"} {
		targetPackages := []map[string]string{{"Name": "go-module"}}
		if mode == "empty-expected-target" {
			targetPackages = []map[string]string{}
		}
		results = append(results, map[string]any{
			"Target":   target,
			"Class":    "lang-pkgs",
			"Type":     "gobinary",
			"Packages": targetPackages,
		})
	}
	if mode == "python-language-package" {
		results = append(results, map[string]any{
			"Target":   "usr/lib/python3.13/site-packages",
			"Class":    "lang-pkgs",
			"Type":     "python-pkg",
			"Packages": []map[string]string{{"Name": "fixture-python-package"}},
		})
	}
	report := map[string]any{
		"SchemaVersion": 2,
		"ArtifactName":  "fixture",
		"ArtifactType":  "container_image",
		"Metadata":      map[string]string{"ImageID": configDigest},
		"Results":       results,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerImageGateFile(t, path, raw, 0o644)
}

func writeRunnerImageGateOCI(t *testing.T, archivePath string, created time.Time, layerContents string) string {
	t.Helper()
	layoutDir := t.TempDir()
	layer := []byte(layerContents)
	layerDigest := runnerImageGateDigest(layer)
	config := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      created.UTC().Format(time.RFC3339),
		"config":       map[string]string{"User": "65534:65534"},
		"rootfs":       map[string]any{"type": "layers", "diff_ids": []string{layerDigest}},
	}
	configRaw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configDigest := runnerImageGateDigest(configRaw)
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    configDigest,
			"size":      len(configRaw),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.oci.image.layer.v1.tar",
			"digest":    layerDigest,
			"size":      len(layer),
		}},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := runnerImageGateDigest(manifestRaw)
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{{
			"mediaType": "application/vnd.oci.image.manifest.v1+json",
			"digest":    manifestDigest,
			"size":      len(manifestRaw),
			"platform":  map[string]string{"architecture": "amd64", "os": "linux"},
		}},
	}
	indexRaw, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerImageGateFile(t, filepath.Join(layoutDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644)
	writeRunnerImageGateFile(t, filepath.Join(layoutDir, "index.json"), indexRaw, 0o644)
	writeRunnerImageGateFile(t, filepath.Join(layoutDir, "blobs", "sha256", strings.TrimPrefix(layerDigest, "sha256:")), layer, 0o644)
	writeRunnerImageGateFile(t, filepath.Join(layoutDir, "blobs", "sha256", strings.TrimPrefix(configDigest, "sha256:")), configRaw, 0o644)
	writeRunnerImageGateFile(t, filepath.Join(layoutDir, "blobs", "sha256", strings.TrimPrefix(manifestDigest, "sha256:")), manifestRaw, 0o644)
	writeRunnerImageGateTar(t, layoutDir, archivePath, created)
	return configDigest
}

func writeRunnerImageGateTar(t *testing.T, sourceDir, archivePath string, modTime time.Time) {
	t.Helper()
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(archive)
	err = filepath.Walk(sourceDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == sourceDir {
			return nil
		}
		name, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(name)
		header.ModTime = modTime
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeRunnerImageGateFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func runnerImageGateDigest(contents []byte) string {
	digest := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
