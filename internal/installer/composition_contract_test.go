package installer_test

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"golang.org/x/crypto/ssh"
	"helm.sh/helm/v3/pkg/chart/loader"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func compositionRenderError(t *testing.T, values map[string]any) error {
	t.Helper()
	chart, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = installer.Render(chart, values, "oberth")
	return err
}

func TestCompositionRequiresTypedFields(t *testing.T) {
	t.Parallel()
	for _, values := range []map[string]any{
		{"runs": map[string]any{"parallel": nil}},
		{"logs": map[string]any{"run_limit_bytes": nil}},
		{"schedules": map[string]any{"max_entries": nil}},
		{"assist": map[string]any{"enabled": false}},
		{"assist": map[string]any{"ttl_seconds": nil}},
		{"server": map[string]any{"runtime": nil}},
		{"server": map[string]any{"runtime": "not an object"}},
		{"server": map[string]any{"runtime": []any{}}},
		{"server": map[string]any{"runtime": map[string]any{"git_binary": nil}}},
		{"server": map[string]any{"runtime": map[string]any{"source_client_burst": 2}}},
		{"secretStore": map[string]any{"audience": nil}},
		{"secretStore": map[string]any{"audience": " trailing "}},
		{"server": map[string]any{"witness": map[string]any{}}},
		{"witness": map[string]any{"endpoint": "https://witness.oberth.ci"}},
	} {
		if err := compositionRenderError(t, values); err == nil {
			t.Fatal("configuration refused by the server was rendered", values)
		}
	}
	config := serverJSON(t, rendered(t, map[string]any{"runs": map[string]any{"parallel": 0}, "schedules": map[string]any{"max_entries": 0}, "assist": map[string]any{"ttl_seconds": 1}}))
	if jsonOf(t, config["runs"]) != `{"parallel":0}` || jsonOf(t, config["schedules"]) != `{"max_entries":0}` || jsonOf(t, config["assist"]) != `{"ttl_seconds":1}` {
		t.Fatal("required zero omitted", config)
	}
}

func TestCompositionHTTPSValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://oberth.ci:8443", true}, {"https://oberth.ci/base", true}, {"https://[::1]:8443", true},
		{"http://oberth.ci:8200", false}, {"https://user@oberth.ci", false}, {"https://@oberth.ci", false},
		{"https://user:password@oberth.ci", false}, {"https://oberth.ci?q=1", false}, {"https://oberth.ci#part", false},
		{"https://:8200", false}, {"https://[]:8200", false}, {"https:opaque", false}, {"https://oberth.ci:bad", false},
	} {
		for _, values := range []map[string]any{{"server": map[string]any{"public": map[string]any{"url": tc.url}}}, {"secretStore": map[string]any{"address": tc.url}}} {
			if err := compositionRenderError(t, values); (err == nil) != tc.ok {
				t.Fatal(tc, err)
			}
		}
	}
}

func TestCompositionDatabaseStaysUnderDataRoot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file string
		ok   bool
	}{
		{"/var/lib/oberth/oberth.db", true}, {"/var/lib/oberth/data/../restored.db", true},
		{"/var/lib/oberth", false}, {"/var/lib/oberth/", false}, {"var/lib/oberth/oberth.db", false},
		{"/var/lib/oberth/../other.db", false}, {"/var/lib/oberth/data/../../other.db", false}, {"/var/lib/oberth/..", false},
	} {
		if err := compositionRenderError(t, map[string]any{"database": map[string]any{"file": tc.file}}); (err == nil) != tc.ok {
			t.Fatal(tc, err)
		}
	}
}

func TestCompositionSSHPortBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		port int
		ok   bool
	}{{0, false}, {1, true}, {65535, true}, {65536, false}} {
		if err := compositionRenderError(t, map[string]any{"server": map[string]any{"public": map[string]any{"ssh_port": tc.port}}}); (err == nil) != tc.ok {
			t.Fatal(tc, err)
		}
	}
}

func TestCompositionConfigFileByteLimit(t *testing.T) {
	t.Parallel()
	base := object(t, rendered(t, nil), "ConfigMap", "oberth-server")["data"].(map[string]any)["server.json"].(string)
	for _, unit := range []string{"x", "é"} {
		for _, over := range []int{0, 1} {
			padding := 1<<20 - len(base) + over
			value := "/usr/bin/git" + strings.Repeat(unit, padding/len(unit)) + strings.Repeat("x", padding%len(unit))
			values := map[string]any{"server": map[string]any{"runtime": map[string]any{"git_binary": value}}}
			if over != 0 {
				if err := compositionRenderError(t, values); err == nil || !strings.Contains(err.Error(), "1 MiB") {
					t.Fatal("oversized config not refused", err)
				}
				continue
			}
			objects := rendered(t, values)
			file := object(t, objects, "ConfigMap", "oberth-server")["data"].(map[string]any)["server.json"].(string)
			if len(file) != 1<<20 || !json.Valid([]byte(file)) {
				t.Fatal("rendered config bytes differ from the checked bound", len(file))
			}
		}
	}
}

