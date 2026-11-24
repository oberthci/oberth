package installer_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	"helm.sh/helm/v3/pkg/chart/loader"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func serverJSON(t *testing.T, objects []map[string]any) map[string]any {
	t.Helper()
	raw := object(t, objects, "ConfigMap", "oberth-server")["data"].(map[string]any)["server.json"].(string)
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func serverDeployment(t *testing.T, objects []map[string]any) appsv1.Deployment {
	t.Helper()
	var deployment appsv1.Deployment
	if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, "Deployment", "oberth"))), &deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

// The CLI configuration and composition types define this exact wire object.
func TestAdoptedServerJSONContract(t *testing.T) {
	t.Parallel()
	protocol := stateport.DefaultProtocol()
	for _, parallel := range []int{0, 7} {
		values := map[string]any{
			"pipeline":    map[string]any{"namespace": "jobs"},
			"database":    map[string]any{"file": "/var/lib/oberth/restored.db"},
			"secretStore": map[string]any{"address": "https://store.oberth.ci", "kv_mount": "values", "auth_mount": "cluster", "writer_role": "writer", "audience": "oberth-store"},
			"runs":        map[string]any{"parallel": parallel}, "logs": map[string]any{"run_limit_bytes": 1024},
			"schedules": map[string]any{"max_entries": 12}, "assist": map[string]any{"ttl_seconds": 60},
			"server": map[string]any{"public": map[string]any{"url": "https://oberth.ci:34443", "ssh_port": 34022}},
		}
		objects := rendered(t, values)
		want := map[string]any{
			"version": 1, "namespace": "oberth", "pipeline_namespace": "jobs", "argo_namespace": "oberth",
			"files":         configuredServerFiles(),
			"public":        map[string]any{"url": "https://oberth.ci:34443", "ssh_port": 34022},
			"listen":        map[string]any{"https": ":8443", "ssh": ":2222", "source": ":8445", "health": ":8081"},
			"database_file": "/var/lib/oberth/restored.db",
			"protocol":      map[string]any{"schema_identity": protocol.SchemaIdentity, "audit_domain": protocol.AuditDomain},
			"secret_store":  map[string]any{"address": "https://store.oberth.ci", "kv_mount": "values", "auth_mount": "cluster", "writer_role": "writer", "audience": "oberth-store"},
			"logs":          values["logs"], "schedules": values["schedules"], "assist": values["assist"],
			"runtime": configuredServerRuntime(),
			"installation": map[string]any{
				"bundled_store": true, "chosen": []any{}, "data_size": "10Gi", "tmp_size": "2Gi",
				"https_node_port": 30443, "ssh_node_port": 30022,
				"link_local_filter": true, "pod_defaults": true, "pod_security": "baseline",
				"source_image":     "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-source-plugin:dev",
				"controller_image": "quay.io/argoproj/workflow-controller:v4.0.15@sha256:7014032b9d4b70ff87e279800781b74238a18955d8acba5437424359c530ccde",
				"executor_image":   "quay.io/argoproj/argoexec:v4.0.15@sha256:985c24ef182e066842c424f5aff5ee0a838ca359d78ee2beb415229737fa99a8",
				"store_image":      "docker.io/openbao/openbao:2.7.2@sha256:5046911075e137e154ce49ab051502f4ec7cf367d484ae4a4dce665d9b90561f",
			},
		}
		if got := serverJSON(t, objects); jsonOf(t, got) != jsonOf(t, want) {
			t.Fatalf("server contract differs:\n%s\nwant:\n%s", jsonOf(t, got), jsonOf(t, want))
		}
		cm := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
		if cm["parallelism"] != jsonOf(t, parallel) {
			t.Fatal("controller run cap differs")
		}
		object(t, objects, "Namespace", "jobs")
	}
}

