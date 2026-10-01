package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/service"

	"k8s.io/client-go/kubernetes"
)

const maxSecretStoreVerifyOutput = 64 << 10

// buildSecretStoreVerifier exposes the real CLI verifier using only the running
// server's configuration. The service enforces admin authorization before this
// callback may read trust files, request a token, or contact the secret store.
func buildSecretStoreVerifier(options serveOptions, kube kubernetes.Interface) service.SecretStoreVerifier {
	return func(ctx context.Context, request api.SecretStoreVerifyRequest) (api.SecretStoreVerifyResponse, error) {
		sources := secretStoreVerifySources{
			server: func() (secretStoreVerifyConfig, error) {
				return secretStoreVerifyConfig{
					address: options.secretStoreAddress, authMount: options.secretStoreAuthMount,
					role: options.secretStoreRole, caCertPath: options.secretStoreCACert,
					saTokenPath: options.secretStoreSAToken, kvMount: options.secretStoreKVMount,
					insecureHTTP: options.secretStoreInsecureHTTP,
					paths:        append([]string(nil), options.secretStorePaths...),
				}, nil
			},
			release: func() (releaseTierVerifyConfig, error) {
				return releaseTierVerifyConfig{
					vaultAddress: options.argoVaultAddress, vaultCACertPath: options.argoVaultCACert,
					vaultCredentialedRole: options.argoVaultCredentialedRole,
					credentialedAccount:   options.argoCredentialedAccount, pipelineNamespace: options.argoNamespace,
				}, nil
			},
			kube: func() (kubernetes.Interface, error) {
				if kube == nil {
					return nil, errors.New("kubernetes client unavailable")
				}
				return kube, nil
			},
		}
		arguments := []string{"--timeout=" + strconv.Itoa(request.Timeout) + "s"}
		if request.ReleaseTier {
			arguments = append(arguments, "--release-tier")
		}
		if request.Repo != "" {
			arguments = append(arguments, "--repo="+request.Repo)
		}
		if request.Tier != "" {
			arguments = append(arguments, "--tier="+request.Tier)
		}
		if request.Keys {
			arguments = append(arguments, "--keys")
		}
		for _, path := range request.Paths {
			arguments = append(arguments, "--path="+path)
		}
		for _, expect := range request.Expect {
			arguments = append(arguments, "--expect="+expect)
		}
		var output secretStoreVerifyOutput
		err := runSecretStoreVerifyWithSources(ctx, arguments, &output, sources)
		if ctx.Err() != nil {
			return api.SecretStoreVerifyResponse{}, ctx.Err()
		}
		if err != nil {
			_, _ = fmt.Fprintf(&output, "verification failed: %v\n", err)
		}
		if output.overflow {
			return api.SecretStoreVerifyResponse{Output: "Verification output exceeded 64 KiB; narrow the requested paths or field names and retry. No complete verification result is available.\n"}, nil
		}
		return api.SecretStoreVerifyResponse{Verified: err == nil, Output: output.buffer.String()}, nil
	}
}

type secretStoreVerifyOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (output *secretStoreVerifyOutput) Write(body []byte) (int, error) {
	if len(body) > maxSecretStoreVerifyOutput-output.buffer.Len() {
		output.overflow = true
		return 0, errors.New("secret-store verification output exceeds 64 KiB")
	}
	return output.buffer.Write(body)
}
