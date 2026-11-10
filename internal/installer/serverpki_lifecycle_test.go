package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestServerPKIRemovalRefusesServedSANsBeforeAnyWrite(t *testing.T) {
	leaf := secretWithCertificate(t, []string{"ci.example.com"}, []string{"192.0.2.1"}).Data["tls.crt"]
	for _, cfg := range []Config{
		{TLSRemoveDNSNames: []string{"ci.example.com"}},
		{TLSRemoveIPs: []string{"192.0.2.1"}},
	} {
		store := openBaoExec{run: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
			if !slices.Contains(args, "deploy/oberth") {
				t.Fatalf("OpenBao touched before removal refusal: %v", args)
			}
			return leaf, nil
		}}
		_, _, err := ensureServerPKI(t.Context(), cfg, store, "test-token", nil)
		if err == nil || !strings.Contains(err.Error(), "currently served") || !strings.Contains(err.Error(), "--tls-extra-") {
			t.Fatalf("served SAN removal: %v", err)
		}
	}
}

func pkiLifecycleDeps(t *testing.T, dns, ips []string) (Deps, *fakeBaoRunner) {
	t.Helper()
	base, _ := serverPKINames(Config{})
	role := serverPKIRole(append(base, dns...), nil)
	for _, ip := range ips {
		role["allowed_ip_sans_cidr"] = append(anyStrings(role["allowed_ip_sans_cidr"]), ip+"/32")
	}
	role["allow_ip_sans"] = len(ips) > 0
	role["ttl"], role["max_ttl"] = 2592000, 2592000
	roleJSON, _ := json.Marshal(map[string]any{"data": role})
	issuerJSON, _ := json.Marshal(map[string]any{"data": map[string]any{"certificate": string(newTestServerCA(append(base, dns...)))}})
	responses := freshStoreResponses()
	responses["read -format=json oberth-pki/roles/oberth-server"] = fakeBaoResponse{out: string(roleJSON)}
	responses["read -format=json oberth-pki/issuer/default"] = fakeBaoResponse{out: string(issuerJSON)}
	runner := &fakeBaoRunner{t: t, responses: responses}
	deps := secretStoreTestDeps(runner, io.Discard, currentDeployment(), runningOpenBaoPod())
	leaf := secretWithCertificate(t, append(base, "ci.example.com"), []string{"192.0.2.1"}).Data["tls.crt"]
	deps.RunCommand = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "deploy/oberth") {
			return leaf, nil
		}
		return runner.run(ctx, stdin, name, args...)
	}
	return deps, runner
}

func TestPKIPreflightChecksDeployedAndServedNames(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-token")
	for _, permitsNames := range []bool{false, true} {
		var dns, ips []string
		if permitsNames {
			dns, ips = []string{"ci.example.com"}, []string{"192.0.2.1"}
		}
		deps, runner := pkiLifecycleDeps(t, dns, ips)
		deps.RunHelm = func(context.Context, []string) ([]byte, error) {
			return []byte(`{"tls":{"pki":{"enabled":true},"extraDNSNames":["ci.example.com"],"extraIPs":["192.0.2.1"]}}`), nil
		}
		err := PreflightPKI(t.Context(), Config{}, deps)
		if permitsNames {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "--tls-extra-dns-name ci.example.com") || !strings.Contains(err.Error(), "--tls-extra-ip 192.0.2.1") {
			t.Fatalf("missing SAN permissions not explained: %v", err)
		}
		for _, call := range runner.calls {
			if !strings.HasPrefix(call.command, "read ") {
				t.Fatalf("preflight wrote to OpenBao: %s", call.command)
			}
		}
	}
}

func TestServerPKIRemovesOnlyUnservedNames(t *testing.T) {
	deps, runner := pkiLifecycleDeps(t, []string{"ci.example.com", "retired.example.com"}, []string{"192.0.2.1", "192.0.2.2"})
	cfg := Config{TLSRemoveDNSNames: []string{"retired.example.com"}, TLSRemoveIPs: []string{"192.0.2.2"}}
	_, replaced, err := ensureServerPKI(t.Context(), cfg, newOpenBaoExec(deps, DefaultOpenBaoNamespace, "openbao-0"), "test-token", map[string]baoMount{"oberth-pki/": {Type: "pki"}})
	if err != nil || replaced {
		t.Fatalf("remove unserved names: replaced=%v err=%v", replaced, err)
	}
	write := runner.callsByCommand()["write oberth-pki/roles/oberth-server -"].stdin
	if strings.Contains(write, "retired.example.com") || strings.Contains(write, "192.0.2.2/32") || !strings.Contains(write, "ci.example.com") || !strings.Contains(write, "192.0.2.1/32") {
		t.Fatalf("role removal did not preserve the other names: %s", write)
	}
}

