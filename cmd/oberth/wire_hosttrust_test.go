package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestExpectedForgePinMustBelongToTheSelectedEndpoint(t *testing.T) {
	g := gittest.New(t)
	forge, other := composeForge(t, g), composeForge(t, g)
	a := serverAdministration{&serverOwners{c: &composition{config: serverConfig{DatabaseFile: filepath.Join(t.TempDir(), "state.db")}}}}
	q := apicontract.UpstreamAddArgs{Name: "origin", BaseURL: "ssh://" + forge.address, CheckRepo: "team/widget"}
	_, err := a.PrepareUpstream(t.Context(), q)
	var problem *publicerr.Error
	if !errors.As(err, &problem) || !strings.Contains(strings.Join(problem.Context, "\n"), ssh.FingerprintSHA256(forge.key)) {
		t.Fatal("unconfirmed host did not show its offered fingerprint", err)
	}
	q.ExpectedHostFingerprint = ssh.FingerprintSHA256(forge.key)
	prepared, err := a.PrepareUpstream(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(a.pinsPath()); !os.IsNotExist(err) {
		t.Fatal("pins changed before audited persistence", err)
	}
	if err = prepared.Persist(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := client.ReadPrivate(a.pinsPath())
	if err != nil {
		t.Fatal(err)
	}
	q.BaseURL = "ssh://" + other.address
	if _, err = a.PrepareUpstream(t.Context(), q); err == nil {
		t.Fatal("another endpoint accepted the confirmed fingerprint")
	}
	after, err := client.ReadPrivate(a.pinsPath())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused pin changed trust", err)
	}
	testForgeRepin(t, a, forge)
}

func testForgeRepin(t *testing.T, a serverAdministration, forge composedForge) {
	t.Helper()
	path, key := composeSSHKey(t, t.TempDir(), "new-host-key")
	raw, err := client.ReadPrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	forge.config.Store(cfg)
	unrelated := knownhosts.Line([]string{"unrelated.oberth.ci:2200"}, key) + "\n"
	before := savedHostPins(t, a)
	if err = client.WritePrivate(a.pinsPath(), append(before, unrelated...)); err != nil {
		t.Fatal(err)
	}
	q := apicontract.UpstreamAddArgs{Name: "origin", BaseURL: "ssh://" + forge.address, CheckRepo: "team/widget", ExpectedHostFingerprint: ssh.FingerprintSHA256(key)}
	if _, err = a.PrepareUpstream(t.Context(), q); err == nil {
		t.Fatal("changed host key accepted without repin")
	}
	q.Repin = true
	prepared, err := a.PrepareUpstream(t.Context(), q)
	if err != nil || !slices.Equal(prepared.Replaced, []string{ssh.FingerprintSHA256(forge.key)}) || !slices.Equal(prepared.Offered, []string{ssh.FingerprintSHA256(key)}) {
		t.Fatal("repin lost old/new fingerprints", prepared, err)
	}
	if err = prepared.Persist(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := savedHostPins(t, a)
	if !strings.Contains(string(after), unrelated) || strings.Contains(string(after), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(forge.key)))) {
		t.Fatal("repin damaged unrelated trust", err)
	}
}

func TestFreshDatabaseIsExclusiveAndExistingEvidenceIsRetained(t *testing.T) {
	c := testComposition(t)
	c.config.DatabaseFile = filepath.Join(t.TempDir(), "fresh", "state.db")
	parts, err := openStorage(t.Context(), c)
	if err != nil {
		t.Fatal("fresh storage did not open", err)
	}
	_, head, err := c.store.Audit(t.Context(), 0, 20)
	if err != nil || head.ID != 0 || head.SHA256 != stateport.AuditGenesis() {
		t.Fatal("fresh database has no canonical audit genesis", head, err)
	}
	if err = newLifecycle(t.Context(), parts).stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(c.config.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	parts, err = openStorage(t.Context(), c)
	if err != nil {
		t.Fatal("existing storage did not open", err)
	}
	if err = newLifecycle(t.Context(), parts).stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(c.config.DatabaseFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("reopen replaced existing database evidence", err)
	}
}

func savedHostPins(t *testing.T, a serverAdministration) []byte {
	t.Helper()
	pins, err := client.ReadPrivate(a.pinsPath())
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

func TestExistingDatabaseCannotBecomeFreshGenesis(t *testing.T) {
	c := testComposition(t)
	c.config.DatabaseFile = filepath.Join(t.TempDir(), "state.db")
	original := []byte("existing database evidence")
	if err := os.WriteFile(c.config.DatabaseFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openStorage(t.Context(), c); err == nil {
		t.Fatal("invalid existing database was initialized")
	}
	after, err := os.ReadFile(c.config.DatabaseFile)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("existing database evidence was overwritten", err)
	}
}

func TestPinPublicationRefusesChangedSnapshot(t *testing.T) {
	forge := composeForge(t, gittest.New(t))
	a := serverAdministration{&serverOwners{c: &composition{config: serverConfig{DatabaseFile: filepath.Join(t.TempDir(), "state.db")}}}}
	prepared, err := a.PrepareUpstream(t.Context(), apicontract.UpstreamAddArgs{Name: "origin", BaseURL: "ssh://" + forge.address, CheckRepo: "team/widget", ExpectedHostFingerprint: ssh.FingerprintSHA256(forge.key)})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(a.pinsPath()), 0700); err != nil {
		t.Fatal(err)
	}
	current := []byte("# independently changed pins\n")
	if err = client.WritePrivate(a.pinsPath(), current); err != nil {
		t.Fatal(err)
	}
	if err = prepared.Persist(t.Context()); err == nil {
		t.Fatal("stale confirmation replaced current pins")
	}
	if !bytes.Equal(savedHostPins(t, a), current) {
		t.Fatal("stale confirmation damaged current pins")
	}
}

func TestHostScanCancellationClosesHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := (hostScanner{}).Scan(ctx, listener.Addr().String(), ssh.KeyAlgoED25519); done <- err }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled scanner returned a host key")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled host handshake did not close")
	}
}

