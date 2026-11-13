package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/kubeconfig"
)

func clusterCAHash(config *rest.Config) (string, error) {
	if config == nil || config.Host == "" {
		return "", errors.New("cluster API server is required")
	}
	ca := config.CAData
	if len(ca) == 0 && config.CAFile != "" {
		source, err := filepath.EvalSymlinks(config.CAFile)
		if err != nil {
			return "", err
		}
		file, err := os.OpenInRoot(filepath.Dir(source), filepath.Base(source))
		if err != nil {
			return "", err
		}
		var readErr error
		ca, readErr = io.ReadAll(io.LimitReader(file, 1<<20+1))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return "", err
		}
	}
	if len(ca) > 1<<20 {
		return "", errors.New("cluster CA bundle exceeds 1 MiB")
	}
	sum := sha256.Sum256(ca)
	return hex.EncodeToString(sum[:]), nil
}

func verifyRecordedCluster(contextName string, config *rest.Config) error {
	target, err := RecordedKubernetesTarget()
	if err != nil {
		return err
	}
	if target.Context == "" || target.Context != contextName {
		return nil
	}
	hash, err := clusterCAHash(config)
	if err != nil {
		return err
	}
	if target.APIServer != config.Host || target.CASHA256 != hash || target.TLSServerName != config.ServerName || target.InsecureTLS != config.Insecure {
		return errors.New("recorded Kubernetes cluster identity changed (API server or CA); select another context explicitly or restore the recorded configuration")
	}
	return nil
}

func verifiedKubernetesCommand(name string, args, environment []string, local bool) ([]string, []string, *os.File, func() error, error) {
	args, environment, lease, cleanup, err := kubeconfig.Command(name, args, environment)
	if err != nil || local {
		return args, environment, lease, cleanup, err
	}
	// Verify the immutable private copy the child will actually read, rather than
	// checking a source kubeconfig which may change between check and execution.
	raw, config, err := kubeconfig.Client(kubeconfig.Rules(args[1]), "")
	if err == nil {
		err = verifyRecordedCluster(raw.CurrentContext, config)
	}
	if err != nil {
		return nil, nil, nil, func() error { return nil }, errors.Join(err, cleanup())
	}
	return args, environment, lease, cleanup, nil
}
