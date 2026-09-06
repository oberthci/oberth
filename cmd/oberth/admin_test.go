package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func registerTestUpstream(t *testing.T, databasePath, name, baseURL string) {
	t.Helper()
	registerTestUpstreamWithKey(t, databasePath, name, baseURL, "")
}

func registerTestUpstreamWithKey(t *testing.T, databasePath, name, baseURL, keyName string) {
	t.Helper()
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.RegisterUpstream(context.Background(), "admin@test", model.UpstreamSpec{
		Name: name, Kind: "ssh", BaseURL: baseURL, KeyName: keyName,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRepoAddMapsRepositoryToNamedUpstreamAndIsIdempotent(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "codeberg", "ssh://git@codeberg.org/acme")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")
	var output bytes.Buffer
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, &output, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "registered repository widget -> upstream github") {
		t.Fatalf("repo add output = %q", output.String())
	}
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.RepositoryByName(context.Background(), "widget")
	closeErr := database.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if repository.UpstreamID != 2 || repository.DefaultBranch != "main" {
		t.Fatalf("repository mapping = %+v", repository)
	}
	output.Reset()
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, &output, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "already mapped") {
		t.Fatalf("idempotent repo add output = %q", output.String())
	}
	// Rewritten with issue #264: the same bare name under a DIFFERENT
	// upstream is a distinct repository, not a remap — registration must
	// succeed, the bare name becomes ambiguous, and qualified selectors
	// address each repository individually.
	output.Reset()
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "codeberg",
	}, &output, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatalf("cross-upstream same-name add must succeed (issue #264): %v", err)
	}
	if !strings.Contains(output.String(), "registered repository widget -> upstream codeberg") {
		t.Fatalf("cross-upstream repo add output = %q", output.String())
	}
	database, err = store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.RepositoryByName(context.Background(), "widget"); !errors.Is(err, store.ErrAmbiguous) {
		t.Fatalf("bare lookup after duplicate registration = %v, want ErrAmbiguous", err)
	}
	codebergWidget, err := database.RepositoryByName(context.Background(), "codeberg/acme/widget")
	if err != nil || codebergWidget.UpstreamID != 1 {
		t.Fatalf("codeberg-qualified widget = %+v, %v", codebergWidget, err)
	}
	githubWidget, err := database.RepositoryByName(context.Background(), "github/octocat/widget")
	if err != nil || githubWidget.UpstreamID != 2 {
		t.Fatalf("github-qualified widget = %+v, %v", githubWidget, err)
	}
}

func TestRepoAddRejectsUnknownUpstreamAndInvalidName(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "codeberg", "ssh://git@codeberg.org/acme")
	err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation})
	if err == nil || !strings.Contains(err.Error(), `upstream "github" is not registered`) || !strings.Contains(err.Error(), "codeberg") {
		t.Fatalf("unknown upstream error = %v", err)
	}
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "../escape", "codeberg",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err == nil {
		t.Fatal("path-escaping repository name was accepted")
	}
}

func TestUpstreamListShowsConfiguredUpstreams(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "oberth.sqlite")
	registerTestUpstream(t, databasePath, "codeberg", "ssh://git@codeberg.org/acme")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")
	_, keyPEM, _ := testSSHIdentity(t)
	keyPath := filepath.Join(directory, "id_ed25519")
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runUpstreamList(context.Background(), []string{
		"--database", databasePath, "--upstream-key", keyPath,
	}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "codeberg") || !strings.Contains(text, "ssh://git@github.com/octocat") ||
		!strings.Contains(text, "KEY FINGERPRINT") || !strings.Contains(text, "KEY NAME") || !strings.Contains(text, "SHA256:") {
		t.Fatalf("upstream list output = %q", text)
	}
}

// TestUpstreamListShowsPerUpstreamKeyFingerprints pins the multi-key listing
// contract: an upstream with a dedicated key name resolves its fingerprint
// from the projected per-upstream file, a shared-key upstream resolves the
// shared file, the two fingerprints differ, and only fingerprints — never key
// material — appear in the output.
func TestUpstreamListShowsPerUpstreamKeyFingerprints(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "oberth.sqlite")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")
	registerTestUpstreamWithKey(t, databasePath, "codeberg", "ssh://git@codeberg.org/acme", "id_ed25519-codeberg")
	_, sharedPEM, _ := testSSHIdentity(t)
	_, dedicatedPEM, _ := testSSHIdentity(t)
	keyPath := filepath.Join(directory, "id_ed25519")
	if err := os.WriteFile(keyPath, sharedPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "id_ed25519-codeberg"), dedicatedPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runUpstreamList(context.Background(), []string{
		"--database", databasePath, "--upstream-key", keyPath,
	}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "id_ed25519-codeberg") {
		t.Fatalf("per-upstream key name missing from listing: %q", text)
	}
	if strings.Contains(text, "PRIVATE KEY") {
		t.Fatalf("key material leaked into listing: %q", text)
	}
	fingerprints := make(map[string]bool)
	for _, field := range strings.Fields(text) {
		if strings.HasPrefix(field, "SHA256:") {
			fingerprints[field] = true
		}
	}
	if len(fingerprints) != 2 {
		t.Fatalf("expected two distinct fingerprints (shared and dedicated), got %d in %q", len(fingerprints), text)
	}
}

// TestUpstreamListDegradesUnreadableDedicatedKey pins the degrade contract:
// a dedicated key name whose file is not projected yet lists "-" instead of
// silently displaying the shared key's fingerprint as if it were dedicated.
func TestUpstreamListDegradesUnreadableDedicatedKey(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "oberth.sqlite")
	registerTestUpstreamWithKey(t, databasePath, "codeberg", "ssh://git@codeberg.org/acme", "id_ed25519-codeberg")
	_, sharedPEM, _ := testSSHIdentity(t)
	keyPath := filepath.Join(directory, "id_ed25519")
	if err := os.WriteFile(keyPath, sharedPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runUpstreamList(context.Background(), []string{
		"--database", databasePath, "--upstream-key", keyPath,
	}, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "SHA256:") {
		t.Fatalf("unprojected dedicated key must degrade to '-', got %q", output.String())
	}
}

func TestUpstreamRemoveRequiresConfirmationAndRemovesMappings(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runUpstreamWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "github",
	}, &output, upstreamDependencies{input: strings.NewReader("no\n"), mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "aborted") {
		t.Fatalf("declined confirmation output = %q", output.String())
	}
	output.Reset()
	if err := runUpstreamWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "github",
	}, &output, upstreamDependencies{input: strings.NewReader("yes\n"), mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "removed upstream github") || !strings.Contains(output.String(), "widget") {
		t.Fatalf("upstream remove output = %q", output.String())
	}
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstreams, listErr := database.ListUpstreams(context.Background())
	_, repoErr := database.RepositoryByName(context.Background(), "widget")
	closeErr := database.Close()
	if listErr != nil || closeErr != nil {
		t.Fatal(listErr, closeErr)
	}
	if len(upstreams) != 0 || !errors.Is(repoErr, store.ErrNotFound) {
		t.Fatalf("post-removal state: upstreams=%d repoErr=%v", len(upstreams), repoErr)
	}
}

func TestUpstreamRemoveFailsClosedWhenRepositoryHasHistory(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.RepositoryByName(context.Background(), "widget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: repository.ID, RefKind: model.RefBranch, Ref: "refs/heads/main",
		SHA: strings.Repeat("a", 40), Actor: "test@host", Trigger: "push",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	err = runUpstreamWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "--yes", "github",
	}, io.Discard, upstreamDependencies{mutationGate: allowTestMutation})
	if err == nil || !strings.Contains(err.Error(), "immutable CI history") {
		t.Fatalf("history-guard error = %v", err)
	}
}

