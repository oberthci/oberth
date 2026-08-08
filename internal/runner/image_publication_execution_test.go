package runner

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const runnerPublicationRepository = "europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/cloudtaser-oberth-ci"

func TestRunnerImagePublicationExecution(t *testing.T) {
	t.Run("existing exact digest is adopted without push", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		result := fixture.run(t)
		if result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, "push"); got != 0 {
			t.Fatalf("push calls = %d, want 0", got)
		}
		fixture.requirePassedPublication(t, false, true)
		fixture.requireEphemeralCraneConfig(t)
	})

	t.Run("existing mismatched digest fails without push", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-mismatch")
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "immutable runner tag already resolves to") {
			t.Fatalf("mismatched tag result = %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, "push"); got != 0 {
			t.Fatalf("push calls = %d, want 0", got)
		}
		fixture.requireFailedPublication(t)
	})

	t.Run("missing tag is pushed then read back", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "push-success")
		result := fixture.run(t)
		if result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, "push"); got != 1 {
			t.Fatalf("push calls = %d, want 1", got)
		}
		fixture.requirePassedPublication(t, true, false)
	})

	t.Run("large transfers have separate bounded timeouts", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "push-success")
		fixture.registryTimeout = "2s"
		fixture.registryUploadTimeout = "7m"
		fixture.registryPullTimeout = "11m"
		result := fixture.run(t)
		if result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		if got := fixture.timeoutDurations(t, "push"); !slices.Equal(got, []string{"7m"}) {
			t.Fatalf("push timeouts = %v, want [7m]", got)
		}
		digestDurations := fixture.timeoutDurations(t, "digest")
		if len(digestDurations) == 0 {
			t.Fatal("digest did not run through a bounded timeout")
		}
		for _, duration := range digestDurations {
			if duration != "2s" {
				t.Fatalf("digest timeout = %q, want 2s", duration)
			}
		}
		if got := fixture.timeoutDurations(t, "pull"); !slices.Equal(got, []string{"11m", "11m"}) {
			t.Fatalf("pull timeouts = %v, want [11m 11m]", got)
		}
	})

	t.Run("uncertain push is reconciled by exact digest", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "uncertain-adopt")
		result := fixture.run(t)
		if result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, "push"); got != 1 {
			t.Fatalf("push calls = %d, want 1", got)
		}
		fixture.requirePassedPublication(t, true, true)
	})

	t.Run("persistent missing result has bounded retries", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "always-missing")
		result := fixture.run(t)
		if result.err == nil {
			t.Fatal("publication unexpectedly succeeded")
		}
		if got := fixture.craneCalls(t, "push"); got < 1 || got > 3 {
			t.Fatalf("push calls = %d, want 1..3", got)
		}
		fixture.requireFailedPublication(t)
	})

	t.Run("registry-controlled graph mismatch fails", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		fixture.remoteOCI = filepath.Join(fixture.root, "remote-corrupt.oci")
		writeRunnerPublicationOCI(t, fixture.remoteOCI, "corrupt-layer")
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "read-back") {
			t.Fatalf("corrupt graph result = %v\n%s", result.err, result.output)
		}
		fixture.requireFailedPublication(t)
	})

	t.Run("registry-controlled missing blob fails", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		fixture.remoteOCI = filepath.Join(fixture.root, "remote-missing.oci")
		writeRunnerPublicationOCI(t, fixture.remoteOCI, "missing-layer")
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "missing layer 0 blob") {
			t.Fatalf("missing blob result = %v\n%s", result.err, result.output)
		}
		fixture.requireFailedPublication(t)
	})

	t.Run("registry-controlled unreferenced blob fails", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		fixture.remoteOCI = filepath.Join(fixture.root, "remote-unreferenced.oci")
		writeRunnerPublicationOCI(t, fixture.remoteOCI, "unreferenced-blob")
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "missing or unreachable blobs") {
			t.Fatalf("unreferenced blob result = %v\n%s", result.err, result.output)
		}
		fixture.requireFailedPublication(t)
	})

	t.Run("unexpected registry layout member fails", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		fixture.remoteOCI = filepath.Join(fixture.root, "remote-extra.oci")
		writeRunnerPublicationOCI(t, fixture.remoteOCI, "unexpected-file")
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "unexpected OCI layout entry") {
			t.Fatalf("unexpected layout result = %v\n%s", result.err, result.output)
		}
		fixture.requireFailedPublication(t)
	})

	t.Run("global credential helper is rejected before registry access", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		writeRunnerPublicationFile(t, filepath.Join(fixture.dockerConfig, "config.json"), []byte(`{"credHelpers":{"europe-west4-docker.pkg.dev":"gcloud"}}`), 0o600)
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "direct europe-west4-docker.pkg.dev auth") {
			t.Fatalf("credential helper result = %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, ""); got != 0 {
			t.Fatalf("crane calls = %d, want 0", got)
		}
		fixture.requireFailedPublication(t)
		fixture.requireNoPublicationWorkdirs(t)
	})
}

func TestRunnerImagePublicationRejectsUnboundedTimeoutBeforeNetwork(t *testing.T) {
	fixture := newRunnerPublicationFixture(t, "existing-exact")
	fixture.registryTimeout = "153722867280912931m"
	result := fixture.run(t)
	if result.err == nil || !strings.Contains(result.output, "RUNNER_REGISTRY_TIMEOUT") {
		t.Fatalf("unbounded timeout result = %v\n%s", result.err, result.output)
	}
	if got := fixture.craneCalls(t, ""); got != 0 {
		t.Fatalf("crane calls = %d, want 0", got)
	}
	fixture.requireFailedPublication(t)
}

func TestRunnerImagePublicationRejectsUnboundedUploadTimeoutBeforeNetwork(t *testing.T) {
	fixture := newRunnerPublicationFixture(t, "existing-exact")
	fixture.registryUploadTimeout = "31m"
	result := fixture.run(t)
	if result.err == nil || !strings.Contains(result.output, "RUNNER_REGISTRY_UPLOAD_TIMEOUT") {
		t.Fatalf("unbounded upload timeout result = %v\n%s", result.err, result.output)
	}
	if got := fixture.craneCalls(t, ""); got != 0 {
		t.Fatalf("crane calls = %d, want 0", got)
	}
	fixture.requireFailedPublication(t)
}

