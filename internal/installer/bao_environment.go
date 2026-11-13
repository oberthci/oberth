package installer

import (
	"errors"
	"os"
	"strings"
)

// Keep operator credentials in this invocation only. Child tools receive them
// solely through the explicitly constructed OpenBao stdin request.
func captureBaoEnvironment(deps *Deps) error {
	if !deps.baoTokenCaptured {
		deps.baoToken = os.Getenv(baoTokenEnvVar)
		deps.baoTokenCaptured = true
	}
	if err := os.Unsetenv(baoTokenEnvVar); err != nil {
		return err
	}
	return os.Unsetenv(vaultTokenEnvVar)
}
func installerBaoToken(deps Deps) string {
	if deps.baoTokenCaptured {
		return deps.baoToken
	}
	return os.Getenv(baoTokenEnvVar)
}
func installerChildEnvironment() []string {
	var result []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name != baoTokenEnvVar && name != vaultTokenEnvVar {
			result = append(result, entry)
		}
	}
	return result
}

const recoveryInputRemedy = "Supply an owner-only install credentials file containing OPENBAO_ROOT_TOKEN with --resume-secrets-file PATH (and --resume-age-identity PATH for age encryption), or reuse it as --secrets-file PATH. BAO_TOKEN is also accepted. The installer reads the token privately; do not print it or copy it into a command."

func installRootToken(cfg Config, deps Deps) string {
	if len(cfg.resumeRootToken) > 0 {
		return string(cfg.resumeRootToken)
	}
	return installerBaoToken(deps)
}

func missingRootToken() error {
	return &installStepError{check: CheckResult{Code: "secret_store_credentials", Reason: "no OpenBao root token is available for server PKI inspection or production secret-store setup", Remedy: recoveryInputRemedy}}
}

// OpenBao's alternate token name is scrubbed, not accepted as install input.
const vaultTokenEnvVar = "VAULT_TOKEN"

func unavailableServerPKI(token string, lookupErr error) error {
	if token == "" {
		return missingRootToken()
	}
	return errors.Join(errors.New("cannot check server PKI: the OpenBao pod is unavailable; restore it in the selected namespace and resume"), lookupErr)
}