func registerTestUplink(t *testing.T, databasePath, identity, fingerprint string) {
	t.Helper()
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	digest := make([]byte, 32)
	if _, err := cryptorand.Read(digest); err != nil {
		t.Fatal(err)
	}
	credential, err := database.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{
		Name: identity, Digest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.RegisterUplink(context.Background(), "admin@test", model.UplinkSpec{
		Fingerprint: fingerprint, Identity: identity, TokenCredentialID: credential.ID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUplinkListAndRemoveRevokeTokenImmediately(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUplink(t, databasePath, "operator@host", "SHA256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	registerTestUplink(t, databasePath, "agent@runner", "SHA256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	var output bytes.Buffer
	if err := runUplinkWithDependencies(context.Background(), []string{
		"list", "--database", databasePath,
	}, strings.NewReader(""), &output, uplinkDependencies{}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "operator@host") || !strings.Contains(text, "agent@runner") ||
		!strings.Contains(text, "LAST SEEN") || !strings.Contains(text, "never") {
		t.Fatalf("uplink list output = %q", text)
	}
	output.Reset()
	if err := runUplinkWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "agent@runner",
	}, strings.NewReader(""), &output, uplinkDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "removed uplink agent@runner") {
		t.Fatalf("uplink remove output = %q", output.String())
	}
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	remaining, listErr := database.ListUplinkStatuses(context.Background())
	closeErr := database.Close()
	if listErr != nil || closeErr != nil {
		t.Fatal(listErr, closeErr)
	}
	if len(remaining) != 1 || remaining[0].Identity != "operator@host" {
		t.Fatalf("post-removal uplinks = %+v", remaining)
	}
	err = runUplinkWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "ghost@nowhere",
	}, strings.NewReader(""), io.Discard, uplinkDependencies{mutationGate: allowTestMutation})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing uplink error = %v", err)
	}
}

func TestRepoAddFailsClosedWhenAuditGateRejects(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, io.Discard, repoDependencies{mutationGate: func(context.Context, string, string) error {
		return errors.New("injected external audit failure")
	}})
	if err == nil || !strings.Contains(err.Error(), "injected external audit failure") {
		t.Fatalf("gate rejection error = %v", err)
	}
	if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database was opened after audit gate rejection: %v", err)
	}
}

func allowTestMutation(context.Context, string, string) error { return nil }

