package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

func TestPathWithinAcceptsChildAndRejectsSibling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		root string
		path string
		want bool
	}{
		{root: "/data", path: "/data/oberth.sqlite", want: true},
		{root: "/data", path: "/data/sub/file.db", want: true},
		{root: "/data", path: "/data", want: true},
		{root: "/data", path: "/tmp/file.db", want: false},
		{root: "/data", path: "/data/../tmp/escape", want: false},
		{root: "/data", path: "relative", want: false},
		{root: "/data", path: "/dataextra/file.db", want: false},
	}
	for _, test := range tests {
		if got := pathWithin(test.root, test.path); got != test.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", test.root, test.path, got, test.want)
		}
	}
}

func TestClassifyViewErrorMapsSentinelsAndMasksUnknown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		err         error
		wantCode    int
		wantMessage string
	}{
		{
			name: "invalid input keeps message", err: fmt.Errorf("%w: repo is required", service.ErrInvalidInput),
			wantCode: http.StatusBadRequest, wantMessage: "service: invalid input: repo is required",
		},
		{
			name: "forbidden masks detail", err: fmt.Errorf("%w: repo_remove requires an admin uplink", service.ErrForbidden),
			wantCode: http.StatusForbidden, wantMessage: "forbidden",
		},
		{
			name: "not found masks detail", err: fmt.Errorf("%w: repository", store.ErrNotFound),
			wantCode: http.StatusNotFound, wantMessage: "not found",
		},
		{
			// State-based refusals are actionable answers for the caller:
			// an admin refused a repo_remove for in-flight runs or immutable
			// history must see the reason, not an opaque reference ID.
			name: "invalid state keeps message", err: fmt.Errorf("%w: repository x has 2 in-flight run(s)", store.ErrInvalidState),
			wantCode: http.StatusConflict, wantMessage: "store: invalid state transition: repository x has 2 in-flight run(s)",
		},
		{
			name: "unavailable masks detail", err: fmt.Errorf("%w: repository removal", service.ErrUnavailable),
			wantCode: http.StatusServiceUnavailable, wantMessage: "service unavailable",
		},
		{
			name: "lock held returns conflict", err: fmt.Errorf("issue lock: %w", store.ErrLockHeld),
			wantCode: http.StatusConflict, wantMessage: "issue lock held by another identity",
		},
		{
			name: "lock not owned returns conflict", err: fmt.Errorf("issue lock: %w", store.ErrLockNotOwned),
			wantCode: http.StatusConflict, wantMessage: "issue lock is expired or not owned by this identity",
		},
		{
			name: "context canceled returns service unavailable", err: fmt.Errorf("read log: %w", context.Canceled),
			wantCode: http.StatusServiceUnavailable, wantMessage: "request canceled",
		},
		{
			name: "context deadline exceeded returns gateway timeout", err: fmt.Errorf("query: %w", context.DeadlineExceeded),
			wantCode: http.StatusGatewayTimeout, wantMessage: "request timed out",
		},
		{
			// Unknown errors must never carry internals (paths, SQL state)
			// to the client; they collapse to a generic internal error.
			name: "unknown collapses to internal", err: errors.New("sqlite: disk I/O error at /data/oberth.sqlite"),
			wantCode: http.StatusInternalServerError, wantMessage: "internal error",
		},
	}
	for _, test := range tests {
		code, message := classifyViewError(test.err)
		if code != test.wantCode || message != test.wantMessage {
			t.Errorf("%s: classifyViewError = (%d, %q), want (%d, %q)", test.name, code, message, test.wantCode, test.wantMessage)
		}
	}
}

func TestSecretStoreStatusUnconfigured(t *testing.T) {
	t.Parallel()
	status := secretStoreStatus(serveOptions{})
	if status == nil || status.Configured || status.Address != "" || status.Transport != "" {
		t.Fatalf("unconfigured status = %+v", status)
	}
}