func TestAdoptedServerProcessContract(t *testing.T) {
	t.Parallel()
	deployment := serverDeployment(t, rendered(t, nil))
	pod := deployment.Spec.Template.Spec
	if *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || *pod.TerminationGracePeriodSeconds != 30 {
		t.Fatal("server replica, strategy or grace differs")
	}
	if pod.ServiceAccountName != "oberth" || pod.AutomountServiceAccountToken != nil && !*pod.AutomountServiceAccountToken {
		t.Fatal("standard in-cluster service-account mount disabled")
	}
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 0 {
		t.Fatal("unexpected server process")
	}
	server := pod.Containers[0]
	if !reflect.DeepEqual(server.Command, []string{"/usr/local/bin/oberth"}) || !reflect.DeepEqual(server.Args, []string{"serve", "--config", "/etc/oberth/config/server.json"}) || len(server.Env)+len(server.EnvFrom) != 0 {
		t.Fatal("server must consume exactly one configuration flag")
	}
}

func TestAdoptedServerListeners(t *testing.T) {
	t.Parallel()
	server := serverDeployment(t, rendered(t, nil)).Spec.Template.Spec.Containers[0]
	for path, probe := range map[string]*corev1.Probe{"/livez": server.LivenessProbe, "/readyz": server.ReadinessProbe} {
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != path || probe.HTTPGet.Port.IntVal != 8081 || probe.HTTPGet.Scheme != corev1.URISchemeHTTP {
			t.Fatal("isolated health probe differs", path, probe)
		}
	}
	ports := map[string]int32{}
	for _, port := range server.Ports {
		ports[port.Name] = port.ContainerPort
	}
	if !reflect.DeepEqual(ports, map[string]int32{"https": 8443, "ssh": 2222, "source": 8445, "health": 8081}) {
		t.Fatal("server listener ports differ", ports)
	}
}

func TestAdoptedServerMountContract(t *testing.T) {
	t.Parallel()
	deployment := serverDeployment(t, rendered(t, map[string]any{"secretStore": map[string]any{"audience": "store-only", "caSecret": "external-store-ca"}}))
	pod := deployment.Spec.Template.Spec
	want := map[string]bool{"/var/lib/oberth": false, "/run/oberth": false, "/tmp": false, "/etc/oberth/config": true, "/etc/oberth/ca": true, "/etc/oberth/ssh": true, "/etc/oberth/forge": true, "/etc/oberth/admin": true, "/etc/oberth/store": true, "/var/run/secrets/secretstore": true}
	got := map[string]bool{}
	volumes := map[string]corev1.Volume{}
	for _, volume := range pod.Volumes {
		volumes[volume.Name] = volume
	}
	for _, mount := range pod.Containers[0].VolumeMounts {
		got[mount.MountPath] = mount.ReadOnly
		if _, ok := volumes[mount.Name]; !ok || mount.SubPath != "" {
			t.Fatal("mount has no volume or pins a projected file", mount)
		}
	}
	if !reflect.DeepEqual(got, want) || len(volumes) != len(want) {
		t.Fatal("server mounts differ", got)
	}
}

func serverVolumes(t *testing.T, values map[string]any) map[string]corev1.Volume {
	t.Helper()
	volumes := map[string]corev1.Volume{}
	for _, volume := range serverDeployment(t, rendered(t, values)).Spec.Template.Spec.Volumes {
		volumes[volume.Name] = volume
	}
	return volumes
}

func TestAdoptedServerStorage(t *testing.T) {
	t.Parallel()
	volumes := serverVolumes(t, nil)
	if volumes["data"].PersistentVolumeClaim.ClaimName != "oberth-data" || volumes["config"].ConfigMap.Name != "oberth-server" || volumes["runtime"].EmptyDir.Medium != corev1.StorageMediumMemory || volumes["tmp"].EmptyDir.SizeLimit.Value() < 1946157056 {
		t.Fatal("server data, configuration, socket or scratch volume differs")
	}
}

