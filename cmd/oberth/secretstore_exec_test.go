package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- parseExecArgs tests ---

func TestParseExecArgsValid(t *testing.T) {
	dir, paths, cmd, err := parseExecArgs([]string{
		"--dir=/run/secrets",
		"--path=oberth/data/release/r2-upload-token",
		"--path=oberth/data/release/cosign-secret",
		"--", "release.sh", "publish",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/run/secrets" {
		t.Fatalf("dir = %q, want /run/secrets", dir)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %d, want 2", len(paths))
	}
	if len(cmd) != 2 || cmd[0] != "release.sh" {
		t.Fatalf("cmd = %v", cmd)
	}
}

func TestParseExecArgsSpaceForm(t *testing.T) {
	dir, paths, cmd, err := parseExecArgs([]string{
		"-dir", "/run/secrets",
		"-path", "oberth/data/release/r2",
		"--", "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/run/secrets" {
		t.Fatalf("dir = %q", dir)
	}
	if len(paths) != 1 || paths[0] != "oberth/data/release/r2" {
		t.Fatalf("paths = %v", paths)
	}
	if len(cmd) != 1 {
		t.Fatalf("cmd = %v", cmd)
	}
}

func TestParseExecArgsMissingDir(t *testing.T) {
	_, _, _, err := parseExecArgs([]string{
		"--path=oberth/data/release/r2", "--", "true",
	})
	if err == nil || !strings.Contains(err.Error(), "--dir") {
		t.Fatalf("expected --dir error, got: %v", err)
	}
}

func TestParseExecArgsMissingPaths(t *testing.T) {
	_, _, _, err := parseExecArgs([]string{
		"--dir=/run/secrets", "--", "true",
	})
	if err == nil || !strings.Contains(err.Error(), "--path") {
		t.Fatalf("expected --path error, got: %v", err)
	}
}

func TestParseExecArgsMissingCommand(t *testing.T) {
	_, _, _, err := parseExecArgs([]string{
		"--dir=/run/secrets", "--path=oberth/data/x",
	})
	if err == nil || !strings.Contains(err.Error(), "command") {
		t.Fatalf("expected missing command error, got: %v", err)
	}
}

func TestParseExecArgsDuplicateLocalNames(t *testing.T) {
	_, _, _, err := parseExecArgs([]string{
		"--dir=/run/secrets",
		"--path=oberth/data/release/r2",
		"--path=other/data/release/r2",
		"--", "true",
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate local name error, got: %v", err)
	}
}

func TestParseExecArgsTooManyPaths(t *testing.T) {
	args := []string{"--dir=/run/secrets"}
	for i := 0; i < maxExecPaths+1; i++ {
		args = append(args, "--path=oberth/data/release/secret-"+string(rune('a'+i%26)))
	}
	args = append(args, "--", "true")
	_, _, _, err := parseExecArgs(args)
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("expected too-many-paths error, got: %v", err)
	}
}

// --- isExecStoreCredential tests ---

func TestIsExecStoreCredentialStripsAllPrefixes(t *testing.T) {
	for _, name := range []string{
		"VAULT_ADDR", "VAULT_TOKEN", "VAULT_CACERT",
		"BAO_ADDR", "BAO_TOKEN",
		"CONSUL_HTTP_ADDR",
		"OBERTH_VAULT_ROLE",
	} {
		if !isExecStoreCredential(name) {
			t.Errorf("%s should be stripped", name)
		}
	}
	for _, name := range []string{
		"PATH", "HOME", "OBERTH_REPO", "GOPATH",
		"OBERTH_SECRETSTORE_DIR",
	} {
		if isExecStoreCredential(name) {
			t.Errorf("%s should not be stripped", name)
		}
	}
}

// --- tmpfs check test ---

func TestExecRefusesNonTmpfs(t *testing.T) {
	// Patch the statfs function to return ext4 magic.
	original := secretExecFstatfs
	secretExecFstatfs = func(_ int) (int64, error) {
		return 0xEF53, nil // ext4 magic
	}
	defer func() { secretExecFstatfs = original }()

	dir := t.TempDir()
	t.Setenv("VAULT_ADDR", "https://vault.example:8200")
	t.Setenv("OBERTH_VAULT_ROLE", "test-role")

	err := runSecretStoreExec(t.Context(), []string{
		"--dir=" + dir,
		"--path=oberth/data/release/test",
		"--", "true",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "tmpfs") {
		t.Fatalf("expected tmpfs refusal, got: %v", err)
	}
}

// --- materialize subcommand tests (ported from cmd/oberth-secret-materialize) ---

func TestMaterializeWritesEachValue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("COSIGN_KEY", "KEY-MATERIAL")
	t.Setenv("COSIGN_PASSWORD", "PASSPHRASE")

	err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir,
		"COSIGN_KEY=cosign-secret/cosign.key",
		"COSIGN_PASSWORD=cosign-secret/cosign.password",
		"--", "true",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	for path, want := range map[string]string{
		"cosign-secret/cosign.key":      "KEY-MATERIAL",
		"cosign-secret/cosign.password": "PASSPHRASE",
	} {
		content, readErr := os.ReadFile(filepath.Join(dir, path))
		if readErr != nil {
			t.Fatalf("%s: %v", path, readErr)
		}
		if string(content) != want {
			t.Errorf("%s = %q, want %q", path, content, want)
		}
	}
}

func TestMaterializeSecretFilesAreOwnerReadableOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("R2_UPLOAD_TOKEN", "token-value")

	if err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir, "R2_UPLOAD_TOKEN=r2-upload-token/token", "--", "true",
	}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "r2-upload-token/token"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != secretExecFileMode {
		t.Fatalf("mode = %o, want %o", perm, secretExecFileMode)
	}
}

func TestMaterializeStripsVariablesFromChildEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	output := filepath.Join(t.TempDir(), "env.txt")
	t.Setenv("COSIGN_PASSWORD", "PASSPHRASE")
	t.Setenv("KEEP_ME", "visible")

	script := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\n{ echo \"pw=[${COSIGN_PASSWORD:-}]\"; echo \"keep=[${KEEP_ME:-}]\"; "+
			"echo \"dir=[${OBERTH_SECRETSTORE_DIR:-}]\"; } > \""+output+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir, "COSIGN_PASSWORD=cosign-secret/cosign.password", "--", script,
	}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	seen, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(seen)
	if !strings.Contains(text, "pw=[]") {
		t.Errorf("the credential survived in the child environment:\n%s", text)
	}
	if !strings.Contains(text, "keep=[visible]") {
		t.Errorf("an unrelated variable was stripped:\n%s", text)
	}
	if !strings.Contains(text, "dir=["+dir+"]") {
		t.Errorf("OBERTH_SECRETSTORE_DIR was not set for the child:\n%s", text)
	}
}

func TestMaterializeMissingVariableFailsNamingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir, "COSIGN_KEY=cosign-secret/cosign.key", "--", "true",
	}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected a failure when the variable is absent")
	}
	if !strings.Contains(err.Error(), "COSIGN_KEY") {
		t.Fatalf("error should name the variable, got: %v", err)
	}
}

func TestMaterializeEmptyValueWritesEmptyFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("COSIGN_PASSWORD", "")
	err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir, "COSIGN_PASSWORD=cosign-secret/cosign.password", "--", "true",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("empty value should be accepted, got: %v", err)
	}
	content, readErr := os.ReadFile(filepath.Join(dir, "cosign-secret/cosign.password"))
	if readErr != nil {
		t.Fatalf("file not created: %v", readErr)
	}
	if len(content) != 0 {
		t.Fatalf("expected zero-length file, got %d bytes", len(content))
	}
	info, statErr := os.Stat(filepath.Join(dir, "cosign-secret/cosign.password"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if perm := info.Mode().Perm(); perm != secretExecFileMode {
		t.Errorf("mode = %o, want %o", perm, secretExecFileMode)
	}
}

func TestMaterializeDestinationCannotEscapeDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "secrets")
	t.Setenv("SNEAKY", "value")

	for _, destination := range []string{
		"../escaped/file",
		"/etc/cron.d/payload",
		"a/../../b/c",
		"only-one-segment",
		"three/segments/deep",
	} {
		err := runSecretStoreMaterialize(t.Context(), []string{
			"-dir", dir, "SNEAKY=" + destination, "--", "true",
		}, io.Discard, io.Discard)
		if err == nil {
			t.Errorf("destination %q was accepted", destination)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escaped")); !os.IsNotExist(err) {
		t.Fatal("a file was written outside the secret directory")
	}
}

func TestMaterializeDuplicateDestinationIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("FIRST", "one")
	t.Setenv("SECOND", "two")
	err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir,
		"FIRST=cosign-secret/cosign.key",
		"SECOND=cosign-secret/cosign.key",
		"--", "true",
	}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected a refusal when two mappings claim one path")
	}
}

func TestMaterializeRequiresSeparatorAndCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("A", "v")
	if err := runSecretStoreMaterialize(t.Context(), []string{"-dir", dir, "A=n/k"}, io.Discard, io.Discard); err == nil {
		t.Error("expected a failure when no command follows --")
	}
	if err := runSecretStoreMaterialize(t.Context(), []string{"A=n/k", "--", "true"}, io.Discard, io.Discard); err == nil {
		t.Error("expected a failure when -dir is absent")
	}
	if err := runSecretStoreMaterialize(t.Context(), []string{"-dir", dir, "--", "true"}, io.Discard, io.Discard); err == nil {
		t.Error("expected a failure when no mappings are given")
	}
}

func TestMaterializeChildStdoutIsRedacted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("SECRET_TOKEN", "super-secret-credential-value")
	t.Setenv("VISIBLE_VAR", "hello-world")

	script := filepath.Join(t.TempDir(), "echo-stdout.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\necho \"token=super-secret-credential-value visible=hello-world\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir,
		"SECRET_TOKEN=secret-token/value",
		"VISIBLE_VAR=visible-var/value",
		"--", script,
	}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}

	output := stdout.String()
	if strings.Contains(output, "super-secret-credential-value") {
		t.Fatalf("secret value leaked to stdout: %q", output)
	}
	if !strings.Contains(output, "***") {
		t.Fatalf("expected redaction marker in stdout, got: %q", output)
	}
}

func TestMaterializeChildStderrIsRedacted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("SECRET_TOKEN", "another-secret-value")

	script := filepath.Join(t.TempDir(), "echo-stderr.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\necho \"leaked: another-secret-value\" >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir,
		"SECRET_TOKEN=secret-token/value",
		"--", script,
	}, io.Discard, &stderr); err != nil {
		t.Fatal(err)
	}

	output := stderr.String()
	if strings.Contains(output, "another-secret-value") {
		t.Fatalf("secret value leaked to stderr: %q", output)
	}
	if !strings.Contains(output, "***") {
		t.Fatalf("expected redaction marker in stderr, got: %q", output)
	}
}

func TestMaterializeNonSecretOutputPassesThrough(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("TOKEN", "redact-me-please")

	script := filepath.Join(t.TempDir(), "echo-safe.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\necho \"this is ordinary build output\"\necho \"nothing secret here\" >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir,
		"TOKEN=token/value",
		"--", script,
	}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(stdout.String()); got != "this is ordinary build output" {
		t.Errorf("stdout = %q, want %q", got, "this is ordinary build output")
	}
	if got := strings.TrimSpace(stderr.String()); got != "nothing secret here" {
		t.Errorf("stderr = %q, want %q", got, "nothing secret here")
	}
}