func TestSecretStoreStatusConfiguredHTTPS(t *testing.T) {
	t.Parallel()
	status := secretStoreStatus(serveOptions{
		secretStoreAddress:   "https://bao.internal:8200",
		secretStoreAuthMount: "custom-mount",
		secretStoreRole:      "oberth",
	})
	if !status.Configured || status.Address != "https://bao.internal:8200" ||
		status.AuthMount != "custom-mount" || status.Role != "oberth" || status.Transport != "https" {
		t.Fatalf("configured HTTPS status = %+v", status)
	}
}

func TestSecretStoreStatusConfiguredInsecureHTTP(t *testing.T) {
	t.Parallel()
	status := secretStoreStatus(serveOptions{
		secretStoreAddress:   "http://bao.internal:8200",
		secretStoreRole:      "oberth",
		secretStoreAuthMount: "",
	})
	if !status.Configured || status.Transport != "insecure-http" {
		t.Fatalf("insecure HTTP status = %+v", status)
	}
	// Empty mount should fall back to the default.
	if status.AuthMount != secretstore.DefaultAuthMountPath {
		t.Fatalf("empty mount fallback = %q, want %q", status.AuthMount, secretstore.DefaultAuthMountPath)
	}
}

func TestWaitForAuditIntegrityImmediateSuccess(t *testing.T) {
	t.Parallel()
	err := waitForAuditIntegrity(context.Background(), func(context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("immediate gate pass = %v", err)
	}
}

func TestWaitForAuditIntegrityPollsUntilClear(t *testing.T) {
	t.Parallel()
	calls := 0
	err := waitForAuditIntegrity(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("not ready")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("polled gate = %v", err)
	}
	if calls < 3 {
		t.Fatalf("gate was called %d times, expected at least 3", calls)
	}
}

func TestWaitForAuditIntegrityRespectsContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := waitForAuditIntegrity(ctx, func(context.Context) error {
		return errors.New("never clear")
	})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v, want context.Canceled", err)
	}
}

func TestWaitForAuditIntegrityRejectsNilGate(t *testing.T) {
	t.Parallel()
	err := waitForAuditIntegrity(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("nil gate error = %v", err)
	}
}

func TestLoadTSARootsEmptyPathReturnsSystemPool(t *testing.T) {
	t.Parallel()
	roots, err := loadTSARoots("")
	if err != nil {
		t.Fatalf("system pool: %v", err)
	}
	if roots == nil {
		t.Fatal("system pool is nil")
	}
}

func TestLoadTSARootsCustomPEM(t *testing.T) {
	t.Parallel()
	// Generate a self-signed certificate to use as a trust root.
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          mustBigInt(),
		BasicConstraintsValid: true, IsCA: true,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "roots.pem")
	if err := os.WriteFile(path, pemData, 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := loadTSARoots(path)
	if err != nil {
		t.Fatalf("custom PEM: %v", err)
	}
	if roots == nil {
		t.Fatal("custom PEM returned nil pool")
	}
}

func TestLoadTSARootsInvalidPEM(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadTSARoots(path)
	if err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("invalid PEM error = %v", err)
	}
}

func TestLoadTSARootsMissingFile(t *testing.T) {
	t.Parallel()
	_, err := loadTSARoots(filepath.Join(t.TempDir(), "absent.pem"))
	if err == nil {
		t.Fatal("missing file did not return error")
	}
}

func TestWitnessChainResetRequestedReportsAcknowledgment(t *testing.T) {
	t.Parallel()
	zero := witnessChainReset{}
	if zero.requested() {
		t.Fatal("zero value should not be requested")
	}
	active := witnessChainReset{acknowledgedTip: strings.Repeat("a", 64)}
	if !active.requested() {
		t.Fatal("non-empty acknowledgedTip should be requested")
	}
}