func TestRunnerImagePublicationRejectsUnboundedPullTimeoutBeforeNetwork(t *testing.T) {
	for _, invalid := range []string{"31m", "1801s"} {
		t.Run(invalid, func(t *testing.T) {
			fixture := newRunnerPublicationFixture(t, "existing-exact")
			fixture.registryPullTimeout = invalid
			result := fixture.run(t)
			if result.err == nil || !strings.Contains(result.output, "RUNNER_REGISTRY_PULL_TIMEOUT") {
				t.Fatalf("unbounded pull timeout result = %v\n%s", result.err, result.output)
			}
			if got := fixture.craneCalls(t, ""); got != 0 {
				t.Fatalf("crane calls = %d, want 0", got)
			}
			fixture.requireFailedPublication(t)
		})
	}
}

func TestRunnerImageDeployGateRequiresCurrentPublication(t *testing.T) {
	t.Run("passing publication and live immutable refs authorize deployment", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		result := fixture.run(t)
		if result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		pin := fixture.runPin(t)
		if pin.err != nil {
			t.Fatalf("deployment gate failed: %v\n%s", pin.err, pin.output)
		}
		fixture.requireLatestPullRefs(t)
		if got := fixture.timeoutDurations(t, "pull"); !slices.Equal(got, []string{"11m", "11m", "11m", "11m"}) {
			t.Fatalf("publication and deployment pull timeouts = %v, want four 11m bounds", got)
		}
		digestDurations := fixture.timeoutDurations(t, "digest")
		if len(digestDurations) == 0 {
			t.Fatal("publication and deployment digest probes did not use bounded timeouts")
		}
		for _, duration := range digestDurations {
			if duration != "2s" {
				t.Fatalf("publication or deployment digest timeout = %q, want 2s", duration)
			}
		}
	})

	t.Run("seconds-form pull timeout reaches publication and deployment pulls", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		fixture.registryPullTimeout = "7s"
		if result := fixture.run(t); result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		if pin := fixture.runPin(t); pin.err != nil {
			t.Fatalf("deployment gate failed: %v\n%s", pin.err, pin.output)
		}
		if got := fixture.timeoutDurations(t, "pull"); !slices.Equal(got, []string{"7s", "7s", "7s", "7s"}) {
			t.Fatalf("publication and deployment pull timeouts = %v, want four 7s bounds", got)
		}
	})

	t.Run("unbounded pull timeout is rejected before deployment registry access", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		if result := fixture.run(t); result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		callsBefore := fixture.craneCalls(t, "")
		fixture.registryPullTimeout = "31m"
		pin := fixture.runPin(t)
		if pin.err == nil || !strings.Contains(pin.output, "RUNNER_REGISTRY_PULL_TIMEOUT") {
			t.Fatalf("deployment gate result = %v\n%s", pin.err, pin.output)
		}
		if got := fixture.craneCalls(t, ""); got != callsBefore {
			t.Fatalf("deployment gate made %d registry calls after invalid timeout, want 0", got-callsBefore)
		}
	})

	t.Run("timed-out pull fails once and removes partial state", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "pull-timeout")
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "tag read-back failed after 1 bounded attempt(s): registry command timed out after 11m") {
			t.Fatalf("timed-out pull result = %v\n%s", result.err, result.output)
		}
		if got := fixture.timeoutDurations(t, "pull"); !slices.Equal(got, []string{"11m"}) {
			t.Fatalf("pull timeout attempts = %v, want one 11m attempt", got)
		}
		fixture.requireFailedPublication(t)
		fixture.requireNoPublicationWorkdirs(t)
	})

	t.Run("newer failed publication invalidates older success", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		if result := fixture.run(t); result.err != nil {
			t.Fatalf("initial publication failed: %v\n%s", result.err, result.output)
		}
		fixture.scenario = "existing-mismatch"
		if result := fixture.run(t); result.err == nil {
			t.Fatal("mismatched publication unexpectedly succeeded")
		}
		callsBefore := fixture.craneCalls(t, "")
		pin := fixture.runPin(t)
		if pin.err == nil || !strings.Contains(pin.output, "latest publication attempt is not passing") {
			t.Fatalf("deployment gate result = %v\n%s", pin.err, pin.output)
		}
		if got := fixture.craneCalls(t, ""); got != callsBefore {
			t.Fatalf("deployment gate made %d registry calls after local publication failure, want 0", got-callsBefore)
		}
	})

	t.Run("live tag movement is rejected", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		if result := fixture.run(t); result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		fixture.scenario = "existing-mismatch"
		pin := fixture.runPin(t)
		if pin.err == nil || !strings.Contains(pin.output, "live immutable runner tag") {
			t.Fatalf("deployment gate result = %v\n%s", pin.err, pin.output)
		}
	})

	t.Run("live registry graph loss is rejected", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		if result := fixture.run(t); result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		fixture.remoteOCI = filepath.Join(fixture.root, "remote-missing-blob.oci")
		writeRunnerPublicationOCI(t, fixture.remoteOCI, "missing-layer")
		pin := fixture.runPin(t)
		if pin.err == nil || !strings.Contains(pin.output, "live tag OCI graph") {
			t.Fatalf("deployment gate result = %v\n%s", pin.err, pin.output)
		}
	})

	t.Run("live registry unreferenced blob is rejected", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		if result := fixture.run(t); result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		fixture.remoteOCI = filepath.Join(fixture.root, "remote-unreferenced-blob.oci")
		writeRunnerPublicationOCI(t, fixture.remoteOCI, "unreferenced-blob")
		pin := fixture.runPin(t)
		if pin.err == nil || !strings.Contains(pin.output, "live tag OCI graph") {
			t.Fatalf("deployment gate result = %v\n%s", pin.err, pin.output)
		}
	})

	t.Run("publication from another contract is rejected before network", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		if result := fixture.run(t); result.err != nil {
			t.Fatalf("publication failed: %v\n%s", result.err, result.output)
		}
		fixture.updateJSON(t, fixture.publication, func(value map[string]any) {
			value["publisher"].(map[string]any)["contractSHA256"] = strings.Repeat("0", 64)
		})
		callsBefore := fixture.craneCalls(t, "")
		pin := fixture.runPin(t)
		if pin.err == nil || !strings.Contains(pin.output, "latest publication attempt is not passing") {
			t.Fatalf("deployment gate result = %v\n%s", pin.err, pin.output)
		}
		if got := fixture.craneCalls(t, ""); got != callsBefore {
			t.Fatalf("deployment gate made %d registry calls for a foreign publication contract, want 0", got-callsBefore)
		}
	})
}

