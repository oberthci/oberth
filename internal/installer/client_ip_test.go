package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/invite"
)

func TestClientPortProbePreservesIPChoice(t *testing.T) {
	for _, host := range []string{"192.0.2.10", "2001:db8::10", "127.0.0.1", "::1"} {
		for _, endpoint := range []string{"", host} {
			var addresses []string
			result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: host}, ClientPortProbeDeps{
				EndpointAddress: endpoint,
				LookupHost:      func(context.Context, string) ([]string, error) { t.Fatal("IP choice triggered DNS"); return nil, nil },
				Dial:            func(_ context.Context, address string) error { addresses = append(addresses, address); return nil },
			})
			if err != nil || result.Host != host || result.DNS != nil || result.NeedsAction || !slices.Equal(result.InstallFlags[:2], []string{"--client-host", host}) {
				t.Fatalf("IP choice changed: %+v %v", result, err)
			}
			if !slices.Equal(addresses, []string{net.JoinHostPort(host, "30443"), net.JoinHostPort(host, "30022")}) {
				t.Fatal(addresses)
			}
		}
	}
	result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: "192.0.2.10"}, ClientPortProbeDeps{})
	if err != nil || !strings.Contains(result.Advice, "NAT-isolated") || !strings.Contains(result.Advice, "routable from the client") || !strings.Contains(result.Advice, "port-forward") {
		t.Fatal(result, err)
	}
}

// Issue the production leaf name set with a fixture CA whose DNS constraints
// are unchanged. This exercises SANs independently of CA-level IP constraints.
func clientIPLeaf(t *testing.T, authority testAuthority, cfg Config) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	dns, ips := serverPKINames(cfg)
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: dns, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, ip := range ips {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP(ip))
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapPrivateKey(t, key)
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, &key.PublicKey, authority.key)
	if err != nil {
		t.Fatal(err)
	}
	return der, key
}

func clientIPServer(t *testing.T, host string, authority testAuthority, cfg Config) (string, *atomic.Int32) {
	t.Helper()
	leaf, key := clientIPLeaf(t, authority, cfg)
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{leaf, authority.certificate.Raw}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL + "/mcp", calls
}

func TestIPClientSANPinTrustAndSettings(t *testing.T) {
	requireOpenSSL(t)
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			cfg := Config{ClientHost: host}
			review := Review(cfg)
			if !slices.Contains(review.IPs, host) {
				t.Fatal("Review omitted chosen IP SAN")
			}

			var summary bytes.Buffer
			printReadyWithNextSteps(&summary, cfg, Deps{ClientHost: host}, nil, false, false)
			if !strings.Contains(summary.String(), "Client address: "+host+" (covered by server certificate IP SAN)") || !strings.Contains(summary.String(), "https://"+URLHost(host)+":30443") {
				t.Fatal("summary lost chosen IP or coverage", summary.String())
			}
			cfg.TLSExtraDNSNames, cfg.TLSExtraIPs = review.DNSNames, review.IPs
			dns, _ := serverPKINames(cfg)
			authority := newTestAuthorityFor(t, "IP client CA", dns...)
			address, calls := clientIPServer(t, host, authority, cfg)
			pin, err := CAPin(string(authority.pem))
			if err != nil {
				t.Fatal(err)
			}
			pinned, err := invite.PinnedClient(address, pin)
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.CloseIdleConnections()
			response, err := pinned.Post(address, "application/json", strings.NewReader(`{"request":"fixture"}`))
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if calls.Load() != 1 {
				t.Fatal("pinned connection never reached the IP server")
			}
			caFile := filepath.Join(t.TempDir(), "ca.crt")
			if err := os.WriteFile(caFile, authority.pem, 0600); err != nil {
				t.Fatal(err)
			}
			rest, err := client.New(t.Context(), client.Config{BaseURL: strings.TrimSuffix(address, "/mcp"), CACert: caFile, Token: testToken})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rest.GetRaw(t.Context(), "/mcp", nil); err != nil {
				t.Fatal(err)
			}
			for _, shell := range trustShells() {
				t.Run(shell, func(t *testing.T) { checkIPTrustSettings(t, shell, address, pin, authority.pem) })
			}
		})
	}
}

