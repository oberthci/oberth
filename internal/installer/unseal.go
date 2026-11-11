package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const maxUnsealFile = 4 << 20

// UnsealConfig names public coordinates only. Credentials travel over stdin.
type UnsealConfig struct {
	SecretsFile string
	AgeIdentity string
	Context     string
	Namespace   string
	Pod         string
}

// ReadUnsealSecret accepts the installer ceremony format, never shell code.
// Child output and parser errors may echo credentials and are never returned.
// The caller must clear the returned buffer after use.
func ReadUnsealSecret(ctx context.Context, path, identity string, run CommandRunner) ([]byte, error) { //nolint:gosec // G304: caller-selected file; O_NOFOLLOW, regular-file and owner-only checks precede reading
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: operator-named file, validated below
	if err != nil {
		return nil, errors.New("cannot open secrets file")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("secrets file must be a regular owner-only file (chmod 600)")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxUnsealFile+1))
	if err != nil || len(raw) > maxUnsealFile {
		clear(raw)
		return nil, errors.New("cannot read secrets file or file exceeds size limit")
	}
	defer func() { clear(raw) }()
	if bytes.HasPrefix(raw, []byte("age-encryption.org/v1")) || bytes.HasPrefix(raw, []byte("-----BEGIN AGE ENCRYPTED FILE-----")) {
		if identity == "" {
			return nil, errors.New("encrypted secrets file needs --age-identity pointing to an age identity file")
		}
		if run == nil {
			run = privateUnsealCommand
		}
		plain, decryptErr := run(ctx, raw, "age", "--decrypt", "--identity", identity)
		defer clear(plain)
		if decryptErr != nil || len(plain) > maxUnsealFile {
			return nil, errors.New("cannot decrypt secrets file; check age and the identity file")
		}
		return parseUnsealSecret(plain)
	}
	return parseUnsealSecret(raw)
}

func parseUnsealSecret(raw []byte) ([]byte, error) {
	var key []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		name, value, ok := bytes.Cut(line, []byte("="))
		if !ok {
			clear(key)
			return nil, errors.New("invalid secrets file")
		}
		if !bytes.Equal(name, []byte("OPENBAO_UNSEAL_KEY")) {
			continue
		}
		if len(key) != 0 || len(value) == 0 || len(value) > 4096 || bytes.ContainsAny(value, " \t\r\x00") {
			clear(key)
			return nil, errors.New("secrets file needs exactly one nonempty OpenBao unseal key")
		}
		key = bytes.Clone(value)
	}
	if len(key) == 0 {
		return nil, errors.New("secrets file has no OpenBao unseal key")
	}
	return key, nil
}

// Unseal allows only a validated seal status to leave this boundary.
func Unseal(ctx context.Context, cfg UnsealConfig, run CommandRunner) (bool, error) {
	if cfg.Context == "" || cfg.Namespace == "" || cfg.Pod == "" || strings.HasPrefix(cfg.Pod, "-") {
		return true, errors.New("unseal requires an explicit context, namespace and pod")
	}
	if run == nil {
		run = privateUnsealCommand
	}
	b := openBaoExec{run: run, contextName: cfg.Context, namespace: cfg.Namespace, pod: cfg.Pod}
	statusOut, statusErr := b.bao(ctx, nil, "status", "-format=json")
	defer clear(statusOut)
	var status struct {
		Initialized *bool `json:"initialized"`
		Sealed      *bool `json:"sealed"`
	}
	if json.Unmarshal(statusOut, &status) != nil || status.Initialized == nil || status.Sealed == nil {
		return true, errors.New("cannot read OpenBao seal status; check kubectl, context and pod")
	}
	if !*status.Initialized {
		return true, errors.New("OpenBao is not initialized")
	}
	if !*status.Sealed {
		if statusErr != nil {
			return true, errors.New("cannot read OpenBao seal status")
		}
		return false, nil
	}
	key, err := ReadUnsealSecret(ctx, cfg.SecretsFile, cfg.AgeIdentity, run)
	if err != nil {
		return true, err
	}
	defer clear(key)
	payload, err := json.Marshal(struct {
		Key string `json:"key"`
	}{string(key)})
	if err != nil {
		return true, errors.New("cannot encode unseal request")
	}
	defer clear(payload)
	response, err := b.bao(ctx, payload, "write", "-format=json", "sys/unseal", "-")
	defer clear(response)
	if err != nil {
		return true, errors.New("OpenBao unseal request failed; no command output was displayed")
	}
	var result struct {
		Sealed *bool `json:"sealed"`
	}
	if json.Unmarshal(response, &result) != nil || result.Sealed == nil {
		return true, errors.New("cannot read OpenBao unseal status")
	}
	if *result.Sealed {
		return true, errors.New("OpenBao remains sealed")
	}
	return false, nil
}

// Keep stdout separate from diagnostics: warnings must not become decrypted
// plaintext or corrupt a systemd credential. Diagnostics are never displayed.
func privateUnsealCommand(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed executables and structured public arguments selected by this package
	command.Env = installerChildEnvironment()
	command.Stdin = bytes.NewReader(input)
	command.Stderr = io.Discard
	return command.Output()
}