func TestRunnerImagePublicationAndDeployLocks(t *testing.T) {
	t.Run("publisher rejects a symlink lock directory without touching its target", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		target := filepath.Join(fixture.root, "lock-target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(target, "marker")
		sentinel := []byte("do not truncate\n")
		writeRunnerPublicationFile(t, marker, sentinel, 0o600)
		if err := os.Symlink(target, runnerPublicationLockDirectory(fixture)); err != nil {
			t.Fatal(err)
		}
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "runner lock directory must be a private directory") {
			t.Fatalf("symlink lock directory result = %v\n%s", result.err, result.output)
		}
		raw, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(sentinel) {
			t.Fatalf("lock directory target = %q, want sentinel", raw)
		}
		if got := fixture.craneCalls(t, ""); got != 0 {
			t.Fatalf("crane calls = %d, want 0", got)
		}
	})

	t.Run("publisher rejects an insecure lock directory", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		lockDirectory := runnerPublicationLockDirectory(fixture)
		if err := os.Mkdir(lockDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lockDirectory, 0o755); err != nil {
			t.Fatal(err)
		}
		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "runner lock directory must be owned by the current user with mode 0700") {
			t.Fatalf("insecure lock directory result = %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, ""); got != 0 {
			t.Fatalf("crane calls = %d, want 0", got)
		}
	})

	t.Run("competing publisher cannot replace active attempt state", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		sentinel := []byte("active publication owns this state\n")
		writeRunnerPublicationFile(t, fixture.publication, sentinel, 0o644)
		release := holdRunnerPublicationLock(t, runnerPublicationLockPath(t, fixture, "publication.lock"))
		defer release()

		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "another runner publication attempt holds") {
			t.Fatalf("competing publication result = %v\n%s", result.err, result.output)
		}
		got, err := os.ReadFile(fixture.publication)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(sentinel) {
			t.Fatalf("competing publisher replaced active state with %q", got)
		}
	})

	t.Run("publisher refuses concurrent evidence generation before network", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		lockPath := runnerPublicationLockPath(t, fixture, "evidence.lock")
		release := holdRunnerPublicationLock(t, lockPath)
		defer release()

		result := fixture.run(t)
		if result.err == nil || !strings.Contains(result.output, "evidence generation is still in progress") {
			t.Fatalf("publication result = %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, ""); got != 0 {
			t.Fatalf("crane calls = %d, want 0", got)
		}
		fixture.requireFailedPublication(t)
	})

	t.Run("deploy gate refuses an active publication", func(t *testing.T) {
		fixture := newRunnerPublicationFixture(t, "existing-exact")
		release := holdRunnerPublicationLock(t, runnerPublicationLockPath(t, fixture, "publication.lock"))
		defer release()

		result := fixture.runPin(t)
		if result.err == nil || !strings.Contains(result.output, "publication is still in progress") {
			t.Fatalf("deployment gate result = %v\n%s", result.err, result.output)
		}
		if got := fixture.craneCalls(t, ""); got != 0 {
			t.Fatalf("crane calls = %d, want 0", got)
		}
	})
}

func runnerPublicationLockDirectory(fixture *runnerPublicationFixture) string {
	repoSum := sha256.Sum256([]byte(fixture.repo))
	return filepath.Join(fixture.root, "oberth-runner-locks-"+hex.EncodeToString(repoSum[:]))
}

