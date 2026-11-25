// Package fake supplies an offline store HTTP boundary for setup and runtime contracts.
//
// Spec: docs/plans/c9-install-and-upgrade.md#invariants
package fake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
)

type Call struct {
	Method, Path  string
	Authenticated bool
}
type Store struct {
	mu          sync.Mutex
	Initialized bool
	Sealed      bool
	Revoked     bool
	Resources   map[string]map[string]any
	Calls       []Call
	Fail        string
	Snapshot    []byte
}

func (s *Store) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	s.Calls = append(s.Calls, Call{r.Method, path, r.Header.Get("X-Vault-Token") != ""})
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if s.Resources == nil {
		s.Resources = map[string]map[string]any{}
	}
	if path == "/sys/storage/raft/snapshot" && r.Method == "GET" && s.Snapshot != nil {
		return &http.Response{StatusCode: s.health(), Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(s.Snapshot)), Request: r}, nil
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	_ = r.Body.Close()
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	code, result := s.reply(r.Method, path, data)
	b, _ := json.Marshal(result)
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(b))), Request: r}, nil
}
func (s *Store) reply(method, path string, data map[string]any) (int, any) {
	if path == s.Fail {
		return 503, map[string]any{}
	}
	switch path {
	case "/sys/health":
		return s.health(), nil
	case "/sys/seal-status":
		return 200, map[string]any{"initialized": s.Initialized, "sealed": s.Sealed}
	case "/sys/init":
		if s.Initialized {
			return 400, nil
		}
		s.Initialized = true
		s.Sealed = true
		return 200, map[string]any{"keys_base64": []string{"private-share"}, "root_token": "temporary-bootstrap"}
	case "/sys/unseal":
		s.Sealed = false
		return 200, map[string]bool{"sealed": false}
	case "/auth/token/revoke-self":
		s.Revoked = true
		return 204, nil
	case "/sys/mounts", "/sys/auth":
		resources := map[string]any{}
		for name, value := range s.Resources {
			prefix := path + "/"
			if strings.HasPrefix(name, prefix) {
				resources[strings.TrimPrefix(name, prefix)+"/"] = value
			}
		}
		return 200, map[string]any{"data": resources}
	}
	if s.Sealed {
		return 503, nil
	}
	return s.resource(method, path, data)
}
func (s *Store) resource(method, path string, data map[string]any) (int, any) {
	if strings.HasSuffix(path, "/login") {
		s.Resources[path] = data
		return 200, map[string]any{"auth": map[string]any{"client_token": "short-lived-session"}}
	}
	if method == "GET" {
		if value, ok := s.Resources[path]; ok {
			return 200, map[string]any{"data": value}
		}
		return 404, nil
	}
	if method == "DELETE" {
		delete(s.Resources, path)
		return 204, nil
	}
	if strings.HasPrefix(path, "/sys/auth/") && !strings.Contains(strings.TrimPrefix(path, "/sys/auth/"), "/") {
		data["accessor"] = "auth_kubernetes_fixture"
	}
	s.Resources[path] = data
	return 204, nil
}

func (s *Store) health() int {
	if !s.Initialized {
		return http.StatusNotImplemented
	}
	if s.Sealed {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}
