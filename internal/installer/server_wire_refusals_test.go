package installer_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestServerWireRefusesInvalidValues(t *testing.T) {
	t.Parallel()
	raw := jsonOf(t, serverJSON(t, rendered(t, nil)))
	for field, values := range invalidServerValues() {
		t.Run(field, func(t *testing.T) {
			for _, value := range values {
				var changed map[string]any
				if err := json.Unmarshal([]byte(raw), &changed); err != nil {
					t.Fatal(err)
				}
				parts := strings.Split(field, ".")
				parent := changed
				for _, part := range parts[:len(parts)-1] {
					parent = parent[part].(map[string]any)
				}
				parent[parts[len(parts)-1]] = value
				if _, err := decodeChartServer([]byte(jsonOf(t, changed))); err == nil {
					t.Fatal("invalid server value accepted", field, value)
				}
			}
		})
	}
}

func invalidServerValues() map[string][]any {
	return map[string][]any{
		"version":                           {0, 2},
		"namespace":                         {""},
		"argo_namespace":                    {""},
		"pipeline_namespace":                {"", "oberth"},
		"public.url":                        {"http://oberth.ci", "https://user@oberth.ci", "https://oberth.ci?q=1", "https://oberth.ci#fragment", "https://"},
		"public.ssh_port":                   {0, 65536},
		"listen.https":                      {"localhost"},
		"listen.ssh":                        {"localhost"},
		"listen.source":                     {"localhost"},
		"database_file":                     {"relative.db"},
		"files.ca_certificate":              {"relative.pem"},
		"protocol.schema_identity":          {"", ".invalid", strings.Repeat("s", 129)},
		"protocol.audit_domain":             {"", "invalid domain"},
		"secret_store.address":              {"http://store.oberth.ci", "https://user@store.oberth.ci"},
		"secret_store.audience":             {"", " audience", "audience "},
		"secret_store.kv_mount":             {""},
		"secret_store.auth_mount":           {""},
		"secret_store.writer_role":          {""},
		"runs.parallel":                     {-1},
		"logs.run_limit_bytes":              {0},
		"schedules.max_entries":             {-1, 257},
		"assist.ttl_seconds":                {0},
		"runtime.git_binary":                {"git"},
		"runtime.source_timeout_seconds":    {0, 3601},
		"runtime.admission_timeout_seconds": {0, 3601},
		"runtime.source_client_burst":       {2},
		"runtime.default_branch":            {"main", "refs/tags/v1", "refs/heads/", "refs/heads/a..b", "refs/heads/a.lock", "refs/heads/.hidden", "refs/heads/a\n"},
		"runtime.release_routes": {
			map[string]any{"bad": []map[string]string{{"pattern": "v*", "file": ".oberth/release.yaml"}}},
			map[string]any{"origin/team/widget": []map[string]string{{"pattern": "refs/tags/v*", "file": ".oberth/release.yaml"}}},
			map[string]any{"origin/team/widget": []map[string]string{{"pattern": "v*", "file": "../release.yaml"}}},
			map[string]any{"origin/team/widget": []map[string]string{{"pattern": "v*", "file": ".oberth/release.yaml", "future": "unknown"}}},
		},
	}
}

func TestServerWireRejectsDuplicatesAndTrailingData(t *testing.T) {
	t.Parallel()
	raw := jsonOf(t, serverJSON(t, rendered(t, nil)))
	for _, changed := range []string{
		raw + "{}",
		strings.Replace(raw, "\"namespace\":", "\"Namespace\":\"shadow\",\"namespace\":", 1),
		strings.Replace(raw, "\"version\":1", "\"version\":1,\"version\":1", 1),
		strings.Replace(raw, "\"source_policy\":{}", "\"source_policy\":{\"repo\":[],\"repo\":[]}", 1),
		strings.Replace(raw, "\"release_routes\":{}", "\"release_routes\":{\"repo\":[],\"repo\":[]}", 1),
		strings.Replace(raw, "\"schema_identity\":", "\"unknown\":true,\"schema_identity\":", 1),
	} {
		if changed == raw {
			t.Fatal("invalid fixture was not changed")
		}
		if _, err := decodeChartServer([]byte(changed)); err == nil {
			t.Fatal("ambiguous or unknown server config accepted")
		}
	}
}