func runnerPublicationLockPath(t *testing.T, fixture *runnerPublicationFixture, name string) string {
	t.Helper()
	lockDirectory := runnerPublicationLockDirectory(fixture)
	if err := os.Mkdir(lockDirectory, 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if err := os.Chmod(lockDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(lockDirectory, name)
}

func TestRunnerEvidenceGateRejectsUnsafeOCIArchive(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	for _, path := range []string{
		"cmd/oberth-runner/main.go",
		"internal/gitoid/gitoid.go",
		"internal/runner/runner.go",
		"pkg/periapsis/types.go",
		"Dockerfile.runner",
		"Dockerfile.runner.dockerignore",
		"go.mod",
		"go.sum",
		"charts/oberth/values.yaml",
	} {
		writeRunnerPublicationFile(t, filepath.Join(repo, path), []byte(path+"\n"), 0o644)
	}
	for _, name := range []string{"verify-runner-evidence.sh", "verify-runner-image.sh", "runner-oci.py"} {
		source := filepath.Join("../../hack", name)
		raw, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		writeRunnerPublicationFile(t, filepath.Join(repo, "hack", name), raw, 0o755)
	}
	runRunnerPublicationCommand(t, repo, "git", "init", "-q")
	runRunnerPublicationCommand(t, repo, "git", "add", "--all")
	runRunnerPublicationCommand(t, repo, "git", "-c", "user.name=Oberth Test", "-c", "user.email=oberth@example.invalid", "commit", "-qm", "fixture")
	groupExecutable := filepath.Join(repo, "internal", "runner", "runner.go")
	if err := os.Chmod(groupExecutable, 0o654); err != nil {
		t.Fatal(err)
	}
	if status := strings.TrimSpace(runRunnerPublicationCommand(t, repo, "git", "status", "--porcelain=v1")); status != "" {
		t.Fatalf("group-execute-only mode change unexpectedly dirtied Git: %s", status)
	}
	revision := strings.TrimSpace(runRunnerPublicationCommand(t, repo, "git", "rev-parse", "HEAD"))
	epochRaw := strings.TrimSpace(runRunnerPublicationCommand(t, repo, "git", "show", "-s", "--format=%ct", "HEAD"))

	evidenceDir := filepath.Join(root, "evidence")
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	values := filepath.Join(repo, "charts", "oberth", "values.yaml")
	oci := filepath.Join(evidenceDir, "unsafe.oci")
	receipt := filepath.Join(evidenceDir, "receipt.json")
	scan := filepath.Join(evidenceDir, "scan.json")
	state := filepath.Join(evidenceDir, "state.json")
	writeUnsafeRunnerPublicationTar(t, oci)
	writeRunnerPublicationJSON(t, scan, map[string]any{})

	contextSHA := runnerEvidenceContextSHA(t, repo, root, epochRaw)
	receiptValue := map[string]any{
		"schema": 1,
		"passed": true,
		"source": map[string]any{
			"revision":        revision,
			"sourceDateEpoch": epochRaw,
			"dirty":           false,
			"gateSHA256":      runnerPublicationSHA256File(t, filepath.Join(repo, "hack", "verify-runner-image.sh")),
			"contextSHA256":   contextSHA,
		},
		"image": map[string]any{
			"dockerfileSHA256": runnerPublicationSHA256File(t, filepath.Join(repo, "Dockerfile.runner")),
			"archiveSHA256":    runnerPublicationSHA256File(t, oci),
		},
		"scanner": map[string]any{
			"databaseUpdatedAt": time.Now().UTC().Format(time.RFC3339),
			"reportSHA256":      runnerPublicationSHA256File(t, scan),
			"reportFile":        filepath.Base(scan),
			"exitCode":          0,
			"high":              0,
			"critical":          0,
			"secrets":           0,
		},
	}
	writeRunnerPublicationJSON(t, receipt, receiptValue)
	writeRunnerPublicationJSON(t, state, map[string]any{
		"schema":              1,
		"status":              "passed",
		"exitCode":            0,
		"receiptSHA256":       runnerPublicationSHA256File(t, receipt),
		"sourceRevision":      revision,
		"sourceContextSHA256": contextSHA,
	})

	command := exec.Command("sh", "-c", `umask 077; exec "$@"`, "runner-evidence-umask",
		filepath.Join(repo, "hack", "verify-runner-evidence.sh"), values, oci, receipt, scan, state)
	command.Dir = repo
	command.Env = append(os.Environ(), "RUNNER_VERIFY_TMPDIR="+root)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "unsafe OCI archive member") {
		t.Fatalf("unsafe local OCI result = %v\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatalf("unsafe OCI member escaped verification work directory: %v", err)
	}
}

func TestRunnerOCIHelperRejectsSubjectDescriptor(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "subject.oci")
	layout := filepath.Join(root, "layout")
	manifest := writeRunnerPublicationOCI(t, archive, "subject")
	helper, err := filepath.Abs("../../hack/runner-oci.py")
	if err != nil {
		t.Fatal(err)
	}
	extract := exec.Command("python3", "-I", "-S", "-B", helper, "extract", archive, layout, "subject fixture")
	if output, err := extract.CombinedOutput(); err != nil {
		t.Fatalf("extract subject fixture: %v\n%s", err, output)
	}
	validate := exec.Command("python3", "-I", "-S", "-B", helper, "validate", layout, manifest, "subject fixture")
	output, err := validate.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "subject descriptors are outside") {
		t.Fatalf("subject descriptor result = %v\n%s", err, output)
	}
}

func runnerEvidenceContextSHA(t *testing.T, repo, root, epoch string) string {
	t.Helper()
	contextRoot := filepath.Join(root, "context-hash")
	script := `set -eu
	umask 002
	repo=$1
work=$2
epoch=$3
mkdir -p "$work/context"
tar -C "$repo" -cf "$work/context.tar" -- \
  Dockerfile.runner Dockerfile.runner.dockerignore go.mod go.sum \
  cmd/oberth-runner internal/gitoid internal/runner pkg/periapsis
tar -C "$work/context" -xf "$work/context.tar"
find "$work/context" -type d -exec chmod 0755 {} +
find "$work/context" -type f -perm -0100 -exec chmod 0755 {} +
find "$work/context" -type f ! -perm -0100 -exec chmod 0644 {} +
(
  cd "$work/context" || exit 1
  find . -mindepth 1 | LC_ALL=C sort | while IFS= read -r entry; do
    if test -L "$entry"; then
      printf 'link %s %s\n' "$entry" "$(readlink "$entry")"
    elif test -d "$entry"; then
      printf 'dir %s %s\n' "$(stat -c '%a' "$entry")" "$entry"
    else
      printf 'file %s %s %s\n' "$(stat -c '%a' "$entry")" "$(sha256sum "$entry" | cut -d ' ' -f 1)" "$entry"
    fi
  done
) | sha256sum | cut -d ' ' -f 1
`
	command := exec.Command("sh", "-c", script, "runner-evidence-context", repo, contextRoot, epoch)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("hash runner context: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func runRunnerPublicationCommand(t *testing.T, directory, name string, arguments ...string) string {
	t.Helper()
	command := exec.Command(name, arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, output)
	}
	return string(output)
}

func holdRunnerPublicationLock(t *testing.T, path string) func() {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		t.Fatal(err)
	}
	return func() {
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			t.Errorf("unlock publication fixture: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Errorf("close publication fixture lock: %v", err)
		}
	}
}

