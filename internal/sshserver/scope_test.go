package sshserver

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// scopedResolver serves one key scoped to the given upstreams (#073).
type scopedResolver struct {
	fingerprint string
	scope       []string
}

func (resolver scopedResolver) ResolveFingerprint(got string) (string, bool, error) {
	return "laptop@work", got == resolver.fingerprint, nil
}

func (resolver scopedResolver) ResolveFingerprintScope(got string) (string, []string, bool, error) {
	return "laptop@work", resolver.scope, got == resolver.fingerprint, nil
}

func scopeUpstreamOf(input string) (string, error) {
	switch input {
	case "oberth", "oberthci/oberth", "github/oberthci/oberth", "github/oberthci/new-repo":
		return "github", nil
	case "codeberg/cloudtaser/secret", "cloudtaser/secret":
		return "codeberg", nil
	}
	return "", errors.New("app: no upstream with org \"elsewhere\"; known: oberthci (github), cloudtaser (codeberg)")
}

func TestScopedKeyReachesOnlyItsUpstreams(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	run := func(t *testing.T, scope []string, upstreamOf func(string) (string, error), command string) (*fakeGit, string, error) {
		t.Helper()
		git := &fakeGit{refs: map[string]string{"refs/heads/main": shaA}}
		server, err := New(Config{
			HostSigner: newSigner(t), Resolver: scopedResolver{fingerprint: fingerprint, scope: scope},
			Git: git, OnUpdate: noOpUpdateHandler(), UpstreamOf: upstreamOf,
		})
		if err != nil {
			t.Fatal(err)
		}
		client, stop := startServer(t, server, clientSigner)
		defer stop()
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		session.Stderr = &stderr
		err = session.Run(command)
		return git, stderr.String(), err
	}

	for _, command := range []string{"git-upload-pack 'oberth.git'", "git-upload-pack 'github/oberthci/oberth.git'", "git-receive-pack 'github/oberthci/new-repo'"} {
		// The fake knows no such repository; reaching it is what counts.
		git, stderr, _ := run(t, []string{"github"}, scopeUpstreamOf, command)
		git.mu.Lock()
		reached := git.service != "" || git.ensureCalls > 0
		git.mu.Unlock()
		if !reached || strings.Contains(stderr, "scoped uplink") {
			t.Fatalf("in-scope %s: reached git %v, stderr %q", command, reached, stderr)
		}
	}

	// Out of scope, unresolvable, or with no way to resolve: refused before
	// Git runs, with one message that names nothing but the caller's path.
	for _, test := range []struct {
		command    string
		upstreamOf func(string) (string, error)
	}{
		{"git-upload-pack 'codeberg/cloudtaser/secret.git'", scopeUpstreamOf},
		{"git-upload-pack 'cloudtaser/secret'", scopeUpstreamOf},
		{"git-receive-pack 'codeberg/cloudtaser/secret'", scopeUpstreamOf},
		{"git-receive-pack 'elsewhere/brand-new'", scopeUpstreamOf},
		{"git-upload-pack 'oberth.git'", nil},
	} {
		git, stderr, err := run(t, []string{"github"}, test.upstreamOf, test.command)
		git.mu.Lock()
		reached := git.service != "" || git.ensureCalls > 0
		git.mu.Unlock()
		if err == nil || reached {
			t.Fatalf("%s: err %v, reached git %v", test.command, err, reached)
		}
		if !strings.Contains(stderr, "not found; a scoped uplink reaches only its upstreams") {
			t.Fatalf("%s: stderr %q", test.command, stderr)
		}
		if strings.Contains(stderr, "codeberg") && !strings.Contains(test.command, "codeberg") || strings.Contains(stderr, "known:") {
			t.Fatalf("%s: refusal names another upstream: %q", test.command, stderr)
		}
	}

	// An unscoped key is not resolved against upstreams at all.
	git, stderr, _ := run(t, nil, func(string) (string, error) {
		t.Error("UpstreamOf called for an unscoped key")
		return "", errors.New("unexpected")
	}, "git-upload-pack 'codeberg/cloudtaser/secret.git'")
	git.mu.Lock()
	defer git.mu.Unlock()
	if git.ensureCalls == 0 || strings.Contains(stderr, "scoped uplink") {
		t.Fatalf("unscoped fetch did not reach git: %q", stderr)
	}
}