func TestAdminMutationGateUsesLiveDaemonSocket(t *testing.T) {
	socket := filepath.Join("/tmp", "oberth-admin-"+filepath.Base(t.TempDir())+".sock")
	if err := validateAdminSocket(socket); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	server := newAdminGateServer(func(context.Context) error {
		called <- struct{}{}
		return nil
	}, "/data/oberth.sqlite")
	listener, err := listenAdminSocket(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		_ = removeStaleAdminSocket(socket)
	})
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	if err := requestAdminMutationGate(context.Background(), socket, "test.mutation", "/data/oberth.sqlite"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("live daemon mutation gate was not invoked")
	}
	if err := requestAdminMutationGate(context.Background(), socket, "test.mutation", "/tmp/rolled-back.sqlite"); err == nil ||
		!strings.Contains(err.Error(), "not the live daemon database") {
		t.Fatalf("mismatched database gate error = %v", err)
	}
	select {
	case <-called:
		t.Fatal("daemon gate ran for a non-live database target")
	default:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("admin server exit = %v", err)
	}
	if err := removeStaleAdminSocket(socket); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamSecretMutationFailsClosedBeforePatch(t *testing.T) {
	directory := t.TempDir()
	client := newBootstrapClient("oberth", "oberth-upstream-key", "oberth-known-hosts")
	hostKey := codebergSSHHostKey(t)
	databasePath := filepath.Join(directory, "oberth.sqlite")
	err := runUpstreamWithDependencies(context.Background(), []string{
		"add", "--database", databasePath,
		"--upstream-key", filepath.Join(directory, "missing-key"),
		"--known-hosts", filepath.Join(directory, "missing-hosts"),
		"codeberg", "ssh://git@codeberg.org/acme",
	}, io.Discard, upstreamDependencies{
		input:            strings.NewReader("yes\nyes\n"),
		kubernetesClient: func() (kubernetes.Interface, error) { return client, nil },
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error { return nil },
		mutationGate: func(context.Context, string, string) error {
			return errors.New("injected external audit failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected external audit failure") {
		t.Fatalf("bootstrap error = %v, want fail-closed audit rejection", err)
	}
	if countSecretPatches(client) != 0 {
		t.Fatal("upstream Secret changed after audit gate rejection")
	}
	if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database was opened after audit gate rejection: %v", err)
	}
}

func TestUpstreamAddGeneratesConfirmsPersistsAndRecovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	client := newBootstrapClient("ci", "custom-upstream-key", "custom-known-hosts")
	hostKey := codebergSSHHostKey(t)
	scanCalls := 0
	probeCalls := 0
	probeErr := errors.New("deploy key is not registered yet")
	dependencies := upstreamDependencies{
		input:        strings.NewReader("yes\nyes\n"),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(_ context.Context, host, port string) ([]ssh.PublicKey, error) {
			scanCalls++
			if host != "codeberg.org" || port != "22" {
				t.Fatalf("scan target = %s:%s", host, port)
			}
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(_ context.Context, baseURL string, privateKey, knownHosts []byte) error {
			probeCalls++
			if baseURL != "ssh://git@codeberg.org/acme" || len(privateKey) == 0 || len(knownHosts) == 0 {
				t.Fatalf("probe inputs: base=%q private=%d known_hosts=%d", baseURL, len(privateKey), len(knownHosts))
			}
			return probeErr
		},
		// Keep the registration wait (issue 007) fast and deterministic: a
		// few poll attempts, then the timeout fallback.
		probeInterval: 2 * time.Millisecond,
		probeWait:     20 * time.Millisecond,
	}
	privatePath := filepath.Join(directory, "projected", "custom_key")
	knownHostsPath := filepath.Join(directory, "projected-hosts", "trusted_hosts")
	databasePath := filepath.Join(directory, "oberth.sqlite")
	arguments := []string{
		"add",
		"--database", databasePath,
		"--upstream-key", privatePath,
		"--known-hosts", knownHostsPath,
		"--namespace", "ci",
		"--upstream-key-secret", "custom-upstream-key",
		"--known-hosts-secret", "custom-known-hosts",
		"codeberg", "ssh://git@codeberg.org/acme",
	}
	var output bytes.Buffer
	err := runUpstreamWithDependencies(ctx, arguments, &output, dependencies)
	// Issue #839: a completed generation flow exits zero — the key was
	// generated, persisted, and printed. Issue 007: the command then waits
	// for registration, and only the timeout falls back to the rerun hint.
	if err != nil {
		t.Fatalf("generated bootstrap error = %v, want nil with rerun hint", err)
	}
	if !strings.Contains(output.String(), "Waiting for key registration on codeberg.org") ||
		!strings.Contains(output.String(), "timed out") ||
		!strings.Contains(output.String(), "then rerun:") ||
		!strings.Contains(output.String(), "oberth upstream add") ||
		!strings.Contains(output.String(), "codeberg ssh://git@codeberg.org/acme") {
		t.Fatalf("bootstrap output missing wait + rerun fallback: %q", output.String())
	}
	if scanCalls != 1 || probeCalls == 0 || upstreamCount(t, databasePath) != 0 {
		t.Fatalf("initial bootstrap: scans=%d probes=%d upstreams=%d", scanCalls, probeCalls, upstreamCount(t, databasePath))
	}

	privateSecret, err := client.CoreV1().Secrets("ci").Get(ctx, "custom-upstream-key", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	privateBody := privateSecret.Data["custom_key"]
	publicBody := privateSecret.Data["custom_key.pub"]
	if len(privateBody) == 0 || len(publicBody) == 0 {
		t.Fatalf("persisted key sizes = private:%d public:%d", len(privateBody), len(publicBody))
	}
	if _, err := ssh.ParsePrivateKey(privateBody); err != nil {
		t.Fatalf("persisted private key: %v", err)
	}
	if !bytes.Contains(output.Bytes(), bytes.TrimSpace(publicBody)) {
		t.Fatalf("generated public key was not printed: %q", output.String())
	}
	if bytes.Contains(output.Bytes(), privateBody) || strings.Contains(output.String(), "OPENSSH PRIVATE KEY") {
		t.Fatal("command output exposed the generated private key")
	}
	// codeberg.org is a well-known forge: host-key fingerprints are auto-accepted
	// and not displayed. The upstream is not registered because the probe fails.
	if strings.Contains(output.String(), ssh.FingerprintSHA256(hostKey)) {
		t.Fatalf("well-known forge should not display host-key fingerprints: %q", output.String())
	}

	hostsSecret, err := client.CoreV1().Secrets("ci").Get(ctx, "custom-known-hosts", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expectedHostLine := knownhosts.Line([]string{"codeberg.org:22"}, hostKey)
	if !strings.Contains(string(hostsSecret.Data["trusted_hosts"]), expectedHostLine) {
		t.Fatalf("known_hosts = %q", hostsSecret.Data["trusted_hosts"])
	}
	patchNames := secretPatchNames(client)
	if strings.Join(patchNames, ",") != "custom-known-hosts,custom-upstream-key" {
		t.Fatalf("name-scoped Secret patches = %v", patchNames)
	}
	for _, action := range client.Actions() {
		if action.GetResource().Resource == "secrets" && action.GetVerb() == "create" {
			t.Fatal("bootstrap used unrestricted Secret create")
		}
	}

	patchesBefore := countSecretPatches(client)
	probesBefore := probeCalls
	var recoveryOutput bytes.Buffer
	// Issue 007: an unregistered recovered key now waits too, then exits
	// zero with the same rerun fallback instead of a hard probe error.
	err = runUpstreamWithDependencies(ctx, arguments, &recoveryOutput, dependencies)
	if err != nil {
		t.Fatalf("unregistered recovered key = %v, want nil with rerun fallback", err)
	}
	if !strings.Contains(recoveryOutput.String(), "Waiting for key registration on codeberg.org") ||
		!strings.Contains(recoveryOutput.String(), "then rerun:") {
		t.Fatalf("recovery output missing wait fallback: %q", recoveryOutput.String())
	}
	if probeCalls <= probesBefore || upstreamCount(t, databasePath) != 0 || countSecretPatches(client) != patchesBefore {
		t.Fatalf("failed probe: probes=%d->%d upstreams=%d patches=%d->%d", probesBefore, probeCalls, upstreamCount(t, databasePath), patchesBefore, countSecretPatches(client))
	}
	if !bytes.Contains(recoveryOutput.Bytes(), bytes.TrimSpace(publicBody)) || bytes.Contains(recoveryOutput.Bytes(), privateBody) {
		t.Fatalf("recovery output did not safely re-advertise the public key: %q", recoveryOutput.String())
	}

	probeErr = nil
	probesBefore = probeCalls
	var registeredOutput bytes.Buffer
	if err := runUpstreamWithDependencies(ctx, arguments, &registeredOutput, dependencies); err != nil {
		t.Fatal(err)
	}
	if probeCalls != probesBefore+1 || upstreamCount(t, databasePath) != 1 {
		t.Fatalf("registered rerun: probes=%d->%d upstreams=%d output=%q", probesBefore, probeCalls, upstreamCount(t, databasePath), registeredOutput.String())
	}
}

// TestUpstreamAddWaitsForKeyRegistrationInOneInvocation is the issue 007
// acceptance path: generate, print, wait while the operator registers the key
// in another tab, then complete registration — one command, no rerun.
func TestUpstreamAddWaitsForKeyRegistrationInOneInvocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	hostKey := codebergSSHHostKey(t)
	probeCalls := 0
	dependencies := upstreamDependencies{
		input:        strings.NewReader("yes\nyes\n"),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error {
			probeCalls++
			if probeCalls < 3 {
				return errors.New("deploy key is not registered yet")
			}
			return nil
		},
		probeInterval: 2 * time.Millisecond,
		probeWait:     5 * time.Second,
	}
	arguments := []string{
		"add",
		"--database", filepath.Join(directory, "oberth.sqlite"),
		"--upstream-key", filepath.Join(directory, "projected", "id_ed25519"),
		"--known-hosts", filepath.Join(directory, "projected-hosts", "known_hosts"),
		"--namespace", "ci",
		"codeberg", "ssh://git@codeberg.org/acme",
	}
	var output bytes.Buffer
	if err := runUpstreamWithDependencies(ctx, arguments, &output, dependencies); err != nil {
		t.Fatalf("single-invocation registration = %v\noutput: %s", err, output.String())
	}
	if probeCalls != 3 {
		t.Fatalf("probe calls = %d, want 3 (two failures, then acceptance)", probeCalls)
	}
	text := output.String()
	if !strings.Contains(text, "Waiting for key registration on codeberg.org") ||
		!strings.Contains(text, "Key authenticated") ||
		strings.Contains(text, "then rerun:") {
		t.Fatalf("single-invocation output = %q", text)
	}
	if upstreamCount(t, filepath.Join(directory, "oberth.sqlite")) != 1 {
		t.Fatal("upstream was not registered after the wait succeeded")
	}
}

// TestUpstreamAddRegistrationWaitCancelsCleanly pins the Ctrl+C contract:
// cancellation stops the wait, keeps the persisted key, exits zero, and
// prints the rerun fallback.
func TestUpstreamAddRegistrationWaitCancelsCleanly(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	directory := t.TempDir()
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	hostKey := codebergSSHHostKey(t)
	dependencies := upstreamDependencies{
		input:        strings.NewReader("yes\nyes\n"),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error {
			cancel() // the operator presses Ctrl+C while the command waits
			return errors.New("deploy key is not registered yet")
		},
		probeInterval: 2 * time.Millisecond,
		probeWait:     5 * time.Second,
	}
	databasePath := filepath.Join(directory, "oberth.sqlite")
	arguments := []string{
		"add",
		"--database", databasePath,
		"--upstream-key", filepath.Join(directory, "projected", "id_ed25519"),
		"--known-hosts", filepath.Join(directory, "projected-hosts", "known_hosts"),
		"--namespace", "ci",
		"codeberg", "ssh://git@codeberg.org/acme",
	}
	var output bytes.Buffer
	if err := runUpstreamWithDependencies(ctx, arguments, &output, dependencies); err != nil {
		t.Fatalf("canceled wait = %v, want clean rerun fallback\noutput: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "stopped waiting") || !strings.Contains(output.String(), "then rerun:") {
		t.Fatalf("canceled output = %q", output.String())
	}
	secret, err := client.CoreV1().Secrets("ci").Get(context.Background(), "oberth-upstream-key", metav1.GetOptions{})
	if err != nil || len(secret.Data["id_ed25519"]) == 0 {
		t.Fatalf("generated key was not preserved across cancellation: %v", err)
	}
	if upstreamCount(t, databasePath) != 0 {
		t.Fatal("canceled wait still registered the upstream")
	}
}

// TestUpstreamAddStoresPerUpstreamDataKeys pins the multi-forge key
// contract: a new SSH upstream gets a dedicated Ed25519 identity stored as
// additional data keys of the one chart-owned upstream-key Secret, applied
// under a per-upstream server-side-apply field manager so one upstream's
// registration can never prune another upstream's key material, and the
// registered row records the dedicated key name.
func TestUpstreamAddStoresPerUpstreamDataKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	hostKey := codebergSSHHostKey(t)
	dependencies := upstreamDependencies{
		input:        strings.NewReader("yes\nyes\n"),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error { return nil },
	}
	databasePath := filepath.Join(directory, "oberth.sqlite")
	var output bytes.Buffer
	if err := runUpstreamWithDependencies(ctx, []string{
		"add",
		"--database", databasePath,
		"--upstream-key", filepath.Join(directory, "projected", "id_ed25519"),
		"--known-hosts", filepath.Join(directory, "projected-hosts", "known_hosts"),
		"--namespace", "ci",
		"--dedicated-key",
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, dependencies); err != nil {
		t.Fatalf("upstream add = %v\noutput: %s", err, output.String())
	}
	secret, err := client.CoreV1().Secrets("ci").Get(ctx, "oberth-upstream-key", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret.Data["id_ed25519-codeberg"]) == 0 || len(secret.Data["id_ed25519-codeberg.pub"]) == 0 {
		t.Fatalf("per-upstream data keys missing; got keys %v", dataKeyNames(secret.Data))
	}
	if len(secret.Data["id_ed25519"]) != 0 {
		t.Fatal("per-upstream add must not write the shared data key")
	}
	manager := ""
	for _, action := range client.Actions() {
		patch, ok := action.(k8stesting.PatchActionImpl)
		if !ok || patch.Name != "oberth-upstream-key" {
			continue
		}
		manager = patch.PatchOptions.FieldManager
	}
	if manager != "oberth-upstream-bootstrap-codeberg" {
		t.Fatalf("per-upstream key apply used field manager %q, want oberth-upstream-bootstrap-codeberg", manager)
	}
	database, err := store.OpenAdminClient(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	registered, err := database.UpstreamByName(ctx, "codeberg")
	if err != nil {
		t.Fatal(err)
	}
	if registered.KeyName != "id_ed25519-codeberg" {
		t.Fatalf("registered key name = %q, want id_ed25519-codeberg", registered.KeyName)
	}
	if !strings.Contains(output.String(), "rollout restart") {
		t.Fatalf("add output must explain the restart-to-activate step, got %q", output.String())
	}
}

func TestUpstreamAddDedicatedKeyRerunHintPreservesFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	hostKey := codebergSSHHostKey(t)
	dependencies := upstreamDependencies{
		input:        strings.NewReader("yes\nyes\n"),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error {
			return errors.New("deploy key is not registered yet")
		},
		probeInterval: 2 * time.Millisecond,
		probeWait:     20 * time.Millisecond,
	}
	var output bytes.Buffer
	err := runUpstreamWithDependencies(ctx, []string{
		"add",
		"--database", filepath.Join(directory, "oberth.sqlite"),
		"--upstream-key", filepath.Join(directory, "projected", "id_ed25519"),
		"--known-hosts", filepath.Join(directory, "projected-hosts", "known_hosts"),
		"--namespace", "ci",
		"--dedicated-key",
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, dependencies)
	if err != nil {
		t.Fatalf("upstream add = %v", err)
	}
	rerun := output.String()
	if !strings.Contains(rerun, "then rerun:") {
		t.Fatalf("output missing rerun hint: %q", rerun)
	}
	if !strings.Contains(rerun, "--dedicated-key") {
		t.Fatalf("rerun hint dropped --dedicated-key: %q", rerun)
	}
	if !strings.Contains(rerun, "codeberg ssh://git@codeberg.org/acme") {
		t.Fatalf("rerun hint missing name/URL: %q", rerun)
	}
}

func TestUpstreamAddRerunHintPreservesNonDefaultFlags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	client := newBootstrapClient("custom-ns", "custom-key-secret", "custom-hosts-secret")
	hostKey := codebergSSHHostKey(t)
	dependencies := upstreamDependencies{
		input:        strings.NewReader("yes\nyes\n"),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error {
			return errors.New("deploy key is not registered yet")
		},
		probeInterval: 2 * time.Millisecond,
		probeWait:     20 * time.Millisecond,
	}
	var output bytes.Buffer
	err := runUpstreamWithDependencies(ctx, []string{
		"add",
		"--database", filepath.Join(directory, "oberth.sqlite"),
		"--upstream-key", filepath.Join(directory, "projected", "custom_key"),
		"--known-hosts", filepath.Join(directory, "projected-hosts", "known_hosts"),
		"--namespace", "custom-ns",
		"--upstream-key-secret", "custom-key-secret",
		"--known-hosts-secret", "custom-hosts-secret",
		"--dedicated-key",
		"--expected-host-fingerprint", ssh.FingerprintSHA256(hostKey),
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, dependencies)
	if err != nil {
		t.Fatalf("upstream add = %v", err)
	}
	rerun := output.String()
	for _, want := range []string{
		"--dedicated-key",
		"--namespace custom-ns",
		"--upstream-key-secret custom-key-secret",
		"--known-hosts-secret custom-hosts-secret",
		"--expected-host-fingerprint",
	} {
		if !strings.Contains(rerun, want) {
			t.Errorf("rerun hint missing %q: %q", want, rerun)
		}
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"ssh://git@github.com/org", "ssh://git@github.com/org"},
		{"", "''"},
		{"has;semi", "'has;semi'"},
	} {
		if got := shellQuote(tc.input); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func dataKeyNames(data map[string][]byte) []string {
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	return names
}

// TestUpstreamAddRejectsUnsafeNames pins ValidateUpstreamName at the CLI
// boundary: the name becomes a Secret data key, a projected file name, and an
// SSH config identity path element, so everything outside a DNS-1123 label is
// refused before any side effect.
// TestUpstreamAddHelpIncludesExpectedHostFingerprint pins the flag visibility:
// the help text for upstream add must advertise --expected-host-fingerprint so
// the error message's instruction is actionable.
func TestUpstreamAddHelpIncludesExpectedHostFingerprint(t *testing.T) {
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{
		"add", "-h",
	}, &output, upstreamDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "expected-host-fingerprint") {
		t.Fatalf("help text missing --expected-host-fingerprint: %q", output.String())
	}
}

// TestUpstreamAddExpectedHostFingerprintFlagPlumbed proves that
// --expected-host-fingerprint is wired through to UpstreamSSHBootstrap: a
// mismatched fingerprint produces the fingerprint-specific error rather than
// the generic "--yes cannot trust" error that would appear if the flag were
// not plumbed.
func TestUpstreamAddExpectedHostFingerprintFlagPlumbed(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	client := newBootstrapClient("oberth", "oberth-upstream-key", "oberth-known-hosts")
	hostKey, _, _ := testSSHIdentity(t)
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{
		"add",
		"--database", filepath.Join(directory, "oberth.sqlite"),
		"--upstream-key", filepath.Join(directory, "missing-key"),
		"--known-hosts", filepath.Join(directory, "missing-hosts"),
		"--yes",
		"--expected-host-fingerprint", "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"myforge", "ssh://git@forge.example.com/acme",
	}, &output, upstreamDependencies{
		input:        strings.NewReader("yes\n"),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error {
			return nil
		},
	})
	// With the flag properly plumbed, the error is "fingerprint mismatch"
	// because the scanned key doesn't match the expected fingerprint.
	// Without the flag, --yes alone would produce "--yes cannot trust".
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("expected fingerprint mismatch error (proves flag is plumbed), got %v", err)
	}
}

func TestUpstreamAddRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"", "../escape", "has space", "Upper", "dot.name", "under_score",
		"trail-", "tab\tname", "newline\nname",
		strings.Repeat("a", 54),
	} {
		err := runUpstreamWithDependencies(context.Background(), []string{
			"add", "--database", filepath.Join(t.TempDir(), "oberth.sqlite"),
			name, "ssh://git@codeberg.org/acme",
		}, io.Discard, upstreamDependencies{mutationGate: allowTestMutation})
		if err == nil || !strings.Contains(err.Error(), "upstream name") {
			t.Fatalf("name %q: error = %v, want upstream name rejection", name, err)
		}
	}
}

func TestUpstreamAddRequiresBothExplicitConfirmations(t *testing.T) {
	t.Parallel()
	hostKey, _, _ := testSSHIdentity(t)
	for _, test := range []struct {
		name      string
		input     string
		wantScans int
	}{
		{name: "generation rejected", input: "no\n"},
		// Use a non-well-known forge so the host-key prompt appears.
		{name: "host trust rejected", input: "yes\nno\n", wantScans: 1},
		// Key generation is a low-stakes confirmation and accepts a short
		// "y" (wantScans proves the flow got past generation); host-key
		// trust is a pin decision and must reject anything but "yes".
		{name: "generation accepts short y", input: "y\nno\n", wantScans: 1},
		{name: "host trust rejects short y", input: "yes\ny\n", wantScans: 1},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			client := newBootstrapClient("oberth", "oberth-upstream-key", "oberth-known-hosts")
			scans := 0
			var output bytes.Buffer
			err := runUpstreamWithDependencies(context.Background(), []string{
				"add", "--database", filepath.Join(directory, "oberth.sqlite"),
				"--upstream-key", filepath.Join(directory, "missing-key"),
				"--known-hosts", filepath.Join(directory, "missing-hosts"),
				"myforge", "ssh://git@forge.example.com/acme",
			}, &output, upstreamDependencies{
				input:            strings.NewReader(test.input),
				mutationGate:     allowTestMutation,
				kubernetesClient: func() (kubernetes.Interface, error) { return client, nil },
				scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
					scans++
					return []ssh.PublicKey{hostKey}, nil
				},
				probe: func(context.Context, string, []byte, []byte) error {
					t.Fatal("authentication probe ran before bootstrap completed")
					return nil
				},
			})
			if err == nil {
				t.Fatal("bootstrap unexpectedly succeeded")
			}
			if scans != test.wantScans {
				t.Fatalf("host-key scans = %d, want %d", scans, test.wantScans)
			}
			secrets, listErr := client.CoreV1().Secrets("oberth").List(context.Background(), metav1.ListOptions{})
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(secrets.Items) != 2 || len(secrets.Items[0].Data) != 0 || len(secrets.Items[1].Data) != 0 || countSecretPatches(client) != 0 {
				t.Fatalf("rejected bootstrap changed placeholder Secrets: items=%d patches=%d", len(secrets.Items), countSecretPatches(client))
			}
		})
	}
}