func TestWitnessChainResetPrintfWithAndWithoutLogger(t *testing.T) {
	t.Parallel()
	// Without a logger, printf must not panic.
	noLogger := witnessChainReset{}
	noLogger.printf("should not panic: %s", "test")

	// With a logger, printf must write to the log.
	var buf bytes.Buffer
	withLogger := witnessChainReset{logger: log.New(&buf, "", 0)}
	withLogger.printf("test message: %d", 42)
	if !strings.Contains(buf.String(), "test message: 42") {
		t.Fatalf("logger output = %q", buf.String())
	}
}

func TestRunServeHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runServe(context.Background(), []string{"--help"}, &output)
	if err != nil {
		t.Fatalf("runServe --help error = %v", err)
	}
	// The help output should mention some flags.
	text := output.String()
	if !strings.Contains(text, "data") || !strings.Contains(text, "ssh-listen") {
		t.Fatalf("help output missing expected flags: %q", text)
	}
}

func TestRunServeRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{"--argo-namespace", "argo", "extra"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "serve accepts flags only") {
		t.Fatalf("positional argument error = %v", err)
	}
}

func TestParseServeOptionsValidatesSecretStoreFlagsWithoutAddress(t *testing.T) {
	t.Parallel()
	argoBase := []string{"--argo-namespace", "argo-pipelines"}
	for _, extra := range [][]string{
		{"--secretstore-role", "oberth"},
		{"--secretstore-ca-cert", "/etc/ca.pem"},
		{"--secretstore-sa-token", "/var/run/token"},
		{"--secretstore-path", "oberth/data/test"},
		{"--secretstore-insecure-http"},
		{"--secretstore-transit-mount", "transit"},
		{"--secretstore-transit-key", "plan-key"},
	} {
		arguments := append(append([]string(nil), argoBase...), extra...)
		if _, err := parseServeOptions(arguments, io.Discard); err == nil {
			t.Fatalf("secretstore flags without address accepted: %q", arguments)
		}
	}
}

func TestParseServeOptionsValidatesSecretStoreAddressRequiresRole(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--secretstore-address", "https://bao:8200",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires --secretstore-role") {
		t.Fatalf("missing role error = %v", err)
	}
}

func TestParseServeOptionsValidatesTransitRequiresBoth(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--secretstore-address", "https://bao:8200",
		"--secretstore-role", "oberth",
		"--secretstore-transit-mount", "transit",
		// missing --secretstore-transit-key
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "transit requires both") {
		t.Fatalf("transit without key error = %v", err)
	}
}

func TestParseServeOptionsValidatesPushBannerURL(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--push-banner-url", "http://insecure.example.com",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "push banner URL") {
		t.Fatalf("insecure push banner error = %v", err)
	}
}

func TestParseServeOptionsAcceptsValidPushBannerURL(t *testing.T) {
	t.Parallel()
	options, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--push-banner-url", "https://watch.oberth.ci/runs",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.pushBannerURL != "https://watch.oberth.ci/runs" {
		t.Fatalf("push banner URL = %q", options.pushBannerURL)
	}
}

func TestParseServeOptionsValidatesArgoNamespaceSameAsNamespace(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "oberth",
		"--namespace", "oberth",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("same namespace error = %v", err)
	}
}

func TestParseServeOptionsValidatesWitnessChainResetFormat(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"short",
		"not-hex-at-all-but-right-length-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		strings.Repeat("g", 64),
	} {
		_, err := parseServeOptions([]string{
			"--argo-namespace", "argo-pipelines",
			"--audit-rekor-url", "https://rekor.example.test",
			"--accept-witness-chain-reset", value,
		}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "accept-witness-chain-reset") {
			t.Fatalf("invalid reset UUID %q error = %v", value, err)
		}
	}
}

func TestParseServeOptionsAcceptsValid64CharHexReset(t *testing.T) {
	t.Parallel()
	uuid := strings.Repeat("ab", 32) // 64 hex chars
	options, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--audit-rekor-url", "https://rekor.example.test",
		"--accept-witness-chain-reset", uuid,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.acceptWitnessChainReset != uuid {
		t.Fatalf("accepted UUID = %q, want %q", options.acceptWitnessChainReset, uuid)
	}
}

