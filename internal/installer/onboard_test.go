package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// --- validatePublicKeyFile tests ---

func TestValidatePublicKeyFileRejectsPrivateKey(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(pemBlock)
	err = validatePublicKeyFile(pemBytes)
	if err == nil {
		t.Fatal("expected error for private key input")
	}
	if !strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("error should mention PRIVATE key, got: %v", err)
	}
	if !strings.Contains(err.Error(), ".pub") {
		t.Fatalf("error should mention .pub, got: %v", err)
	}
}

func TestValidatePublicKeyFileRejectsGarbage(t *testing.T) {
	t.Parallel()
	err := validatePublicKeyFile([]byte("this is not a key at all"))
	if err == nil {
		t.Fatal("expected error for garbage input")
	}
	if !strings.Contains(err.Error(), "valid SSH public key") {
		t.Fatalf("error should mention valid SSH public key, got: %v", err)
	}
}

func TestValidatePublicKeyFileAcceptsAuthorizedKey(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubKey := ssh.MarshalAuthorizedKey(signer.PublicKey())
	if err := validatePublicKeyFile(pubKey); err != nil {
		t.Fatalf("valid authorized key should be accepted, got: %v", err)
	}
}

// --- validateUplinkIdentity tests ---

func TestValidateUplinkIdentityAcceptsValid(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"alice@laptop", "bob@dev-machine", "ci@runner01"} {
		if err := validateUplinkIdentity(id); err != nil {
			t.Errorf("valid identity %q rejected: %v", id, err)
		}
	}
}

func TestValidateUplinkIdentityRejectsInvalid(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"@me@",  // multiple @ with empty name
		"alice", // no @
		"@host", // empty name
		"name@", // empty host
		"",      // empty string
		"a @b",  // space in identity
		"a\t@b", // tab in identity
		"a@b@c", // multiple @
	} {
		if err := validateUplinkIdentity(id); err == nil {
			t.Errorf("invalid identity %q was accepted", id)
		} else if !strings.Contains(err.Error(), "name@host") {
			t.Errorf("error for %q should mention name@host format, got: %v", id, err)
		}
	}
}

// --- sanitizeExecOutput tests ---

func TestSanitizeExecOutputStripsTokenLines(t *testing.T) {
	t.Parallel()
	input := "Uplink token for tester@box (shown once):\noberth_secret_token_value\nDone.\n"
	got := sanitizeExecOutput(input)
	if strings.Contains(got, "oberth_") {
		t.Fatalf("token line should be stripped, got: %q", got)
	}
	if !strings.Contains(got, "Uplink token for tester@box") {
		t.Fatalf("non-token lines should be preserved, got: %q", got)
	}
	if !strings.Contains(got, "Done.") {
		t.Fatalf("trailing non-token lines should be preserved, got: %q", got)
	}
}

// --- selectOption tests ---

// selectorDeps builds a Deps with the given input, an output buffer, and an
// optional MakeRaw function. When makeRaw is nil the text fallback fires.
func selectorDeps(input []byte, makeRaw func() (func(), error)) (Deps, *bytes.Buffer) {
	var buf bytes.Buffer
	deps := Deps{
		Output:     &buf,
		Input:      bytes.NewReader(input),
		IsTerminal: func() bool { return makeRaw != nil },
		MakeRaw:    makeRaw,
	}
	return deps, &buf
}

func noopMakeRaw() (func(), error) { return func() {}, nil }

func TestSelectOptionRawModeDefaultEnter(t *testing.T) {
	t.Parallel()
	deps, _ := selectorDeps([]byte{'\r'}, noopMakeRaw)

	idx, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("expected default index 0, got %d", idx)
	}
}

func TestSelectOptionRawModeArrowRight(t *testing.T) {
	t.Parallel()
	// Right arrow (\x1b[C) then Enter (\r).
	deps, _ := selectorDeps([]byte{0x1b, '[', 'C', '\r'}, noopMakeRaw)

	idx, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1 after right arrow, got %d", idx)
	}
}

func TestSelectOptionRawModeArrowDownUp(t *testing.T) {
	t.Parallel()
	// Down (\x1b[B), Down (clamps at 1), Up (\x1b[A), Enter.
	deps, _ := selectorDeps([]byte{
		0x1b, '[', 'B', // down -> 1
		0x1b, '[', 'B', // down -> still 1 (clamped)
		0x1b, '[', 'A', // up -> 0
		'\r',
	}, noopMakeRaw)

	idx, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("expected index 0 after down-down-up, got %d", idx)
	}
}