func TestRunnerImagePublicationRejectsLocalMismatchBeforeNetwork(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *runnerPublicationFixture)
	}{
		{
			name: "state receipt hash",
			mutate: func(t *testing.T, fixture *runnerPublicationFixture) {
				fixture.updateJSON(t, fixture.state, func(value map[string]any) {
					value["receiptSHA256"] = strings.Repeat("0", 64)
				})
			},
		},
		{
			name: "receipt manifest",
			mutate: func(t *testing.T, fixture *runnerPublicationFixture) {
				fixture.updateJSON(t, fixture.receipt, func(value map[string]any) {
					value["image"].(map[string]any)["manifest"] = "sha256:" + strings.Repeat("f", 64)
				})
				fixture.refreshReceiptHash(t)
			},
		},
		{
			name: "chart digest",
			mutate: func(t *testing.T, fixture *runnerPublicationFixture) {
				writeRunnerPublicationFile(t, fixture.values, []byte("runnerImage:\n  ref: \""+runnerPublicationRepository+"@sha256:"+strings.Repeat("e", 64)+"\"\n"), 0o644)
			},
		},
		{
			name: "chart repository prefix",
			mutate: func(t *testing.T, fixture *runnerPublicationFixture) {
				writeRunnerPublicationFile(t, fixture.values, []byte("runnerImage:\n  ref: \"example.invalid/runner@"+fixture.manifest+"\"\n"), 0o644)
			},
		},
		{
			name: "unsafe local OCI archive",
			mutate: func(t *testing.T, fixture *runnerPublicationFixture) {
				writeUnsafeRunnerPublicationTar(t, fixture.oci)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRunnerPublicationFixture(t, "existing-exact")
			test.mutate(t, fixture)
			result := fixture.run(t)
			if result.err == nil {
				t.Fatalf("publication unexpectedly accepted %s mismatch", test.name)
			}
			if got := fixture.craneCalls(t, ""); got != 0 {
				t.Fatalf("crane calls = %d, want 0\n%s", got, result.output)
			}
			fixture.requireFailedPublication(t)
		})
	}
}

type runnerPublicationFixture struct {
	root                  string
	repo                  string
	binDir                string
	artifacts             string
	values                string
	oci                   string
	receipt               string
	scan                  string
	state                 string
	publication           string
	dockerConfig          string
	craneLog              string
	timeoutLog            string
	craneStateDir         string
	manifest              string
	remoteOCI             string
	scenario              string
	registryTimeout       string
	registryUploadTimeout string
	registryPullTimeout   string
}

type runnerPublicationResult struct {
	output string
	err    error
}

func newRunnerPublicationFixture(t *testing.T, scenario string) *runnerPublicationFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	fixture := &runnerPublicationFixture{
		root:                  root,
		repo:                  repo,
		binDir:                filepath.Join(repo, "bin"),
		artifacts:             filepath.Join(root, "repo", "artifacts"),
		dockerConfig:          filepath.Join(root, "caller-docker-config"),
		craneLog:              filepath.Join(root, "crane.log"),
		timeoutLog:            filepath.Join(root, "timeout.log"),
		craneStateDir:         filepath.Join(root, "crane-state"),
		scenario:              scenario,
		registryTimeout:       "2s",
		registryUploadTimeout: "7m",
		registryPullTimeout:   "11m",
	}
	fixture.values = filepath.Join(fixture.repo, "charts", "oberth", "values.yaml")
	fixture.oci = filepath.Join(fixture.repo, "dist", "oberth-runner.oci")
	fixture.receipt = filepath.Join(fixture.artifacts, "runner-image-receipt.json")
	fixture.scan = filepath.Join(fixture.artifacts, "runner-image-scan.json")
	fixture.state = filepath.Join(fixture.artifacts, "runner-image-state.json")
	fixture.publication = filepath.Join(fixture.artifacts, "runner-image-publication.json")
	for _, dir := range []string{
		filepath.Join(fixture.repo, "hack"), filepath.Dir(fixture.values), filepath.Dir(fixture.oci),
		fixture.artifacts, fixture.binDir, fixture.dockerConfig, fixture.craneStateDir,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	script, err := os.ReadFile("../../hack/publish-runner-image.sh")
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerPublicationFile(t, filepath.Join(fixture.repo, "hack", "publish-runner-image.sh"), script, 0o755)
	ociHelper, err := os.ReadFile("../../hack/runner-oci.py")
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerPublicationFile(t, filepath.Join(fixture.repo, "hack", "runner-oci.py"), ociHelper, 0o755)
	writeRunnerPublicationFile(t, filepath.Join(fixture.repo, "hack", "verify-runner-evidence.sh"), []byte(`#!/bin/sh
set -eu
test "$#" -eq 5
grep -F 'europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/cloudtaser-oberth-ci@sha256:' "$1" >/dev/null
test -s "$2"
test -s "$3"
test -s "$4"
test -s "$5"
`), 0o755)
	pin, err := os.ReadFile("../../hack/verify-runner-pin.sh")
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerPublicationFile(t, filepath.Join(fixture.repo, "hack", "verify-runner-pin.sh"), pin, 0o755)
	writeRunnerPublicationFile(t, filepath.Join(fixture.binDir, "crane"), []byte(runnerPublicationCraneStub), 0o755)
	writeRunnerPublicationFile(t, filepath.Join(fixture.binDir, "timeout"), []byte(runnerPublicationTimeoutStub), 0o755)
	craneSHA := runnerPublicationSHA256File(t, filepath.Join(fixture.binDir, "crane"))
	writeRunnerPublicationFile(t, filepath.Join(fixture.repo, "hack", "runner-crane.sha256"), []byte(craneSHA+"  bin/crane\n"), 0o644)
	writeRunnerPublicationFile(t, filepath.Join(fixture.dockerConfig, "config.json"), []byte(`{"auths":{"europe-west4-docker.pkg.dev":{"auth":"dGVzdDp0ZXN0"}}}`), 0o600)

	fixture.manifest = writeRunnerPublicationOCI(t, fixture.oci, "")
	fixture.remoteOCI = fixture.oci
	writeRunnerPublicationFile(t, fixture.values, []byte("runnerImage:\n  ref: \""+runnerPublicationRepository+"@"+fixture.manifest+"\"\n"), 0o644)
	writeRunnerPublicationFile(t, fixture.scan, []byte("{}\n"), 0o644)
	archiveSHA := runnerPublicationSHA256File(t, fixture.oci)
	receipt := map[string]any{
		"schema": 1, "passed": true,
		"source": map[string]any{
			"revision":      "0123456789abcdef0123456789abcdef01234567",
			"contextSHA256": strings.Repeat("a", 64), "dirty": false,
		},
		"image": map[string]any{
			"manifest": fixture.manifest, "archiveSHA256": archiveSHA,
			"configDigest": runnerPublicationDigest([]byte(`{"architecture":"amd64","config":{"User":"65534:65534"},"os":"linux"}`)),
			"layerCount":   1,
		},
		"scanner": map[string]any{"exitCode": 0, "high": 0, "critical": 0, "secrets": 0},
	}
	writeRunnerPublicationJSON(t, fixture.receipt, receipt)
	state := map[string]any{
		"schema": 1, "status": "passed", "exitCode": 0,
		"sourceRevision":      receipt["source"].(map[string]any)["revision"],
		"sourceContextSHA256": receipt["source"].(map[string]any)["contextSHA256"],
		"receiptSHA256":       runnerPublicationSHA256File(t, fixture.receipt),
	}
	writeRunnerPublicationJSON(t, fixture.state, state)
	return fixture
}

