package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRealLogFollowerAcrossInstallerRotation(t *testing.T) {
	binary := composeBinary(t)
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "continues", true: "one clear failure"}[broken], func(t *testing.T) {
			realLogRotation(t, binary, broken)
		})
	}
}

func realLogRotation(t *testing.T, binary string, broken bool) {
	t.Helper()
	configHome := t.TempDir()
	f := signedMaintenanceFixtureAt(t, filepath.Join(configHome, "oberth"))
	var rotated atomic.Bool
	server, auth := logRotationEndpoint(t, &f, &rotated)
	profile := f.session.profile
	var err error
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "log", "12345678", "build")
	command.Env = []string{"HOME=" + configHome, "XDG_CONFIG_HOME=" + configHome, "OBERTH_PROFILE=installed"}
	output := &rotationOutput{seen: make(chan struct{})}
	var diagnostic bytes.Buffer
	command.Stdout, command.Stderr = output, &diagnostic
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-output.seen:
	case err = <-done:
		t.Fatal("follower did not start", err, output.String(), diagnostic.String())
	case <-ctx.Done():
		t.Fatal("follower did not read initial output")
	}
	upgradeSignedFixture(t, &f)
	if _, err = os.Stat(profile.AdminCert); !os.IsNotExist(err) {
		t.Fatal("rotation retained old certificate", err)
	}
	if broken {
		auth.Set(apicontract.Caller{}, publicerr.New(publicerr.Forbidden, "Identity was revoked.", "Use a current identity.", nil))
	}
	rotated.Store(true)
	server.CloseClientConnections()
	select {
	case err = <-done:
	case <-ctx.Done():
		t.Fatal("follower did not finish after rotation")
	}
	result := rotationResult{err: err, out: output.String(), diagnostic: diagnostic.String()}
	result.check(t, broken)
}

type rotationResult struct {
	err             error
	out, diagnostic string
}

func (r rotationResult) check(t *testing.T, broken bool) {
	t.Helper()
	if broken {
		want := "Log connection could not be renewed.\nRe-run the command.\n"
		if r.err == nil || r.diagnostic != want {
			t.Fatal("unclear rotation failure", r.err, r.diagnostic)
		}
		return
	}
	if r.err != nil || r.diagnostic != "" || strings.Count(r.out, "before-rotation\n") != 1 || strings.Count(r.out, "after-rotation\n") != 1 {
		t.Fatal("follower did not continue exactly once", r.err, r.out, r.diagnostic)
	}
}

func logRotationEndpoint(t *testing.T, f *signedMaintenance, rotated *atomic.Bool) (*httptest.Server, *fake.Authenticator) {
	t.Helper()
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Run: func(context.Context) (apicontract.RunRecord, error) {
		run := stepmodel.RunSummary{ID: "12345678", Status: stepmodel.RunRunning, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepRunning}}}
		if rotated.Load() {
			run.Status, run.Steps[0].Status = stepmodel.RunPassed, stepmodel.StepPassed
		}
		return apicontract.RunRecord{RunSummary: run}, nil
	}}
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	logs.Run = func(context.Context) (apicontract.LogResult, error) {
		args, _, _ := logs.Observed()
		offset := args[len(args)-1].ByteOffset
		if offset == nil || *offset == 0 {
			return apicontract.LogResult{Content: "before-rotation\n", Meta: logcontract.Meta{NextByteOffset: 16, Follow: true}}, nil
		}
		if *offset != 16 {
			t.Error("log cursor changed across reconnect", *offset)
		}
		if rotated.Load() {
			return apicontract.LogResult{Content: "after-rotation\n", Meta: logcontract.Meta{NextByteOffset: 31}}, nil
		}
		return apicontract.LogResult{Meta: logcontract.Meta{NextByteOffset: 16, Follow: true}}, nil
	}
	b.RunLogs.Handler = logs
	registry, err := api.Builtin(b)
	if err != nil {
		t.Fatal(err)
	}
	runtime := fake.NewClock().Runtime()
	runtime.Now = time.Now //nolint:forbidigo // The real CLI and TLS sockets use wall time.
	auth := fake.NewAuth(apicontract.Caller{Actor: actor.Actor{Identity: "operator", Fingerprint: "admin", Admin: true}, CredentialID: "admin"})
	endpoint, err := api.New(api.Config{Registry: registry, Auth: auth, Runtime: runtime, Classify: publicerr.Classify, FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(endpoint.BeginDrain)
	server := httptest.NewUnstartedServer(endpoint.Handler())
	server.TLS = logRotationTLS(t, *f)
	server.StartTLS()
	t.Cleanup(server.Close)
	address := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	profile := f.session.profile
	profile.ServerURL = address
	metadata, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(f.session.app.base, "installed", "installation.json")
	if err = client.WritePrivate(profilePath, metadata); err != nil {
		t.Fatal(err)
	}
	f.session.choices.URL = address
	return server, auth
}

func logRotationTLS(t *testing.T, f signedMaintenance) *tls.Config {
	t.Helper()
	secret, err := f.session.engine.Cluster.Client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-ca", meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	caBlock, _ := pem.Decode(secret.Data["tls.crt"])
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(secret.Data["tls.key"])
	signer, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(f.session.profile.AdminCert, f.session.profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now() //nolint:forbidigo // The real CLI verifies this short-lived server certificate.
	template := &x509.Certificate{SerialNumber: big.NewInt(123), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, leaf.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: pair.PrivateKey}}}
}