func TestParseServeOptionsAcceptsValid80CharHexReset(t *testing.T) {
	t.Parallel()
	uuid := strings.Repeat("cd", 40) // 80 hex chars
	options, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--audit-rekor-url", "https://rekor.example.test",
		"--accept-witness-chain-reset", uuid,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.acceptWitnessChainReset != uuid {
		t.Fatalf("accepted UUID = %q, want %q", options.acceptWitnessChainReset, uuid)
	}
}

func TestStringListSetRejectsEmpty(t *testing.T) {
	t.Parallel()
	var list stringList
	if err := list.Set("valid"); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != "valid" {
		t.Fatalf("list = %v", list)
	}
	if err := list.Set("  "); err == nil {
		t.Fatal("empty value accepted")
	}
	if err := list.Set(""); err == nil {
		t.Fatal("blank value accepted")
	}
}

func TestStringListString(t *testing.T) {
	t.Parallel()
	list := stringList{"a", "b", "c"}
	if got := list.String(); got != "a,b,c" {
		t.Fatalf("String() = %q, want %q", got, "a,b,c")
	}
}

func TestLoadOptionalCACertEmptyPathReturnsNil(t *testing.T) {
	t.Parallel()
	pool, err := loadOptionalCACert("")
	if err != nil || pool != nil {
		t.Fatalf("empty path: pool=%v, err=%v", pool, err)
	}
}

func TestLoadOptionalCACertValidPEM(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          mustBigInt(),
		BasicConstraintsValid: true, IsCA: true,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemData, 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := loadOptionalCACert(path)
	if err != nil {
		t.Fatal(err)
	}
	if pool == nil {
		t.Fatal("valid PEM returned nil pool")
	}
}

func TestLoadOptionalCACertInvalidPEM(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(path, []byte("not valid PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOptionalCACert(path)
	if err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("invalid PEM error = %v", err)
	}
}

func TestLoadOptionalCACertMissingFile(t *testing.T) {
	t.Parallel()
	_, err := loadOptionalCACert(filepath.Join(t.TempDir(), "absent.pem"))
	if err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestRunServeRejectsBadFlag(t *testing.T) {
	t.Parallel()
	err := runServe(context.Background(), []string{"--completely-invalid"}, io.Discard)
	if err == nil {
		t.Fatal("bad flag accepted")
	}
	if !errors.Is(err, errUsage) {
		t.Fatalf("bad flag error = %v, want errUsage wrapped", err)
	}
}

func TestParseServeOptionsAcceptsArgoVaultCACert(t *testing.T) {
	t.Parallel()
	// Create a valid CA cert file for the startup read validation.
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          mustBigInt(),
		BasicConstraintsValid: true, IsCA: true,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(cryptorand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pemData, 0o600); err != nil {
		t.Fatal(err)
	}
	options, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--argo-vault-address", "https://bao.internal:8200",
		"--argo-vault-credentialed-role", "oberth-release",
		"--argo-vault-ca-cert", caPath,
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.argoVaultCACert != caPath {
		t.Fatalf("argoVaultCACert = %q, want %q", options.argoVaultCACert, caPath)
	}
}

func TestParseServeOptionsRejectsRelativeArgoVaultCACert(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--argo-vault-ca-cert", "relative/ca.pem",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "clean absolute path") {
		t.Fatalf("relative CA cert path error = %v", err)
	}
}

func TestParseServeOptionsRejectsArgoVaultRoleWithoutAddress(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--argo-vault-credentialed-role", "oberth-release",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "needs --argo-vault-address") {
		t.Fatalf("role without address error = %v", err)
	}
}

func TestParseServeOptionsRejectsInvalidArgoWorkflowTimings(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--argo-workflow-ttl", "0",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("zero TTL error = %v", err)
	}
}

func TestParseServeOptionsRejectsMissingArgoNamespace(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions(nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "argo-namespace is required") {
		t.Fatalf("missing argo namespace error = %v", err)
	}
}