func TestUpstreamAddUsesProvidedProjectedMaterialWithoutKubernetes(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	hostKey, privateBody, _ := testSSHIdentity(t)
	privatePath := filepath.Join(directory, "id_ed25519")
	knownHostsPath := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(privatePath, privateBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHostsPath, []byte(knownhosts.Line([]string{"codeberg.org:22"}, hostKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientCalled := false
	probeCalls := 0
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{
		"add", "--database", filepath.Join(directory, "oberth.sqlite"),
		"--upstream-key", privatePath, "--known-hosts", knownHostsPath,
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, upstreamDependencies{
		input:        strings.NewReader(""),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			clientCalled = true
			return nil, errors.New("Kubernetes must not be contacted")
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return nil, errors.New("scanner must not run")
		},
		probe: func(_ context.Context, baseURL string, key, hosts []byte) error {
			probeCalls++
			if baseURL == "" || len(key) == 0 || len(hosts) == 0 {
				t.Fatal("authentication probe did not receive validated projected material")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if clientCalled || probeCalls != 1 || strings.Contains(output.String(), "Generate") {
		t.Fatalf("provided-key path: kubernetes=%t probes=%d output=%q", clientCalled, probeCalls, output.String())
	}
}

func TestUpstreamAddProjectedProbeFailureReadvertisesOnlyPublicKey(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	hostKey, privateBody, publicBody := testSSHIdentity(t)
	privatePath := filepath.Join(directory, "id_ed25519")
	knownHostsPath := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(privatePath, privateBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(knownHostsPath, []byte(knownhosts.Line([]string{"codeberg.org:22"}, hostKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(directory, "oberth.sqlite")
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{
		"add", "--database", databasePath,
		"--upstream-key", privatePath, "--known-hosts", knownHostsPath,
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, upstreamDependencies{
		input:        strings.NewReader(""),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			t.Fatal("Kubernetes must not be contacted for valid projected material")
			return nil, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			t.Fatal("host-key scanner must not run for valid projected material")
			return nil, nil
		},
		probe: func(context.Context, string, []byte, []byte) error {
			return errors.New("public key is not registered")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "public key is not registered") {
		t.Fatalf("probe error = %v", err)
	}
	if !bytes.Contains(output.Bytes(), bytes.TrimSpace(publicBody)) || bytes.Contains(output.Bytes(), privateBody) || strings.Contains(output.String(), "OPENSSH PRIVATE KEY") {
		t.Fatalf("probe-failure output = %q", output.String())
	}
	if upstreamCount(t, databasePath) != 0 {
		t.Fatal("failed authentication probe registered the upstream")
	}
}

func TestUplinkAddReadsOneBoundedKeyFromStdinOrRawArgument(t *testing.T) {
	t.Parallel()
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	authorized := ssh.MarshalAuthorizedKey(sshKey)

	for _, test := range []struct {
		name     string
		argument string
		input    []byte
	}{
		{name: "stdin", argument: "-", input: authorized},
		{name: "raw argument", argument: strings.TrimSpace(string(authorized))},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			certificatePath := filepath.Join(directory, "tls.crt")
			writeTestCertificate(t, certificatePath, publicKey, privateKey)
			var output bytes.Buffer
			if err := runUplinkWithDependencies(context.Background(), []string{
				"add", "--database", filepath.Join(directory, "oberth.sqlite"), "--tls-cert", certificatePath,
				test.argument, "agent@host",
			}, bytes.NewReader(test.input), &output, uplinkDependencies{mutationGate: allowTestMutation}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Uplink token for agent@host") {
				t.Fatalf("uplink output = %q", output.String())
			}
		})
	}
}

func TestUplinkMutationsPassThroughDaemonGate(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "tls.crt")
	writeTestCertificate(t, certificatePath, publicKey, privateKey)
	var operations []string
	gate := func(_ context.Context, operation, _ string) error {
		operations = append(operations, operation)
		return nil
	}
	if err := runUplinkWithDependencies(context.Background(), []string{
		"add", "--database", filepath.Join(directory, "oberth.sqlite"), "--tls-cert", certificatePath,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshKey))), "agent@host",
	}, strings.NewReader(""), io.Discard, uplinkDependencies{mutationGate: gate}); err != nil {
		t.Fatal(err)
	}
	want := "uplink.database.open,uplink.token.create,uplink.register,uplink.token.activate"
	if got := strings.Join(operations, ","); got != want {
		t.Fatalf("gated mutations = %q, want %q", got, want)
	}
}

func TestUplinkAddRejectsOversizeAndTrailingStdinKeys(t *testing.T) {
	t.Parallel()
	_, _, authorized := testSSHIdentity(t)
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "two keys", body: append(append([]byte(nil), authorized...), authorized...)},
		{name: "oversize", body: bytes.Repeat([]byte("x"), (64<<10)+1)},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := runUplinkWithDependencies(context.Background(), []string{
				"add", "--database", filepath.Join(t.TempDir(), "oberth.sqlite"), "--tls-cert", filepath.Join(t.TempDir(), "missing.crt"),
				"-", "agent@host",
			}, bytes.NewReader(test.body), &output, uplinkDependencies{mutationGate: allowTestMutation})
			if err == nil {
				t.Fatal("invalid stdin key passed")
			}
			if output.Len() != 0 {
				t.Fatalf("invalid key produced output %q", output.String())
			}
		})
	}
}

func codebergSSHHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	const publishedKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIVIC02vnjFyL+I4RHfvIGNtOgJMe769VTF1VR4EB3ZB"
	hostKey, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(publishedKey))
	if err != nil {
		t.Fatalf("parse Codeberg published SSH host key: %v", err)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("parse Codeberg published SSH host key: unexpected trailing data %q", rest)
	}
	return hostKey
}

