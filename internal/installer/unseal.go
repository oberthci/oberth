package installer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/kubeconfig"
)

type UnsealConfig struct {
	KubeconfigPath   string
	ContextName      string
	OpenBaoNamespace string
	SecretsFile      string
	AgeIdentity      string
	StatusOnly       bool
	SealToHost       string
	Yes              bool
}

type SealStatus struct {
	Initialized bool `json:"initialized"`
	Sealed      bool `json:"sealed"`
}

// ExecuteUnseal reaches the verified OpenBao pod on the selected context. No
// root token is needed, and command output containing secrets never escapes.
func ExecuteUnseal(ctx context.Context, cfg UnsealConfig, host InstallDeps) (SealStatus, error) {
	if host.RunCommand == nil {
		host.RunCommand = privateUnsealCommand
	}
	host.Output = io.Discard
	host = host.withDefaults()
	limit := 2 * time.Minute
	if cfg.StatusOnly {
		limit = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if _, err := host.LookPath("kubectl"); err != nil {
		return SealStatus{}, errors.New("kubectl is required; install it before unsealing")
	}
	run := host.RunCommand
	var deps Deps
	if cfg.KubeconfigPath != "" {
		rules := kubeconfig.Rules("")
		rules.ExplicitPath = cfg.KubeconfigPath
		kube, rest, name, err := loadKubeConfigWithRules(rules, cfg.ContextName, "", io.Discard)
		if err != nil {
			return SealStatus{}, errors.New("cannot load selected kubeconfig")
		}
		deps.KubeClient, deps.RestConfig, deps.ContextName = kube, rest, name
		run = withKubeconfigArg(run, cfg.KubeconfigPath)
	} else {
		kube, rest, name, err := host.LoadKubeConfig(cfg.ContextName)
		if err != nil {
			return SealStatus{}, errors.New("cannot load selected kubeconfig")
		}
		deps.KubeClient, deps.RestConfig, deps.ContextName = kube, rest, name
	}
	identity := publicClusterIdentity(Config{KubeconfigPath: cfg.KubeconfigPath}, deps.RestConfig, deps.ContextName)
	if !checkClusterTrust(deps.RestConfig, identity, &InstallResult{}) {
		return SealStatus{}, errors.New("unseal requires an HTTPS kubeconfig with an explicit trusted cluster CA and TLS verification enabled")
	}
	deps.RunCommand = run
	if cfg.OpenBaoNamespace == "" {
		cfg.OpenBaoNamespace = DefaultOpenBaoNamespace
	}
	return unsealWithDeps(ctx, cfg, deps)
}

func unsealWithDeps(ctx context.Context, cfg UnsealConfig, deps Deps) (SealStatus, error) {
	pod, err := findOpenBaoPod(ctx, deps, cfg.OpenBaoNamespace)
	if err != nil {
		return SealStatus{}, errors.New("cannot verify running OpenBao pod and controller")
	}
	client := newOpenBaoExec(deps, cfg.OpenBaoNamespace, pod)
	status, err := readSealStatus(ctx, client)
	if err != nil {
		return status, err
	}
	if cfg.StatusOnly {
		return status, nil
	}
	if !status.Initialized {
		return status, errors.New("OpenBao is not initialized; finish installation first")
	}
	if !status.Sealed && cfg.SealToHost == "" {
		return status, nil
	}
	key, err := readRecoveryUnsealKey(ctx, cfg.SecretsFile, cfg.AgeIdentity)
	if err != nil {
		return status, err
	}
	defer clear(key)
	if cfg.SealToHost != "" {
		return status, sealUnsealKeyToHost(ctx, cfg, deps, key)
	}
	input := append([]byte(`{"key":"`), key...)
	input = append(input, []byte(`"}`)...)
	defer clear(input)
	out, callErr := client.bao(ctx, input, "write", "-format=json", "sys/unseal", "-")
	clear(out)
	if callErr != nil {
		return status, errors.New("OpenBao unseal failed; verify the recovery file and selected cluster")
	}
	// Read status independently; never trust an echoed key or arbitrary response.
	status, err = readSealStatus(ctx, client)
	if err != nil {
		return status, err
	}
	if status.Sealed {
		return status, errors.New("OpenBao remains sealed; this command requires the installer's single-share recovery key")
	}
	return status, nil
}

func readSealStatus(ctx context.Context, client openBaoExec) (SealStatus, error) {
	out, commandErr := client.bao(ctx, nil, "status", "-format=json")
	defer clear(out)
	var state struct {
		Initialized *bool `json:"initialized"`
		Sealed      *bool `json:"sealed"`
	}
	if json.Unmarshal(out, &state) != nil || state.Initialized == nil || state.Sealed == nil {
		return SealStatus{}, errors.New("cannot read OpenBao seal status")
	}
	// bao status exits nonzero while sealed; an unsealed result must succeed.
	if !*state.Sealed && commandErr != nil {
		return SealStatus{}, errors.New("cannot confirm OpenBao unsealed status")
	}
	return SealStatus{Initialized: *state.Initialized, Sealed: *state.Sealed}, nil
}

const maxRecoveryBytes = 1 << 20

func openPrivateRecoveryFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: explicit operator path, descriptor validated below.
	if err != nil {
		return nil, errors.New("cannot open private recovery input")
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.New("cannot inspect private recovery input")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 || info.Size() > maxRecoveryBytes {
		_ = f.Close()
		return nil, errors.New("recovery inputs must be owner-only regular files owned by this user, without links, at most 1 MiB")
	}
	return f, nil
}

func readRecoveryUnsealKey(ctx context.Context, path, identityPath string) ([]byte, error) {
	f, err := openPrivateRecoveryFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, maxRecoveryBytes+1))
	defer clear(body)
	if err != nil || len(body) > maxRecoveryBytes {
		return nil, errors.New("cannot read recovery input")
	}
	if bytes.HasPrefix(body, []byte("age-encryption.org/")) || bytes.HasPrefix(body, []byte("-----BEGIN AGE ENCRYPTED FILE-----")) {
		plain, err := decryptRecovery(ctx, body, identityPath)
		if err != nil {
			return nil, err
		}
		defer clear(plain)
		return recoveryKey(plain)
	}
	return recoveryKey(body)
}

