package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	urlpath "path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This wire type follows cmd/oberth/config.go and server_runtime.go on C11.
// C11 runs the real loader against the chart after the install landing.
type chartServerConfig struct {
	Installation json.RawMessage `json:"installation"`
	Files        struct {
		SecretStoreCA    string `json:"secret_store_ca" format:"absolute-path"`
		SecretStoreToken string `json:"secret_store_token" format:"absolute-path"`
		CACertificate    string `json:"ca_certificate" format:"absolute-path"`
		CAPrivateKey     string `json:"ca_private_key" format:"absolute-path"`
		AdminPublicKey   string `json:"admin_public_key" format:"absolute-path"`
		AdminPushKey     string `json:"admin_push_key" format:"absolute-path"`
		SSHHostKey       string `json:"ssh_host_key" format:"absolute-path"`
		ForgeKey         string `json:"forge_key" format:"absolute-path"`
	} `json:"files"`
	Version           int    `json:"version"`
	Namespace         string `json:"namespace"`
	ArgoNamespace     string `json:"argo_namespace"`
	PipelineNamespace string `json:"pipeline_namespace"`
	Public            struct {
		URL     string `json:"url"`
		SSHPort int    `json:"ssh_port"`
	} `json:"public"`
	Listen struct {
		HTTPS  string `json:"https"`
		SSH    string `json:"ssh"`
		Source string `json:"source"`
		Health string `json:"health"`
	} `json:"listen"`
	DatabaseFile string             `json:"database_file"`
	Protocol     stateport.Protocol `json:"protocol"`
	SecretStore  struct {
		Address    string `json:"address"`
		Audience   string `json:"audience"`
		KVMount    string `json:"kv_mount"`
		AuthMount  string `json:"auth_mount"`
		WriterRole string `json:"writer_role"`
	} `json:"secret_store"`
	Logs struct {
		RunLimitBytes int64 `json:"run_limit_bytes"`
	} `json:"logs"`
	Schedules struct {
		MaxEntries int `json:"max_entries"`
	} `json:"schedules"`
	Assist struct {
		TTLSeconds int `json:"ttl_seconds"`
	} `json:"assist"`
	Runtime chartServerRuntime `json:"runtime"`
}

type chartServerRuntime struct {
	SourceDelivery          sourceBudgetTuple                      `json:"source_delivery"`
	ReceiveMaxInputBytes    int64                                  `json:"receive_max_input_bytes"`
	GitBinary               string                                 `json:"git_binary"`
	SourceTimeoutSeconds    int                                    `json:"source_timeout_seconds"`
	AdmissionTimeoutSeconds int                                    `json:"admission_timeout_seconds"`
	SourceClientBurst       int                                    `json:"source_client_burst"`
	DefaultBranch           string                                 `json:"default_branch"`
	ImagePrefixes           []string                               `json:"image_prefixes"`
	HelperImage             string                                 `json:"helper_image"`
	Defaults                runtimeconfig.WorkflowDefaultsSnapshot `json:"workflow_defaults"`
	SourcePolicy            json.RawMessage                        `json:"source_policy"`
	Routes                  workflowpolicy.ReleaseRoutes           `json:"release_routes"`
	IssueStaleSeconds       int                                    `json:"issue_stale_after_seconds"`
	IssueActiveStaleSeconds int                                    `json:"issue_active_stale_after_seconds"`
}

func decodeChartServer(raw []byte) (chartServerConfig, error) {
	var cfg chartServerConfig
	if len(raw) > 1<<20 {
		return cfg, fmt.Errorf("configuration exceeds 1 MiB")
	}
	if err := uniqueChartJSON(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return cfg, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("expected one server configuration object")
	}
	if err := requireChartFields(raw, reflect.TypeFor[chartServerConfig]()); err != nil {
		return cfg, err
	}
	return cfg, validateChartValues(cfg)
}

