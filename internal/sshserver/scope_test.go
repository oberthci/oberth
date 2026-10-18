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

func scopeQualify(input string, scope []string) (string, error) {
	switch input {
	case "oberth", "oberthci/oberth", "github/oberthci/oberth":
		return "github/oberthci/oberth", nil
	case "github/oberthci/new-repo":
		return input, nil
	case "codeberg/cloudtaser/secret", "cloudtaser/secret":
		return "codeberg/cloudtaser/secret", nil // resolves, but out of a github scope
	}
	return "", errors.New("app: repository is not in this uplink's scope")
}

func TestScopedKeyReachesOnlyItsUpstreams(t *testing.T) {
	t.Parallel()
	clientSigner := newSigner(t)
	fingerprint := ssh.FingerprintSHA256(clientSigner.PublicKey())
	run := func(t *testing.T, scope []string, qualify func(string, []string) (string, error), command string) (*fakeGit, string, error) {
		t.Helper()
		git := &fakeGit{refs: map[string]string{"refs/heads/main": shaA}}
		server, err := New(Config{
			HostSigner: newSigner(t), Resolver: scopedResolver{fingerprint: fingerprint, scope: scope},
			Git: git, OnUpdate: noOpUpdateHandler(), QualifyInScope: qualify,
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
		// The fake knows no such repository; reaching it, under the
		// canonical in-scope path, is what counts.
		git, stderr, _ := run(t, []string{"github"}, scopeQualify, command)
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
		command string
		qualify func(string, []string) (string, error)
	}{
		{"git-upload-pack 'codeberg/cloudtaser/secret.git'", scopeQualify},
		{"git-upload-pack 'cloudtaser/secret'", scopeQualify},
		{"git-receive-pack 'codeberg/cloudtaser/secret'", scopeQualify},
		{"git-receive-pack 'elsewhere/brand-new'", scopeQualify},
		{"git-upload-pack 'oberth.git'", nil},
	} {
		git, stderr, err := run(t, []string{"github"}, test.qualify, test.command)
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
	git, stderr, _ := run(t, nil, func(string, []string) (string, error) {
		t.Error("QualifyInScope called for an unscoped key")
		return "", errors.New("unexpected")
	}, "git-upload-pack 'codeberg/cloudtaser/secret.git'")
	git.mu.Lock()
	defer git.mu.Unlock()
	if git.ensureCalls == 0 || strings.Contains(stderr, "scoped uplink") {
		t.Fatalf("unscoped fetch did not reach git: %q", stderr)
	}
}

// Git serves the canonical in-scope path, so a short name cannot be resolved
// again, differently, by the cache.
func TestScopedKeyServesTheCanonicalPath(t *testing.T) {
	t.Parallel()
	server := &Server{qualifyInScope: scopeQualify}
	for input, want := range map[string]string{"oberth": "github/oberthci/oberth", "oberthci/oberth": "github/oberthci/oberth"} {
		if got, err := server.authorizeScope(sessionActor{identity: "laptop@work", scope: []string{"github"}}, input); err != nil || got != want {
			t.Errorf("authorizeScope(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if got, err := server.authorizeScope(sessionActor{identity: "owner@tuxbox"}, "oberth"); err != nil || got != "oberth" {
		t.Errorf("unscoped path rewritten to %q, %v", got, err)
	}
}