// --- #248: structured fetch marker tests ---

func TestEmitSecretFetchMarkerOmitsValues(t *testing.T) {
	var out bytes.Buffer
	fetched := map[string]map[string][]byte{
		"oberth/data/release/cosign-secret": {
			"COSIGN_KEY":      []byte("SUPER-SECRET-PEM-BYTES"),
			"COSIGN_PASSWORD": []byte("hunter2"),
		},
		"oberth/data/release/r2-upload-token": {
			"R2_UPLOAD_TOKEN": []byte("tok_value_9"),
		},
	}
	paths := []string{
		"oberth/data/release/cosign-secret",
		"oberth/data/release/r2-upload-token",
	}

	emitSecretFetchMarker(&out, paths, fetched)

	line := out.String()
	if !strings.HasPrefix(line, "[oberth-secret-fetch] ") {
		t.Fatalf("marker prefix missing, got: %q", line)
	}
	for _, secret := range []string{"SUPER-SECRET-PEM-BYTES", "hunter2", "tok_value_9"} {
		if strings.Contains(line, secret) {
			t.Fatalf("marker leaked a secret value %q: %q", secret, line)
		}
	}

	var marker struct {
		Paths []string            `json:"paths"`
		Keys  map[string][]string `json:"keys"`
		OK    bool                `json:"ok"`
	}
	payload := strings.TrimPrefix(strings.TrimSpace(line), "[oberth-secret-fetch] ")
	if err := json.Unmarshal([]byte(payload), &marker); err != nil {
		t.Fatalf("marker is not valid JSON: %v (%q)", err, payload)
	}
	if !marker.OK {
		t.Fatal("marker ok = false, want true")
	}
	if len(marker.Paths) != 2 {
		t.Fatalf("marker paths = %v, want both declared paths", marker.Paths)
	}
	wantKeys := map[string][]string{
		"oberth/data/release/cosign-secret":   {"COSIGN_KEY", "COSIGN_PASSWORD"},
		"oberth/data/release/r2-upload-token": {"R2_UPLOAD_TOKEN"},
	}
	for path, want := range wantKeys {
		got := marker.Keys[path]
		if len(got) != len(want) {
			t.Fatalf("marker keys[%s] = %v, want %v", path, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("marker keys[%s] = %v, want sorted %v", path, got, want)
			}
		}
	}
}

func TestEmitSecretFetchMarkerEmptyFetch(t *testing.T) {
	var out bytes.Buffer
	emitSecretFetchMarker(&out, nil, nil)
	line := strings.TrimSpace(out.String())
	payload := strings.TrimPrefix(line, "[oberth-secret-fetch] ")
	var marker map[string]any
	if err := json.Unmarshal([]byte(payload), &marker); err != nil {
		t.Fatalf("empty-fetch marker is not valid JSON: %v (%q)", err, line)
	}
}

// --- #249: short secret redaction tests ---

func TestExecChildRedactsShortSecret(t *testing.T) {
	dir := t.TempDir()
	secret := []byte("abc12") // 5 bytes, well below the old 8-byte threshold

	script := filepath.Join(t.TempDir(), "echo-short.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\necho \"token=abc12\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	err := execChild(dir, []string{script}, [][]byte{secret}, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	output := stdout.String()
	if strings.Contains(output, "abc12") {
		t.Fatalf("short secret leaked to stdout: %q", output)
	}
	if !strings.Contains(output, "***") {
		t.Fatalf("expected redaction marker in stdout, got: %q", output)
	}
}

func TestMaterializeRedactsShortSecret(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	t.Setenv("SHORT_TOKEN", "abc12") // 5 bytes

	script := filepath.Join(t.TempDir(), "echo-short.sh")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\necho \"token=abc12\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runSecretStoreMaterialize(t.Context(), []string{
		"-dir", dir,
		"SHORT_TOKEN=short-token/value",
		"--", script,
	}, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}

	output := stdout.String()
	if strings.Contains(output, "abc12") {
		t.Fatalf("short secret value leaked to stdout: %q", output)
	}
	if !strings.Contains(output, "***") {
		t.Fatalf("expected redaction marker in stdout, got: %q", output)
	}
}

