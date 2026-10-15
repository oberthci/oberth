package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

const controllerServerTestImage = "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func controllerServerFixture(t *testing.T, client *fake.Clientset) *appsv1.Deployment {
	t.Helper()
	labels := map[string]string{"app.kubernetes.io/instance": "oberth", "app.kubernetes.io/name": "oberth"}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth", UID: "deployment-uid", Generation: 2, Labels: labels}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "oberth", Image: controllerServerTestImage, Args: []string{"serve", "--argo-controller-profile=nonroot-static-v1"}}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 2, UpdatedReplicas: 1, AvailableReplicas: 1}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "oberth-rs", Namespace: "oberth", UID: "rs-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: ptr.To(true)}}}, Spec: appsv1.ReplicaSetSpec{Template: *dep.Spec.Template.DeepCopy()}}
	rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "abc"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "oberth-pod", Namespace: "oberth", UID: "server-pod-uid", Labels: labels, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: ptr.To(true)}}}, Spec: *dep.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "oberth", Ready: true, ImageID: controllerServerTestImage, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if _, err := client.AppsV1().Deployments("oberth").Create(t.Context(), dep, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AppsV1().ReplicaSets("oberth").Create(t.Context(), rs, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("oberth").Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	return dep
}

func TestUpgradeControllerActualPodIdentity(t *testing.T) {
	for _, mode := range []string{"ready", "foreign-owner", "foreign-template", "args", "image", "not-ready", "terminating", "extra-old-pod", "changed-during-exec", "wrong-version"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, _ := controllerUpgradeFixture(t)
			client := controllerProfileKube(t, "oberth-argo")
			deps.KubeClient = client
			dep := controllerServerFixture(t, client)
			pod, err := client.CoreV1().Pods("oberth").Get(t.Context(), "oberth-pod", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "foreign-owner":
				pod.OwnerReferences[0].UID = "foreign"
			case "args":
				pod.Spec.Containers[0].Args = []string{"serve"}
			case "image":
				pod.Spec.Containers[0].Image = "foreign"
			case "not-ready":
				pod.Status.ContainerStatuses[0].Ready = false
			case "terminating":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			case "extra-old-pod":
				old := pod.DeepCopy()
				old.Name = "old-pod"
				old.UID = "old-uid"
				now := metav1.Now()
				old.DeletionTimestamp = &now
				_, err = client.CoreV1().Pods("oberth").Create(t.Context(), old, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			case "foreign-template":
				rs, e := client.AppsV1().ReplicaSets("oberth").Get(t.Context(), "oberth-rs", metav1.GetOptions{})
				if e != nil {
					t.Fatal(e)
				}
				rs.Spec.Template.Spec.Containers[0].Args = []string{"foreign"}
				_, err = client.AppsV1().ReplicaSets("oberth").Update(t.Context(), rs, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = client.CoreV1().Pods("oberth").Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			execs := 0
			deps.RunCommand = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
				execs++
				if name != "kubectl" || !slices.Contains(args, "oberth-pod") || slices.Contains(args, "deploy/oberth") {
					t.Fatal("not fixed Pod exec")
				}
				if mode == "changed-during-exec" {
					pod.UID = "replacement"
					if _, err := client.CoreV1().Pods("oberth").Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "wrong-version" {
					return []byte("oberth v0.17.22"), nil
				}
				return []byte("oberth v0.17.23"), nil
			}
			err = verifyControllerServer(t.Context(), cfg, deps, dep)
			if (err == nil) != (mode == "ready") {
				t.Fatalf("mode %s outcome %v", mode, err)
			}
			if mode == "ready" || mode == "changed-during-exec" || mode == "wrong-version" {
				if execs != 1 {
					t.Fatal("expected one exact Pod exec")
				}
			} else if execs != 0 {
				t.Fatal("exec before ownership proof")
			}
		})
	}
}

func TestRunUpgradeControllerPartialFailureNeverSucceeds(t *testing.T) {
	chart, err := os.ReadFile("testdata/argo-workflows-1.0.24.tgz")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"argo-failure", "oberth-failure", "rollout-failure", "version-failure", "pod-failure", "complete", "complete-terminal-history", "ambiguous-controller"} {
		t.Run(mode, func(t *testing.T) {
			cfg, deps, calls := controllerUpgradeFixture(t)
			client := controllerProfileKube(t, "oberth-argo")
			complete := mode == "complete" || mode == "complete-terminal-history"
			if mode == "complete-terminal-history" || mode == "ambiguous-controller" {
				raw, err := os.ReadFile("../argojob/testdata/controller-terminal-history.json")
				if err != nil {
					t.Fatal(err)
				}
				var history []corev1.Pod
				if err := json.Unmarshal(raw, &history); err != nil {
					t.Fatal(err)
				}
				for i := range history {
					if _, err := client.CoreV1().Pods("oberth-argo").Create(t.Context(), &history[i], metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "ambiguous-controller" {
					other, err := client.CoreV1().Pods("oberth-argo").Get(t.Context(), "controller-fixture", metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					other.Name, other.UID = "another-controller", "another-controller-uid"
					if _, err := client.CoreV1().Pods("oberth-argo").Create(t.Context(), other, metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			deps.KubeClient = client
			dep := controllerServerFixture(t, client)
			if mode == "rollout-failure" {
				dep.Status.UpdatedReplicas = 0
				if _, err := client.AppsV1().Deployments("oberth").UpdateStatus(t.Context(), dep, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "pod-failure" {
				if err := client.CoreV1().Pods("oberth").Delete(t.Context(), "oberth-pod", metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			base := deps.RunHelm
			deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "show" {
					return []byte("image:\n  ref: " + controllerServerTestImage + "\n"), nil
				}
				if args[0] == "pull" {
					dir := args[slices.Index(args, "--destination")+1]
					return nil, os.WriteFile(filepath.Join(dir, "argo-workflows-1.0.24.tgz"), chart, 0o600)
				}
				raw, err := base(ctx, args)
				if args[0] == "upgrade" && ((mode == "argo-failure" && args[1] == "argo-workflows") || (mode == "oberth-failure" && args[1] == "oberth")) {
					return nil, errors.New("fixed fixture failure")
				}
				return raw, err
			}
			deps.RunCommand = func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
				if mode == "version-failure" {
					return []byte("oberth v0.17.22"), nil
				}
				return []byte("oberth v0.17.23"), nil
			}
			fixtureActions := len(client.Actions())
			result, err := RunUpgrade(t.Context(), cfg, deps)
			if (err == nil) != complete || result.Upgraded != complete {
				t.Fatalf("partial transition mislabeled: %+v %v", result, err)
			}
			if mode == "ambiguous-controller" && (err == nil || !strings.Contains(err.Error(), "argo profile readback failed")) {
				t.Fatal("did not reach exact controller ambiguity refusal boundary")
			}
			upgrades := []string{}
			for _, call := range *calls {
				if call[0] == "rollback" {
					t.Fatal("rollback attempted")
				}
				if call[0] == "upgrade" {
					upgrades = append(upgrades, call[1])
				}
			}
			want := []string{"argo-workflows", "oberth"}
			if mode == "argo-failure" || mode == "ambiguous-controller" {
				want = want[:1]
			}
			if !slices.Equal(upgrades, want) {
				t.Fatalf("wrong mutation order %v", upgrades)
			}
			if !complete && strings.Contains(deps.Output.(*bytes.Buffer).String(), "Ready —") {
				t.Fatal("partial success output")
			}
			for _, action := range client.Actions()[fixtureActions:] {
				if action.GetResource().Resource == "pods" && slices.Contains([]string{"delete", "delete-collection", "update", "patch"}, action.GetVerb()) {
					t.Fatal("profile verification mutated historical or current Pods")
				}
			}
		})
	}
}
