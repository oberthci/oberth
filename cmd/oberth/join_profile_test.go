package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type joinSigner struct {
	ca  *x509.Certificate
	key crypto.PrivateKey
	now func() time.Time
}

func (s joinSigner) response(t *testing.T, r *http.Request) *http.Response {
	t.Helper()
	var call struct {
		Params struct {
			Arguments model.InviteRedemption `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
		t.Fatal(err)
	}
	csr, err := clientcert.ParseCSR([]byte(call.Params.Arguments.CSR))
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(12), Subject: csr.Subject, NotBefore: s.now(), NotAfter: s.now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, s.ca, csr.PublicKey, s.key)
	if err != nil {
		t.Fatal(err)
	}
	result := model.InviteCertificate{SSHURL: "ssh://git@oberth.local:2222", Identity: "agent", Scope: []string{"project"}, Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "application/json")
	if err = json.NewEncoder(recorder).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"structuredContent": result}}); err != nil {
		t.Fatal(err)
	}
	response := recorder.Result()
	response.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{s.ca, s.ca}}
	return response
}

type joinScenario struct {
	app                   application
	out, diagnostic       *bytes.Buffer
	sshCommand            *string
	administrativeCalls   *int
	invitation, sha       string
	administrator, uplink string
}

func TestJoinBindsUplinkAndRetainsAdministratorForPromoteRelease(t *testing.T) {
	for _, name := range []string{"install", "oberth.local"} {
		t.Run(name, func(t *testing.T) { joinPromoteReleaseCase(t, name) })
	}
}
func joinPromoteReleaseCase(t *testing.T, administrator string) {
	t.Helper()
	scenario := newJoinScenario(t, administrator)
	for _, args := range [][]string{{"join", scenario.invitation}, {"promote", scenario.sha, "--repo", "project"}, {"release", "v0.1.0"}} {
		if code := scenario.app.run(t.Context(), args); code != 0 {
			t.Fatalf("%v failed (%d): %s", args, code, scenario.diagnostic.String())
		}
	}
	assertJoinScenario(t, scenario)
}
func newJoinScenario(t *testing.T, administrator string) *joinScenario {
	t.Helper()
	bindings := fake.Bindings()
	sha := strings.Repeat("a", 40)
	bindings.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: apicontract.PromoteResult{ID: "promotion", TestedRunID: "tested"}}
	bindings.PromoteStatus.Handler = &fake.Handler[apicontract.PromoteStatusArgs, apicontract.PromotionWaitResult]{Value: apicontract.PromotionWaitResult{Promotion: &model.Promotion{ID: "promotion", Status: model.PromotionPassed, ResultSHA: sha}, Publication: &model.Publication{ID: "publication:promotion", Status: model.PublicationDelivered}}}
	bindings.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "project", DefaultBranch: "main"}}}}
	bindings.Wait.Handler = &fake.Handler[apicontract.WaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "release", Status: stepmodel.RunPassed, CommitSHA: &sha}}}
	bindings.RunCheck.Handler = &fake.Handler[apicontract.RunCheckArgs, apicontract.RunCheckResult]{Value: apicontract.RunCheckResult{OK: true, ReleaseTag: "v0.1.0", CommitSHA: &sha}}
	bindings.EventsWait.Handler = &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Value: apicontract.EventResult{EventPage: model.EventPage{Events: []model.AgentEvent{{ID: 1, Kind: string(model.EventReleasePublished), RunID: "release"}}}}}

	app, out, diagnostic := fixture(t, bindings)
	signer, metadata := joinAdministratorProfile(t, &app, administrator)
	calls := joinAdministrativeTransport(t, &app, signer)
	ssh := recordJoinGit(t, &app, metadata.KnownHosts, sha)
	path := saveJoinInvitation(t, metadata, signer)
	uplink := "oberth.local"
	if administrator == uplink {
		uplink += "-uplink"
	}
	return &joinScenario{app, out, diagnostic, ssh, calls, path, sha, administrator, uplink}
}
func joinAdministratorProfile(t *testing.T, app *application, administrator string) (joinSigner, client.Profile) {
	t.Helper()
	clock := fake.NewClock()
	ca, key := authority(t, clock.Now())
	block, _ := pem.Decode(ca)
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(key)
	private, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signer := joinSigner{parsed, private, clock.Now}
	app.base = t.TempDir()
	if err = os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	install := filepath.Join(app.base, administrator)
	if err = os.Mkdir(install, 0700); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(install, "known_hosts")
	metadata := client.Profile{ServerURL: "https://oberth.local", SSHURL: "ssh://git@oberth.local:2222", AdminKey: filepath.Join(install, "admin-v1.key"), AdminCert: filepath.Join(install, "admin-v1.crt"), CAFile: filepath.Join(install, "ca.crt"), KnownHosts: known}
	hostKey, err := ssh.NewPublicKey(parsed.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(metadata)
	for name, data := range map[string][]byte{"installation.json": raw, "admin-v1.key": []byte("retained-admin-key"), "admin-v1.crt": ca, "ca.crt": ca, "known_hosts": []byte(knownhosts.Line([]string{"[oberth.local]:2222"}, hostKey) + "\n"), "token": []byte("valid")} {
		if err = client.WritePrivate(filepath.Join(install, name), data); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Symlink(administrator, filepath.Join(app.base, "current")); err != nil {
		t.Fatal(err)
	}

	app.env = func(string) string { return "" }
	return signer, metadata
}
func joinAdministrativeTransport(t *testing.T, app *application, signer joinSigner) *int {
	t.Helper()
	original := app.config.Transport
	administrativeCalls := 0
	app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/mcp/enroll" {
			return signer.response(t, r), nil
		}
		administrativeCalls++
		if r.Header.Get("Authorization") != "Bearer valid" {
			t.Error("request did not use current administrator")
		}
		return original.RoundTrip(r)
	})

	return &administrativeCalls
}
func recordJoinGit(t *testing.T, app *application, known, sha string) *string {
	t.Helper()
	app.directory = t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "oberth.repository", "project"}, {"config", "oberth.knownHosts", known}, {"config", "oberth.profile", filepath.Dir(known)}} {
		if _, err := runGit(t.Context(), app.directory, args...); err != nil {
			t.Fatal(err)
		}
	}
	var sshCommand string
	app.git = func(ctx context.Context, args []string) (string, error) {
		if len(args) == 5 && args[0] == "config" && args[3] == "remote.oberth.receivepack" {
			sshCommand = args[4]
		}
		if args[0] == "push" || args[0] == "fetch" || args[0] == "tag" || args[0] == "rev-parse" && args[1] != "--absolute-git-dir" {
			return sha, nil
		}
		return runGit(ctx, app.directory, args...)
	}
	return &sshCommand
}

// fixtureHostKeys is a well-formed installation host-key record for tests.
const fixtureHostKeys = "[oberth.local]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB\n"

// issuerHostKeys is an install record that pins issuerSSH.
const issuerSSH, issuerHostKeys = "ssh://git@127.0.0.1:2222", "[127.0.0.1]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEB\n"

// issuerKey returns a client-key path whose profile records hostKeys as the
// installation's SSH trust for sshURL, as install saves it for an administrator.
func issuerKey(t *testing.T, sshURL, hostKeys string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts-issuer")
	profile, _ := json.Marshal(client.Profile{ServerURL: "https://127.0.0.1", SSHURL: sshURL, KnownHosts: known})
	for name, data := range map[string][]byte{"installation.json": profile, "known_hosts-issuer": []byte(hostKeys)} {
		if err := client.WritePrivate(filepath.Join(dir, name), data); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "admin.key")
}

func saveJoinInvitation(t *testing.T, metadata client.Profile, signer joinSigner) string {
	t.Helper()
	invitation := model.InvitationFile{Invitation: model.Invitation{Identity: "agent", Scope: []string{"project"}, ServerURL: metadata.ServerURL, CAPin: clientcert.Pin(signer.ca), ExpiresAt: signer.now().Add(time.Hour)}, KnownHosts: fixtureHostKeys, Secret: "fake-invitation"}
	path := filepath.Join(filepath.Dir(metadata.AdminKey), "invite.json")
	raw, _ := json.Marshal(invitation)
	if err := client.WritePrivate(path, raw); err != nil {
		t.Fatal(err)
	}
	return path
}
func assertJoinScenario(t *testing.T, scenario *joinScenario) {
	t.Helper()
	app, out, sshCommand, administrativeCalls := scenario.app, scenario.out, *scenario.sshCommand, *scenario.administrativeCalls
	selected, err := os.Readlink(filepath.Join(app.base, "current"))
	if err != nil || selected != scenario.administrator {
		t.Fatal("join replaced current administrator")
	}
	joinedKey := filepath.Join(app.base, scenario.uplink, client.UplinkPushKeyFile)
	config, readErr := client.ReadPrivate(filepath.Join(filepath.Dir(joinedKey), "ssh.config"))
	if readErr != nil || !strings.HasPrefix(sshCommand, shellArgument(filepath.Join(filepath.Dir(joinedKey), "git-ssh"))+" --config ") || !strings.Contains(string(config), joinedKey) || !strings.Contains(string(config), "IdentitiesOnly yes") || strings.Contains(string(config), "admin-v1.key") {
		t.Fatalf("checkout does not use joined uplink: %s", sshCommand)
	}
	if _, err = client.ReadPrivate(joinedKey); err != nil {
		t.Fatal(err)
	}
	profileLine := "current profile: install"
	if scenario.administrator == "oberth.local" {
		profileLine = "current profile:\noberth.local"
	}
	if administrativeCalls < 5 || !strings.Contains(out.String(), profileLine) {
		t.Fatal("administrator commands did not run without overrides")
	}
}

func TestSecretstoreExecUsesStepPortAndOpaqueArgv(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	called := false
	app.secretExec = func(_ context.Context, request installationRequest, _ io.Reader, _ io.Writer) error {
		called = true
		if request.name != "secretstore exec" || strings.Join(request.args, "|") != "sh|-c|printf secret-value|--help|-h|--url|opaque" {
			t.Fatal("step argv changed")
		}
		return nil
	}
	if code := app.run(t.Context(), []string{"secretstore", "exec", "--", "sh", "-c", "printf secret-value", "--help", "-h", "--url", "opaque"}); code != 0 || !called {
		t.Fatalf("step entry failed: %d %s", code, diagnostic.String())
	}
	if code := app.run(t.Context(), []string{"secret", "exec", "--", "true"}); code != 2 {
		t.Fatal("retired spelling accepted")
	}
	if code := app.run(t.Context(), []string{"secretstore", "exec", "true"}); code != 2 {
		t.Fatal("missing separator accepted")
	}
	app.secretExec = nil
	diagnostic.Reset()
	if code := app.run(t.Context(), []string{"secretstore", "exec", "--", "true"}); code != 1 || !strings.Contains(diagnostic.String(), "OBERTH_SECRETSTORE_CONFIG") {
		t.Fatal("default step client did not explain its missing input")
	}
}