func TestLogReconnectsOnceOnAuthenticationFailure(t *testing.T) {
	for _, failures := range []int{1, 2} {
		b := fake.Bindings()
		calls := 0
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Run: func(context.Context) (apicontract.RunRecord, error) {
			calls++
			if calls <= failures {
				return apicontract.RunRecord{}, publicerr.New(publicerr.Forbidden, "Identity was revoked.", "Use a current identity.", nil)
			}
			return apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "12345678", Status: stepmodel.RunPassed}}, nil
		}}
		app, _, diagnostic := fixture(t, b)
		code := app.run(t.Context(), []string{"log", "12345678"})
		if calls != 2 || failures == 1 && (code != 0 || diagnostic.Len() != 0) || failures == 2 && (code != 1 || diagnostic.String() != "Log connection could not be renewed.\nRe-run the command.\n") {
			t.Fatal("authentication recovery was unbounded or unclear", failures, calls, code, diagnostic.String())
		}
	}
}

func TestLogReconnectKeepsOriginalTrust(t *testing.T) {
	f := signedMaintenanceFixture(t)
	b := fake.Bindings()
	calls := 0
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Run: func(context.Context) (apicontract.RunRecord, error) {
		calls++
		if err := os.WriteFile(f.session.profile.CAFile, []byte("changed trust"), 0600); err != nil {
			t.Fatal(err)
		}
		return apicontract.RunRecord{}, publicerr.New(publicerr.Forbidden, "Identity was revoked.", "Use a current identity.", nil)
	}}
	app, _, diagnostic := fixture(t, b)
	env := app.env
	app.env = func(name string) string {
		switch name {
		case "OBERTH_CA_FILE":
			return f.session.profile.CAFile
		case "OBERTH_CLIENT_CERT":
			return f.session.profile.AdminCert
		case "OBERTH_CLIENT_KEY":
			return f.session.profile.AdminKey
		default:
			return env(name)
		}
	}
	if code := app.run(t.Context(), []string{"log", "12345678"}); code != 1 || calls != 1 || diagnostic.String() != "Log connection could not be renewed.\nRe-run the command.\n" {
		t.Fatal("log reconnect accepted replacement trust", code, calls, diagnostic.String())
	}
}

type rotationOutput struct {
	mu   sync.Mutex
	body bytes.Buffer
	seen chan struct{}
	once sync.Once
}

func (w *rotationOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.body.Write(p)
	if strings.Contains(w.body.String(), "before-rotation\n") {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func (w *rotationOutput) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.body.String() }