func TestSelectOptionRawModeCtrlC(t *testing.T) {
	t.Parallel()
	deps, _ := selectorDeps([]byte{0x03}, noopMakeRaw)

	_, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted, got: %v", err)
	}
}

func TestSelectOptionRawModeRestoreCalledOnCtrlC(t *testing.T) {
	t.Parallel()
	restored := false
	makeRaw := func() (func(), error) {
		return func() { restored = true }, nil
	}
	deps, _ := selectorDeps([]byte{0x03}, makeRaw)

	_, _ = selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if !restored {
		t.Fatal("terminal restore must be called on Ctrl+C")
	}
}

func TestSelectOptionRawModeRestoreCalledOnSuccess(t *testing.T) {
	t.Parallel()
	restored := false
	makeRaw := func() (func(), error) {
		return func() { restored = true }, nil
	}
	deps, _ := selectorDeps([]byte{'\r'}, makeRaw)

	_, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("terminal restore must be called on success")
	}
}

func TestSelectOptionTextFallbackDefault(t *testing.T) {
	t.Parallel()
	// MakeRaw is nil — text fallback.
	deps, buf := selectorDeps([]byte("\n"), nil)

	idx, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("expected default index 0 on empty input, got %d", idx)
	}
	output := buf.String()
	if !strings.Contains(output, "[G]enerate / [P]rovide: ") {
		t.Fatalf("text fallback must show [X]yz prompt:\n%s", output)
	}
}

func TestSelectOptionTextFallbackLetter(t *testing.T) {
	t.Parallel()
	deps, _ := selectorDeps([]byte("p\n"), nil)

	idx, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1 for 'p' input, got %d", idx)
	}
}

func TestSelectOptionTextFallbackInvalid(t *testing.T) {
	t.Parallel()
	deps, _ := selectorDeps([]byte("x\n"), nil)

	idx, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != -1 {
		t.Fatalf("expected -1 for invalid input, got %d", idx)
	}
}

func TestSelectOptionTextFallbackCtrlC(t *testing.T) {
	t.Parallel()
	// In text mode, Ctrl+C (0x03) is handled by readLine as ErrInterrupted.
	deps, _ := selectorDeps([]byte{0x03}, nil)

	_, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted from text fallback Ctrl+C, got: %v", err)
	}
}

