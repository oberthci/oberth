//go:build linux

// These helpers exercise Linux server composition.

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourceplugin"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const composedWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: build
  templates:
  - name: build
    inputs:
      artifacts:
      - name: source
        mode: 0755
        path: /work/src
        plugin:
          name: oberth
          key: '{{workflow.parameters.oberth-source}}'
          configuration: '{"version":1,"kind":"main"}'
    container:
      image: oberth.ci/build:1
      command: ["true"]
      workingDir: /work/src
`

func (f *composedFixture) administratorClient(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{Options: client.Options{URL: f.config.Public.URL}, Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func (f *composedFixture) invitation(t *testing.T, identity string) model.InvitationFile {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "invitation.json")
	_, err := f.administratorClient(t).Invite(t.Context(), apicontract.UplinkInviteArgs{Identity: identity, Scope: []string{"origin"}, ServerURL: f.config.Public.URL}, path, f.hostPins(t))
	if err != nil {
		t.Fatal("composed invite", err)
	}
	data, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	var invitation model.InvitationFile
	if err = json.Unmarshal(data, &invitation); err != nil {
		t.Fatal(err)
	}
	if len(invitation.Secret) < apicontract.InvitationSecretMinChars {
		t.Fatal("invitation violates the service secret rule")
	}
	return invitation
}

func (f *composedFixture) enrollJoined(t *testing.T) {
	t.Helper()
	invitation := f.invitation(t, "joined-agent")
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "joined")
	result, err := client.Join(t.Context(), client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS).Transport}, invitation, dir)
	if err != nil {
		t.Fatal("composed join", err)
	}
	if result.Identity != invitation.Identity || result.SSHFingerprint == "" {
		t.Fatal("joined another identity", result)
	}
	f.identity = filepath.Join(dir, client.UplinkPushKeyFile)
}

func (f *composedFixture) prepareCommit(t *testing.T) string {
	t.Helper()
	f.git.Write(t, ".oberth/build.yaml", []byte(composedWorkflow))
	f.git.Write(t, ".oberth/release.yaml", []byte(composedWorkflow))
	commit := f.git.CommitFile(t, "pipeline")
	f.git.Git(t, f.git.Work, "push", f.git.Bare, "HEAD:refs/heads/main")
	return commit
}

// hostPins is the installation's own SSH host-key record for its listener.
func (f *composedFixture) hostPins(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(f.config.Files.SSHHostKey)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	advertised, err := url.Parse(advertisedSSH(f.config))
	if err != nil {
		t.Fatal(err)
	}
	return knownhosts.Line([]string{f.ssh.Addr().String(), advertised.Host}, signer.PublicKey()) + "\n"
}

func (f *composedFixture) bindClient(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.trust, []byte(f.hostPins(t)), 0600); err != nil {
		t.Fatal(err)
	}
	session := flowSession{ctx: t.Context(), app: application{directory: f.git.Work, executable: func() (string, error) { return f.binary, nil }}}
	command, err := session.pushCommand(filepath.Dir(f.identity), f.trust, &url.URL{Scheme: "ssh", Host: f.ssh.Addr().String()}, f.identity)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.bindGitRemote(gittest.Repository, command); err != nil {
		t.Fatal(err)
	}
}
func (f *composedFixture) waitStarted(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-f.trace.ready:
	case err := <-done:
		t.Fatalf("server failed: %v; %s", err, f.trace.String())
	case <-time.After(10 * time.Second):
		t.Fatalf("server not ready: %s", f.trace.String())
	}
}

func (f *composedFixture) run(t *testing.T) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, f.config, f.deps, f.trace); close(done) }()
	t.Cleanup(func() {
		select {
		case <-f.allowList:
		default:
			close(f.allowList)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("server did not join")
		}
	})
	return cancel, done
}

func (f *composedFixture) loadSourcePlugin(t *testing.T, q pluginRequest) {
	t.Helper()
	socket := pluginSocket(t)
	ctx, cancel := context.WithCancel(t.Context())
	cfg := sourceplugin.Config{Endpoint: "https://" + f.sourceListener.Addr().String(), ServerName: f.publicHostname(), CAPEM: f.ca, TokenFile: q.TokenFile, Now: f.deps.clock.Now, DialContext: q.Dial}
	done := make(chan int, 1)
	go func() { done <- sourceplugin.Run(ctx, []string{socket}, cfg, f.trace) }()
	defer joinInProcessPlugin(t, done, cancel)
	response, err := loadPluginArtifact(t, socket, q)
	if err != nil || !response.GetSuccess() {
		t.Fatal("native plugin did not load the admitted source", err, f.trace.String())
	}
}

func joinInProcessPlugin(t *testing.T, done <-chan int, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Error("in-process source plugin did not join cleanly", code)
		}
	case <-time.After(5 * time.Second):
		t.Error("in-process source plugin did not stop")
	}
}

func storeOperatorRequest(t *testing.T, cfg serverConfig, certificate []byte) installationRequest {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{"config.json": data, "ca.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), "token": []byte("operator-token")} {
		if err = os.WriteFile(filepath.Join(dir, name), value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	flags := installationFlags("secretstore setup")
	if err = flags.Parse([]string{"--config", filepath.Join(dir, "config.json"), "--ca-file", filepath.Join(dir, "ca.crt"), "--token-file", filepath.Join(dir, "token")}); err != nil {
		t.Fatal(err)
	}
	return installationRequest{name: "secretstore setup", flags: flags}
}

func renewInstalledClient(t *testing.T, f *composedFixture, dir string) {
	t.Helper()
	key, err := os.ReadFile(filepath.Join(dir, "admin.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	csr, err := client.RenewalCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "admin.crt"), filepath.Join(dir, "admin.key"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.Config{Options: client.Options{URL: f.config.Public.URL}, Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, cert).Transport})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var renewed apicontract.Identity
	err = c.Invoke(t.Context(), "client_renew", apicontract.ClientRenewArgs{CSR: string(csr)}, &renewed)
	if err != nil {
		t.Fatal("installed CA renewal", err)
	}
	leaf, err := clientcert.ParseCertificate([]byte(renewed.Certificate))
	if err != nil || leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatal("renewed signature", err)
	}
	if err = client.InstallRenewed(filepath.Join(dir, "admin.crt"), client.Renewal{Key: key, CA: f.ca, Certificate: renewed.Certificate, Now: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
}

func (f *composedFixture) checkServerParts(t *testing.T) {
	t.Helper()
	status, err := (serverAdministration{f.owners}).Status(t.Context(), "system_status")
	if err != nil || !status.Ready || status.Detail != "" {
		t.Fatal("composed part readiness is missing", status, err)
	}
	found := map[string]bool{}
	for _, p := range status.Parts {
		if !p.Ready || len([]rune(p.DisplayReason)) > 40 {
			t.Fatal("composed part is not ready", p)
		}
		found[p.Name] = true
	}
	for _, name := range []string{"store", "cluster", "source", "schedules", "logs"} {
		if !found[name] {
			t.Fatal("status omitted composed part", name)
		}
	}
}
