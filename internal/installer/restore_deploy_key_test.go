package installer

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestRestorePrintsOnlyCurrentPublicDeployKeyAndNextStep(t *testing.T) {
	public, private := newEd25519HostKey(t)
	calls := 0
	deps := Deps{ContextName: "selected", RunCommand: func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
		calls++
		if len(input) != 0 || name != "kubectl" || strings.Join(args, " ") != "--context selected exec -n oberth pod -c oberth -- /usr/local/bin/oberth admin read-file /run/oberth-identities/upstream/id_ed25519.pub" {
			t.Fatal("unexpected public key read")
		}
		return ssh.MarshalAuthorizedKey(public), nil
	}}
	key := readRestoreDeployPublicKey(t.Context(), deps, "oberth", "pod", restoreDeployPublicPath(serverDeployment{}))
	if key == "" || calls != 1 {
		t.Fatal("public deploy key missing")
	}
	var output bytes.Buffer
	printRestoreDeployKey(&output, RestoreConfig{ContextName: "selected", Namespace: "oberth"}, restoreTarget{DeployPublicKey: key})
	if !strings.Contains(output.String(), key) || !strings.Contains(output.String(), "with write access") || !strings.Contains(output.String(), "--check") || !strings.Contains(output.String(), "selected") || bytes.Contains(output.Bytes(), private) {
		t.Fatal("restore output lost public guidance or disclosed private key")
	}
	deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) { return private, nil }
	if got := readRestoreDeployPublicKey(t.Context(), deps, "oberth", "pod", "/tmp/key.pub"); got != "" {
		t.Fatal("private material was accepted as public output")
	}
}
