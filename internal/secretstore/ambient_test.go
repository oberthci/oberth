package secretstore

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// These values are public synthetic fixture data. No live credential or endpoint
// is used. Assert actual HTTPS requests, including clones created after New.
func TestClientRejectsAmbientAuthorityOnEveryRequest(t *testing.T) {
	for _, casing := range []string{"lowercase", "mixed-case"} {
		for _, timing := range []string{"before-construction", "after-construction"} {
			t.Run(casing+"/"+timing, func(t *testing.T) {
				for _, key := range []string{"VAULT_TOKEN", "VAULT_NAMESPACE", "VAULT_HEADERS", "VAULT_WRAP_TTL"} {
					t.Setenv(key, "")
				}
				ambient := make(map[string]string)
				inject := func() {
					headers := map[string]string{"authorization": "ambient-authorization", "cookie": "ambient-cookie", "x-ambient-marker": "ambient-arbitrary", "x-vault-token": "ambient-header-token", "x-vault-namespace": "ambient-header-namespace", "x-vault-request": "ambient-ssrf-override", "x-vault-wrap-ttl": "99m", "x-vault-policy-override": "true"}
					if casing == "mixed-case" {
						upper := make(map[string]string, len(headers))
						for key, value := range headers {
							upper[strings.ToUpper(key)] = value
						}
						headers = upper
					}
					encoded, err := json.Marshal(headers)
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("VAULT_TOKEN", "ambient-client-token")
					t.Setenv("VAULT_NAMESPACE", "ambient-client-namespace")
					t.Setenv("VAULT_HEADERS", string(encoded))
					t.Setenv("VAULT_WRAP_TTL", "7m")
					for _, key := range []string{"VAULT_TOKEN", "VAULT_NAMESPACE", "VAULT_HEADERS", "VAULT_WRAP_TTL"} {
						ambient[key] = os.Getenv(key)
					}
				}
				type sessionUse struct{ reads, revokes int }
				var mu sync.Mutex
				issued := map[string]*sessionUse{}
				sealChecks, logins := 0, 0
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 {
						t.Error("fixture request was not TLS 1.3")
					}
					if values := r.Header.Values("X-Vault-Request"); len(values) != 1 || values[0] != "true" {
						t.Errorf("%s: deliberate SSRF header was contaminated", r.URL.Path)
					}
					for key := range r.Header {
						lower := strings.ToLower(key)
						if lower == "authorization" || lower == "cookie" || lower == "x-ambient-marker" || (strings.HasPrefix(lower, "x-vault-") && lower != "x-vault-request" && lower != "x-vault-token") {
							t.Errorf("%s: ambient header %s reached transport", r.URL.Path, key)
						}
					}
					switch r.URL.Path {
					case "/v1/sys/seal-status":
						sealChecks++
						if r.Header.Get("X-Vault-Token") != "" {
							t.Error("seal status carried ambient token")
						}
						_, _ = w.Write([]byte(`{"sealed":false}`))
					case "/v1/auth/kubernetes/login":
						if r.Header.Get("X-Vault-Token") != "" {
							t.Error("Kubernetes login carried ambient token")
						}
						var input map[string]string
						if json.NewDecoder(r.Body).Decode(&input) != nil || input["role"] != "exact-fixture-role" || !strings.HasPrefix(input["jwt"], "synthetic-sa-") {
							t.Error("explicit Kubernetes identity changed")
							w.WriteHeader(403)
							return
						}
						logins++
						token := "issued-" + input["jwt"]
						if issued[token] != nil {
							t.Error("ServiceAccount login was reused")
						}
						issued[token] = &sessionUse{}
						_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": token, "lease_duration": 60}})
					case "/v1/fixture/data/allowed", "/v1/auth/token/revoke-self":
						token := r.Header.Get("X-Vault-Token")
						use := issued[token]
						if use == nil || len(r.Header.Values("X-Vault-Token")) != 1 {
							t.Error("request did not use its own issued session token")
							w.WriteHeader(403)
							return
						}
						if strings.HasSuffix(r.URL.Path, "revoke-self") {
							use.revokes++
							w.WriteHeader(204)
							return
						}
						use.reads++
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"owner": token}, "metadata": map[string]int{"version": 1}}})
					default:
						t.Error("unexpected fixture request path")
						w.WriteHeader(404)
					}
				}))
				defer server.Close()
				if timing == "before-construction" {
					inject()
				}
				var serial atomic.Int64
				client, err := New(Config{Address: server.URL, Role: "exact-fixture-role", CACertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), ServiceAccountTokenSource: func(context.Context) ([]byte, error) { return fmt.Appendf(nil, "synthetic-sa-%d", serial.Add(1)), nil }})
				if err != nil {
					t.Fatal(err)
				}
				if timing == "after-construction" {
					inject()
				}
				if sealed, err := client.SealStatus(context.Background()); err != nil || sealed {
					t.Fatalf("seal status failed: %v", err)
				}
				const count = 12
				var group sync.WaitGroup
				owners := make(chan string, count)
				for range count {
					group.Go(func() {
						values, err := client.FetchKV(context.Background(), []string{"fixture/data/allowed"})
						if err != nil {
							t.Errorf("explicit session fetch failed: %v", err)
							return
						}
						defer func() {
							for _, fields := range values {
								clearKVValues(fields)
							}
						}()
						owners <- string(values["fixture/data/allowed"]["owner"])
					})
				}
				group.Wait()
				for key, value := range ambient {
					if os.Getenv(key) != value {
						t.Errorf("client mutated process environment %s", key)
					}
				}
				close(owners)
				seen := map[string]bool{}
				for owner := range owners {
					if seen[owner] {
						t.Error("concurrent fetches shared a session identity")
					}
					seen[owner] = true
				}
				mu.Lock()
				defer mu.Unlock()
				if sealChecks != 1 || logins != count || len(seen) != count {
					t.Fatal("actual seal/login/read coverage incomplete")
				}
				for _, use := range issued {
					if use.reads != 1 || use.revokes != 1 {
						t.Fatal("issued session not independently read and revoked exactly once")
					}
				}
			})
		}
	}
}

