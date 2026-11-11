package app

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
)

// Both status views publish the server's own SSH host key, so a client can
// pin the Git endpoint from the CA-verified API instead of trusting it on
// first use.
func TestHealthStatusPublishesServerSSHHostKey(t *testing.T) {
	t.Parallel()
	key, _ := testClientIdentity(t)
	health := Health{SSHHostKey: key}
	for name, read := range map[string]func() (any, error){
		"status": func() (any, error) { return health.Status(context.Background()) },
		"scoped": func() (any, error) { return health.ScopedStatus(context.Background(), api.Actor{}) },
	} {
		value, err := read()
		if err != nil {
			t.Fatal(err)
		}
		status := value.(HealthStatus)
		if status.SSHHostKeyFingerprint != ssh.FingerprintSHA256(key) || !strings.HasPrefix(status.SSHHostKey, "ssh-ed25519 ") {
			t.Fatalf("%s host key = %q %q", name, status.SSHHostKey, status.SSHHostKeyFingerprint)
		}
	}
}