func TestSelectOptionMakeRawFailsFallsBack(t *testing.T) {
	t.Parallel()
	// MakeRaw returns an error — must fall back to text.
	makeRaw := func() (func(), error) {
		return nil, errors.New("not a terminal")
	}
	deps, buf := selectorDeps([]byte("g\n"), makeRaw)

	idx, err := selectOption(context.Background(), deps, false, "Deploy key", []string{"Generate", "Provide"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("expected index 0 on MakeRaw failure fallback, got %d", idx)
	}
	output := buf.String()
	if !strings.Contains(output, "[G]enerate / [P]rovide: ") {
		t.Fatalf("MakeRaw failure must use text fallback:\n%s", output)
	}
}

// --- listSSHKeys tests ---

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestListSSHKeysPrivateKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a valid private key.
	writeFile(t, filepath.Join(dir, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nfakedata\n-----END OPENSSH PRIVATE KEY-----\n")
	// Write a valid public key — should be excluded.
	writeFile(t, filepath.Join(dir, "id_ed25519.pub"), "ssh-ed25519 AAAA... user@host\n")
	// Write a non-key file — should be excluded.
	writeFile(t, filepath.Join(dir, "config"), "Host *\n")
	// Write another private key.
	writeFile(t, filepath.Join(dir, "work"), "-----BEGIN RSA PRIVATE KEY-----\nfakedata\n-----END RSA PRIVATE KEY-----\n")
	// Write a file without a key header — should be excluded.
	writeFile(t, filepath.Join(dir, "random.txt"), "not a key\n")
	// Write a dotfile — should be excluded.
	writeFile(t, filepath.Join(dir, ".hidden"), "-----BEGIN OPENSSH PRIVATE KEY-----\n")

	keys := listSSHKeys(dir, false)
	if len(keys) != 2 {
		t.Fatalf("expected 2 private keys, got %d: %v", len(keys), keys)
	}
	// Should be sorted alphabetically.
	if keys[0] != "~/.ssh/id_ed25519" {
		t.Errorf("expected first key '~/.ssh/id_ed25519', got %q", keys[0])
	}
	if keys[1] != "~/.ssh/work" {
		t.Errorf("expected second key '~/.ssh/work', got %q", keys[1])
	}
}

func TestListSSHKeysPublicKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "id_ed25519.pub"), "ssh-ed25519 AAAA... user@host\n")
	writeFile(t, filepath.Join(dir, "id_rsa.pub"), "ssh-rsa AAAA... user@host\n")
	writeFile(t, filepath.Join(dir, "id_ecdsa.pub"), "ecdsa-sha2-nistp256 AAAA... user@host\n")
	writeFile(t, filepath.Join(dir, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\n") // private, excluded
	writeFile(t, filepath.Join(dir, "known_hosts"), "github.com ecdsa...\n")                // metadata, excluded

	keys := listSSHKeys(dir, true)
	if len(keys) != 3 {
		t.Fatalf("expected 3 public keys, got %d: %v", len(keys), keys)
	}
	// Sorted alphabetically.
	if keys[0] != "~/.ssh/id_ecdsa.pub" {
		t.Errorf("expected '~/.ssh/id_ecdsa.pub', got %q", keys[0])
	}
	if keys[1] != "~/.ssh/id_ed25519.pub" {
		t.Errorf("expected '~/.ssh/id_ed25519.pub', got %q", keys[1])
	}
	if keys[2] != "~/.ssh/id_rsa.pub" {
		t.Errorf("expected '~/.ssh/id_rsa.pub', got %q", keys[2])
	}
}

func TestListSSHKeysEmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keys := listSSHKeys(dir, false)
	if keys != nil {
		t.Fatalf("expected nil for empty dir, got %v", keys)
	}
}

func TestListSSHKeysUnreadableDir(t *testing.T) {
	t.Parallel()
	keys := listSSHKeys("/nonexistent/path", false)
	if keys != nil {
		t.Fatalf("expected nil for unreadable dir, got %v", keys)
	}
}

func TestListSSHKeysSkipsMetadataFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	for _, name := range []string{"known_hosts", "known_hosts.old", "config", "authorized_keys", "authorized_keys2", "environment"} {
		writeFile(t, filepath.Join(dir, name), "-----BEGIN OPENSSH PRIVATE KEY-----\n")
	}

	keys := listSSHKeys(dir, false)
	if keys != nil {
		t.Fatalf("expected nil (all metadata files excluded), got %v", keys)
	}
}

func TestListSSHKeysSkipsSymlinks(t *testing.T) {
	t.Parallel()
	// Symlinks are never offered — neither links inside ~/.ssh nor links
	// escaping it. The scan classifies with Lstat so the link target is
	// never opened. A symlinked key stays reachable via manual path entry.
	dir := t.TempDir()
	outside := t.TempDir()

	writeFile(t, filepath.Join(dir, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nfakedata\n")
	writeFile(t, filepath.Join(outside, "escaped_key"), "-----BEGIN OPENSSH PRIVATE KEY-----\nfakedata\n")

	if err := os.Symlink(filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "link_inside")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "escaped_key"), filepath.Join(dir, "link_outside")); err != nil {
		t.Fatal(err)
	}

	keys := listSSHKeys(dir, false)
	if len(keys) != 1 || keys[0] != "~/.ssh/id_ed25519" {
		t.Fatalf("expected only the regular file, got %v", keys)
	}
}