func TestParseServeOptionsRejectsEmptyNamespace(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--namespace", "  ",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("empty namespace error = %v", err)
	}
}

func TestParseServeOptionsRejectsNegativeConcurrency(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--max-concurrent-jobs", "-1",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("negative concurrency error = %v", err)
	}
}

func TestParseServeOptionsRejectsExcessiveConcurrency(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--max-concurrent-jobs", "65",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("excessive concurrency error = %v", err)
	}
}

func TestParseServeOptionsRejectsNonCleanDatabase(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--database", "/data/../escape/oberth.sqlite",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "database must be") {
		t.Fatalf("non-clean database error = %v", err)
	}
}

func TestParseServeOptionsRejectsIdenticalListenAddresses(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo-pipelines",
		"--ssh-listen", ":2222",
		"--https-listen", ":2222",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("same listen addresses error = %v", err)
	}
}

func TestParseServeOptionsRejectsAuditRekorOrphanFlags(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "rekor-ca without url",
			args: []string{"--argo-namespace", "argo", "--audit-rekor-ca", "/etc/ca.pem"},
			want: "requires --audit-rekor-url",
		},
		{
			name: "rekor-pubkey without url",
			args: []string{"--argo-namespace", "argo", "--audit-rekor-pubkey", "/etc/rekor.pub"},
			want: "requires --audit-rekor-url",
		},
		{
			name: "rekor-insecure without url",
			args: []string{"--argo-namespace", "argo", "--audit-rekor-insecure-http"},
			want: "requires --audit-rekor-url",
		},
		{
			name: "witness-reset without url",
			args: []string{"--argo-namespace", "argo", "--accept-witness-chain-reset", strings.Repeat("a", 64)},
			want: "requires the external Rekor witness",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseServeOptions(test.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("%s: error = %v, want %q", test.name, err, test.want)
			}
		})
	}
}

func TestParseServeOptionsRejectsNonCleanAuditPaths(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
	}{
		{
			name: "rekor ca relative",
			args: []string{"--argo-namespace", "argo", "--audit-rekor-url", "https://rekor.example.test",
				"--audit-rekor-ca", "relative.pem"},
		},
		{
			name: "tsa ca relative",
			args: []string{"--argo-namespace", "argo", "--audit-tsa-url", "https://tsa.example.test",
				"--audit-tsa-ca", "relative.pem"},
		},
		{
			name: "secretstore ca relative",
			args: []string{"--argo-namespace", "argo",
				"--secretstore-address", "https://bao:8200", "--secretstore-role", "oberth",
				"--secretstore-ca-cert", "relative.pem"},
		},
		{
			name: "secretstore sa-token relative",
			args: []string{"--argo-namespace", "argo",
				"--secretstore-address", "https://bao:8200", "--secretstore-role", "oberth",
				"--secretstore-sa-token", "relative-token"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseServeOptions(test.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "clean absolute path") {
				t.Fatalf("%s: error = %v", test.name, err)
			}
		})
	}
}

func TestParseServeOptionsRejectsSecretStoreTransitInsecure(t *testing.T) {
	t.Parallel()
	_, err := parseServeOptions([]string{
		"--argo-namespace", "argo",
		"--secretstore-address", "https://bao:8200",
		"--secretstore-role", "oberth",
		"--secretstore-transit-mount", "transit",
		"--secretstore-transit-key", "plan-key",
		"--secretstore-insecure-http",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "verified HTTPS") {
		t.Fatalf("transit insecure error = %v", err)
	}
}

func TestReadArgoVaultCACertEmptyReturnsEmpty(t *testing.T) {
	t.Parallel()
	body, err := readArgoVaultCACert("")
	if err != nil || body != "" {
		t.Fatalf("empty path: %q, %v", body, err)
	}
}

func TestReadArgoVaultCACertReadsFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ca.pem")
	content := "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := readArgoVaultCACert(path)
	if err != nil || body != content {
		t.Fatalf("read CA cert = %q, %v", body, err)
	}
}