func testSSHIdentity(t *testing.T) (ssh.PublicKey, []byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "test")
	if err != nil {
		t.Fatal(err)
	}
	return sshKey, pem.EncodeToMemory(block), ssh.MarshalAuthorizedKey(sshKey)
}

func countSecretPatches(client *fake.Clientset) int {
	return len(secretPatchNames(client))
}

func secretPatchNames(client *fake.Clientset) []string {
	var names []string
	for _, action := range client.Actions() {
		if action.GetResource().Resource != "secrets" {
			continue
		}
		if action.GetVerb() == "patch" {
			names = append(names, action.(k8stesting.PatchAction).GetName())
		}
	}
	return names
}

func newBootstrapClient(namespace, privateKeySecret, knownHostsSecret string) *fake.Clientset {
	return fake.NewClientset(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: privateKeySecret, Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: knownHostsSecret, Namespace: namespace}},
	)
}

func upstreamCount(t *testing.T, databasePath string) int {
	t.Helper()
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	values, err := database.ListUpstreams(context.Background())
	closeErr := database.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return len(values)
}

func TestAccessAllowFailsClosedWithoutMutationGate(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	err := runAccessAllowWithDependencies(context.Background(), []string{
		"--database", databasePath, "--namespace", "oberth",
		"terraform", "*", "terraform/credentials",
	}, io.Discard, accessDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) { return fake.NewClientset(), nil },
		mutationGate: func(_ context.Context, _, _ string) error {
			return errors.New("injected gate failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected gate failure") {
		t.Fatalf("expected gate failure, got %v", err)
	}
	if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("database was opened despite gate rejection")
	}
}

func TestAccessRevokeFailsClosedWithoutMutationGate(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	err := runAccessRevokeWithDependencies(context.Background(), []string{
		"--database", databasePath, "--namespace", "oberth",
		"terraform", "*", "terraform/credentials",
	}, io.Discard, accessDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) { return fake.NewClientset(), nil },
		mutationGate: func(_ context.Context, _, _ string) error {
			return errors.New("injected gate failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected gate failure") {
		t.Fatalf("expected gate failure, got %v", err)
	}
	if _, statErr := os.Stat(databasePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("database was opened despite gate rejection")
	}
}

func TestAccessAllowUpdatesConfigMapAndSQLite(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	client := fake.NewClientset()
	var output bytes.Buffer
	if err := runAccessAllowWithDependencies(context.Background(), []string{
		"--database", databasePath, "--namespace", "oberth",
		"terraform", "*", "terraform/credentials",
	}, &output, accessDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) { return client, nil },
		mutationGate:     allowTestMutation,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "granted: terraform/*") {
		t.Fatalf("access allow output = %q", output.String())
	}

	// Verify ConfigMap was created.
	cm, err := client.CoreV1().ConfigMaps("oberth").Get(context.Background(), "oberth-secret-access", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ConfigMap not found: %v", err)
	}
	if !strings.Contains(cm.Data["grants"], "terraform/credentials") {
		t.Fatalf("ConfigMap grants = %q", cm.Data["grants"])
	}

	// Verify sqlite was synced.
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := database.SecretAccessList(context.Background(), "", false)
	closeErr := database.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if len(grants) != 1 || grants[0].Repo != "terraform" || grants[0].Secret != "terraform/credentials" {
		t.Fatalf("grants = %+v", grants)
	}
}