func TestListSSHKeysSkipsOversizedFiles(t *testing.T) {
	t.Parallel()
	// Files larger than 16 KB are never SSH keys; the size gate must fire
	// before the file is opened, from Lstat metadata.
	dir := t.TempDir()

	big := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("A", 17*1024) + "\n"
	writeFile(t, filepath.Join(dir, "huge_blob"), big)
	writeFile(t, filepath.Join(dir, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nfakedata\n")

	keys := listSSHKeys(dir, false)
	if len(keys) != 1 || keys[0] != "~/.ssh/id_ed25519" {
		t.Fatalf("expected the oversized file to be skipped, got %v", keys)
	}
}

func TestListSSHKeysSkipsCertPub(t *testing.T) {
	t.Parallel()
	// SSH certificate files (*-cert.pub) are not key candidates — they are
	// certificates issued by an SSH CA, not standalone public keys suitable
	// for uplink registration.
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "id_ed25519.pub"), "ssh-ed25519 AAAA... user@host\n")
	writeFile(t, filepath.Join(dir, "id_ed25519-cert.pub"), "ssh-ed25519-cert-v01@openssh.com AAAA... user@host\n")
	writeFile(t, filepath.Join(dir, "id_rsa-cert.pub"), "ssh-rsa-cert-v01@openssh.com AAAA... user@host\n")

	keys := listSSHKeys(dir, true)
	if len(keys) != 1 {
		t.Fatalf("expected 1 public key (cert files excluded), got %d: %v", len(keys), keys)
	}
	if keys[0] != "~/.ssh/id_ed25519.pub" {
		t.Errorf("expected '~/.ssh/id_ed25519.pub', got %q", keys[0])
	}
}

// --- expandSSHKeyPath tests ---