func (fixture *runnerPublicationFixture) run(t *testing.T) runnerPublicationResult {
	t.Helper()
	command := exec.Command(filepath.Join(fixture.repo, "hack", "publish-runner-image.sh"),
		fixture.values, fixture.oci, fixture.receipt, fixture.scan, fixture.state, fixture.publication)
	command.Dir = fixture.repo
	command.Env = []string{
		"PATH=" + runnerPublicationFixturePATH(fixture),
		"LANG=C", "LC_ALL=C", "TZ=UTC",
		"DOCKER_CONFIG=" + fixture.dockerConfig,
		"RUNNER_PUBLISH_MAX_ATTEMPTS=3",
		"RUNNER_PUBLISH_RETRY_DELAY=0",
		"RUNNER_REGISTRY_TIMEOUT=" + fixture.registryTimeout,
		"RUNNER_REGISTRY_UPLOAD_TIMEOUT=" + fixture.registryUploadTimeout,
		"RUNNER_REGISTRY_PULL_TIMEOUT=" + fixture.registryPullTimeout,
		"RUNNER_PUBLISH_TMPDIR=" + fixture.root,
		"RUNNER_VERIFY_TMPDIR=" + fixture.root,
		"RUNNER_TEST_CRANE_LOG=" + fixture.craneLog,
		"RUNNER_TEST_TIMEOUT_LOG=" + fixture.timeoutLog,
		"RUNNER_TEST_CRANE_STATE=" + fixture.craneStateDir,
		"RUNNER_TEST_EXPECTED_DIGEST=" + fixture.manifest,
		"RUNNER_TEST_REMOTE_OCI=" + fixture.remoteOCI,
		"RUNNER_TEST_SCENARIO=" + fixture.scenario,
	}
	output, err := command.CombinedOutput()
	return runnerPublicationResult{output: string(output), err: err}
}

func (fixture *runnerPublicationFixture) runPin(t *testing.T) runnerPublicationResult {
	t.Helper()
	command := exec.Command(filepath.Join(fixture.repo, "hack", "verify-runner-pin.sh"),
		fixture.values, fixture.oci, fixture.receipt, fixture.scan, fixture.state, fixture.publication)
	command.Dir = fixture.repo
	command.Env = []string{
		"PATH=" + runnerPublicationFixturePATH(fixture),
		"LANG=C", "LC_ALL=C", "TZ=UTC",
		"RUNNER_REGISTRY_TIMEOUT=" + fixture.registryTimeout,
		"RUNNER_REGISTRY_PULL_TIMEOUT=" + fixture.registryPullTimeout,
		"RUNNER_VERIFY_TMPDIR=" + fixture.root,
		"RUNNER_TEST_CRANE_LOG=" + fixture.craneLog,
		"RUNNER_TEST_TIMEOUT_LOG=" + fixture.timeoutLog,
		"RUNNER_TEST_CRANE_STATE=" + fixture.craneStateDir,
		"RUNNER_TEST_EXPECTED_DIGEST=" + fixture.manifest,
		"RUNNER_TEST_REMOTE_OCI=" + fixture.remoteOCI,
		"RUNNER_TEST_SCENARIO=" + fixture.scenario,
		"REGISTRY_AUTH_FILE=" + filepath.Join(fixture.root, "ambient-auth.json"),
		"XDG_RUNTIME_DIR=" + filepath.Join(fixture.root, "ambient-runtime"),
		"RUNNER_TEST_REQUIRE_ANONYMOUS_CONFIG=1",
	}
	output, err := command.CombinedOutput()
	return runnerPublicationResult{output: string(output), err: err}
}

func runnerPublicationFixturePATH(fixture *runnerPublicationFixture) string {
	return strings.Join([]string{
		fixture.binDir,
		os.Getenv(runnerTestJQDirectoryEnv),
		os.Getenv(runnerTestPythonDirectoryEnv),
		"/usr/bin",
		"/bin",
	}, string(os.PathListSeparator))
}

