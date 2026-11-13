package argojob

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wfclientset "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func installLiveTaskResultPolicy(t *testing.T, cluster liveCluster) {
	t.Helper()
	body, err := exec.CommandContext(t.Context(), "helm", "template", "oberth", "../../charts/oberth", "--namespace", "oberth", "--set", "image.ref=example.invalid/oberth@sha256:"+strings.Repeat("1", 64), "--set", "argo.namespace="+cluster.namespace, "--show-only", "templates/argo-task-result-policy.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render policy: %v: %s", err, body)
	}
	api := cluster.kube.AdmissionregistrationV1()
	for _, doc := range strings.Split(string(body), "\n---") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			var policy admissionv1.ValidatingAdmissionPolicy
			if err := yaml.Unmarshal([]byte(doc), &policy); err != nil {
				t.Fatal(err)
			}
			if _, err := api.ValidatingAdmissionPolicies().Create(t.Context(), &policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = api.ValidatingAdmissionPolicies().Delete(context.Background(), policy.Name, metav1.DeleteOptions{})
			})
			deadline := time.Now().Add(time.Minute)
			for {
				observed, err := api.ValidatingAdmissionPolicies().Get(t.Context(), policy.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if observed.Status.ObservedGeneration >= observed.Generation {
					if observed.Status.TypeChecking != nil && len(observed.Status.TypeChecking.ExpressionWarnings) != 0 {
						t.Fatalf("policy type warnings: %+v", observed.Status.TypeChecking.ExpressionWarnings)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("policy type checking did not complete")
				}
				time.Sleep(200 * time.Millisecond)
			}
		case "ValidatingAdmissionPolicyBinding":
			var binding admissionv1.ValidatingAdmissionPolicyBinding
			if err := yaml.Unmarshal([]byte(doc), &binding); err != nil {
				t.Fatal(err)
			}
			if _, err := api.ValidatingAdmissionPolicyBindings().Create(t.Context(), &binding, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = api.ValidatingAdmissionPolicyBindings().Delete(context.Background(), binding.Name, metav1.DeleteOptions{})
			})
		}
	}
}

func liveTaskResultAccount(t *testing.T, cluster liveCluster, account string) {
	t.Helper()
	ctx := t.Context()
	t.Cleanup(func() {
		_ = cluster.kube.RbacV1().RoleBindings(cluster.namespace).Delete(context.Background(), account, metav1.DeleteOptions{})
		_ = cluster.kube.RbacV1().Roles(cluster.namespace).Delete(context.Background(), account, metav1.DeleteOptions{})
		_ = cluster.kube.CoreV1().ServiceAccounts(cluster.namespace).Delete(context.Background(), account, metav1.DeleteOptions{})
	})
	if _, err := cluster.kube.CoreV1().ServiceAccounts(cluster.namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: account}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: account}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{"argoproj.io"}, Resources: []string{"workflowtaskresults"}, Verbs: []string{"create", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create", "patch"}},
	}}
	if _, err := cluster.kube.RbacV1().Roles(cluster.namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: account}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: cluster.namespace}}}
	if _, err := cluster.kube.RbacV1().RoleBindings(cluster.namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func liveTaskResultIdentity(t *testing.T, cluster liveCluster, workflow *wfv1.Workflow, account string, bound bool) *rest.Config {
	t.Helper()
	request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(600))}}
	var holder *corev1.Pod
	if bound {
		controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
		if err != nil {
			t.Fatal(err)
		}
		holder, err = controller.executorIdentityPod(t.Context(), workflow, account)
		if err != nil {
			t.Fatal(err)
		}
		request.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: holder.Name, UID: holder.UID}
	}
	token, err := cluster.kube.CoreV1().ServiceAccounts(cluster.namespace).CreateToken(t.Context(), account, request, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// AnonymousClientConfig drops the administrator certificate as well as all
	// other inherited credentials, so this client really uses the issued token.
	config := rest.AnonymousClientConfig(cluster.restConfig)
	config.BearerToken = token.Status.Token
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	review, err := kube.AuthenticationV1().SelfSubjectReviews().Create(t.Context(), &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	user := review.Status.UserInfo
	if user.Username != "system:serviceaccount:"+cluster.namespace+":"+account {
		t.Fatal("client did not authenticate as requested ServiceAccount")
	}
	if bound && (len(user.Extra["authentication.kubernetes.io/pod-name"]) != 1 || user.Extra["authentication.kubernetes.io/pod-name"][0] != holder.Name || len(user.Extra["authentication.kubernetes.io/pod-uid"]) != 1 || user.Extra["authentication.kubernetes.io/pod-uid"][0] != string(holder.UID)) {
		t.Fatal("authenticated claims did not bind the actual holder")
	}
	body, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(t.ArtifactDir(), fmt.Sprintf("authenticated-identity-bound-%t.json", bound)), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return config
}

func livePolicyDenies(t *testing.T, err error, policy string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), policy) {
		t.Fatalf("wanted native %s denial, got %v", policy, err)
	}
}

