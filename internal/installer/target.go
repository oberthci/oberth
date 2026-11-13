package installer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/kubeconfig"
)

type KubernetesTarget struct {
	Context    string `json:"context"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
	Namespace  string `json:"namespace"`
}

func RecordedKubernetesTarget() (KubernetesTarget, error) {
	root, err := client.ConfigDir()
	if err != nil {
		return KubernetesTarget{}, err
	}
	path := filepath.Join(root, "kubernetes.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return KubernetesTarget{}, nil
	}
	if err != nil {
		return KubernetesTarget{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return KubernetesTarget{}, errors.New("install target must be an owner-only regular file")
	}
	if err := client.TrustClientFiles(root); err != nil {
		return KubernetesTarget{}, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: fixed name in the validated operator configuration directory.
	if err != nil {
		return KubernetesTarget{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var target KubernetesTarget
	if decoder.Decode(&target) != nil || target.Context == "" || target.Namespace == "" {
		return target, errors.New("invalid recorded Kubernetes install target")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return target, errors.New("invalid trailing install target data")
	}
	return target, nil
}

func recordKubernetesTarget(cfg Config, deps Deps) error {
	if deps.ContextName == "" {
		return errors.New("install target context is required")
	}
	root, err := deps.clientConfigRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	if err := client.TrustClientFiles(root); err != nil {
		return err
	}
	target := KubernetesTarget{Context: deps.ContextName, Kubeconfig: cfg.KubeconfigPath, Namespace: cfg.Namespace}
	if target.Namespace == "" {
		target.Namespace = DefaultNamespace
	}
	if target.Kubeconfig != "" {
		target.Kubeconfig, err = filepath.Abs(target.Kubeconfig)
		if err != nil {
			return err
		}
	}
	body, err := json.Marshal(target)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(root, "kubernetes.json"), append(body, '\n'), 0600)
}

// Explicit flags win. Commands without them use the install record, never an
// ambient current-context; an initial single-context configuration is resolved
// explicitly and ambiguous configurations require --context.
func pinnedKubernetesCommand(name string, args, environment []string) ([]string, []string, *os.File, func() error, error) {
	base := filepath.Base(name)
	if base != "kubectl" && base != "helm" {
		return kubeconfig.Command(name, args, environment)
	}
	explicitContext, explicitConfig := false, false
	configPath := ""
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if (arg == "--context" || arg == "--kube-context") && (i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "--")) {
			return nil, nil, nil, func() error { return nil }, errors.New("explicit Kubernetes context must be nonempty")
		}
		if arg == "--context=" || arg == "--kube-context=" {
			return nil, nil, nil, func() error { return nil }, errors.New("explicit Kubernetes context must be nonempty")
		}
		explicitContext = explicitContext || arg == "--context" || arg == "--kube-context" || strings.HasPrefix(arg, "--context=") || strings.HasPrefix(arg, "--kube-context=")
		explicitConfig = explicitConfig || arg == "--kubeconfig" || strings.HasPrefix(arg, "--kubeconfig=")
		if arg == "--kubeconfig" && i+1 < len(args) {
			configPath = args[i+1]
		}
		if strings.HasPrefix(arg, "--kubeconfig=") {
			configPath = strings.TrimPrefix(arg, "--kubeconfig=")
		}
	}
	local := len(args) > 0 && ((base == "kubectl" && args[0] == "config") || (base == "helm" && (args[0] == "repo" || args[0] == "version" || args[0] == "template" || args[0] == "show" || args[0] == "lint")))
	if !explicitContext && !local {
		target, err := RecordedKubernetesTarget()
		if err != nil {
			return nil, nil, nil, func() error { return nil }, err
		}
		if target.Context != "" {
			contextFlag := "--context"
			if base == "helm" {
				contextFlag = "--kube-context"
			}
			args = append([]string{contextFlag, target.Context}, args...)
			if !explicitConfig && target.Kubeconfig != "" {
				args = append([]string{"--kubeconfig", target.Kubeconfig}, args...)
			}
		} else {
			{
				rules := kubeconfig.Rules(configPath)
				raw, err := kubeconfig.Read(rules)
				if err != nil || len(raw.Contexts) != 1 {
					return nil, nil, nil, func() error { return nil }, fmt.Errorf("select --context or install a recorded Kubernetes target")
				}
				for selected := range raw.Contexts {
					flag := "--context"
					if base == "helm" {
						flag = "--kube-context"
					}
					args = append([]string{flag, selected}, args...)
				}
			}
		}
	}
	return kubeconfig.Command(name, args, environment)
}
