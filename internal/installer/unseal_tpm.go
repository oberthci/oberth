package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
)

// SealUnsealCredential explicitly opts a single local node into TPM2-bound
// recovery. No software key fallback is permitted.
func SealUnsealCredential(ctx context.Context, cfg UnsealConfig, path string, run CommandRunner) error {
	if runtime.GOOS != "linux" {
		return errors.New("TPM2 auto-unseal requires Linux with systemd and TPM2; nothing was written")
	}
	machineID, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return errors.New("cannot verify this host's machine ID")
	}
	return sealUnsealCredential(ctx, cfg, path, machineID, run)
}

func sealUnsealCredential(ctx context.Context, cfg UnsealConfig, path string, machineID []byte, run CommandRunner) error {
	if cfg.Context == "" || len(bytes.TrimSpace(machineID)) == 0 {
		return errors.New("TPM2 auto-unseal needs an explicit context and this host's machine ID")
	}
	if run == nil {
		run = privateUnsealCommand
	}
	probe, err := run(ctx, nil, "systemd-creds", "has-tpm2")
	clear(probe)
	if err != nil {
		return errors.New("TPM2 auto-unseal refused: systemd TPM2 support is unavailable")
	}
	nodes, err := run(ctx, nil, "kubectl", "--context", cfg.Context, "get", "nodes", "-o", "json")
	defer clear(nodes)
	var cluster struct {
		Items []struct {
			Spec struct {
				ProviderID string `json:"providerID"`
			} `json:"spec"`
			Status struct {
				NodeInfo struct {
					MachineID string `json:"machineID"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err != nil || json.Unmarshal(nodes, &cluster) != nil || len(cluster.Items) != 1 {
		return errors.New("TPM2 auto-unseal refused: requires exactly one verified local node")
	}
	node := cluster.Items[0]
	if (node.Spec.ProviderID != "" && !strings.HasPrefix(node.Spec.ProviderID, "k3s://")) || node.Status.NodeInfo.MachineID != string(bytes.TrimSpace(machineID)) {
		return errors.New("TPM2 auto-unseal refused on managed clusters or a node other than this host")
	}
	checked, err := CheckSecretsFilePath(path)
	if err != nil {
		return errors.New("auto-unseal credential needs a new path outside a git worktree")
	}
	key, err := ReadUnsealSecret(ctx, cfg.SecretsFile, cfg.AgeIdentity, run)
	if err != nil {
		return err
	}
	defer clear(key)
	plain := append([]byte("OPENBAO_UNSEAL_KEY="), key...)
	plain = append(plain, '\n')
	defer clear(plain)
	encrypted, err := run(ctx, plain, "systemd-creds", "encrypt", "--with-key=tpm2", "--tpm2-device=auto", "--name=oberth-secrets", "-", "-")
	defer clear(encrypted)
	if err != nil || len(encrypted) == 0 {
		return errors.New("TPM2 credential encryption failed; nothing was written")
	}
	file, err := os.OpenFile(checked, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: validated new operator-selected path
	if err != nil {
		return errors.New("cannot create auto-unseal credential; existing files are never overwritten")
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(checked)
		}
	}()
	if _, err := file.Write(encrypted); err != nil {
		return errors.New("cannot write encrypted auto-unseal credential")
	}
	if err := file.Sync(); err != nil {
		return errors.New("cannot sync encrypted auto-unseal credential")
	}
	if err := file.Close(); err != nil {
		return errors.New("cannot close encrypted auto-unseal credential")
	}
	keep = true
	return nil
}