func TestCompositionProbesStartImmediately(t *testing.T) {
	t.Parallel()
	pod := serverDeployment(t, rendered(t, nil)).Spec.Template.Spec
	server := pod.Containers[0]
	if server.StartupProbe != nil {
		t.Fatal("startup probe suppresses early liveness")
	}
	if server.LivenessProbe == nil || server.LivenessProbe.InitialDelaySeconds != 0 {
		t.Fatal("liveness does not start at bind")
	}
	if server.ReadinessProbe == nil || server.ReadinessProbe.InitialDelaySeconds != 0 {
		t.Fatal("readiness must observe startup 503 responses")
	}
	if *pod.TerminationGracePeriodSeconds != 30 {
		t.Fatal("shutdown grace must remain 30 seconds")
	}
}

func TestCompositionCARolloutUsesMountedTrust(t *testing.T) {
	t.Parallel()
	f := setup(t)
	prepare(t, f)
	values := f.charts.Previews[0].Values
	source := values["server"].(map[string]any)["runtime"].(map[string]any)["source"].(map[string]any)
	ca := source["ca_pem"].(string)
	if ca == "" {
		t.Fatal("installer did not supply mounted public CA")
	}
	before := serverDeployment(t, rendered(t, values)).Spec.Template.Annotations
	sum := sha256.Sum256([]byte(ca))
	if before["checksum/ca"] != hex.EncodeToString(sum[:]) {
		t.Fatal("CA checksum does not cover the observed public certificate")
	}
	source["ca_pem"] = ca + "\n"
	after := serverDeployment(t, rendered(t, values)).Spec.Template.Annotations
	if before["checksum/ca"] == after["checksum/ca"] {
		t.Fatal("CA Secret content change does not roll the Pod")
	}
}

func TestCompositionSourceUsesServedLeafName(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Namespace, f.choices.PipelineNamespace = "frontdoor", "jobs"
	f.choices.URL = "https://public.oberth.ci"
	prepare(t, f)
	objects := renderedIn(t, f.charts.Previews[0].Values, "frontdoor")
	object(t, objects, "Service", "oberth")
	cm := object(t, objects, "ConfigMap", "oberth-source")["data"].(map[string]any)
	var source runtimeconfig.SourcePluginConfig
	if err := json.Unmarshal([]byte(cm["source.json"].(string)), &source); err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{DNSNames: []string{"public.oberth.ci", "oberth", "oberth.frontdoor.svc"}}
	if err := leaf.VerifyHostname(source.ServerName); err != nil {
		t.Fatal("source plugin verifies a name the server does not serve", err)
	}
	if source.Endpoint != "https://oberth-source.frontdoor.svc:8445" || source.ServerName != "oberth.frontdoor.svc" {
		t.Fatal("source routing or TLS service name differs", source)
	}
}

func TestCompositionAdminMountIsOnePublicKeyPerFile(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	secret, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"admin.pub", "push.pub"} {
		data := secret.Data[name]
		key, _, options, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil || len(options) != 0 || len(rest) != 0 {
			t.Fatal("admin mount is not one unadorned authorized-key line", name, err)
		}
		if string(data) != string(ssh.MarshalAuthorizedKey(key)) {
			t.Fatal("admin mount has noncanonical or trailing key data", name)
		}
	}
}

func TestCompositionRequiresCACertificateSigningUsage(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	secret, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), "oberth-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(secret.Data["tls.crt"])
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(secret.Data["tls.key"])
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	private := key.(*ecdsa.PrivateKey)
	cert.KeyUsage = 0
	der, err := x509.CreateCertificate(f.engine.Random, cert, cert, private.Public(), private)
	if err != nil {
		t.Fatal(err)
	}
	secret.Data["tls.crt"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if _, err = f.client.CoreV1().Secrets("oberth").Update(t.Context(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if plan, err := f.engine.Prepare(t.Context(), f.release, f.choices); err == nil {
		plan.Close()
		t.Fatal("CA without certificate-signing usage accepted")
	}
}