func TestPKIPreflightWarnsWhenServedCertificateIsUnreadable(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "test-token")
	for _, invalidPEM := range []bool{false, true} {
		for _, permitsNames := range []bool{false, true} {
			for _, repair := range []bool{false, true} {
				t.Run(fmt.Sprintf("invalid-pem=%t/permits-names=%t/repair=%t", invalidPEM, permitsNames, repair), func(t *testing.T) {
					var dns, ips []string
					if permitsNames {
						dns, ips = []string{"ci.example.com"}, []string{"192.0.2.1"}
					}
					deps, runner := pkiLifecycleDeps(t, dns, ips)
					var output bytes.Buffer
					deps.Output = &output
					deps.RunCommand = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
						if slices.Contains(args, "deploy/oberth") {
							if invalidPEM {
								return []byte("unreadable certificate"), nil
							}
							return nil, errors.New("server is crash looping")
						}
						return runner.run(ctx, stdin, name, args...)
					}
					cfg := Config{Namespace: DefaultNamespace, serverPKI: true, InstallSecretStore: repair,
						TLSExtraDNSNames: []string{"ci.example.com"}, TLSExtraIPs: []string{"192.0.2.1"}}
					err := preflightServerIdentities(t.Context(), &cfg, deps, true)
					if permitsNames || repair {
						if err != nil {
							t.Fatalf("unreadable leaf blocked preflight/repair: %v", err)
						}
					} else if err == nil || !strings.Contains(err.Error(), "missing names") || !strings.Contains(err.Error(), "ci.example.com") || !strings.Contains(err.Error(), "192.0.2.1") {
						t.Fatalf("role/expected-name mismatch must still fail: %v", err)
					}
					if !strings.Contains(output.String(), "WARNING:") || !strings.Contains(output.String(), "expected TLS names only") {
						t.Fatalf("missing unreadable leaf warning: %s", output.String())
					}
					for _, call := range runner.calls {
						if !strings.HasPrefix(call.command, "read ") {
							t.Fatalf("preflight wrote to OpenBao: %s", call.command)
						}
					}
				})
			}
		}
	}
}

func TestServerPKIRemovalFailsClosedWhenServedCertificateIsUnreadable(t *testing.T) {
	for _, invalidPEM := range []bool{false, true} {
		for _, cfg := range []Config{
			{TLSRemoveDNSNames: []string{"retired.example.com"}},
			{TLSRemoveIPs: []string{"192.0.2.2"}},
		} {
			store := openBaoExec{run: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
				if !slices.Contains(args, "deploy/oberth") {
					t.Fatalf("OpenBao touched before removal refusal: %v", args)
				}
				if invalidPEM {
					return []byte("unreadable certificate"), nil
				}
				return nil, errors.New("server is crash looping")
			}}
			if _, _, err := ensureServerPKI(t.Context(), cfg, store, "test-token", nil); err == nil {
				t.Fatal("removal accepted without checking the served certificate")
			}
		}
	}
}

func TestReinstallPreservesRuntimeTLSNames(t *testing.T) {
	deps := Deps{KubeClient: fake.NewClientset(currentDeployment()), RunHelm: func(context.Context, []string) ([]byte, error) {
		return []byte(`{"tls":{"pki":{"enabled":true},"extraDNSNames":["ci.example.com"],"extraIPs":["192.0.2.1"]}}`), nil
	}}
	cfg := Config{Namespace: DefaultNamespace, TLSExtraDNSNames: []string{"new.example.com"}}
	if err := preserveServerTLSNames(t.Context(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), " ")
	for _, name := range []string{"ci.example.com", "new.example.com", "192.0.2.1"} {
		if !strings.Contains(args, name) {
			t.Fatalf("runtime TLS configuration dropped %s: %s", name, args)
		}
	}
}