func TestAccessRevokeUpdatesConfigMapAndSQLite(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	// Create the ConfigMap with two grants.
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "oberth-secret-access",
			Namespace:       "oberth",
			ResourceVersion: "1",
		},
		Data: map[string]string{
			"grants": "- {repo: terraform, step: \"*\", secret: terraform/credentials}\n- {repo: oberth, step: \"*\", secret: cosign-secret}\n",
		},
	})
	// Seed the sqlite to match.
	{
		database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Grant(context.Background(), "terraform", "*", "terraform/credentials", "admin@localhost"); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Grant(context.Background(), "oberth", "*", "cosign-secret", "admin@localhost"); err != nil {
			t.Fatal(err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := runAccessRevokeWithDependencies(context.Background(), []string{
		"--database", databasePath, "--namespace", "oberth",
		"terraform", "*", "terraform/credentials",
	}, &output, accessDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) { return client, nil },
		mutationGate:     allowTestMutation,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "revoked: terraform/*") {
		t.Fatalf("access revoke output = %q", output.String())
	}

	// Verify one grant remaining in sqlite.
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := database.SecretAccessList(context.Background(), "", false)
	closeErr := database.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if len(grants) != 1 || grants[0].Repo != "oberth" {
		t.Fatalf("remaining grants = %+v", grants)
	}
}

// captureInteractiveRunner returns a runInteractive stub that records the
// command and args it was called with instead of executing anything.
func captureInteractiveRunner(t *testing.T) (func(context.Context, string, ...string) error, *[]string) {
	t.Helper()
	var captured []string
	return func(_ context.Context, name string, args ...string) error {
		captured = append([]string{name}, args...)
		return nil
	}, &captured
}

// capturedStdinExec records a command invocation with its stdin content.
type capturedStdinExec struct {
	name  string
	args  []string
	stdin []byte
}

// captureStdinRunner returns a runWithStdin stub that records the command,
// args, and stdin content instead of executing anything.
func captureStdinRunner(t *testing.T) (func(context.Context, io.Reader, string, ...string) error, *[]capturedStdinExec) {
	t.Helper()
	var captured []capturedStdinExec
	return func(_ context.Context, stdin io.Reader, name string, args ...string) error {
		var stdinBytes []byte
		if stdin != nil {
			stdinBytes, _ = io.ReadAll(stdin)
		}
		captured = append(captured, capturedStdinExec{name: name, args: args, stdin: stdinBytes})
		return nil
	}, &captured
}

// TestHostModeUpstreamAddGenerateAutoYes pins the host-mode --yes path:
// the [G]enerate / [P]rovide prompt is skipped, and the command execs into
// the pod with --yes for auto-generation.
func TestHostModeUpstreamAddGenerateAutoYes(t *testing.T) {
	t.Parallel()
	runner, captured := captureInteractiveRunner(t)
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"--yes",
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader(""),
		runInteractive: runner,
	})
	if err != nil {
		t.Fatalf("host-mode --yes error = %v\noutput: %s", err, output.String())
	}
	args := *captured
	if len(args) == 0 {
		t.Fatal("kubectl exec was not called")
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--yes") {
		t.Fatalf("kubectl exec args missing --yes: %q", joined)
	}
	if !strings.Contains(joined, "codeberg") || !strings.Contains(joined, "ssh://git@codeberg.org/acme") {
		t.Fatalf("kubectl exec args missing name/URL: %q", joined)
	}
	// No [G]enerate/[P]rovide prompt should appear in the output.
	if strings.Contains(output.String(), "[G]enerate") {
		t.Fatalf("host mode with --yes should not show the key selection prompt, got %q", output.String())
	}
}

// TestHostModeUpstreamAddGenerateInteractive pins the host-mode interactive
// path: the user is prompted with [G]enerate / [P]rovide and chooses G.
// The command execs into the pod interactively (no --yes) for generation.
func TestHostModeUpstreamAddGenerateInteractive(t *testing.T) {
	t.Parallel()
	runner, captured := captureInteractiveRunner(t)
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("g\n"),
		runInteractive: runner,
	})
	if err != nil {
		t.Fatalf("host-mode generate error = %v\noutput: %s", err, output.String())
	}
	if !strings.Contains(output.String(), "[G]enerate") {
		t.Fatal("host mode should show the key selection prompt")
	}
	args := *captured
	joined := strings.Join(args, " ")
	// Generate + interactive: exec should use -it and NOT include --yes
	if !strings.Contains(joined, "-it") {
		t.Fatalf("interactive Generate should exec with -it: %q", joined)
	}
	if strings.Contains(joined, "--yes") {
		t.Fatalf("interactive Generate should not pass --yes: %q", joined)
	}
}

// TestHostModeUpstreamAddProvide pins the host-mode Provide path: the user
// supplies an existing private key, which is streamed to the pod via a
// kubectl exec provide-key invocation. The command then execs into the pod
// with --yes for the main add flow (the persisted key is found by the pod).
func TestHostModeUpstreamAddProvide(t *testing.T) {
	t.Parallel()
	runner, interactiveCaptured := captureInteractiveRunner(t)
	stdinRunner, stdinCaptured := captureStdinRunner(t)
	_, privatePEM, _ := testSSHIdentity(t)
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "my_deploy_key")
	if err := os.WriteFile(keyPath, privatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("p\n" + keyPath + "\n"),
		runInteractive: runner,
		runWithStdin:   stdinRunner,
	})
	if err != nil {
		t.Fatalf("host-mode provide error = %v\noutput: %s", err, output.String())
	}
	// The provide-key exec must have been called with the key on stdin.
	if len(*stdinCaptured) != 1 {
		t.Fatalf("expected one provide-key exec, got %d", len(*stdinCaptured))
	}
	provideExec := (*stdinCaptured)[0]
	provideJoined := strings.Join(append([]string{provideExec.name}, provideExec.args...), " ")
	if !strings.Contains(provideJoined, "upstream provide-key") {
		t.Fatalf("provide exec missing 'upstream provide-key': %q", provideJoined)
	}
	if !strings.Contains(provideJoined, "codeberg") {
		t.Fatalf("provide exec missing upstream name: %q", provideJoined)
	}
	if !bytes.Equal(provideExec.stdin, privatePEM) {
		t.Fatal("provided key was not streamed to the pod's stdin")
	}
	// The main add exec must include --yes (pod finds the persisted key).
	interactiveArgs := *interactiveCaptured
	if !strings.Contains(strings.Join(interactiveArgs, " "), "--yes") {
		t.Fatalf("Provide should pass --yes to kubectl exec: %q", strings.Join(interactiveArgs, " "))
	}
}

// TestHostModeUpstreamAddProvideWithDedicatedKey pins the --dedicated-key
// flag in host-mode Provide: the provide-key exec includes the flag and the
// main add exec includes it.
func TestHostModeUpstreamAddProvideWithDedicatedKey(t *testing.T) {
	t.Parallel()
	runner, interactiveCaptured := captureInteractiveRunner(t)
	stdinRunner, stdinCaptured := captureStdinRunner(t)
	_, privatePEM, _ := testSSHIdentity(t)
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "my_deploy_key")
	if err := os.WriteFile(keyPath, privatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"--dedicated-key",
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("p\n" + keyPath + "\n"),
		runInteractive: runner,
		runWithStdin:   stdinRunner,
	})
	if err != nil {
		t.Fatalf("host-mode dedicated-key provide error = %v\noutput: %s", err, output.String())
	}
	if len(*stdinCaptured) != 1 {
		t.Fatalf("expected one provide-key exec, got %d", len(*stdinCaptured))
	}
	provideJoined := strings.Join(append([]string{(*stdinCaptured)[0].name}, (*stdinCaptured)[0].args...), " ")
	if !strings.Contains(provideJoined, "--dedicated-key") {
		t.Fatalf("provide exec missing --dedicated-key: %q", provideJoined)
	}
	interactiveJoined := strings.Join(*interactiveCaptured, " ")
	if !strings.Contains(interactiveJoined, "--dedicated-key") {
		t.Fatalf("main exec missing --dedicated-key: %q", interactiveJoined)
	}
}