func (fixture *runnerPublicationFixture) requirePassedPublication(t *testing.T, pushed, reconciled bool) {
	t.Helper()
	value := fixture.readJSON(t, fixture.publication)
	if value["status"] != "passed" || value["exitCode"] != float64(0) {
		t.Fatalf("publication state = %#v", value)
	}
	result := value["result"].(map[string]any)
	if result["pushed"] != pushed || result["reconciled"] != reconciled {
		t.Fatalf("publication result = %#v, want pushed=%v reconciled=%v", result, pushed, reconciled)
	}
	destination := value["destination"].(map[string]any)
	wantTag := "runner-" + strings.TrimPrefix(fixture.manifest, "sha256:")
	if destination["repository"] != runnerPublicationRepository || destination["tag"] != wantTag ||
		destination["tagRef"] != runnerPublicationRepository+":"+wantTag ||
		destination["digestRef"] != runnerPublicationRepository+"@"+fixture.manifest ||
		destination["manifest"] != fixture.manifest {
		t.Fatalf("publication destination = %#v", destination)
	}
	readBack := value["readBack"].(map[string]any)
	if readBack["tagManifest"] != fixture.manifest || readBack["digestManifest"] != fixture.manifest ||
		readBack["configDigest"] == "" || readBack["layerCount"] != float64(1) ||
		readBack["tagOCILayoutSHA256"] == "" || readBack["digestOCILayoutSHA256"] == "" {
		t.Fatalf("publication read-back = %#v", readBack)
	}
	if got := fixture.craneCalls(t, "pull"); got != 2 {
		t.Fatalf("pull calls = %d, want exact tag and digest read-back", got)
	}
	fixture.requireLatestPullRefs(t)
	publisher := value["publisher"].(map[string]any)
	if publisher["contractSHA256"] == "" {
		t.Fatalf("publication does not bind its publisher contract: %#v", publisher)
	}
}

func (fixture *runnerPublicationFixture) requireFailedPublication(t *testing.T) {
	t.Helper()
	value := fixture.readJSON(t, fixture.publication)
	if value["status"] != "failed" || value["exitCode"] == float64(0) {
		t.Fatalf("publication state = %#v, want failed nonzero", value)
	}
}

func (fixture *runnerPublicationFixture) requireEphemeralCraneConfig(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(fixture.craneLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			t.Fatalf("crane log line = %q", line)
		}
		config := parts[2]
		if config == fixture.dockerConfig || !strings.Contains(config, "runner-image-publish") {
			t.Fatalf("crane used non-ephemeral DOCKER_CONFIG %q", config)
		}
		if _, statErr := os.Stat(config); !os.IsNotExist(statErr) {
			t.Fatalf("ephemeral DOCKER_CONFIG %q survived cleanup: %v", config, statErr)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.dockerConfig, "config.json")); err != nil {
		t.Fatalf("caller credential material was removed: %v", err)
	}
	fixture.requireNoPublicationWorkdirs(t)
}

func (fixture *runnerPublicationFixture) requireNoPublicationWorkdirs(t *testing.T) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(fixture.root, "runner-image-publish.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("ephemeral publication work directories survived cleanup: %v", matches)
	}
	if _, err := os.Stat(filepath.Join(fixture.dockerConfig, "config.json")); err != nil {
		t.Fatalf("caller credential material was removed: %v", err)
	}
}

func (fixture *runnerPublicationFixture) craneCalls(t *testing.T, operation string) int {
	t.Helper()
	raw, err := os.ReadFile(fixture.craneLog)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		if operation == "" || strings.HasPrefix(line, operation+"|") {
			count++
		}
	}
	return count
}

func (fixture *runnerPublicationFixture) timeoutDurations(t *testing.T, operation string) []string {
	t.Helper()
	raw, err := os.ReadFile(fixture.timeoutLog)
	if err != nil {
		t.Fatal(err)
	}
	var durations []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) == 2 && parts[0] == operation {
			durations = append(durations, parts[1])
		}
	}
	return durations
}

func (fixture *runnerPublicationFixture) requireLatestPullRefs(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(fixture.craneLog)
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 || parts[0] != "pull" {
			continue
		}
		arguments := strings.Fields(parts[1])
		if len(arguments) != 3 || arguments[0] != "--format=oci" {
			t.Fatalf("pull arguments = %q, want --format=oci REF DEST", parts[1])
		}
		refs = append(refs, arguments[1])
	}
	if len(refs) < 2 {
		t.Fatalf("pull refs = %v, want tag and digest", refs)
	}
	manifest := fixture.manifest
	tag := "runner-" + strings.TrimPrefix(manifest, "sha256:")
	want := []string{
		runnerPublicationRepository + ":" + tag,
		runnerPublicationRepository + "@" + manifest,
	}
	if got := refs[len(refs)-2:]; !slices.Equal(got, want) {
		t.Fatalf("latest pull refs = %v, want %v", got, want)
	}
}

func (fixture *runnerPublicationFixture) refreshReceiptHash(t *testing.T) {
	t.Helper()
	fixture.updateJSON(t, fixture.state, func(value map[string]any) {
		value["receiptSHA256"] = runnerPublicationSHA256File(t, fixture.receipt)
	})
}

func (fixture *runnerPublicationFixture) updateJSON(t *testing.T, path string, update func(map[string]any)) {
	t.Helper()
	value := fixture.readJSON(t, path)
	update(value)
	writeRunnerPublicationJSON(t, path, value)
}

func (fixture *runnerPublicationFixture) readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