func TestUpstreamNamespaceAndProbeMustAgree(t *testing.T) {
	for _, scenario := range []struct {
		endpoint, check string
		valid           bool
	}{
		{"ssh://forge.oberth.ci/team", "team/widget", true},
		{"ssh://forge.oberth.ci/scm/team", "team/widget", true},
		{"ssh://forge.oberth.ci/team", "", true},
		{"ssh://forge.oberth.ci/team", "other/widget", false},
	} {
		endpoint, err := url.Parse(scenario.endpoint)
		if err != nil {
			t.Fatal(err)
		}
		org, err := upstreamOrganization(apicontract.UpstreamAddArgs{Name: "origin", CheckRepo: scenario.check}, endpoint)
		if (err == nil) != scenario.valid {
			t.Fatal("namespace binding changed", scenario, err)
		}
		if scenario.valid && org != "team" {
			t.Fatal("namespace was added twice", org)
		}
	}
}

func TestUpstreamTrustRefusalPrintsOfferedFingerprints(t *testing.T) {
	forge := composeForge(t, gittest.New(t))
	q := apicontract.UpstreamAddArgs{Name: "origin", BaseURL: "ssh://" + forge.address + "/team"}
	a := serverAdministration{&serverOwners{c: &composition{config: serverConfig{DatabaseFile: filepath.Join(t.TempDir(), "state.db")}}}}
	_, refused := a.PrepareUpstream(t.Context(), q)
	if refused == nil {
		t.Fatal("unconfirmed host was accepted")
	}
	b := fake.Bindings()
	b.UpstreamAdd.Handler = &fake.Handler[apicontract.UpstreamAddArgs, apicontract.Upstream]{PrepareError: refused}
	app, out, diagnostic := fixture(t, b)
	if app.run(t.Context(), []string{"upstream", "add", "--name", q.Name, "--base-url", q.BaseURL}) != 1 {
		t.Fatal("unconfirmed upstream exit changed")
	}
	if out.Len() != 0 || !strings.Contains(diagnostic.String(), ssh.FingerprintSHA256(forge.key)) {
		t.Fatal("offered fingerprints were hidden by default", diagnostic.String())
	}
}

func TestStoreCreateExclusivelyPreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	original := []byte("existing database evidence")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Create(t.Context(), path, store.DefaultOptions(testComposition(t).clock.Now))
	if opened != nil {
		_ = opened.Close()
	}
	if err == nil {
		t.Fatal("Create accepted an existing file")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("Create overwrote existing evidence", err)
	}
}
func TestUnreachableForgeReportsNoReceivedKey(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	a := serverAdministration{&serverOwners{c: &composition{config: serverConfig{DatabaseFile: filepath.Join(t.TempDir(), "state.db")}}}}
	_, err = a.PrepareUpstream(t.Context(), apicontract.UpstreamAddArgs{Name: "origin", BaseURL: "ssh://" + address, CheckRepo: "team/widget"})
	var problem *publicerr.Error
	if !errors.As(err, &problem) || problem.Reason() != "No SSH host key was received from the forge." || len(problem.Context) != 1 || !strings.Contains(problem.Context[0], address) || strings.Contains(problem.Error(), "Offered fingerprints") {
		t.Fatal("unreachable forge was misdiagnosed", err)
	}
	if !strings.Contains(problem.Detail, "connection refused") || strings.Count(problem.Detail, "connection refused") != 1 {
		t.Fatal("the dial failure is not the refusal's detail", problem.Detail)
	}
}