// TestHostModeUpstreamAddProvideRejectsPassphraseKey pins the passphrase
// rejection in the host-mode Provide path — local validation catches it
// before any exec.
func TestHostModeUpstreamAddProvideRejectsPassphraseKey(t *testing.T) {
	t.Parallel()
	encryptedPEM := []byte(`-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABB9RpM+Pu
8MKLyKb+oSLJCVAAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAIIWqpJFMrDXfC9a3Gg
Y/3VO7SoxlAtMeoxJjPPEV5LN1AAAAoIE8rhtCb6PYxZyJwSfXoMPtTYF7gidFQD4bB3H5
N4AUb4CX/1VBpPPb5FvqaFjh/Bf7VPj3a8fFGdGE/D/rkFcE8W/bEYaHfCM0bXHVYMIvHE
5G+c0aA8IEMmQV+OaKpGxqYEkDSRLa6y8SfJSxGqSPa/juTZEhp/TkJN99MvDCHUxCuPJi
hP7xKHxBw8yjBLB8XLMDY1IrxfKnEVVwwlA=
-----END OPENSSH PRIVATE KEY-----
`)
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "encrypted_key")
	if err := os.WriteFile(keyPath, encryptedPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("p\n" + keyPath + "\n"),
		runInteractive: func(context.Context, string, ...string) error { t.Fatal("should not exec"); return nil },
		runWithStdin:   func(context.Context, io.Reader, string, ...string) error { t.Fatal("should not exec"); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "passphrase-protected") {
		t.Fatalf("encrypted key error = %v, want passphrase rejection", err)
	}
}

// TestHostModeUpstreamAddProvideRejectsNonexistentFile pins the error when
// the user provides a path to a file that does not exist.
func TestHostModeUpstreamAddProvideRejectsNonexistentFile(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("p\n/nonexistent/key\n"),
		runInteractive: func(context.Context, string, ...string) error { t.Fatal("should not exec"); return nil },
		runWithStdin:   func(context.Context, io.Reader, string, ...string) error { t.Fatal("should not exec"); return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("nonexistent key error = %v, want 'no such file'", err)
	}
}

// TestHostModeUpstreamAddProvideRefusesOverwrite pins that a pod-side
// overwrite rejection propagates back through the host Provide path. The
// overwrite check itself is in the pod's provide-key handler; this test
// verifies the host flow surfaces the error (tested end-to-end by
// TestInPodProvideKeyRefusesOverwrite).
func TestHostModeUpstreamAddProvideRefusesOverwrite(t *testing.T) {
	t.Parallel()
	_, newPEM, _ := testSSHIdentity(t)
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "new_key")
	if err := os.WriteFile(keyPath, newPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("p\n" + keyPath + "\n"),
		runInteractive: func(context.Context, string, ...string) error { t.Fatal("should not exec add"); return nil },
		runWithStdin: func(_ context.Context, _ io.Reader, _ string, _ ...string) error {
			return errors.New("the Secret oberth/oberth-upstream-key already holds a key at id_ed25519; refusing to overwrite")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrite guard error = %v, want 'refusing to overwrite'", err)
	}
}

// TestInPodProvideKeyFailsClosedWithoutMutationGate verifies that the in-pod
// provide-key handler refuses to apply a Secret when the audit mutation gate
// is unavailable — mirroring TestUpstreamSecretMutationFailsClosedBeforePatch.
func TestInPodProvideKeyFailsClosedWithoutMutationGate(t *testing.T) {
	t.Parallel()
	_, privatePEM, _ := testSSHIdentity(t)
	client := newBootstrapClient("oberth", "oberth-upstream-key", "oberth-known-hosts")
	var output bytes.Buffer
	err := runUpstreamProvideKey(context.Background(), []string{"codeberg"}, bytes.NewReader(privatePEM), &output, upstreamDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) { return client, nil },
		mutationGate: func(context.Context, string, string) error {
			return errors.New("injected external audit failure")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected external audit failure") {
		t.Fatalf("gate rejection error = %v, want fail-closed audit rejection", err)
	}
	if countSecretPatches(client) != 0 {
		t.Fatal("upstream Secret changed after audit gate rejection")
	}
}

// TestInPodProvideKeyRefusesOverwrite verifies that the in-pod provide-key
// handler refuses to overwrite an existing data key in the upstream Secret.
func TestInPodProvideKeyRefusesOverwrite(t *testing.T) {
	t.Parallel()
	_, existingPEM, _ := testSSHIdentity(t)
	_, newPEM, _ := testSSHIdentity(t)
	client := fake.NewClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "oberth-upstream-key", Namespace: "oberth"},
			Data:       map[string][]byte{"id_ed25519": existingPEM},
		},
	)
	var output bytes.Buffer
	err := runUpstreamProvideKey(context.Background(), []string{"codeberg"}, bytes.NewReader(newPEM), &output, upstreamDependencies{
		kubernetesClient: func() (kubernetes.Interface, error) { return client, nil },
		mutationGate:     allowTestMutation,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("overwrite guard error = %v, want 'refusing to overwrite'", err)
	}
	if countSecretPatches(client) != 0 {
		t.Fatal("upstream Secret was patched despite overwrite guard")
	}
}

// TestHostModeNeverDialsAdminSocket pins the structural invariant that the
// host-mode add flow never dials the pod-private admin mutation gate socket.
// The hostUpstreamDeps struct has no mutationGate field, so the only code
// paths are kubectl exec invocations — asserted here.
func TestHostModeNeverDialsAdminSocket(t *testing.T) {
	t.Parallel()
	interactiveRunner, interactiveCaptured := captureInteractiveRunner(t)
	stdinRunner, stdinCaptured := captureStdinRunner(t)
	_, privatePEM, _ := testSSHIdentity(t)
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "my_deploy_key")
	if err := os.WriteFile(keyPath, privatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("p\n" + keyPath + "\n"),
		runInteractive: interactiveRunner,
		runWithStdin:   stdinRunner,
	})
	if err != nil {
		t.Fatalf("host-mode error = %v", err)
	}
	// Verify only kubectl exec invocations occurred.
	for _, exec := range *stdinCaptured {
		if exec.name != "kubectl" {
			t.Fatalf("host-mode provide invoked %q, not kubectl", exec.name)
		}
	}
	interactiveArgs := *interactiveCaptured
	if len(interactiveArgs) > 0 && interactiveArgs[0] != "kubectl" {
		t.Fatalf("host-mode add invoked %q, not kubectl", interactiveArgs[0])
	}
	// Structural: hostUpstreamDeps has no mutationGate field. The type
	// system enforces this; if a mutationGate field were added, this test
	// file would show a compile error at every struct literal above.
}

// TestHostProvideRejectsGroupAccessibleKeyFile pins the permission parity
// between host-mode Provide and in-pod validateOperatorFile: key files that
// are writable by group or accessible by other are refused.
func TestHostProvideRejectsGroupAccessibleKeyFile(t *testing.T) {
	t.Parallel()
	_, privatePEM, _ := testSSHIdentity(t)
	for _, tc := range []struct {
		name string
		perm os.FileMode
	}{
		{"other-readable", 0o644},
		{"other-writable", 0o602},
		{"group-writable", 0o620},
		{"group-executable", 0o610},
		{"world-executable", 0o601},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			keyPath := filepath.Join(directory, "bad_perms_key")
			if err := os.WriteFile(keyPath, privatePEM, 0o600); err != nil {
				t.Fatal(err)
			}
			// Chmod explicitly — WriteFile is subject to umask.
			if err := os.Chmod(keyPath, tc.perm); err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err := runUpstreamAddFromHost(context.Background(), []string{
				"codeberg", "ssh://git@codeberg.org/acme",
			}, &output, hostUpstreamDeps{
				input:          strings.NewReader("p\n" + keyPath + "\n"),
				runInteractive: func(context.Context, string, ...string) error { t.Fatal("should not exec"); return nil },
				runWithStdin:   func(context.Context, io.Reader, string, ...string) error { t.Fatal("should not exec"); return nil },
			})
			if err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
				t.Fatalf("perm %04o: error = %v, want unsafe permissions rejection", tc.perm, err)
			}
		})
	}
}