func TestAdoptedServerSecretFiles(t *testing.T) {
	t.Parallel()
	volumes := serverVolumes(t, map[string]any{"secretStore": map[string]any{"caSecret": "external-store-ca"}})
	for name, secret := range map[string]struct {
		name  string
		items map[string]string
	}{"ca": {"oberth-ca", map[string]string{"tls.crt": "tls.crt", "tls.key": "tls.key"}}, "ssh": {"oberth-ssh-host", map[string]string{"key": "key"}}, "forge": {"oberth-forge-key", map[string]string{"key": "key"}}, "admin": {"oberth-admin", map[string]string{"admin.pub": "admin.pub", "push.pub": "push.pub"}}, "store-ca": {"external-store-ca", map[string]string{"ca.crt": "ca.crt"}}} {
		volume := volumes[name].Secret
		if volume == nil || volume.SecretName != secret.name {
			t.Fatal("wrong identity Secret", name, volume)
		}
		items := map[string]string{}
		for _, item := range volume.Items {
			items[item.Key] = item.Path
		}
		if !reflect.DeepEqual(items, secret.items) || *volume.DefaultMode != 0440 {
			t.Fatal("wrong projected key files or access", name, items)
		}
	}
}

func TestAdoptedServerStoreToken(t *testing.T) {
	t.Parallel()
	volumes := serverVolumes(t, map[string]any{"secretStore": map[string]any{"audience": "store-only"}})
	projection := volumes["store-token"].Projected
	if projection == nil || len(projection.Sources) != 1 || projection.Sources[0].ServiceAccountToken == nil {
		t.Fatal("store token must be projected separately from API credentials")
	}
	token := projection.Sources[0].ServiceAccountToken
	if token.Audience != "store-only" || token.Path != "token" || *projection.DefaultMode != 0440 {
		t.Fatal("wrong store audience, token path or access", token)
	}
}

func TestAdoptedServerSchemaRefusesOtherForms(t *testing.T) {
	t.Parallel()
	for _, values := range []map[string]any{
		{"server": map[string]any{"data_dir": "/var/lib/oberth"}}, {"server": map[string]any{"secrets_dir": "/etc/oberth/secrets"}},
		{"server": map[string]any{"bootstrap_admin": map[string]any{}}}, {"server": map[string]any{"tls_names": []any{}}},
		{"server": map[string]any{"public": map[string]any{"ssh": "localhost:30022"}}},
		{"server": map[string]any{"listen": map[string]any{"https": ":9443"}}}, {"server": map[string]any{"version": 2}},
		{"server": map[string]any{"protocol": nil}}, {"server": map[string]any{"public": map[string]any{"ssh_port": nil}}},
		{"logs": map[string]any{"limit_bytes": 1024}}, {"schedules": map[string]any{"enabled": true}}, {"assist": map[string]any{"hold_ttl": "20m"}},
		{"pipeline": map[string]any{"namespace": "oberth"}}, {"secretStore": map[string]any{"writer_role": nil}},
	} {
		chart, err := loader.Load(chartPath())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = installer.Render(chart, values, "oberth"); err == nil {
			t.Fatal("non-contract configuration accepted", values)
		}
	}
}

