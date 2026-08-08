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

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/store"
)

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
	hostKey, _, _ := testSSHIdentity(t)
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
	hostKey, _, _ := testSSHIdentity(t)
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
	if !errors.Is(err, app.ErrUpstreamBootstrapIncomplete) {
		t.Fatalf("generated bootstrap error = %v", err)
	}
	if scanCalls != 1 || probeCalls != 0 || upstreamCount(t, databasePath) != 0 {
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
	if !strings.Contains(output.String(), ssh.FingerprintSHA256(hostKey)) || strings.Contains(output.String(), "registered upstream codeberg") {
		t.Fatalf("bootstrap output = %q", output.String())
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
	var recoveryOutput bytes.Buffer
	err = runUpstreamWithDependencies(ctx, arguments, &recoveryOutput, dependencies)
	if err == nil || !strings.Contains(err.Error(), "deploy key is not registered yet") {
		t.Fatalf("unregistered-key probe error = %v", err)
	}
	if probeCalls != 1 || upstreamCount(t, databasePath) != 0 || countSecretPatches(client) != patchesBefore {
		t.Fatalf("failed probe: probes=%d upstreams=%d patches=%d->%d", probeCalls, upstreamCount(t, databasePath), patchesBefore, countSecretPatches(client))
	}
	if !bytes.Contains(recoveryOutput.Bytes(), bytes.TrimSpace(publicBody)) || bytes.Contains(recoveryOutput.Bytes(), privateBody) {
		t.Fatalf("recovery output did not safely re-advertise the public key: %q", recoveryOutput.String())
	}

	probeErr = nil
	var registeredOutput bytes.Buffer
	if err := runUpstreamWithDependencies(ctx, arguments, &registeredOutput, dependencies); err != nil {
		t.Fatal(err)
	}
	if probeCalls != 2 || upstreamCount(t, databasePath) != 1 || !strings.Contains(registeredOutput.String(), "registered upstream codeberg") {
		t.Fatalf("registered rerun: probes=%d upstreams=%d output=%q", probeCalls, upstreamCount(t, databasePath), registeredOutput.String())
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
		{name: "host trust rejected", input: "yes\nno\n", wantScans: 1},
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
				"codeberg", "ssh://git@codeberg.org/acme",
			}, &output, upstreamDependencies{
				input:            strings.NewReader(test.input),
				mutationGate:     allowTestMutation,
				kubernetesClient: func() (kubernetes.Interface, error) { return client, nil },
				scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
					scans++
					return []ssh.PublicKey{hostKey}, nil
				},
				probe: func(context.Context, string, []byte, []byte) error {
					t.Fatal("capability probe ran before bootstrap completed")
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
				t.Fatal("capability probe did not receive validated projected material")
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
		t.Fatal("failed capability probe registered the upstream")
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