func checkIPTrustSettings(t *testing.T, shell, address, pin string, ca []byte) {
	t.Helper()
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	original := []byte(`{"model":"chosen","env":{"KEEP":"value"}}`)
	if err := os.WriteFile(settings, original, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runTrust(t, shell, dir, nil, "connect", "--url", address, "--pin", pin)
	if err != nil {
		t.Fatalf("IP trust refused: %v %s", err, out)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dir, "oberth", "ca.crt")), ca) {
		t.Fatal("wrong CA saved")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	configPath := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(configPath, []byte(`{"keep":"value","mcpServers":{"other":{"url":"https://other.example.test/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = configureClaudeMCP(t.Context(), ClaudeCodeSetup{URL: address, Token: func(context.Context) (string, error) { return testToken, nil }})
	if err != nil {
		t.Fatal(err)
	}
	var configured struct {
		Keep       string
		MCPServers map[string]struct{ URL string }
	}
	if err := json.Unmarshal(mustRead(t, configPath), &configured); err != nil {
		t.Fatal(err)
	}
	if configured.Keep != "value" || configured.MCPServers["other"].URL != "https://other.example.test/mcp" || configured.MCPServers["oberth"].URL != address {
		t.Fatal("MCP settings were not merged with the exact IP URL")
	}

	tools := t.TempDir()
	stub := "#!/bin/sh\nset -eu\nif [ \"$2\" = add-json ]; then printf '%s\\n' \"$6\" >\"$IP_CLIENT_CONFIG\"; fi\n"
	if err := os.WriteFile(filepath.Join(tools, "claude"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	captured := filepath.Join(dir, "captured-mcp.json")
	environment := []string{"IP_CLIENT_CONFIG=" + captured}
	for _, variable := range os.Environ() {
		if value, ok := strings.CutPrefix(variable, "PATH="); ok {
			environment = append(environment, "PATH="+tools+":"+value)
		}
	}
	if out, err := runTrust(t, shell, dir, environment, "configure", address); err != nil {
		t.Fatalf("IP configure: %v %s", err, out)
	}
	var entry struct{ URL string }
	if err := json.Unmarshal(mustRead(t, captured), &entry); err != nil || entry.URL != address {
		t.Fatal("script changed IP URL", err)
	}
	if strings.TrimSpace(string(mustRead(t, filepath.Join(dir, "oberth", "url")))) != address {
		t.Fatal("script saved a different endpoint")
	}
	var saved struct {
		Model string
		Env   map[string]string
	}
	if err := json.Unmarshal(mustRead(t, settings), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Model != "chosen" || saved.Env["KEEP"] != "value" || saved.Env["NODE_EXTRA_CA_CERTS"] != filepath.Join(dir, "oberth", "ca.crt") {
		t.Fatal("trust settings lost existing values")
	}
}

func TestIPClientRefusesMissingSANAndWrongPinBeforeHTTP(t *testing.T) {
	requireOpenSSL(t)
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			dns, _ := serverPKINames(Config{})
			authority := newTestAuthorityFor(t, "IP mismatch CA", dns...)
			// The CA is correct, but the leaf carries only DNS names.
			address, calls := clientIPServer(t, host, authority, Config{})
			for _, tc := range []struct{ name, pin, reason string }{{"missing IP SAN", testPin(t, authority.pem), "IP SAN"}, {"wrong pin", strings.Repeat("0", 64), "pinned"}} {
				t.Run(tc.name, func(t *testing.T) {
					pinned, err := invite.PinnedClient(address, tc.pin)
					if err != nil {
						t.Fatal(err)
					}
					defer pinned.CloseIdleConnections()
					response, err := pinned.Post(address, "application/json", strings.NewReader("must not arrive"))
					if err == nil {
						_ = response.Body.Close()
						t.Fatal("unsafe pinned connection accepted")
					}
					if tc.name == "missing IP SAN" && !strings.Contains(err.Error(), "IP SAN") {
						t.Fatal(err)
					}
					for _, shell := range trustShells() {
						dir := t.TempDir()
						settings := filepath.Join(dir, "settings.json")
						original := []byte(`{"env":{"KEEP":"value"}}`)
						if err := os.WriteFile(settings, original, 0600); err != nil {
							t.Fatal(err)
						}
						out, err := runTrust(t, shell, dir, nil, "connect", "--url", address, "--pin", tc.pin)
						if err == nil || !strings.Contains(out, tc.reason) || !strings.Contains(out, "Nothing was changed") {
							t.Fatalf("unclear refusal: %v %s", err, out)
						}
						if !bytes.Equal(mustRead(t, settings), original) {
							t.Fatal("refusal changed settings")
						}
						if _, err := os.Stat(filepath.Join(dir, "oberth", "ca.crt")); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("refusal stored a CA")
						}
					}
				})
			}

			caFile := filepath.Join(t.TempDir(), "ca.crt")
			if err := os.WriteFile(caFile, authority.pem, 0600); err != nil {
				t.Fatal(err)
			}
			rest, err := client.New(t.Context(), client.Config{BaseURL: strings.TrimSuffix(address, "/mcp"), CACert: caFile, Token: testToken})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rest.GetRaw(t.Context(), "/mcp", nil); err == nil || !strings.Contains(err.Error(), "does not cover") || !strings.Contains(err.Error(), host) {
				t.Fatal("REST client accepted missing IP SAN or unclear refusal", err)
			}
			if calls.Load() != 0 {
				t.Fatal("HTTP body sent before identity and pin verification")
			}
		})
	}
}

func TestPluginConnectRefusesDNSIdentityMismatch(t *testing.T) {
	requireOpenSSL(t)
	dns, _ := serverPKINames(Config{})
	authority := newTestAuthorityFor(t, "DNS mismatch CA", dns...)
	leaf, key := clientIPLeaf(t, authority, Config{})
	address := servePresenting(t, key, leaf, authority.certificate.Raw)
	for _, shell := range trustShells() {
		dir := t.TempDir()
		out, err := runTrust(t, shell, dir, nil, "connect", "--url", address, "--pin", testPin(t, authority.pem))
		if err == nil || !strings.Contains(out, "DNS identity") || !strings.Contains(out, "Nothing was changed") {
			t.Fatal("DNS identity mismatch accepted or unclear", out, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "oberth", "ca.crt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("mismatch saved trust")
		}
	}
}

func TestPluginConnectRefusesUnsupportedOpenSSLBeforeSavingTrust(t *testing.T) {
	for _, path := range []string{"/opt/homebrew/opt/openssl@3/bin/openssl", "/usr/local/opt/openssl@3/bin/openssl"} {
		if _, err := os.Stat(path); err == nil {
			t.Skip("installed fallback exercised by native IP connection tests")
		}
	}
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "openssl"), []byte("#!/bin/sh\necho unsupported verification option >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, shell := range trustShells() {
		dir := t.TempDir()
		out, err := runTrust(t, shell, dir, []string{"PATH=" + tools}, "connect", "--url", "https://192.0.2.10/mcp", "--pin", strings.Repeat("0", 64))
		if err == nil || !strings.Contains(out, "requires OpenSSL with verify_ip and verify_hostname") || !strings.Contains(out, "brew install openssl@3") || !strings.Contains(out, "Nothing was changed") {
			t.Fatal("missing identity verifier did not stop clearly", out, err)
		}
		if !emptyDirectory(t, dir) {
			t.Fatal("unsupported tool wrote trust settings")
		}
	}
}

func TestIPClientPortMappingRespectsAddressFamily(t *testing.T) {
	for _, tc := range []struct {
		host, bind string
		published  bool
	}{
		{"127.0.0.1", "0.0.0.0", true}, {"::1", "::", true},
		{"127.0.0.1", "::", false}, {"::1", "0.0.0.0", false},
		{"127.0.0.1", "127.0.0.1", true}, {"::1", "::1", true},
		{"127.0.0.1", "::1", false}, {"::1", "127.0.0.1", false},
		{"127.0.0.1", "", false}, {"::1", "", false},
	} {
		ports := fmt.Sprintf(`{"30443/tcp":[{"HostIp":%q,"HostPort":"32443"}],"30022/tcp":[{"HostIp":%q,"HostPort":"32022"}]}`, tc.bind, tc.bind)
		result, err := ProbeClientPorts(t.Context(), probePlan(), Config{ClientHost: tc.host}, probeDocker(t, ports))
		if err != nil || result.HTTPS.Published != tc.published || result.SSH.Published != tc.published || result.NeedsAction == tc.published {
			t.Fatal("mapping borrowed from a different address family", tc, result, err)
		}
		if tc.published && (result.HTTPS.Port != "32443" || result.SSH.Port != "32022") {
			t.Fatal("matching IP mapping lost", result)
		}
		if !tc.published && (result.HTTPS.Port != "30443" || result.SSH.Port != "30022") {
			t.Fatal("unverified mapping changed planned ports", result)
		}
	}
}
