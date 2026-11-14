package installer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestRuntimeCertificateUsesAuthenticatedAPIOnly(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		t.Run(map[bool]string{true: "allowed", false: "refused"}[allowed], func(t *testing.T) {
			kube := fake.NewClientset()
			calls := 0
			denied := errors.New("admin identity required")
			deps := Deps{KubeClient: kube, Admin: idleAdminFunc(func(_ context.Context, action string, args, into any) error {
				calls++
				if action != "public-identity" || args.(map[string]string)["name"] != "server-certificate" {
					t.Fatal(action, args)
				}
				if !allowed {
					return denied
				}
				return fixtureDecode(map[string]string{"content": "public leaf"}, into)
			}), RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
				t.Fatal("server exec attempted")
				return nil, nil
			}}
			raw, err := readServerPublicCertificate(t.Context(), Config{}, deps, false)
			if calls != 1 || len(kube.Actions()) != 0 {
				t.Fatal(calls, kube.Actions())
			}
			if allowed {
				if err != nil || string(raw) != "public leaf" {
					t.Fatal(err)
				}
			} else if !errors.Is(err, denied) {
				t.Fatal(err)
			}
		})
	}
}

func TestPreflightIdentityCrossCheckFailureRefuses(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")
	certificate, _ := testCertAndKey(t, []string{"oberth"})
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server":              {out: baoIdentityJSON(1, map[string]string{"tls.crt": string(certificate), "tls.key": "private", "ssh_host_key": "private"})},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-upstream-key": {out: baoIdentityJSON(1, map[string]string{"id_ed25519": "private", "id_ed25519.pub": "public"})},
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/oberth-known-hosts":  {out: baoIdentityJSON(1, map[string]string{"known_hosts": "public"})},
	}}
	pr := &preflightRunner{t: t, baoRunner: runner, podCertFail: true}
	var output bytes.Buffer
	deps := testAdminDeps(Deps{KubeClient: fake.NewClientset(legacyDeployment(), runningOpenBaoPod(), runningOpenBaoStatefulSet()), RunCommand: pr.run, Output: &output})
	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(t.Context(), &cfg, deps, false)
	if err == nil || !strings.Contains(err.Error(), "cannot cross-check") || strings.Contains(output.String(), "cross-check skipped") {
		t.Fatalf("identity failure did not refuse upgrade: %v output=%s", err, &output)
	}
}

func TestAbsentBaoCertificateStillChecksExistingRuntimeSANs(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")
	certificate, _ := testCertAndKey(t, []string{"oberth"})
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {out: "No value found", err: errors.New("exit 2")},
	}}
	deps := testAdminDeps(Deps{
		KubeClient: fake.NewClientset(currentDeployment(), runningOpenBaoPod(), runningOpenBaoStatefulSet()),
		Output:     &bytes.Buffer{},
		RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			if slices.Contains(args, "deploy/oberth") {
				return certificate, nil
			}
			return runner.run(ctx, input, name, args...)
		},
		RunHelm: func(context.Context, []string) ([]byte, error) {
			return []byte(`{}`), nil
		},
	})
	cfg := Config{InstallSecretStore: true, TLSExtraDNSNames: []string{"server.example.test"}}
	err := preflightServerIdentities(t.Context(), &cfg, deps, false)
	if err == nil || !strings.Contains(err.Error(), "--reissue-server-ca") {
		t.Fatalf("missing OpenBao certificate was treated as fresh install: %v", err)
	}
}

func TestRuntimeCertificateDiagnosticsCannotEnableExecFallback(t *testing.T) {
	t.Parallel()
	for _, message := range []string{"permission denied", `untrusted stdout: unknown command "admin"`} {
		calls := 0
		denied := errors.New(message)
		deps := Deps{KubeClient: fake.NewClientset(currentDeployment()),
			Admin: idleAdminFunc(func(context.Context, string, any, any) error { return denied }),
			RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
				calls++
				return []byte("unexpected fallback"), nil
			},
		}
		_, err := serverCACertificate(t.Context(), Config{}, deps)
		if !errors.Is(err, denied) || calls != 0 {
			t.Fatalf("diagnostic enabled fallback: %v calls=%d", err, calls)
		}
	}
}

func TestStoppedRuntimeIdentityNamesRecoveryStep(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-root-token")
	runner := &fakeBaoRunner{t: t, responses: map[string]fakeBaoResponse{
		"read -format=json " + defaultKVPrefix + "/data/identities/oberth/server": {out: "No value found", err: errors.New("exit 2")},
	}}
	deps := testAdminDeps(Deps{
		KubeClient: fake.NewClientset(currentDeployment(), runningOpenBaoPod(), runningOpenBaoStatefulSet()),
		Output:     &bytes.Buffer{},
		RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			if slices.Contains(args, "deploy/oberth") {
				return nil, errors.New("no running server pod")
			}
			return runner.run(ctx, input, name, args...)
		},
		RunHelm: func(context.Context, []string) ([]byte, error) {
			t.Fatal("unverifiable identity reached Helm")
			return nil, nil
		},
	})
	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(t.Context(), &cfg, deps, false)
	if err == nil {
		t.Fatal("missing runtime identity accepted")
	}
	for _, text := range []string{"refusing upgrade", "restore the original server identity bundle", "trusted identity backup", "oberth/identities/oberth/server", "then retry the upgrade", "docs/identity-migration.md"} {
		if !strings.Contains(err.Error(), text) {
			t.Errorf("recovery refusal missing %q: %v", text, err)
		}
	}
}
