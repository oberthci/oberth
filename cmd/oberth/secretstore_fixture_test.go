package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const maximumServeCmdlineBytes = 1 << 20

func defaultKubeClient() (kubernetes.Interface, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster Kubernetes config: %w (run inside the Oberth pod)", err)
	}
	restConfig.Timeout = 10 * time.Second
	return kubernetes.NewForConfig(restConfig)
}

func secretStoreVerifySourcesFromCmdline(procCmdline string, kube kubeClientFactory) secretStoreVerifySources {
	return secretStoreVerifySources{
		server:  func() (secretStoreVerifyConfig, error) { return secretStoreConfigFromServeCmdline(procCmdline) },
		release: func() (releaseTierVerifyConfig, error) { return releaseTierConfigFromServeCmdline(procCmdline) },
		kube:    kube,
	}
}

func runSecretStoreVerify(ctx context.Context, arguments []string, output io.Writer, procCmdline string) error {
	return runSecretStoreVerifyWithSources(ctx, arguments, output, secretStoreVerifySourcesFromCmdline(procCmdline, defaultKubeClient))
}

func secretStoreConfigFromServeCmdline(path string) (secretStoreVerifyConfig, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed /proc/1/cmdline, overridden only by tests.
	if err != nil {
		return secretStoreVerifyConfig{}, fmt.Errorf("read serve process command line: %w", err)
	}
	if len(raw) > maximumServeCmdlineBytes {
		return secretStoreVerifyConfig{}, errors.New("serve process command line exceeds the size bound")
	}
	fields := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(fields) < 2 || !strings.HasSuffix(fields[0], "oberth") || fields[1] != "serve" {
		return secretStoreVerifyConfig{}, errors.New("PID 1 is not an oberth serve process")
	}
	var config secretStoreVerifyConfig
	arguments := fields[2:]
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if len(argument) < 2 || argument[0] != '-' {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-")
		value := ""
		hasValue := false
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			value = name[equals+1:]
			name = name[:equals]
			hasValue = true
		}
		consumeValue := func() error {
			if hasValue {
				return nil
			}
			index++
			if index >= len(arguments) {
				return fmt.Errorf("serve flag --%s has no value", name)
			}
			value = arguments[index]
			return nil
		}
		switch name {
		case "secretstore-address":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.address = value
		case "secretstore-k8s-auth-mount":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.authMount = value
		case "secretstore-role":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.role = value
		case "secretstore-ca-cert":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.caCertPath = value
		case "secretstore-sa-token":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.saTokenPath = value
		case "secretstore-kv-mount":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.kvPath = value
		case "secretstore-path":
			if err := consumeValue(); err != nil {
				return secretStoreVerifyConfig{}, err
			}
			config.paths = append(config.paths, value)
		case "secretstore-insecure-http":
			// The Go flag package accepts booleans only as -flag or -flag=value.
			enabled := true
			if hasValue {
				parsed, parseErr := strconv.ParseBool(value)
				if parseErr != nil {
					return secretStoreVerifyConfig{}, fmt.Errorf("serve flag --secretstore-insecure-http has invalid value %q", value)
				}
				enabled = parsed
			}
			config.insecureHTTP = enabled
		}
	}
	return config, nil
}

func releaseTierConfigFromServeCmdline(path string) (releaseTierVerifyConfig, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- fixed /proc/1/cmdline, overridden only by tests.
	if err != nil {
		return releaseTierVerifyConfig{}, fmt.Errorf("read serve process command line: %w", err)
	}
	if len(raw) > maximumServeCmdlineBytes {
		return releaseTierVerifyConfig{}, errors.New("serve process command line exceeds the size bound")
	}
	fields := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	if len(fields) < 2 || !strings.HasSuffix(fields[0], "oberth") || fields[1] != "serve" {
		return releaseTierVerifyConfig{}, errors.New("PID 1 is not an oberth serve process")
	}
	var config releaseTierVerifyConfig
	arguments := fields[2:]
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if len(argument) < 2 || argument[0] != '-' {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(argument, "-"), "-")
		value := ""
		hasValue := false
		if equals := strings.IndexByte(name, '='); equals >= 0 {
			value = name[equals+1:]
			name = name[:equals]
			hasValue = true
		}
		consumeValue := func() error {
			if hasValue {
				return nil
			}
			index++
			if index >= len(arguments) {
				return fmt.Errorf("serve flag --%s has no value", name)
			}
			value = arguments[index]
			return nil
		}
		switch name {
		case "argo-vault-address":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.vaultAddress = value
		case "argo-vault-ca-cert":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.vaultCACertPath = value
		case "argo-vault-credentialed-role":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.vaultCredentialedRole = value
		case "argo-credentialed-serviceaccount":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.credentialedAccount = value
		case "argo-namespace":
			if err := consumeValue(); err != nil {
				return releaseTierVerifyConfig{}, err
			}
			config.pipelineNamespace = value
		}
	}
	return config, nil
}
