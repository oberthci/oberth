package installer

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReleasedServerCertificateUsesSecretWithoutAdmin(t *testing.T) {
	t.Parallel()
	certificate, _ := testCertAndKey(t, []string{"oberth"})
	calls := 0
	deps := Deps{
		KubeClient: fake.NewClientset(legacyDeployment(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oberth-tls", Namespace: "oberth"}, Data: map[string][]byte{"tls.crt": certificate}}),
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			calls++
			return nil, errors.New(`unknown command "admin"`)
		},
	}
	for _, ca := range []bool{false, true} {
		got, err := readServerPublicCertificate(t.Context(), Config{}, deps, ca)
		if err != nil || !bytes.Equal(got, certificate) || calls != 0 {
			t.Fatalf("released server certificate read: %v, exec calls=%d", err, calls)
		}
	}
}

func TestOldRuntimeCertificateFallbackIsVersionGated(t *testing.T) {
	t.Parallel()
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "permission failure", true: "pre-scratch"}[old], func(t *testing.T) {
			calls := 0
			deps := Deps{
				KubeClient: fake.NewClientset(currentDeployment(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oberth-tls", Namespace: "oberth"}, Data: map[string][]byte{"tls.crt": []byte("stale legacy certificate")}}),
				RunCommand: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
					if slices.Contains(args, "admin") {
						if old {
							return nil, errors.New(`unknown command "admin"`)
						}
						return nil, errors.New("permission denied")
					}
					if !slices.Contains(args, "cat") {
						t.Fatalf("unexpected command %v", args)
					}
					calls++
					if args[len(args)-1] == "/run/oberth-identities/server/tls.crt" {
						return []byte("runtime certificate"), nil
					}
					return nil, errors.New("file not found")
				},
			}
			got, err := serverCACertificate(t.Context(), Config{}, deps)
			if old {
				if err != nil || string(got) != "runtime certificate" || calls != 2 {
					t.Fatalf("legacy fallback: %q %v calls=%d", got, err, calls)
				}
			} else if err == nil || calls != 0 {
				t.Fatalf("non-version failure fell back: %q %v calls=%d", got, err, calls)
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
	deps := Deps{KubeClient: fake.NewClientset(legacyDeployment(), runningOpenBaoPod()), RunCommand: pr.run, Output: &output}
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
	deps := Deps{
		KubeClient: fake.NewClientset(currentDeployment(), runningOpenBaoPod()),
		Output:     &bytes.Buffer{},
		RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
			if slices.Contains(args, "deploy/oberth") {
				return certificate, nil
			}
			return runner.run(ctx, input, name, args...)
		},
		RunHelm: func(context.Context, []string) ([]byte, error) {
			return []byte(`{"argo":{"goProxy":{"enabled":true}}}`), nil
		},
	}
	cfg := Config{InstallSecretStore: true}
	err := preflightServerIdentities(t.Context(), &cfg, deps, false)
	if err == nil || !strings.Contains(err.Error(), "does not cover the goproxy service names") {
		t.Fatalf("missing OpenBao certificate was treated as fresh install: %v", err)
	}
}