func liveTaskResultCases(t *testing.T, cluster liveCluster, workflow, foreign *wfv1.Workflow, account string, index int) {
	t.Helper()
	config := liveTaskResultIdentity(t, cluster, workflow, account, true)
	client, err := wfclientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	results := client.ArgoprojV1alpha1().WorkflowTaskResults(cluster.namespace)
	dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	own := taskResultObject(workflow, fmt.Sprintf("%s-%d", workflow.Name, index+1))
	// Wait for admission caches using the hostile request itself; fail if the
	// rejection comes from RBAC rather than the rendered policy.
	wrong := taskResultObject(foreign, foreign.Name)
	deadline := time.Now().Add(time.Minute)
	for {
		_, err = results.Create(t.Context(), wrong, dry)
		if err != nil && strings.Contains(err.Error(), "argo-task-results") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("policy did not enforce: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, object := range []*wfv1.WorkflowTaskResult{taskResultObject(workflow, workflow.Name), own} {
		if _, err := results.Create(t.Context(), object, dry); err != nil {
			t.Fatalf("same-run create: %v", err)
		}
	}
	for name, mutate := range map[string]func(*wfv1.WorkflowTaskResult){
		"foreign-run":   func(o *wfv1.WorkflowTaskResult) { *o = *wrong.DeepCopy() },
		"foreign-owner": func(o *wfv1.WorkflowTaskResult) { o.OwnerReferences[0].UID = foreign.UID },
		"foreign-label": func(o *wfv1.WorkflowTaskResult) { o.Labels["workflows.argoproj.io/workflow"] = foreign.Name },
		"foreign-name":  func(o *wfv1.WorkflowTaskResult) { o.Name = foreign.Name },
		"ambiguous-holder-segments": func(o *wfv1.WorkflowTaskResult) {
			controller := &Controller{config: cluster.config()}
			role := strings.Split(controller.identityPodName(workflow, account), ".")[2]
			o.OwnerReferences[0].UID = types.UID(string(workflow.UID) + "." + role)
			o.OwnerReferences[0].Name = foreign.Name
			o.Labels["workflows.argoproj.io/workflow"] = foreign.Name
			o.Name = foreign.Name
		},
	} {
		t.Run(name, func(t *testing.T) {
			object := own.DeepCopy()
			mutate(object)
			_, err := results.Create(t.Context(), object, dry)
			livePolicyDenies(t, err, "argo-task-results")
		})
	}
	if _, err := results.Create(t.Context(), own, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"metadata":{"annotations":{"r6":"same-run"}}}`)
	if _, err := results.Patch(t.Context(), own.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("same-run patch: %v", err)
	}
	admin, err := wfclientset.NewForConfig(cluster.restConfig)
	if err != nil {
		t.Fatal(err)
	}
	planted := taskResultObject(foreign, fmt.Sprintf("%s-%d", workflow.Name, index+100))
	if _, err := admin.ArgoprojV1alpha1().WorkflowTaskResults(cluster.namespace).Create(t.Context(), planted, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	takeover := taskResultObject(workflow, planted.Name)
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": takeover.OwnerReferences, "labels": takeover.Labels}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = results.Patch(t.Context(), planted.Name, types.MergePatchType, body, metav1.PatchOptions{})
	livePolicyDenies(t, err, "argo-task-results")
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, generated := range []bool{false, true} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "oberth-executor.forged.executor.run"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: defaultSourceSeedImage}}}}
		if generated {
			pod.Name = ""
			pod.GenerateName = "oberth-executor.forged-"
		}
		_, err := kube.CoreV1().Pods(cluster.namespace).Create(t.Context(), pod, dry)
		livePolicyDenies(t, err, "argo-executor-holders")
	}
	unbound, err := wfclientset.NewForConfig(liveTaskResultIdentity(t, cluster, workflow, account, false))
	if err != nil {
		t.Fatal(err)
	}
	unboundResult := taskResultObject(workflow, fmt.Sprintf("%s-%d", workflow.Name, index+500))
	_, err = unbound.ArgoprojV1alpha1().WorkflowTaskResults(cluster.namespace).Create(t.Context(), unboundResult, dry)
	livePolicyDenies(t, err, "argo-task-results")
}

func TestLiveWorkflowTaskResultPolicy(t *testing.T) {
	cluster := requireLiveCluster(t)
	installLiveTaskResultPolicy(t, cluster)
	var workflows []*wfv1.Workflow
	for _, name := range []string{"executor.r6-policy-own", "r6-policy-own"} {
		wf, err := cluster.workflows.Create(t.Context(), &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: wfv1.WorkflowSpec{Entrypoint: "pause", Suspend: ptr.To(true), Templates: []wfv1.Template{{Name: "pause", Suspend: &wfv1.SuspendTemplate{}}}}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cluster.workflows.Delete(context.Background(), wf.Name, metav1.DeleteOptions{}) })
		workflows = append(workflows, wf)
	}
	for i, account := range []string{"oberth-argo-executor", "r6-operator-capability", "r6-per-step"} {
		liveTaskResultAccount(t, cluster, account)
		t.Run(account, func(t *testing.T) { liveTaskResultCases(t, cluster, workflows[0], workflows[1], account, i) })
	}
}
