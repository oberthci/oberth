package installer

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// One test contains the independently observable hostile controls so a
// single-package, single-test overlay can exercise the reverted guards.
func TestS6Round2Guards(t *testing.T) {
	t.Run("ambient-tokens", func(t *testing.T) {
		leaf := wfv1.Template{Name: "granted", AutomountServiceAccountToken: ptr.To(true), Container: &corev1.Container{Image: "oberth"}, InitContainers: []wfv1.UserContainer{{Container: corev1.Container{Name: "prepare"}}}, Sidecars: []wfv1.UserContainer{{Container: corev1.Container{Name: "sidecar"}}}}
		wf := &wfv1.Workflow{}
		wf.Spec.AutomountServiceAccountToken = ptr.To(true)
		wf.Spec.TemplateDefaults = &wfv1.Template{AutomountServiceAccountToken: ptr.To(true)}
		wf.Spec.Templates = []wfv1.Template{leaf, {Name: "ungranted", Container: &corev1.Container{Image: "oberth"}}}
		siblings := wfv1.Template{Name: "siblings", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main"}}, {Container: corev1.Container{Name: "other"}}}}}
		inline := wfv1.Template{Name: "inline", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "child", Inline: leaf.DeepCopy()}}}}}
		wf.Spec.Templates = append(wf.Spec.Templates, siblings, inline)
		if err := argoworkflow.ForceIdentity(wf, argoworkflow.Identity{Namespace: "pipeline", ServiceAccountName: "step", ExecutorServiceAccountName: "executor"}); err != nil {
			t.Fatal(err)
		}
		var check func(reflect.Value, string)
		check = func(v reflect.Value, where string) {
			if v.Kind() == reflect.Pointer {
				if !v.IsNil() {
					check(v.Elem(), where)
				}
				return
			}
			switch v.Kind() {
			case reflect.Struct:
				for i := 0; i < v.NumField(); i++ {
					f := v.Type().Field(i)
					if !f.IsExported() {
						continue
					}
					value := v.Field(i)
					if f.Name == "AutomountServiceAccountToken" && (value.IsNil() || value.Elem().Bool()) {
						t.Errorf("ambient token enabled at %s.%s (including helpers in that Pod)", where, f.Name)
					}
					check(value, where+"."+f.Name)
				}
			case reflect.Slice:
				for i := 0; i < v.Len(); i++ {
					check(v.Index(i), where+"[]")
				}
			}
		}
		check(reflect.ValueOf(wf), "workflow")
	})
	t.Run("unscoped-grant-key", func(t *testing.T) {
		if err := (model.SecretAccessKey{Repo: "upstream/org/repo", Step: "publish", Path: "oberth/data/release/token"}).Validate(); err == nil {
			t.Fatal("unscoped grant accepted")
		}
	})
	t.Run("unscoped-installer-fallback", func(t *testing.T) {
		const data = "- repo: upstream/org/repo\n  step: publish\n  secret: oberth/data/release/token\n"
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: secretAccessConfigMapName, Namespace: "oberth"}, Data: map[string]string{secretAccessConfigMapKey: data}}
		client := fake.NewClientset(cm)
		if identities, err := ParseAccessListJSON([]byte(`[{"repo":"upstream/org/repo","step":"publish","secret":"oberth/data/release/token"}]`)); err == nil || !strings.Contains(err.Error(), "workflow") || len(identities) != 0 {
			t.Errorf("installer silently accepted unscoped API approval: identities=%v err=%v", identities, err)
		}
		saved, err := client.CoreV1().ConfigMaps("oberth").Get(t.Context(), secretAccessConfigMapName, metav1.GetOptions{})
		if err != nil || saved.Data[secretAccessConfigMapKey] != data {
			t.Fatal("installer rewrote unscoped approval", saved, err)
		}
	})
	t.Run("committed-migration-only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "oberth.sqlite")
		db, err := store.CreateGenesis(t.Context(), path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if db.AppliedStartupMigration(store.WorkflowGrantScopeMigrationID) {
			t.Error("fresh database falsely reports approvals cleared")
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db, err = store.OpenCurrent(t.Context(), path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if db.AppliedStartupMigration(store.WorkflowGrantScopeMigrationID) {
			t.Error("no-op reopen falsely reports approvals cleared")
		}
	})
	t.Run("notice-requires-migration-log", func(t *testing.T) {
		since := time.Date(2026, 11, 13, 12, 0, 0, 123, time.UTC)
		for _, log := range []string{"", "oberth: startup ready\n", "oberth: " + SecretApprovalsResetLog + "\n"} {
			var output bytes.Buffer
			calls := 0
			deps := Deps{Output: &output, ContextName: "test", RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
				calls++
				joined := strings.Join(args, " ")
				if name != "kubectl" || !strings.Contains(joined, "--since-time "+since.Format(time.RFC3339Nano)) || !strings.Contains(joined, "--tail=1000") {
					t.Errorf("unbounded or stale log read: %s %v", name, args)
				}
				return []byte(log), nil
			}}
			printSecretApprovalReset(t.Context(), deps, "pipeline", since)
			want := strings.Contains(log, SecretApprovalsResetLog)
			if strings.Contains(output.String(), SecretApprovalsResetNotice) != want {
				t.Errorf("notice does not match committed migration evidence: log=%q output=%q", log, output.String())
			}
			if want && (!strings.Contains(output.String(), "v0.22.5") || !strings.Contains(output.String(), "oberth secretstore sync")) {
				t.Error("notice omits version or pruning instruction")
			}
			if calls != 1 {
				t.Fatal("unexpected log read count", calls)
			}
		}
	})
}