func TestReadArgoVaultCACertMissingFile(t *testing.T) {
	t.Parallel()
	_, err := readArgoVaultCACert(filepath.Join(t.TempDir(), "absent.pem"))
	if err == nil {
		t.Fatal("missing CA cert file accepted")
	}
}

func TestReadBoundedFileRejectsEmptyFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readBoundedFile(path, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "non-empty regular file") {
		t.Fatalf("empty file error = %v", err)
	}
}

func TestReadBoundedFileRejectsDirectory(t *testing.T) {
	t.Parallel()
	_, err := readBoundedFile(t.TempDir(), 1<<20)
	if err == nil || !strings.Contains(err.Error(), "non-empty regular file") {
		t.Fatalf("directory error = %v", err)
	}
}

func TestReadBoundedFileRejectsOversizedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readBoundedFile(path, 10)
	if err == nil || !strings.Contains(err.Error(), "non-empty regular file") {
		t.Fatalf("oversize file error = %v", err)
	}
}

func TestReadBoundedFileReadsNormalFile(t *testing.T) {
	t.Parallel()
	content := "test content"
	path := filepath.Join(t.TempDir(), "normal.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := readBoundedFile(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != content {
		t.Fatalf("read = %q, want %q", body, content)
	}
}

func TestSecretStoreBootstrapTLSHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runSecretStoreBootstrapTLS([]string{"--help"}, &output)
	if err != nil {
		t.Fatalf("bootstrap-tls --help = %v", err)
	}
}

func TestSecretStoreBootstrapTLSRequiresFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing both", args: nil},
		{name: "only namespace", args: []string{"--namespace", "openbao"}},
		{name: "only output-dir", args: []string{"--output-dir", "/tmp/bao-tls"}},
		{name: "with positional", args: []string{"--output-dir", "/tmp/bao-tls", "--namespace", "openbao", "extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := runSecretStoreBootstrapTLS(test.args, io.Discard)
			if !errors.Is(err, errUsage) {
				t.Fatalf("error = %v, want errUsage", err)
			}
		})
	}
}

func TestSecretStoreSetupHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runSecretStoreSetup([]string{"--help"}, &output)
	if err != nil {
		t.Fatalf("setup --help = %v", err)
	}
}

func TestSecretStoreSetupRejectsPositionalArgs(t *testing.T) {
	t.Parallel()
	err := runSecretStoreSetup([]string{"extra"}, io.Discard)
	if !errors.Is(err, errUsage) {
		t.Fatalf("positional args error = %v, want errUsage", err)
	}
}

func TestSecretStoreBootstrapTLSWithDirectory(t *testing.T) {
	t.Parallel()
	// The directory must NOT exist yet; BootstrapOpenBaoTLS creates it.
	dir := filepath.Join(t.TempDir(), "tls")
	var output bytes.Buffer
	err := runSecretStoreBootstrapTLS([]string{"--output-dir", dir, "--namespace", "openbao"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		t.Fatal("bootstrap-tls produced no CA PEM output")
	}
	if !strings.Contains(output.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("output is not PEM: %q", output.String())
	}
}

func TestValidateAuditURLVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		raw      string
		insecure bool
		want     bool
	}{
		{raw: "https://example.com/path", want: true},
		{raw: "http://example.com/path", want: false},
		{raw: "http://example.com/path", insecure: true, want: true},
		{raw: "ftp://example.com", want: false},
		{raw: "ftp://example.com", insecure: true, want: false},
		{raw: "", want: false},
		{raw: "://missing-scheme", want: false},
		{raw: "https://user:pass@example.com", want: false},
	}
	for _, test := range tests {
		if got := validAuditURL(test.raw, test.insecure); got != test.want {
			t.Errorf("validAuditURL(%q, %v) = %v, want %v", test.raw, test.insecure, got, test.want)
		}
	}
}

func mustBigInt() *big.Int {
	n, _ := cryptorand.Int(cryptorand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}