func requireChartFields(raw []byte, typ reflect.Type) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name := f.Tag.Get("json")
		value, present := fields[name]
		delete(fields, name)
		if !present || string(value) == "null" && f.Type != reflect.TypeFor[json.RawMessage]() {
			return fmt.Errorf("required config field %s is absent", name)
		}
		if f.Tag.Get("format") == "absolute-path" {
			var path string
			_ = json.Unmarshal(value, &path)
			if !filepath.IsAbs(path) {
				return fmt.Errorf("%s must be an absolute file path", name)
			}
		}
		if f.Type.Kind() == reflect.Struct {
			if err := requireChartFields(value, f.Type); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	if len(fields) != 0 {
		return fmt.Errorf("unknown configuration fields: %v", fields)
	}
	return nil
}

func configuredServerFiles() map[string]any {
	return map[string]any{"ca_certificate": "/etc/oberth/ca/tls.crt", "ca_private_key": "/etc/oberth/ca/tls.key", "admin_public_key": "/etc/oberth/admin/admin.pub", "admin_push_key": "/etc/oberth/admin/push.pub", "ssh_host_key": "/etc/oberth/ssh/key", "forge_key": "/etc/oberth/forge/key", "secret_store_ca": "/etc/oberth/store/ca.crt", "secret_store_token": "/var/run/secrets/secretstore/token"}
}

func configuredServerRuntime() map[string]any {
	return map[string]any{"receive_max_input_bytes": 1073741824, "git_binary": "/usr/bin/git", "source_timeout_seconds": 60, "admission_timeout_seconds": 30, "source_client_burst": 96, "source_delivery": map[string]any{"exports": 1, "archives": 2, "transfers": 2, "auth": 32, "workflow_waiters": 16}, "default_branch": "refs/heads/main", "image_prefixes": []string{}, "helper_image": "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth:dev", "workflow_defaults": map[string]any{"workflow_defaults": nil, "digest": ""}, "source_policy": map[string]any{}, "release_routes": map[string]any{}, "issue_stale_after_seconds": 86400, "issue_active_stale_after_seconds": 7200}
}

func TestRenderedServerStrictConfigAndMountedFiles(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Namespace, f.choices.PipelineNamespace = "frontdoor", "jobs"
	f.choices.Values = map[string]any{"secretStore": map[string]any{"audience": "installation-login"}}
	checked := false
	f.engine.Charts = beforeChartApply{ChartPort: f.charts, check: func(r installer.ChartRequest) {
		objects := renderedIn(t, r.Values, r.Namespace)
		pod := serverDeployment(t, objects).Spec.Template.Spec
		for _, file := range configuredServerFiles() {
			assertMountedFile(t, f, pod, file.(string))
		}
		checked = true
	}}
	apply(t, f, prepare(t, f))
	objects := renderedIn(t, f.charts.Previews[0].Values, "frontdoor")
	raw := object(t, objects, "ConfigMap", "oberth-server")["data"].(map[string]any)["server.json"].(string)
	cfg, err := decodeChartServer([]byte(raw))
	if err != nil {
		t.Fatal("rendered file cannot be read by the server contract", err)
	}
	if cfg.Namespace != "frontdoor" || cfg.PipelineNamespace != "jobs" || cfg.ArgoNamespace != "frontdoor" || cfg.SecretStore.Audience != "installation-login" {
		t.Fatal("rendered namespaces or audience differ", cfg)
	}
	pod := serverDeployment(t, objects).Spec.Template.Spec
	for key, file := range configuredServerFiles() {
		var files map[string]string
		data, _ := json.Marshal(cfg.Files)
		if err := json.Unmarshal(data, &files); err != nil || files[key] != file {
			t.Fatal("configured file differs", key, err)
		}
		assertMountedFile(t, f, pod, files[key])
	}
	if !checked || strings.Contains(raw, "executor_account") {
		t.Fatal("separate executor identity configured")
	}
	assertPublicSourceTrust(t, f, objects)
}