func TestExpandSSHKeyPathBareName(t *testing.T) {
	t.Parallel()
	result, err := expandSSHKeyPath("work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".ssh", "work")
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestExpandSSHKeyPathTildePath(t *testing.T) {
	t.Parallel()
	result, err := expandSSHKeyPath("~/.ssh/id_rsa")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".ssh", "id_rsa")
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestExpandSSHKeyPathAbsolute(t *testing.T) {
	t.Parallel()
	result, err := expandSSHKeyPath("/tmp/mykey")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "/tmp/mykey" {
		t.Fatalf("expected '/tmp/mykey', got %q", result)
	}
}

func TestExpandSSHKeyPathEmpty(t *testing.T) {
	t.Parallel()
	result, err := expandSSHKeyPath("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Fatalf("expected empty string, got %q", result)
	}
}

func TestExpandSSHKeyPathSeparatorKeepsLiteralMeaning(t *testing.T) {
	t.Parallel()
	// Input containing a path separator is NEVER rerouted into ~/.ssh/:
	// the displayed path must be the path that is read. Rerouting would
	// silently turn "../x" into ~/x while the table shows "../x".
	for _, in := range []string{"./key", "../key", "dir/key", "../../etc/passwd"} {
		result, err := expandSSHKeyPath(in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", in, err)
		}
		if result != in {
			t.Fatalf("separator-containing input must stay literal: %q became %q", in, result)
		}
	}
}

// --- selectFromList tests ---

func TestSelectFromListDefaultEnter(t *testing.T) {
	t.Parallel()
	// Just press Enter immediately — should return the default index.
	deps, _ := selectorDeps([]byte{'\r'}, noopMakeRaw)

	idx, err := selectFromList(deps, false, []string{"~/.ssh/id_ed25519", "~/.ssh/work", "Type path manually..."}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("expected default index 0, got %d", idx)
	}
}

func TestSelectFromListArrowDown(t *testing.T) {
	t.Parallel()
	// Down arrow then Enter — should select index 1.
	deps, _ := selectorDeps([]byte{
		0x1b, '[', 'B', // down -> index 1
		'\r',
	}, noopMakeRaw)

	idx, err := selectFromList(deps, false, []string{"~/.ssh/id_ed25519", "~/.ssh/work", "Type path manually..."}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("expected index 1 after down arrow, got %d", idx)
	}
}

func TestSelectFromListCtrlC(t *testing.T) {
	t.Parallel()
	deps, _ := selectorDeps([]byte{0x03}, noopMakeRaw)

	_, err := selectFromList(deps, false, []string{"~/.ssh/id_ed25519", "Type path manually..."}, 0)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted, got: %v", err)
	}
}

func TestSelectFromListWraps(t *testing.T) {
	t.Parallel()
	// Up arrow from index 0 should wrap to the last option.
	deps, _ := selectorDeps([]byte{
		0x1b, '[', 'A', // up from 0 -> wraps to 2
		'\r',
	}, noopMakeRaw)

	idx, err := selectFromList(deps, false, []string{"a", "b", "c"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if idx != 2 {
		t.Fatalf("expected index 2 (wrapped from top), got %d", idx)
	}
}

func TestSelectFromListRestoreCalledOnSuccess(t *testing.T) {
	t.Parallel()
	// The deferred restore must run on the success path.
	restored := false
	deps, _ := selectorDeps([]byte{'\r'}, func() (func(), error) {
		return func() { restored = true }, nil
	})

	if _, err := selectFromList(deps, false, []string{"a", "b"}, 0); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("restore was not called on the success path")
	}
}

func TestSelectFromListRestoreCalledOnCtrlC(t *testing.T) {
	t.Parallel()
	// The deferred restore must run when the selection is interrupted.
	restored := false
	deps, _ := selectorDeps([]byte{0x03}, func() (func(), error) {
		return func() { restored = true }, nil
	})

	_, err := selectFromList(deps, false, []string{"a", "b"}, 0)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected ErrInterrupted, got: %v", err)
	}
	if !restored {
		t.Fatal("restore was not called on the Ctrl+C path")
	}
}

func TestSelectFromListRestoreCalledOnReadError(t *testing.T) {
	t.Parallel()
	// Input runs dry (EOF) mid-selection: the error propagates and the
	// deferred restore still runs.
	restored := false
	deps, _ := selectorDeps(nil, func() (func(), error) {
		return func() { restored = true }, nil
	})

	_, err := selectFromList(deps, false, []string{"a", "b"}, 0)
	if err == nil {
		t.Fatal("expected a read error from empty input, got nil")
	}
	if errors.Is(err, errRawModeUnavailable) {
		t.Fatalf("read error must not classify as raw-mode-unavailable: %v", err)
	}
	if !restored {
		t.Fatal("restore was not called on the read-error path")
	}
}

func TestSelectFromListRawModeNilFallsBack(t *testing.T) {
	t.Parallel()
	// MakeRaw nil (non-interactive session): the picker must signal
	// errRawModeUnavailable without consuming any input, so the free-text
	// prompt can read the stream instead.
	deps, _ := selectorDeps([]byte("typed-by-user\n"), nil)

	idx, err := selectFromList(deps, false, []string{"a", "b"}, 0)
	if !errors.Is(err, errRawModeUnavailable) {
		t.Fatalf("expected errRawModeUnavailable, got idx=%d err=%v", idx, err)
	}
	line, readErr := readLine(context.Background(), deps.Input)
	if readErr != nil || line != "typed-by-user" {
		t.Fatalf("input must be untouched for the free-text fallback, got %q err=%v", line, readErr)
	}
}

func TestSelectFromListMakeRawErrorFallsBack(t *testing.T) {
	t.Parallel()
	// MakeRaw returning an error (not a real terminal) must also signal
	// errRawModeUnavailable rather than failing the install.
	deps, _ := selectorDeps([]byte{'\r'}, func() (func(), error) {
		return nil, errors.New("inappropriate ioctl for device")
	})

	_, err := selectFromList(deps, false, []string{"a", "b"}, 0)
	if !errors.Is(err, errRawModeUnavailable) {
		t.Fatalf("expected errRawModeUnavailable, got: %v", err)
	}
}

// --- extractSSHBlockIdentityFile tests ---

func TestExtractSSHBlockIdentityFileQuoted(t *testing.T) {
	t.Parallel()
	config := "Host oberth\n    HostName localhost\n    Port 30022\n    IdentityFile \"/home/user/.ssh/id_ed25519\"\n    IdentitiesOnly yes\n"
	got := extractSSHBlockIdentityFile([]byte(config), "oberth")
	if got != "/home/user/.ssh/id_ed25519" {
		t.Fatalf("expected /home/user/.ssh/id_ed25519, got %q", got)
	}
}

func TestExtractSSHBlockIdentityFileUnquoted(t *testing.T) {
	t.Parallel()
	config := "Host oberth\n    HostName localhost\n    IdentityFile ~/.ssh/work\n"
	got := extractSSHBlockIdentityFile([]byte(config), "oberth")
	if got != "~/.ssh/work" {
		t.Fatalf("expected ~/.ssh/work, got %q", got)
	}
}

func TestExtractSSHBlockIdentityFileWrongBlock(t *testing.T) {
	t.Parallel()
	config := "Host github.com\n    IdentityFile ~/.ssh/github\n\nHost oberth\n    HostName localhost\n    IdentityFile ~/.ssh/oberth\n"
	got := extractSSHBlockIdentityFile([]byte(config), "oberth")
	if got != "~/.ssh/oberth" {
		t.Fatalf("expected ~/.ssh/oberth, got %q", got)
	}
}

func TestExtractSSHBlockIdentityFileNoBlock(t *testing.T) {
	t.Parallel()
	config := "Host github.com\n    IdentityFile ~/.ssh/github\n"
	got := extractSSHBlockIdentityFile([]byte(config), "oberth")
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExtractSSHBlockIdentityFileNoIdentityFile(t *testing.T) {
	t.Parallel()
	config := "Host oberth\n    HostName localhost\n    Port 30022\n"
	got := extractSSHBlockIdentityFile([]byte(config), "oberth")
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestExtractSSHBlockIdentityFileCaseInsensitiveKeyword(t *testing.T) {
	t.Parallel()
	config := "host oberth\n    identityfile ~/.ssh/mykey\n"
	got := extractSSHBlockIdentityFile([]byte(config), "oberth")
	if got != "~/.ssh/mykey" {
		t.Fatalf("expected ~/.ssh/mykey, got %q", got)
	}
}

func TestExtractSSHBlockIdentityFileStopsAtMatchBlock(t *testing.T) {
	t.Parallel()
	config := "Host oberth\n    HostName localhost\n\nMatch host *.example.com\n    IdentityFile ~/.ssh/example\n"
	got := extractSSHBlockIdentityFile([]byte(config), "oberth")
	if got != "" {
		t.Fatalf("expected empty (no IdentityFile before Match block), got %q", got)
	}
}

// --- updateSSHBlockIdentityFile tests ---

func TestUpdateSSHBlockIdentityFile(t *testing.T) {
	t.Parallel()
	config := "Host oberth\n    HostName localhost\n    Port 30022\n    IdentityFile \"/home/user/.ssh/id_ed25519\"\n    IdentitiesOnly yes\n"
	got := string(updateSSHBlockIdentityFile([]byte(config), "oberth", "/home/user/.ssh/work"))
	if !strings.Contains(got, `IdentityFile "/home/user/.ssh/work"`) {
		t.Fatalf("expected updated IdentityFile, got:\n%s", got)
	}
	if strings.Contains(got, "id_ed25519") {
		t.Fatalf("old IdentityFile should be replaced, got:\n%s", got)
	}
}

func TestUpdateSSHBlockIdentityFilePreservesIndent(t *testing.T) {
	t.Parallel()
	config := "Host oberth\n\tIdentityFile ~/.ssh/old\n"
	got := string(updateSSHBlockIdentityFile([]byte(config), "oberth", "/home/user/.ssh/new"))
	if !strings.Contains(got, "\tIdentityFile \"/home/user/.ssh/new\"") {
		t.Fatalf("expected tab-indented IdentityFile, got:\n%s", got)
	}
}

func TestUpdateSSHBlockIdentityFileNoChange(t *testing.T) {
	t.Parallel()
	config := "Host oberth\n    HostName localhost\n"
	got := string(updateSSHBlockIdentityFile([]byte(config), "oberth", "/home/user/.ssh/work"))
	if got != config {
		t.Fatalf("expected no change when no IdentityFile line exists, got:\n%s", got)
	}
}

func TestUpdateSSHBlockIdentityFileOnlyTargetBlock(t *testing.T) {
	t.Parallel()
	config := "Host github.com\n    IdentityFile ~/.ssh/github\n\nHost oberth\n    IdentityFile ~/.ssh/old\n"
	got := string(updateSSHBlockIdentityFile([]byte(config), "oberth", "/home/user/.ssh/new"))
	if !strings.Contains(got, "IdentityFile ~/.ssh/github") {
		t.Fatalf("github block should be untouched, got:\n%s", got)
	}
	if !strings.Contains(got, `IdentityFile "/home/user/.ssh/new"`) {
		t.Fatalf("oberth block should be updated, got:\n%s", got)
	}
}

// --- orgFromBaseURL tests ---

func TestOrgFromBaseURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"ssh://git@github.com/valariantech", "valariantech"},
		{"ssh://git@github.com/acme/", "acme"},
		{"ssh://git@codeberg.org/skipops", "skipops"},
		{"ssh://git@gitlab.com/group/subgroup", "subgroup"},
		{"", ""},
		{"ssh://git@github.com", ""},
		{"ssh://git@github.com/", ""},
	} {
		got := orgFromBaseURL(tc.input)
		if got != tc.want {
			t.Errorf("orgFromBaseURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