// --- #251: swap check tests ---

func TestCheckSwapActiveWithDevices(t *testing.T) {
	swapFile := filepath.Join(t.TempDir(), "swaps")
	if err := os.WriteFile(swapFile, []byte(
		"Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"+
			"/dev/sda2                               partition\t8388604\t\t0\t\t-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !checkSwapActive(swapFile) {
		t.Fatal("expected swap to be detected as active")
	}
}

func TestCheckSwapActiveHeaderOnly(t *testing.T) {
	swapFile := filepath.Join(t.TempDir(), "swaps")
	if err := os.WriteFile(swapFile, []byte(
		"Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if checkSwapActive(swapFile) {
		t.Fatal("expected no swap detected with header-only file")
	}
}

func TestCheckSwapActiveMissingFile(t *testing.T) {
	if checkSwapActive(filepath.Join(t.TempDir(), "nonexistent")) {
		t.Fatal("expected false for missing file")
	}
}

func TestExecWarnsOnActiveSwap(t *testing.T) {
	// Fake /proc/swaps with an active swap device.
	swapFile := filepath.Join(t.TempDir(), "swaps")
	if err := os.WriteFile(swapFile, []byte(
		"Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"+
			"/dev/sda2                               partition\t8388604\t\t0\t\t-2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origSwap := swapCheckPath
	swapCheckPath = swapFile
	defer func() { swapCheckPath = origSwap }()

	// Mock statfs to report tmpfs so the check passes.
	origStatfs := secretExecFstatfs
	secretExecFstatfs = func(_ int) (int64, error) {
		return tmpfsMagic, nil
	}
	defer func() { secretExecFstatfs = origStatfs }()

	dir := t.TempDir()
	t.Setenv("VAULT_ADDR", "https://vault.example:8200")
	t.Setenv("OBERTH_VAULT_ROLE", "test-role")

	var stderr bytes.Buffer
	// The call will fail at Vault login (no real store), but the swap
	// warning is emitted before the store connection attempt.
	_ = runSecretStoreExec(t.Context(), []string{
		"--dir=" + dir,
		"--path=oberth/data/release/test",
		"--", "true",
	}, io.Discard, &stderr)

	if !strings.Contains(stderr.String(), "swap is active") {
		t.Fatalf("expected swap warning in stderr, got: %q", stderr.String())
	}
}

func TestExecNoSwapWarningWhenInactive(t *testing.T) {
	// Fake /proc/swaps with only the header (no swap devices).
	swapFile := filepath.Join(t.TempDir(), "swaps")
	if err := os.WriteFile(swapFile, []byte(
		"Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origSwap := swapCheckPath
	swapCheckPath = swapFile
	defer func() { swapCheckPath = origSwap }()

	origStatfs := secretExecFstatfs
	secretExecFstatfs = func(_ int) (int64, error) {
		return tmpfsMagic, nil
	}
	defer func() { secretExecFstatfs = origStatfs }()

	dir := t.TempDir()
	t.Setenv("VAULT_ADDR", "https://vault.example:8200")
	t.Setenv("OBERTH_VAULT_ROLE", "test-role")

	var stderr bytes.Buffer
	_ = runSecretStoreExec(t.Context(), []string{
		"--dir=" + dir,
		"--path=oberth/data/release/test",
		"--", "true",
	}, io.Discard, &stderr)

	if strings.Contains(stderr.String(), "swap") {
		t.Fatalf("unexpected swap warning when swap is inactive: %q", stderr.String())
	}
}