func TestInstallPublishesAdoptedServerInputs(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.URL, f.choices.SSHPort = "https://oberth.ci:34443", 34022
	f.choices.PipelineNamespace = "jobs"
	f.choices.Values = map[string]any{"database": map[string]any{"file": "/var/lib/oberth/restored.db"}, "runs": map[string]any{"parallel": 0}}
	apply(t, f, prepare(t, f))
	values := f.charts.Previews[0].Values
	config := serverJSON(t, rendered(t, values))
	if jsonOf(t, config["public"]) != `{"ssh_port":34022,"url":"https://oberth.ci:34443"}` || config["pipeline_namespace"] != "jobs" || config["database_file"] != "/var/lib/oberth/restored.db" {
		t.Fatal("install choices differ from server input", config)
	}
	controller := object(t, rendered(t, values), "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	if controller["parallelism"] != "0" {
		t.Fatal("explicit unlimited run cap lost", controller)
	}
	annotations := serverDeployment(t, rendered(t, values)).Spec.Template.Annotations
	if annotations["checksum/admin"] == "" || annotations["checksum/admin"] != values["adminPublicKeySHA256"] {
		t.Fatal("public admin change does not trigger server restart")
	}
	if err := json.Unmarshal([]byte(jsonOf(t, values)), &f.charts.History.Deployed.Values); err != nil {
		t.Fatal(err)
	}
	f.choices = installer.Choices{Context: "test"}
	prepare(t, f)
	rerun := serverJSON(t, rendered(t, f.charts.Previews[1].Values))
	if jsonOf(t, rerun["public"]) != jsonOf(t, config["public"]) || rerun["pipeline_namespace"] != "jobs" || rerun["database_file"] != config["database_file"] {
		t.Fatal("rerun changed adopted inputs", rerun)
	}
}

func TestAdoptedServerNamespaceFromRelease(t *testing.T) {
	t.Parallel()
	objects := renderedIn(t, map[string]any{"pipeline": map[string]any{"namespace": "jobs"}}, "frontdoor")
	config := serverJSON(t, objects)
	if config["namespace"] != "frontdoor" || config["pipeline_namespace"] != "jobs" || serverDeployment(t, objects).Namespace != "frontdoor" {
		t.Fatal("server namespace must come from release", config)
	}
}

func TestServerWitnessIsAbsentUntilAdapterLands(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	if _, present := serverJSON(t, objects)["witness"]; present {
		t.Fatal("unwired witness rendered")
	}
	for _, volume := range serverDeployment(t, objects).Spec.Template.Spec.Volumes {
		if volume.Name == "witness" {
			t.Fatal("unwired witness credential mount rendered")
		}
	}
}

func TestServerRolloutTracksChartSources(t *testing.T) {
	t.Parallel()
	before := serverDeployment(t, rendered(t, nil)).Spec.Template.Annotations
	after := serverDeployment(t, rendered(t, map[string]any{"database": map[string]any{"file": "/var/lib/oberth/restored.db"}})).Spec.Template.Annotations
	if before["checksum/config"] == after["checksum/config"] {
		t.Fatal("changed server source leaves the server using stale configuration")
	}
}

func TestAdoptedServerSectionBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		section, field string
		value          int
		ok             bool
	}{{"logs", "run_limit_bytes", 1, true}, {"logs", "run_limit_bytes", 0, false}, {"schedules", "max_entries", 0, true}, {"schedules", "max_entries", -1, false}, {"schedules", "max_entries", 256, true}, {"schedules", "max_entries", 257, false}, {"assist", "ttl_seconds", 1, true}, {"assist", "ttl_seconds", 0, false}} {
		chart, err := loader.Load(chartPath())
		if err != nil {
			t.Fatal(err)
		}
		_, err = installer.Render(chart, map[string]any{tc.section: map[string]any{tc.field: tc.value}}, "oberth")
		if (err == nil) != tc.ok {
			t.Fatal(tc, err)
		}
	}
}

func TestInstallSuppliesEveryMountedSecretFile(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	objects := rendered(t, f.charts.Previews[0].Values)
	for _, volume := range serverDeployment(t, objects).Spec.Template.Spec.Volumes {
		if volume.Secret == nil {
			continue
		}
		secret, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), volume.Secret.SecretName, metav1.GetOptions{})
		if err != nil {
			t.Fatal("mounted identity was not published", err)
		}
		for _, item := range volume.Secret.Items {
			if len(secret.Data[item.Key]) == 0 {
				t.Fatal("mounted identity file is missing", volume.Name, item.Key)
			}
		}
	}
}