func decryptRecovery(ctx context.Context, cipher []byte, identityPath string) ([]byte, error) {
	identity, err := openPrivateRecoveryFile(identityPath)
	if err != nil {
		return nil, errors.New("encrypted recovery needs --age-identity naming an owner-only age identity file")
	}
	defer func() { _ = identity.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "age", "--decrypt", "--identity", "/dev/fd/3")
	cmd.Env = installerChildEnvironment()
	cmd.ExtraFiles = []*os.File{identity}
	cmd.Stdin = bytes.NewReader(cipher)
	cmd.Stderr = io.Discard
	var plain bytes.Buffer
	cmd.Stdout = &plain
	if err := cmd.Run(); err != nil {
		clear(plain.Bytes())
		return nil, errors.New("age decryption failed; install age and check the identity file")
	}
	if plain.Len() > maxRecoveryBytes {
		clear(plain.Bytes())
		return nil, errors.New("decrypted recovery input is too large")
	}
	return plain.Bytes(), nil
}

func recoveryKey(body []byte) ([]byte, error) {
	var key []byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		name, value, ok := bytes.Cut(line, []byte("="))
		if !ok {
			return nil, errors.New("invalid recovery file format")
		}
		if bytes.Equal(name, []byte("OPENBAO_UNSEAL_KEY")) {
			if key != nil {
				return nil, errors.New("duplicate unseal key in recovery file")
			}
			key = value
		}
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(key)))
	defer clear(decoded)
	size, err := base64.StdEncoding.Decode(decoded, key)
	if err != nil || size < 16 || size > 128 {
		return nil, errors.New("recovery file has no valid single-share unseal key")
	}
	return bytes.Clone(key), nil
}

func PrintSealStatus(w io.Writer, status SealStatus, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(w).Encode(status)
	}
	_, err := fmt.Fprintf(w, "initialized: %t, sealed: %t\n", status.Initialized, status.Sealed)
	return err
}

// Capture only stdout; child diagnostics never enter a transcript or error.
func privateUnsealCommand(ctx context.Context, input []byte, name string, args ...string) (result []byte, resultErr error) {
	args, environment, lease, cleanup, prepareErr := pinnedKubernetesCommand(name, args, installerChildEnvironment())
	if prepareErr != nil {
		return nil, prepareErr
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()

	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed operator executables, structured arguments.
	cmd.Env = environment
	if lease != nil {
		cmd.ExtraFiles = []*os.File{lease}
	}
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), errors.New("private unseal command failed")
	}
	return out.Bytes(), nil
}
