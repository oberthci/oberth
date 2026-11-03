package installer

import (
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
	return os.Unsetenv("VAULT_TOKEN")
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
		if name != baoTokenEnvVar && name != "VAULT_TOKEN" {
			result = append(result, entry)
		}
	}
	return result
}