func TestClientSDKConstructionErrorsDoNotReflectAmbientInput(t *testing.T) {
	const marker = "attacker-marker-never-return"
	for _, input := range []struct{ name, key, value string }{
		{"forbidden-header", "VAULT_HEADERS", `{"X-Vault-` + marker + `":"synthetic"}`},
		{"malformed-headers", "VAULT_HEADERS", marker},
		{"invalid-retry-count", "VAULT_MAX_RETRIES", marker},
	} {
		for _, timing := range []string{"constructor", "clone"} {
			t.Run(input.name+"/"+timing, func(t *testing.T) {
				for _, key := range []string{"VAULT_TOKEN", "VAULT_NAMESPACE", "VAULT_HEADERS", "VAULT_WRAP_TTL", "VAULT_MAX_RETRIES"} {
					t.Setenv(key, "")
				}
				var requests atomic.Int64
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer server.Close()
				config := Config{Address: server.URL, Role: "fixture-role",
					CACertPEM:                 pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
					ServiceAccountTokenSource: func(context.Context) ([]byte, error) { return []byte(testServiceJWT), nil },
				}
				var err error
				if timing == "constructor" {
					t.Setenv(input.key, input.value)
					_, err = New(config)
				} else {
					client, createErr := New(config)
					if createErr != nil {
						t.Fatal(createErr)
					}
					t.Setenv(input.key, input.value)
					_, err = client.FetchKV(context.Background(), []string{"fixture/data/allowed"})
				}
				if err == nil {
					t.Fatal("invalid SDK construction succeeded")
				}
				if strings.Contains(err.Error(), marker) || len(err.Error()) > 160 {
					t.Fatal("SDK construction reflected ambient input")
				}
				if requests.Load() != 0 {
					t.Fatal("denied construction emitted an HTTP request")
				}
			})
		}
	}
}