const runnerPublicationCraneStub = `#!/bin/sh
set -eu
operation=$1
shift
if test "$operation" = version; then
  printf '%s\n' '0.20.3'
  exit 0
fi
if test "${RUNNER_TEST_REQUIRE_ANONYMOUS_CONFIG:-}" = 1; then
  test "${REGISTRY_AUTH_FILE+x}" != x || {
    printf '%s\n' 'REGISTRY_AUTH_FILE leaked into deploy crane' >&2
    exit 91
  }
  test "${XDG_RUNTIME_DIR+x}" != x || {
    printf '%s\n' 'XDG_RUNTIME_DIR leaked into deploy crane' >&2
    exit 92
  }
  test "$(cat "$DOCKER_CONFIG/config.json")" = '{"auths":{}}' || {
    printf '%s\n' 'deploy crane lacks the explicit anonymous Docker config' >&2
    exit 93
  }
  test "$(stat -c '%a' "$DOCKER_CONFIG/config.json")" = 600 || {
    printf '%s\n' 'deploy crane config mode is not 0600' >&2
    exit 94
  }
  case "$0" in
    "$RUNNER_VERIFY_TMPDIR"/oberth-runner-pin.*/bin/crane) ;;
    *)
      printf '%s\n' 'deploy crane was not executed from its private copy' >&2
      exit 95
      ;;
  esac
fi
printf '%s|%s|%s\n' "$operation" "$*" "$DOCKER_CONFIG" >>"$RUNNER_TEST_CRANE_LOG"
pushed=$RUNNER_TEST_CRANE_STATE/pushed
missing() {
  printf '%s\n' 'MANIFEST_UNKNOWN: requested image was not found' >&2
  exit 1
}
case "$operation" in
  digest)
    case "$RUNNER_TEST_SCENARIO" in
      existing-mismatch)
        printf '%s\n' 'sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff'
        ;;
      push-success|uncertain-adopt)
        test -f "$pushed" || missing
        printf '%s\n' "$RUNNER_TEST_EXPECTED_DIGEST"
        ;;
      always-missing)
        missing
        ;;
      *)
        printf '%s\n' "$RUNNER_TEST_EXPECTED_DIGEST"
        ;;
    esac
    ;;
  push)
    case "$RUNNER_TEST_SCENARIO" in
      push-success)
        : >"$pushed"
        printf '%s\n' "$RUNNER_TEST_EXPECTED_DIGEST"
        ;;
      uncertain-adopt)
        : >"$pushed"
        printf '%s\n' 'connection closed after upload' >&2
        exit 124
        ;;
      always-missing)
        printf '%s\n' 'connection closed before upload' >&2
        exit 1
        ;;
      *)
        printf '%s\n' 'unexpected push' >&2
        exit 2
        ;;
    esac
    ;;
  pull)
    destination=
    for argument in "$@"; do destination=$argument; done
    test -n "$destination"
    mkdir -p "$destination"
    tar -xf - -C "$destination" <"$RUNNER_TEST_REMOTE_OCI"
    ;;
  *)
    printf 'unexpected crane operation %s\n' "$operation" >&2
    exit 2
    ;;
esac
`

const runnerPublicationTimeoutStub = `#!/bin/sh
set -eu
duration=
while test "$#" -gt 0; do
  case "$1" in
    --foreground|--signal=*|--kill-after=*) shift ;;
    *) duration=$1; shift; break ;;
  esac
done
test -n "$duration"
test "$#" -ge 2
printf '%s|%s\n' "$2" "$duration" >>"$RUNNER_TEST_TIMEOUT_LOG"
if test "${RUNNER_TEST_SCENARIO:-}" = pull-timeout && test "$2" = pull; then
  destination=
  for argument in "$@"; do destination=$argument; done
  test -n "$destination"
  mkdir -p "$destination"
  printf '%s\n' partial >"$destination/partial"
  exit 124
fi
exec "$@"
`

type runnerPublicationDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

func writeRunnerPublicationOCI(t *testing.T, path, mutation string) string {
	t.Helper()
	config := []byte(`{"architecture":"amd64","config":{"User":"65534:65534"},"os":"linux"}`)
	layer := []byte("runner publication layer\n")
	configDigest := runnerPublicationDigest(config)
	layerDigest := runnerPublicationDigest(layer)
	manifestValue := struct {
		SchemaVersion int                           `json:"schemaVersion"`
		MediaType     string                        `json:"mediaType"`
		Config        runnerPublicationDescriptor   `json:"config"`
		Layers        []runnerPublicationDescriptor `json:"layers"`
		Subject       *runnerPublicationDescriptor  `json:"subject,omitempty"`
	}{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: runnerPublicationDescriptor{
			MediaType: "application/vnd.oci.image.config.v1+json", Digest: configDigest, Size: int64(len(config)),
		},
		Layers: []runnerPublicationDescriptor{{
			MediaType: "application/vnd.oci.image.layer.v1.tar", Digest: layerDigest, Size: int64(len(layer)),
		}},
	}
	if mutation == "subject" {
		manifestValue.Subject = &runnerPublicationDescriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    "sha256:" + strings.Repeat("0", 64),
			Size:      0,
		}
	}
	manifestRaw, err := json.Marshal(manifestValue)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := runnerPublicationDigest(manifestRaw)
	indexRaw, err := json.Marshal(struct {
		SchemaVersion int                           `json:"schemaVersion"`
		Manifests     []runnerPublicationDescriptor `json:"manifests"`
	}{SchemaVersion: 2, Manifests: []runnerPublicationDescriptor{{
		MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest, Size: int64(len(manifestRaw)),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json": indexRaw,
		"blobs/sha256/" + strings.TrimPrefix(configDigest, "sha256:"):   config,
		"blobs/sha256/" + strings.TrimPrefix(layerDigest, "sha256:"):    layer,
		"blobs/sha256/" + strings.TrimPrefix(manifestDigest, "sha256:"): manifestRaw,
	}
	switch mutation {
	case "":
	case "corrupt-layer":
		files["blobs/sha256/"+strings.TrimPrefix(layerDigest, "sha256:")] = []byte("corrupt registry layer\n")
	case "missing-layer":
		delete(files, "blobs/sha256/"+strings.TrimPrefix(layerDigest, "sha256:"))
	case "unreferenced-blob":
		unreferenced := []byte("unreferenced registry blob\n")
		files["blobs/sha256/"+strings.TrimPrefix(runnerPublicationDigest(unreferenced), "sha256:")] = unreferenced
	case "unexpected-file":
		files["unexpected"] = []byte("registry-controlled extra file\n")
	case "subject":
	default:
		t.Fatalf("unknown OCI fixture mutation %q", mutation)
	}
	writeRunnerPublicationTar(t, path, files)
	return manifestDigest
}

func writeUnsafeRunnerPublicationTar(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	body := []byte("escape\n")
	if err := writer.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeRunnerPublicationTar(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(file)
	for _, directory := range []string{"blobs", "blobs/sha256"} {
		if err := writer.WriteHeader(&tar.Header{Name: directory, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeRunnerPublicationJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeRunnerPublicationFile(t, path, append(raw, '\n'), 0o644)
}

func writeRunnerPublicationFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func runnerPublicationSHA256File(t *testing.T, path string) string {
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

func runnerPublicationDigest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}