func TestUpgradePKIRejectsTLSOverrideBypasses(t *testing.T) {
	values := map[string]any{"tls": map[string]any{"pki": map[string]any{"enabled": true}}}
	for _, override := range []string{"tls.pki.enabled=false", "tls.pki.mount=other", "tls.extraDNSNames=other", "tls.extraIPs=192.0.2.2"} {
		if err := checkUpgradePKIKeyConflicts(UpgradeConfig{UserSet: []string{override}}, values); err == nil {
			t.Fatalf("TLS name policy bypass accepted: %s", override)
		}
	}
}

func TestUpgradeReconcilesPKIBeforeHelmAndDryRunNeverWrites(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprint(dryRun), func(t *testing.T) {
			t.Setenv(baoTokenEnvVar, "test-token")
			deps, runner := pkiLifecycleDeps(t, []string{"ci.example.com"}, []string{"192.0.2.1"})
			deps.RestConfig = &rest.Config{Host: "https://127.0.0.1:6443"}
			stop := errors.New("stop after Helm apply")
			rendered, applied := false, false
			previews := 0
			deps.RunHelm = func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.17.80"}]`), nil
				case "get":
					if slices.Contains(args, "--all") {
						out, _ := testUpgradeValuePreflight(args)
						return out, nil
					}
					return []byte(`{"tls":{"pki":{"enabled":true},"extraDNSNames":["ci.example.com"],"extraIPs":["192.0.2.1"]}}`), nil
				case "show":
					return []byte("image:\n  ref: example.invalid/oberth@sha256:" + strings.Repeat("1", 64) + "\n"), nil
				case "template":
					for _, call := range runner.calls {
						if !strings.HasPrefix(call.command, "read ") {
							t.Fatalf("PKI changed before chart validation: %s", call.command)
						}
					}
					rendered = true
					return nil, nil
				case "upgrade":
					if slices.Contains(args, "--dry-run=server") {
						previews++
						if !rendered {
							t.Fatal("preview preceded chart validation")
						}
						for _, call := range runner.calls {
							if !strings.HasPrefix(call.command, "read ") {
								t.Fatalf("PKI changed before preview: %s", call.command)
							}
						}
						return []byte(`{"config":{}}`), nil
					}
					applied = true
					if !rendered || runner.callsByCommand()["write oberth-pki/roles/oberth-server -"].stdin == "" {
						t.Fatal("Helm applied before PKI reconciliation")
					}
					for _, name := range []string{"ci.example.com", "new.example.com", "192.0.2.1"} {
						if !strings.Contains(strings.Join(args, " "), name) {
							t.Fatalf("upgrade dropped %s", name)
						}
					}
					if strings.Contains(strings.Join(args, " "), "watchTunnel") {
						t.Fatal("PKI reconciliation reintroduced the retired connector")
					}
					return nil, stop
				default:
					if out, ok := testUpgradeValuePreflight(args); ok {
						return out, nil
					}
					return nil, fmt.Errorf("unexpected helm call: %v", args)
				}
			}
			result, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.17.80", ChartOverride: "fixture", DryRun: dryRun, NoBackup: true, TLSExtraDNSNames: []string{"new.example.com"}}, deps)
			if result.AlreadyUpToDate || (!dryRun && (!errors.Is(err, stop) || !applied)) || (dryRun && (err != nil || applied)) {
				t.Fatalf("upgrade result=%+v applied=%v err=%v", result, applied, err)
			}
			if !dryRun && previews != 1 {
				t.Fatalf("previews = %d, want 1", previews)
			}
			if dryRun {
				for _, call := range runner.calls {
					if !strings.HasPrefix(call.command, "read ") {
						t.Fatalf("dry-run wrote to PKI: %s", call.command)
					}
				}
			}
		})
	}
}

func TestServerPKIAccessUsesCapturedInstallerToken(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "fixture-invocation-token")
	deps, _ := pkiLifecycleDeps(t, []string{"ci.example.com"}, nil)
	if err := captureBaoEnvironment(&deps); err != nil {
		t.Fatal(err)
	}
	if os.Getenv(baoTokenEnvVar) != "" {
		t.Fatal("token remained in child environment")
	}
	_, token, err := serverPKIAccess(t.Context(), Config{}, deps)
	if err != nil || token != "fixture-invocation-token" {
		t.Fatalf("PKI lost invocation-local credential: %v", err)
	}
}