// TestUpstreamAddHelpNeedsNoKubeconfig verifies that `upstream add --help`
// prints usage and returns nil even on a host with no kubeconfig (item 2).
func TestUpstreamAddHelpNeedsNoKubeconfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent"))
	// Force host mode by unsetting the in-cluster env.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	var output bytes.Buffer
	err := runUpstream(context.Background(), []string{"add", "--help"}, &output)
	if err != nil {
		t.Fatalf("upstream add --help with no kubeconfig = %v", err)
	}
	if !strings.Contains(output.String(), "upstream") {
		t.Fatalf("expected usage output, got %q", output.String())
	}
}

// TestHostProvideKeyStreamingReachesPod verifies that the host Provide path
// calls the provide-key subcommand via kubectl exec with the key material
// piped on stdin, and that the exec args include all required flags.
func TestHostProvideKeyStreamingReachesPod(t *testing.T) {
	t.Parallel()
	runner, _ := captureInteractiveRunner(t)
	stdinRunner, stdinCaptured := captureStdinRunner(t)
	_, privatePEM, _ := testSSHIdentity(t)
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "stream_key")
	if err := os.WriteFile(keyPath, privatePEM, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"--upstream-key-secret", "custom-secret",
		"--namespace", "custom-ns",
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader("p\n" + keyPath + "\n"),
		runInteractive: runner,
		runWithStdin:   stdinRunner,
	})
	if err != nil {
		t.Fatalf("streaming test error = %v", err)
	}
	if len(*stdinCaptured) != 1 {
		t.Fatalf("expected one provide-key exec, got %d", len(*stdinCaptured))
	}
	exec := (*stdinCaptured)[0]
	joined := strings.Join(append([]string{exec.name}, exec.args...), " ")
	for _, want := range []string{
		"kubectl exec -i",
		"oberth upstream provide-key",
		"--namespace custom-ns",
		"--upstream-key-secret custom-secret",
		"codeberg",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("provide exec missing %q: %q", want, joined)
		}
	}
	if !bytes.Equal(exec.stdin, privatePEM) {
		t.Fatal("key material was not streamed to the pod's stdin")
	}
}

// TestHostModeTargetDisclosure verifies that host-mode add prints the
// kubeconfig target before the first mutating action.
func TestHostModeTargetDisclosure(t *testing.T) {
	t.Parallel()
	runner, _ := captureInteractiveRunner(t)
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"--yes",
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader(""),
		runInteractive: runner,
		loadTarget: func() (string, string, error) {
			return "k3s-default", "https://127.0.0.1:6443", nil
		},
	})
	if err != nil {
		t.Fatalf("target disclosure error = %v", err)
	}
	if !strings.Contains(output.String(), "Target: k3s-default (https://127.0.0.1:6443)") {
		t.Fatalf("expected target disclosure, got %q", output.String())
	}
}

// TestInPodModeSkipsKeySelectionPrompt pins that in-pod mode
// (runUpstreamWithDependencies) does NOT show the [G]enerate / [P]rovide
// prompt. This preserves the existing behavior where the key is always
// generated in-pod.
func TestInPodModeSkipsKeySelectionPrompt(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	client := newBootstrapClient("oberth", "oberth-upstream-key", "oberth-known-hosts")
	hostKey := codebergSSHHostKey(t)
	var output bytes.Buffer
	err := runUpstreamWithDependencies(context.Background(), []string{
		"add",
		"--database", filepath.Join(directory, "oberth.sqlite"),
		"--upstream-key", filepath.Join(directory, "projected", "id_ed25519"),
		"--known-hosts", filepath.Join(directory, "projected-hosts", "known_hosts"),
		"--namespace", "oberth",
		"--yes",
		"codeberg", "ssh://git@codeberg.org/acme",
	}, &output, upstreamDependencies{
		input:        strings.NewReader(""),
		mutationGate: allowTestMutation,
		kubernetesClient: func() (kubernetes.Interface, error) {
			return client, nil
		},
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{hostKey}, nil
		},
		probe:         func(context.Context, string, []byte, []byte) error { return nil },
		probeInterval: 2 * time.Millisecond,
		probeWait:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("in-pod mode error = %v\noutput: %s", err, output.String())
	}
	if strings.Contains(output.String(), "[G]enerate") {
		t.Fatal("in-pod mode must not show the key selection prompt")
	}
}

// TestHostModeExecPassesThroughFlags pins that the kubectl exec command
// correctly passes through --dedicated-key, --expected-host-fingerprint,
// --no-wait, and non-default secret names.
func TestHostModeExecPassesThroughFlags(t *testing.T) {
	t.Parallel()
	runner, captured := captureInteractiveRunner(t)
	var output bytes.Buffer
	err := runUpstreamAddFromHost(context.Background(), []string{
		"--yes",
		"--dedicated-key",
		"--expected-host-fingerprint", "SHA256:test",
		"--no-wait",
		"--upstream-key-secret", "custom-key",
		"--known-hosts-secret", "custom-hosts",
		"myforge", "ssh://git@forge.example.com/org",
	}, &output, hostUpstreamDeps{
		input:          strings.NewReader(""),
		runInteractive: runner,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*captured, " ")
	for _, want := range []string{
		"--yes",
		"--dedicated-key",
		"--expected-host-fingerprint SHA256:test",
		"--no-wait",
		"--upstream-key-secret custom-key",
		"--known-hosts-secret custom-hosts",
		"myforge",
		"ssh://git@forge.example.com/org",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("kubectl exec args missing %q: %q", want, joined)
		}
	}
}

func TestRepoRemoveDeletesMappingAndCleansCacheDirectory(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "oberth.sqlite")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}

	// Create a fake git cache directory.
	gitCacheRoot := filepath.Join(directory, "git")
	cachePath := filepath.Join(gitCacheRoot, "widget.git")
	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cachePath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := runRepoWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "--git-cache-root", gitCacheRoot, "widget",
	}, &output, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "removed repository widget") || !strings.Contains(output.String(), "was upstream github") {
		t.Fatalf("repo remove output = %q", output.String())
	}

	// Verify the repository is gone from the database.
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, repoErr := database.RepositoryByName(context.Background(), "widget")
	closeErr := database.Close()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(repoErr, store.ErrNotFound) {
		t.Fatalf("repository still exists after removal: %v", repoErr)
	}

	// Verify the git cache directory was cleaned up.
	if _, err := os.Stat(cachePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git cache directory still exists: %v", err)
	}
}

func TestRepoRemoveRefusesInFlightRuns(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")
	if err := runRepoWithDependencies(context.Background(), []string{
		"add", "--database", databasePath, "widget", "github",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation}); err != nil {
		t.Fatal(err)
	}

	// Create an in-flight run.
	database, err := store.OpenAdminClient(context.Background(), databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.RepositoryByName(context.Background(), "widget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: repository.ID, RefKind: model.RefBranch, Ref: "main",
		SHA: strings.Repeat("a", 40), Actor: "test@host", Trigger: "push",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	err = runRepoWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "widget",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation})
	if err == nil || !strings.Contains(err.Error(), "in-flight") {
		t.Fatalf("expected in-flight run rejection, got %v", err)
	}
}

func TestRepoRemoveRefusesNonexistentRepository(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	registerTestUpstream(t, databasePath, "github", "ssh://git@github.com/octocat")

	err := runRepoWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "nonexistent",
	}, io.Discard, repoDependencies{mutationGate: allowTestMutation})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRepoRemoveFailsClosedWhenAuditGateRejects(t *testing.T) {
	t.Parallel()
	databasePath := filepath.Join(t.TempDir(), "oberth.sqlite")
	err := runRepoWithDependencies(context.Background(), []string{
		"remove", "--database", databasePath, "widget",
	}, io.Discard, repoDependencies{mutationGate: func(context.Context, string, string) error {
		return errors.New("injected external audit failure")
	}})
	if err == nil || !strings.Contains(err.Error(), "injected external audit failure") {
		t.Fatalf("gate rejection error = %v", err)
	}
}

func TestRepoRemoveHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runRepoWithDependencies(context.Background(), []string{"remove", "--help"}, &output, repoDependencies{mutationGate: allowTestMutation})
	if err != nil {
		t.Fatalf("repo remove --help = %v", err)
	}
}