func assertPublicSourceTrust(t *testing.T, f fixture, objects []map[string]any) {
	t.Helper()
	secret, err := f.client.CoreV1().Secrets(f.choices.Namespace).Get(t.Context(), "oberth-ca", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	source := object(t, objects, "ConfigMap", "oberth-source")["data"].(map[string]any)
	var trust struct {
		Endpoint    string `json:"endpoint"`
		ServerName  string `json:"server_name"`
		CAPEM       string `json:"ca_pem"`
		ImageDigest string `json:"image_digest"`
	}
	d := json.NewDecoder(strings.NewReader(source["source.json"].(string)))
	d.DisallowUnknownFields()
	if err = d.Decode(&trust); err != nil || trust.CAPEM != string(secret.Data["tls.crt"]) || trust.ImageDigest == "" {
		t.Fatal("source trust differs from installed authority or plugin", err)
	}
}

type beforeChartApply struct {
	installer.ChartPort
	check func(installer.ChartRequest)
}

func (c beforeChartApply) Apply(ctx context.Context, r installer.ChartRequest) error {
	c.check(r)
	return c.ChartPort.Apply(ctx, r)
}

func assertMountedFile(t *testing.T, f fixture, pod corev1.PodSpec, file string) {
	t.Helper()
	name, key, token := mountedServerFile(pod, file)
	if token != nil {
		expected := f.choices.Values["secretStore"].(map[string]any)["audience"].(string)
		if token.Audience != expected {
			t.Fatal("projected store token audience differs from server configuration")
		}
		return
	}
	if name == "" {
		t.Fatal("configured file has no mounted Secret item", file)
	}
	secret, err := f.client.CoreV1().Secrets(f.choices.Namespace).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil || len(secret.Data[key]) == 0 {
		t.Fatal("install did not publish configured file before chart apply", file, err)
	}
}

func mountedServerFile(pod corev1.PodSpec, file string) (string, string, *corev1.ServiceAccountTokenProjection) {
	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range pod.Containers[0].VolumeMounts {
		mounts[mount.Name] = mount
	}
	for _, volume := range pod.Volumes {
		mount := mounts[volume.Name]
		if mount.MountPath != urlpath.Dir(file) || !mount.ReadOnly || mount.SubPath != "" {
			continue
		}
		if token := projectedTokenFile(volume.Projected, file); token != nil {
			return "", "", token
		}
		if volume.Secret == nil {
			continue
		}
		for _, item := range volume.Secret.Items {
			if item.Path == urlpath.Base(file) {
				return volume.Secret.SecretName, item.Key, nil
			}
		}
	}
	return "", "", nil
}

func TestServerWireRefusesMissingAndUnknownFields(t *testing.T) {
	t.Parallel()
	base := serverJSON(t, rendered(t, nil))
	for _, field := range []string{"files", "files.ca_certificate", "files.ca_private_key", "files.admin_public_key", "files.admin_push_key", "files.ssh_host_key", "files.forge_key", "files.secret_store_ca", "files.secret_store_token", "namespace", "pipeline_namespace", "argo_namespace", "secret_store.audience", "protocol.schema_identity", "protocol.audit_domain", "runtime.git_binary", "runtime.workflow_defaults", "runtime.release_routes"} {
		for _, missing := range []bool{false, true} {
			var changed map[string]any
			if err := json.Unmarshal([]byte(jsonOf(t, base)), &changed); err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(field, ".")
			parent := changed
			if len(parts) == 2 {
				parent = changed[parts[0]].(map[string]any)
			}
			key := parts[len(parts)-1]
			parent[key] = nil
			if missing {
				delete(parent, key)
			}
			if _, err := decodeChartServer([]byte(jsonOf(t, changed))); err == nil {
				t.Fatal("incomplete server contract accepted", field, missing)
			}
		}
	}
	for _, field := range []string{"executor_accounts", "controller_namespace", "login_audience"} {
		base["runtime"].(map[string]any)[field] = "unsupported"
		if _, err := decodeChartServer([]byte(jsonOf(t, base))); err == nil {
			t.Fatal("unsupported runtime field accepted", field)
		}
		delete(base["runtime"].(map[string]any), field)
	}
}

func projectedTokenFile(projected *corev1.ProjectedVolumeSource, file string) *corev1.ServiceAccountTokenProjection {
	if projected == nil {
		return nil
	}
	for _, source := range projected.Sources {
		if source.ServiceAccountToken != nil && source.ServiceAccountToken.Path == urlpath.Base(file) {
			return source.ServiceAccountToken
		}
	}
	return nil
}
