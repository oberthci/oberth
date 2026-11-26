package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestPrintBackupScheduleIsOfflineAndOperatorApplied(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.base = filepath.Join(t.TempDir(), "absent-profile")
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("printing a schedule contacted the server")
		return nil, context.Canceled
	})
	if code := app.run(t.Context(), []string{"backup", "--print-schedule"}); code != 0 || out.String() != installer.BackupSchedule || diagnostic.Len() != 0 {
		t.Fatal("schedule printing needs a profile or changed its example", code, diagnostic)
	}
	objects := backupScheduleObjects(t, out.Bytes())
	checkBackupScheduleAuthority(t, objects)
	checkBackupSchedulePod(t, objects["CronJob"])
	example, err := os.ReadFile("../../docs/backup-schedule.yaml")
	if err != nil || !bytes.Equal(example, out.Bytes()) {
		t.Fatal("documented schedule differs from the printed example", err)
	}
	for _, text := range []string{"concurrencyPolicy: Forbid", "--in-cluster", "REPLACE_WITH_PINNED_OBERTH_IMAGE"} {
		if !bytes.Contains(out.Bytes(), []byte(text)) {
			t.Fatal("schedule omits explicit operator inputs", strings.TrimSpace(text))
		}
	}
}

func backupScheduleObjects(t *testing.T, raw []byte) map[string]unstructured.Unstructured {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	objects := map[string]unstructured.Unstructured{}
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object.Object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if object.GetNamespace() != installer.Defaults().Namespace || object.GetName() != "oberth-backup" {
			t.Fatal("backup resource escaped the server namespace", object.GetKind())
		}
		if _, duplicate := objects[object.GetKind()]; duplicate {
			t.Fatal("duplicate backup resource kind")
		}
		objects[object.GetKind()] = object
	}
	if _, account := objects["ServiceAccount"]; account || len(objects) != 3 {
		t.Fatal("schedule does not contain exactly its three scoped resources; the chart owns the account", len(objects))
	}
	return objects
}

func decodeBackupObject(t *testing.T, object unstructured.Unstructured, out any) {
	t.Helper()
	raw, err := json.Marshal(object.Object)
	if err != nil || json.Unmarshal(raw, out) != nil {
		t.Fatal("invalid typed backup resource", object.GetKind(), err)
	}
}

func checkBackupScheduleAuthority(t *testing.T, objects map[string]unstructured.Unstructured) {
	t.Helper()
	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	decodeBackupObject(t, objects["Role"], &role)
	decodeBackupObject(t, objects["RoleBinding"], &binding)
	expected := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"oberth-admin", "oberth-ca", "oberth-ssh-host", "oberth-forge-key", "oberth-store-tls", installer.BackupSigningSecret}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"oberth-backup"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, ResourceNames: []string{"oberth-backup"}, Verbs: []string{"create"}},
	}
	if !reflect.DeepEqual(role.Rules, expected) || role.Kind != "Role" {
		t.Fatal("backup role or account has excess authority")
	}
	if binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "oberth-backup"}) || !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: "oberth-backup", Namespace: installer.Defaults().Namespace}}) {
		t.Fatal("backup binding grants authority outside its one account")
	}
}

func checkBackupSchedulePod(t *testing.T, object unstructured.Unstructured) {
	t.Helper()
	var job batchv1.CronJob
	decodeBackupObject(t, object, &job)
	if job.Kind != "CronJob" || job.Spec.Schedule != "0 2 * * *" || job.Spec.ConcurrencyPolicy != batchv1.ForbidConcurrent || job.Spec.JobTemplate.Spec.BackoffLimit == nil || *job.Spec.JobTemplate.Spec.BackoffLimit != 2 {
		t.Fatal("backup schedule lost its explicit native execution bounds")
	}
	pod := job.Spec.JobTemplate.Spec.Template.Spec
	checkBackupPodAuthority(t, pod)
	checkBackupPodSecurity(t, pod)
	checkBackupScheduleVolumes(t, pod)
}

func checkBackupPodAuthority(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	if pod.ServiceAccountName != "oberth-backup" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.RestartPolicy != corev1.RestartPolicyNever || pod.HostNetwork || pod.HostPID || pod.HostIPC || len(pod.Containers) != 1 {
		t.Fatal("backup Pod has excess authority or execution")
	}
	container := pod.Containers[0]
	expectedArgs := []string{"backup", "--in-cluster", "--namespace", "$(SERVER_NAMESPACE)", "--directory", "/backups", "--keep", "7"}
	if !reflect.DeepEqual(container.Args, expectedArgs) || len(container.EnvFrom) != 0 || len(container.Env) != 2 {
		t.Fatal("backup job needs a profile or extra private inputs")
	}
}

func checkBackupPodSecurity(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	security := pod.SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.RunAsUser == nil || *security.RunAsUser == 0 || security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("backup Pod does not meet Restricted security settings")
	}
	checkBackupContainerSecurity(t, pod.Containers[0].SecurityContext)
}

func checkBackupContainerSecurity(t *testing.T, sc *corev1.SecurityContext) {
	t.Helper()
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 || !reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatal("backup container is not hardened")
	}
}

func checkBackupScheduleVolumes(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	if len(pod.Volumes) != 3 || pod.Volumes[0].PersistentVolumeClaim == nil || pod.Volumes[0].PersistentVolumeClaim.ClaimName != "oberth-backups" || pod.Volumes[1].EmptyDir == nil || pod.Volumes[1].EmptyDir.Medium != corev1.StorageMediumMemory || pod.Volumes[2].Projected == nil {
		t.Fatal("backup Pod mounts a profile, kubeconfig or persistent inspection scratch")
	}
	for _, volume := range pod.Volumes {
		if volume.HostPath != nil || volume.Secret != nil {
			t.Fatal("backup Pod mounts a static credential or host path")
		}
	}
	checkBackupProjection(t, pod)
}

func checkBackupProjection(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	projected := pod.Volumes[2].Projected.Sources
	if len(projected) != 2 || projected[0].ServiceAccountToken == nil || projected[0].ServiceAccountToken.ExpirationSeconds == nil || *projected[0].ServiceAccountToken.ExpirationSeconds != 600 || projected[1].ConfigMap == nil || projected[1].ConfigMap.Name != "kube-root-ca.crt" {
		t.Fatal("backup Pod does not use its bounded native projection")
	}
}
