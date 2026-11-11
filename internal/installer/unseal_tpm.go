package installer

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sealUnsealKeyToHost(ctx context.Context, cfg UnsealConfig, deps Deps, key []byte) error {
	if !cfg.Yes {
		return errors.New("host-bound auto-unseal means anyone who boots this disk on this machine gets the secrets; explicitly accept with --yes")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("TPM2 credential setup must run as root on the single Linux node")
	}
	machine, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return errors.New("cannot identify this host for TPM2 binding")
	}
	nodes, err := deps.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("cannot inspect nodes for host-bound unseal")
	}
	if err := validateAutoUnsealNode(nodes.Items, strings.TrimSpace(string(machine))); err != nil {
		return err
	}
	out, err := deps.RunCommand(ctx, nil, "systemctl", "is-active", "--quiet", "k3s.service")
	clear(out)
	if err != nil {
		return errors.New("host-bound boot unseal requires the local k3s system service")
	}
	return writeTPMCredential(ctx, cfg.SealToHost, key, deps.RunCommand)
}

func validateAutoUnsealNode(nodes []corev1.Node, machine string) error {
	if len(nodes) != 1 || machine == "" || nodes[0].Status.NodeInfo.MachineID != machine {
		return errors.New("host-bound auto-unseal requires exactly one node whose machine ID matches this host")
	}
	provider := nodes[0].Spec.ProviderID
	if provider != "" && !strings.HasPrefix(provider, "k3s://") {
		return errors.New("host-bound auto-unseal is not available on managed clusters")
	}
	return nil
}

func writeTPMCredential(ctx context.Context, path string, key []byte, run CommandRunner) error {
	file, err := createSecretsFile(path)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = file.file.Close()
		if !committed {
			_ = os.Remove(file.path)
		}
	}()
	input := append([]byte("OPENBAO_UNSEAL_KEY="), key...)
	input = append(input, '\n')
	defer clear(input)
	// An explicit TPM2 key type refuses missing TPM support; never use auto/host/null.
	out, err := run(ctx, input, "systemd-creds", "encrypt", "--with-key=tpm2", "--tpm2-device=auto", "--name=unseal-key", "-", "-")
	defer clear(out)
	if err != nil || len(out) == 0 {
		return errors.New("TPM2 sealing failed; systemd-creds and a usable TPM2 are required; no auto-unseal credential was saved")
	}
	if _, err := file.file.Write(out); err != nil {
		return errors.New("cannot save TPM2 credential")
	}
	if err := file.file.Sync(); err != nil {
		return errors.New("cannot sync TPM2 credential")
	}
	committed = true
	return nil
}
