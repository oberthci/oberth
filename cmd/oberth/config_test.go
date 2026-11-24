package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func validConfig() []byte {
	return []byte(`{"installation":{"pod_defaults":true,"link_local_filter":true,"pod_security":"baseline","bundled_store":true,"https_node_port":30443,"ssh_node_port":30022,"source_image":"source:dev","controller_image":"controller:v4.0.15","executor_image":"executor:v4.0.15","store_image":"store:2.7.2","data_size":"10Gi","tmp_size":"2Gi","origins":{}},"version":1,"files":{"secret_store_ca":"/etc/oberth/store/ca.crt","secret_store_token":"/var/run/secrets/secretstore/token","ca_certificate":"/etc/oberth/ca/tls.crt","ca_private_key":"/etc/oberth/ca/tls.key","admin_public_key":"/etc/oberth/admin/admin.pub","admin_push_key":"/etc/oberth/admin/push.pub","ssh_host_key":"/etc/oberth/ssh/key","forge_key":"/etc/oberth/forge/key"},"namespace":"oberth","argo_namespace":"argo","pipeline_namespace":"pipelines","public":{"url":"https://oberth.local","ssh_port":2222},"listen":{"https":":8443","ssh":":2222","source":":8445","health":":8081"},"database_file":"/var/lib/oberth/oberth.db","protocol":{"schema_identity":"oberth-schema-v1","audit_domain":"oberth-audit-v2"},"secret_store":{"address":"https://store.local","audience":"installation-store-login","kv_mount":"kv","auth_mount":"kubernetes","writer_role":"writer"},"logs":{"run_limit_bytes":67108864},"schedules":{"max_entries":8},"assist":{"ttl_seconds":2400},"runtime":{}}`)
}

func TestConfigReportsEveryProblem(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(validConfig(), &raw); err != nil {
		t.Fatal(err)
	}
	raw["unknown"] = true
	delete(raw, "namespace")
	raw["version"] = 2
	raw["listen"].(map[string]any)["another"] = "unknown"
	data, _ := json.Marshal(raw)
	_, err := decodeConfig(data)
	var e *publicerr.Error
	if !errors.As(err, &e) {
		t.Fatal("invalid config accepted")
	}
	for _, problem := range []string{"unknown: unknown field", "namespace: required", "version: invalid value", "listen.another: unknown field"} {
		if !strings.Contains(e.Detail, problem) {
			t.Fatal("missing problem", problem)
		}
	}
}

func TestRefuseIncompleteComposition(t *testing.T) {
	cfg, err := decodeConfig(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	clock := fake.NewClock()
	noop := func(context.Context, *composition) (Part, error) {
		t.Fatal("constructor called before completeness check")
		return Part{}, nil
	}
	all := constructors{noop, noop, noop, noop, noop}
	variants := []constructors{all, all, all, all, all}
	variants[0].CacheNew = nil
	variants[1].NativeNew = nil
	variants[2].WiringNew = nil
	variants[3].ServiceNew = nil
	variants[4].SourceNew = nil
	for i, builders := range variants {
		deps := serverDependencies{constructors: builders, clock: serverClock{clock.Now, clock.Ticker}, listen: func(context.Context, string, string) (net.Listener, error) {
			t.Fatal("listener bound for incomplete composition")
			return nil, nil
		}}
		err := serve(context.Background(), cfg, deps, io.Discard)
		var public *publicerr.Error
		expected := []string{"gitcache.New", "argojob.New/argowatch.New", "app.New", "service.New", "sourcedelivery.New/sshserver.New"}
		if !errors.As(err, &public) || !strings.Contains(public.Detail, expected[i]) {
			t.Fatal("missing constructor not named", err)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(validConfig(), &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		clone := map[string]json.RawMessage{}
		for key, value := range fields {
			clone[key] = value
		}
		delete(clone, name)
		data, _ := json.Marshal(clone)
		if _, err := decodeConfig(data); err == nil {
			t.Fatalf("missing %s accepted", name)
		}
	}
}

func TestStoreMountConfigIsRequired(t *testing.T) {
	for _, key := range []string{"secret_store_ca", "secret_store_token"} {
		for _, value := range []any{nil, "relative"} {
			var raw map[string]any
			if err := json.Unmarshal(validConfig(), &raw); err != nil {
				t.Fatal(err)
			}
			files := raw["files"].(map[string]any)
			if value == nil {
				delete(files, key)
			} else {
				files[key] = value
			}
			data, _ := json.Marshal(raw)
			_, err := decodeConfig(data)
			var problem *publicerr.Error
			if !errors.As(err, &problem) || !strings.Contains(problem.Detail, "files."+key) {
				t.Fatal("store mount refusal did not name its field", key, err)
			}
		}
	}
}

func validRuntime() []byte {
	return []byte(`{"git_binary":"/usr/bin/git","source_timeout_seconds":60,"admission_timeout_seconds":30,"source_client_burst":96,"default_branch":"refs/heads/main","issue_stale_after_seconds":86400,"issue_active_stale_after_seconds":7200}`)
}
