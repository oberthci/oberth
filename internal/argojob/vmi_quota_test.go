package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func guardedVMIFixture(t *testing.T, id int) (Config, *wfv1.Workflow, *corev1.Pod) {
	t.Helper()
	return guardedVMIRepositoryFixture(t, id, 7)
}

func guardedVMIRepositoryFixture(t *testing.T, id int, repoID int64) (Config, *wfv1.Workflow, *corev1.Pod) {
	t.Helper()
	config := testConfig()
	config.applyDefaults()
	config.VMLauncherServiceAccount = "vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	r := testRequest(periapsis.TriggerCI, kubeVirtWorkflow)
	r.KubeVirtGranted = true
	r.RepositoryID = repoID
	r.Name = fmt.Sprintf("quota-%d", id)
	wf, err := Build(config, r)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = types.UID(fmt.Sprintf("workflow-%d", id))
	wf.Status.Nodes = wfv1.Nodes{"node": {ID: "node", TemplateName: "vm"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("resource-%d", id), Namespace: wf.Namespace, UID: types.UID(fmt.Sprintf("pod-%d", id)), Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "node"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: argoworkflow.NonrootExecutorImage, Command: []string{"argoexec", "resource", "get"}}}}}
	return config, wf, pod
}

// API-like ConfigMap resourceVersion behavior, independent of the quota logic.
// Kubernetes fakes otherwise accept conflicting writes and mask lost updates.
func casQuotaClient() *fake.Clientset {
	client := fake.NewClientset()
	var mu sync.Mutex
	stored := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: vmiQuotaConfigMap, Namespace: testNamespace, ResourceVersion: "1", Labels: map[string]string{"oberth.ci/role": "vmi-reservations"}}}
	revision := 1
	client.PrependReactor("*", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() != "get" && a.GetVerb() != "create" && a.GetVerb() != "update" {
			return false, nil, nil
		}
		name := ""
		switch a.GetVerb() {
		case "get":
			name = a.(ktesting.GetAction).GetName()
		case "create", "update":
			name = a.(interface{ GetObject() runtime.Object }).GetObject().(*corev1.ConfigMap).Name
		}

		if name != vmiQuotaConfigMap {
			return false, nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		switch a.GetVerb() {
		case "get":
			if stored == nil {
				return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, vmiQuotaConfigMap)
			}
			return true, stored.DeepCopy(), nil
		case "create":
			if stored != nil {
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, vmiQuotaConfigMap)
			}
			stored = a.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		case "update":
			value := a.(ktesting.UpdateAction).GetObject().(*corev1.ConfigMap)
			if stored == nil || value.ResourceVersion != stored.ResourceVersion {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, vmiQuotaConfigMap, errors.New("stale resourceVersion"))
			}
			stored = value.DeepCopy()
		}
		revision++
		stored.ResourceVersion = fmt.Sprint(revision)
		return true, stored.DeepCopy(), nil
	})
	return client
}

func quotaDynamic() *dynamicfake.FakeDynamicClient {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmiResource: "VirtualMachineInstanceList"})
	var serial atomic.Int64
	client.PrependReactor("create", "virtualmachineinstances", func(a ktesting.Action) (bool, runtime.Object, error) {
		object := a.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID(fmt.Sprintf("vmi-%d", serial.Add(1))))
		return false, nil, nil
	})
	return client
}

func quotaController(config Config, kube *fake.Clientset, vmi dynamic.Interface) *Controller {
	return &Controller{config: config, kube: kube, vmi: vmi, vmiCreate: func(ctx context.Context, namespace string, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		return vmi.Resource(vmiResource).Namespace(namespace).Create(ctx, object, metav1.CreateOptions{})
	}}
}

func TestVMICreateRefusesOverGrantAcrossConcurrentControllers(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	var group sync.WaitGroup
	var successes atomic.Int64
	for i := range 8 {
		config, wf, pod := guardedVMIFixture(t, i)
		if _, err := kube.CoreV1().Pods(config.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		c := quotaController(config, kube, vmi)
		group.Add(1)
		go func() {
			defer group.Done()
			err := authorizeTestVMI(t, c, wf, pod)
			if err == nil {
				successes.Add(1)
			} else if !strings.Contains(err.Error(), "over grant") && !strings.Contains(err.Error(), "conflicts exhausted") {
				t.Errorf("unexpected refusal: %v", err)
			}
		}()
	}
	group.Wait()
	objects, err := vmi.Resource(vmiResource).Namespace(testConfig().Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if successes.Load() != 2 || len(objects.Items) != 2 {
		t.Fatalf("over grant: successes=%d VMIs=%d", successes.Load(), len(objects.Items))
	}
}

func TestVMICreatePendingTimeoutSurvivesRestartAndAbsence(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	calls := 0
	vmi.PrependReactor("create", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		return true, nil, context.DeadlineExceeded
	})
	c := quotaController(config, kube, vmi)
	if err := authorizeTestVMI(t, c, wf, pod); err == nil {
		t.Fatal("timeout accepted")
	}
	// A restart cannot retry an ambiguous POST, even if a GET finds no object.
	c = quotaController(config, kube, vmi)
	if err := authorizeTestVMI(t, c, wf, pod); err == nil || !strings.Contains(err.Error(), "unresolved VMI create") {
		t.Fatalf("ambiguous retry: %v", err)
	}
	// The first pending VM counts against the grant alongside the second one.
	_, wf2, pod2 := guardedVMIFixture(t, 2)
	if err := authorizeTestVMI(t, c, wf2, pod2); err == nil {
		t.Fatal("second timeout accepted")
	}
	_, wf3, pod3 := guardedVMIFixture(t, 3)
	if err := authorizeTestVMI(t, c, wf3, pod3); err == nil || !strings.Contains(err.Error(), "over grant") {
		t.Fatalf("pending credits refunded: %v", err)
	}
	if calls != 2 {
		t.Fatalf("ambiguous create retried or over-grant submitted: %d", calls)
	}
}

func TestVMICreateDeletionBarrierAllowsFreshSameName(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	c := quotaController(config, kube, vmi)
	if _, err := kube.CoreV1().Pods(config.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := authorizeTestVMI(t, c, wf, pod); err != nil {
		t.Fatal(err)
	}
	// Token delivery is idempotent for one operation; it cannot create twice.
	if err := authorizeTestVMI(t, c, wf, pod); err != nil {
		t.Fatal(err)
	}
	old, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Delete(t.Context(), old.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	next := pod.DeepCopy()
	next.Name += "-next"
	next.UID += "-next"
	if err := authorizeTestVMI(t, c, wf, next); err != nil {
		t.Fatal(err)
	}
	fresh, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), old.GetName(), metav1.GetOptions{})
	if err != nil || fresh.GetUID() == old.GetUID() {
		t.Fatalf("same-name create was not fresh: %v", err)
	}
	if err := authorizeTestVMI(t, c, wf, pod); err == nil {
		t.Fatal("closed operation recreated after its delete")
	}
}

func TestVMICreateRefusesUnknownStateAndUnguardedPods(t *testing.T) {
	for _, kind := range []string{"no-client", "old-pod", "missing-binding", "poison-ledger", "list-failure"} {
		t.Run(kind, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			config, wf, pod := guardedVMIFixture(t, 1)
			c := quotaController(config, kube, vmi)
			switch kind {
			case "no-client":
				c.vmi = nil
			case "old-pod":
				pod.Spec.Containers[0].Command[2] = "create"
			case "missing-binding":
				delete(wf.Annotations, vmiQuotaAnnotation)
			case "poison-ledger":
				cm, err := kube.CoreV1().ConfigMaps(config.Namespace).Get(t.Context(), vmiQuotaConfigMap, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				cm.Data = map[string]string{"reservations.json": "invalid"}
				if _, err = kube.CoreV1().ConfigMaps(config.Namespace).Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "list-failure":
				vmi.PrependReactor("list", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("API unavailable") })
			}
			if err := authorizeTestVMI(t, c, wf, pod); err == nil {
				t.Fatal("unknown state authorized a VMI")
			}
			for _, a := range vmi.Actions() {
				if a.GetVerb() == "create" {
					t.Fatal("refused request reached VMI create")
				}
			}
		})
	}
}

func TestVMICreatePOSTIsNeverAutomaticallyRetried(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("timeout") != vmiAPIRequestTimeout.String() {
			t.Errorf("API request has no bounded server timeout: %s", r.URL)
		}
		if r.Method != "POST" || r.URL.Path != "/apis/kubevirt.io/v1/namespaces/pipeline/virtualmachineinstances" {
			t.Errorf("unexpected create: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"ServiceUnavailable","code":503}`))
	}))
	defer server.Close()
	client, err := rest.UnversionedRESTClientFor(dynamic.ConfigFor(&rest.Config{Host: server.URL}))
	if err != nil {
		t.Fatal(err)
	}
	c := (&Controller{}).WithVMICreateClient(client)
	_, err = c.vmiCreate(t.Context(), "pipeline", &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance", "metadata": map[string]any{"name": "test"}}})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("ambiguous POST was retried: calls=%d err=%v", calls.Load(), err)
	}
}

func TestVMICreateExplicitRejectionReleasesPendingClaim(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	c := quotaController(config, kube, vmi)
	if _, err := kube.CoreV1().Pods(config.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rejected := true
	vmi.PrependReactor("create", "virtualmachineinstances", func(ktesting.Action) (bool, runtime.Object, error) {
		if rejected {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kubevirt.io", Resource: "virtualmachineinstances"}, "vm", errors.New("policy"))
		}
		return false, nil, nil
	})
	if err := authorizeTestVMI(t, c, wf, pod); err == nil {
		t.Fatal("rejection succeeded")
	}
	rejected = false
	_, next, nextPod := guardedVMIFixture(t, 2)
	if err := authorizeTestVMI(t, c, next, nextPod); err != nil {
		t.Fatal(err)
	}
	// The definitive refusal is not permission to retry the same operation.
	if err := authorizeTestVMI(t, c, wf, pod); err == nil {
		t.Fatal("closed rejected operation retried")
	}
}

func TestVMICreateBrokerRefusesBeforeAnyTokenIsIssued(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, first, firstPod := guardedVMIFixture(t, 1)
	c := quotaController(config, kube, vmi)
	if err := authorizeTestVMI(t, c, first, firstPod); err != nil {
		t.Fatal(err)
	}
	_, second, secondPod := guardedVMIFixture(t, 2)
	if err := authorizeTestVMI(t, c, second, secondPod); err != nil {
		t.Fatal(err)
	}
	_, wf, pod := guardedVMIFixture(t, 3)
	pod.Spec.Volumes = []corev1.Volume{{Name: executorTokenVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}}
	pod.Spec.InitContainers = []corev1.Container{{Name: executorTokenWaiter, Image: config.SourceSeedImage, Command: []string{"sh", "-c", executorTokenWaitCommand}, VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: executorTokenWaiter, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	if _, err := kube.CoreV1().Pods(config.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	requests, deliveries := 0, 0
	kube.PrependReactor("create", "serviceaccounts", func(ktesting.Action) (bool, runtime.Object, error) {
		requests++
		return true, nil, errors.New("token should not be requested")
	})
	c.seeder = &SourceSeeder{exec: func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		deliveries++
		return nil
	}}
	if err := c.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err == nil || !strings.Contains(err.Error(), "over grant") {
		t.Fatalf("over-grant Pod got through broker: %v", err)
	}
	if requests != 0 || deliveries != 0 {
		t.Fatalf("over-grant VMI received a token: requests=%d deliveries=%d", requests, deliveries)
	}
}

// This Pod is emitted by the actual v4.0.8 controller, not a local model of
// strategic merge or executor wrapping. See the adjacent fixture provenance.
func TestVMICreateGuardAcceptsActualArgoPod(t *testing.T) {
	data, err := os.ReadFile("testdata/vmi-quota-argo4.0.8/vmi-submitted.json")
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err = json.Unmarshal(data, &pod); err != nil {
		t.Fatal(err)
	}
	for _, main := range pod.Spec.Containers {
		if main.Name != "main" {
			continue
		}
		if main.Image != argoworkflow.NonrootExecutorImage || !guardedVMIGetCommand(main.Command, main.Args) {
			t.Fatalf("actual Argo Pod refused: %+v", main)
		}
		bad := append([]string(nil), main.Command...)
		bad[len(bad)-1] = "create"
		if guardedVMIGetCommand(bad, nil) || guardedVMIGetCommand(main.Command, []string{"--"}) {
			t.Fatal("unguarded command accepted")
		}
		config := testConfig()
		config.applyDefaults()
		if err = validateExecutorTokenTarget(&pod, config.SourceSeedImage, true); err != nil {
			t.Fatal(err)
		}
		config, wf, _ := guardedVMIFixture(t, 1)
		input, err := os.ReadFile("testdata/vmi-quota-argo4.0.8/vmi-workflow.json")
		if err != nil {
			t.Fatal(err)
		}
		var captured wfv1.Workflow
		if err = json.Unmarshal(input, &captured); err != nil {
			t.Fatal(err)
		}
		currentJSON, err := json.Marshal(wf)
		if err != nil {
			t.Fatal(err)
		}
		capturedJSON, err := json.Marshal(&captured)
		if err != nil {
			t.Fatal(err)
		}
		if string(currentJSON) != string(capturedJSON) {
			t.Fatal("Build input changed; regenerate the actual Argo controller proof")
		}

		pod.UID = "actual-argo-resource-pod"
		wf.Status.Nodes = wfv1.Nodes{pod.Annotations["workflows.argoproj.io/node-id"]: {TemplateName: "vm"}}
		if err = authorizeTestVMI(t, quotaController(config, casQuotaClient(), quotaDynamic()), wf, &pod); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("no main container")
}

func TestVMICreateChecksEachBudgetDimension(t *testing.T) {
	for _, dimension := range []string{"VMs", "cores", "memory", "workflow"} {
		t.Run(dimension, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			config, wf, pod := guardedVMIFixture(t, 1)
			c := quotaController(config, kube, vmi)
			if err := authorizeTestVMI(t, c, wf, pod); err != nil {
				t.Fatal(err)
			}
			_, next, np := guardedVMIFixture(t, 2)
			var binding vmiQuotaBinding
			if err := json.Unmarshal([]byte(next.Annotations[vmiQuotaAnnotation]), &binding); err != nil {
				t.Fatal(err)
			}
			binding.Limit = argoworkflow.VMIDemand{VMs: 16, Cores: 64, MemoryBytes: 256 << 30}
			switch dimension {
			case "VMs":
				binding.Limit.VMs = 1
			case "cores":
				binding.Limit.Cores = 3
			case "memory":
				binding.Limit.MemoryBytes = 3 << 30
			case "workflow":
				next = wf.DeepCopy()
				np = pod.DeepCopy()
				np.UID += "-next"
				np.Name += "-next"
			}
			data, err := json.Marshal(binding)
			if err != nil {
				t.Fatal(err)
			}
			next.Annotations[vmiQuotaAnnotation] = string(data)
			if err = authorizeTestVMI(t, c, next, np); err == nil || !strings.Contains(err.Error(), "over grant") {
				t.Fatalf("%s not enforced: %v", dimension, err)
			}
		})
	}
}

func TestVMICreateUnknownLiveVMIStillConsumesCapacity(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	c := quotaController(config, kube, vmi)
	if err := authorizeTestVMI(t, c, wf, pod); err != nil {
		t.Fatal(err)
	}
	objects, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := objects.Items[0].DeepCopy()
	legacy.SetName("legacy-vmi")
	legacy.SetLabels(nil)
	legacy.SetAnnotations(nil)
	legacy.SetOwnerReferences(nil)
	if _, err = vmi.Resource(vmiResource).Namespace(wf.Namespace).Create(t.Context(), legacy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, next, np := guardedVMIFixture(t, 2)
	if err = authorizeTestVMI(t, c, next, np); err == nil || !strings.Contains(err.Error(), "over grant") {
		t.Fatalf("legacy VMI was ignored: %v", err)
	}
}

func TestVMICreateRecoveryRejectsDifferentOperation(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	c := quotaController(config, kube, vmi)
	if err := authorizeTestVMI(t, c, wf, pod); err != nil {
		t.Fatal(err)
	}
	object, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a := object.GetAnnotations()
	a[vmiOperationAnnotation] = "different-pod"
	object.SetAnnotations(a)
	if _, err = vmi.Resource(vmiResource).Namespace(wf.Namespace).Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = authorizeTestVMI(t, c, wf, pod); err == nil {
		t.Fatal("foreign operation consumed existing claim")
	}
}

func authorizeTestVMI(t *testing.T, c *Controller, wf *wfv1.Workflow, pod *corev1.Pod) error {
	t.Helper()
	if _, err := c.kube.CoreV1().Pods(pod.Namespace).Get(t.Context(), pod.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err = c.kube.CoreV1().Pods(pod.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			return err
		}
	}
	return c.ensureVMIForResourcePod(t.Context(), wf, pod)
}

func TestVMICreateStaleObserverCannotResurrectPrunedOperation(t *testing.T) {
	kube, vmi := casQuotaClient(), quotaDynamic()
	config, wf, pod := guardedVMIFixture(t, 1)
	c := quotaController(config, kube, vmi)
	if err := authorizeTestVMI(t, c, wf, pod); err != nil {
		t.Fatal(err)
	}
	if err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Delete(t.Context(), wf.Name+"-amd64", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := kube.CoreV1().Pods(pod.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, next, np := guardedVMIFixture(t, 2)
	if err := authorizeTestVMI(t, c, next, np); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureVMIForResourcePod(t.Context(), wf, pod); err == nil {
		t.Fatal("stale deleted Pod recreated a VMI")
	}
	objects, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(objects.Items) != 1 {
		t.Fatal(objects, err)
	}
}

func TestVMIQuotaBindingIsPartOfAdoptionIdentity(t *testing.T) {
	_, wf, _ := guardedVMIFixture(t, 1)
	hash, err := adoptionSpecHash(wf)
	if err != nil {
		t.Fatal(err)
	}
	binding := Adoption{UID: wf.UID, Identity: wf.Annotations[identityAnnotation], SpecSHA256: hash}
	if !binding.matches(wf) {
		t.Fatal("original does not match")
	}
	wf.Annotations[vmiQuotaAnnotation] = "{}"
	if binding.matches(wf) {
		t.Fatal("mutated quota binding still matches adoption")
	}
}

func TestVMICreateLiveTopologyAndHotplugBounds(t *testing.T) {
	for _, kind := range []string{"sockets", "hotplug-memory", "unparseable"} {
		t.Run(kind, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			config, wf, pod := guardedVMIFixture(t, 1)
			c := quotaController(config, kube, vmi)
			if err := authorizeTestVMI(t, c, wf, pod); err != nil {
				t.Fatal(err)
			}
			object, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "sockets":
				err = unstructured.SetNestedField(object.Object, int64(4), "spec", "domain", "cpu", "sockets")
			case "hotplug-memory":
				err = unstructured.SetNestedField(object.Object, "12Gi", "spec", "domain", "memory", "maxGuest")
			case "unparseable":
				err = unstructured.SetNestedField(object.Object, "invalid", "spec", "domain", "memory", "guest")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = vmi.Resource(vmiResource).Namespace(wf.Namespace).Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			_, next, np := guardedVMIFixture(t, 2)
			if err = authorizeTestVMI(t, c, next, np); err == nil {
				t.Fatalf("%s was treated as free capacity", kind)
			}
		})
	}
}

func TestVMIClaimLostResponseAndFinishFailureRecoverCapacity(t *testing.T) {
	for _, failure := range []string{"lost-response", "finish-failure"} {
		t.Run(failure, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			config, wf, pod := guardedVMIFixture(t, 1)
			c := quotaController(config, kube, vmi)
			normal := c.vmiCreate
			if failure == "lost-response" {
				c.vmiCreate = func(ctx context.Context, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
					_, err := normal(ctx, ns, obj)
					if err != nil {
						return nil, err
					}
					return nil, context.DeadlineExceeded
				}
			}
			failFinish := failure == "finish-failure"
			kube.PrependReactor("update", "configmaps", func(a ktesting.Action) (bool, runtime.Object, error) {
				if !failFinish {
					return false, nil, nil
				}
				cm := a.(ktesting.UpdateAction).GetObject().(*corev1.ConfigMap)
				var ledger vmiLedger
				_ = json.Unmarshal([]byte(cm.Data["reservations.json"]), &ledger)
				for _, claim := range ledger.Claims {
					if claim.UID != "" {
						failFinish = false
						return true, nil, errors.New("finish update lost")
					}
				}
				return false, nil, nil
			})
			if err := authorizeTestVMI(t, c, wf, pod); err == nil {
				t.Fatal("injected failure did not happen")
			}
			// Restart: a different operation must persist recovery of the first UID.
			c = quotaController(config, kube, vmi)
			_, second, sp := guardedVMIFixture(t, 2)
			if err := authorizeTestVMI(t, c, second, sp); err != nil {
				t.Fatal(err)
			}
			if err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Delete(t.Context(), wf.Name+"-amd64", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := kube.CoreV1().Pods(pod.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			_, third, tp := guardedVMIFixture(t, 3)
			if err := authorizeTestVMI(t, c, third, tp); err != nil {
				t.Fatalf("one live VMI leaked a second slot: %v", err)
			}
		})
	}
}

func TestVMIClaimAbandonedBeforePOSTSelfHeals(t *testing.T) {
	for _, scenario := range []string{"expired-gone", "expired-present", "recent-gone", "untimestamped-gone", "partial-pod-list"} {
		t.Run(scenario, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			config, wf, pod := guardedVMIFixture(t, 1)
			c := quotaController(config, kube, vmi)
			var binding vmiQuotaBinding
			if err := json.Unmarshal([]byte(wf.Annotations[vmiQuotaAnnotation]), &binding); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 2; i++ {
				_, w, p := guardedVMIFixture(t, i)
				if _, err := kube.CoreV1().Pods(p.Namespace).Create(t.Context(), p, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				claim := vmiClaim{RepositoryID: 7, PodName: p.Name, PodUID: p.UID, WorkflowUID: w.UID, Name: w.Name + "-amd64", Hash: strings.Repeat("a", 64), Demand: binding.Peak}
				if _, fresh, err := c.reserveVMIClaim(t.Context(), fmt.Sprint(i), claim, binding); err != nil || !fresh {
					t.Fatalf("reserve without POST: fresh=%v err=%v", fresh, err)
				}
			}
			cm, err := c.getVMILedger(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := readVMILedger(cm)
			if err != nil {
				t.Fatal(err)
			}
			claim := ledger.Claims["1"]
			if claim.ReservedAt.IsZero() {
				t.Fatal("claim not timestamped before POST")
			}
			switch scenario {
			case "recent-gone":
				claim.ReservedAt = time.Now().UTC()
			case "untimestamped-gone":
				claim.ReservedAt = time.Time{}
			default:
				claim.ReservedAt = time.Now().Add(-2 * vmiPendingGracePeriod).UTC()
			}
			ledger.Claims["1"] = claim
			if err = c.updateVMILedger(t.Context(), cm, ledger); err != nil {
				t.Fatal(err)
			}
			if scenario != "expired-present" {
				if err = kube.CoreV1().Pods(pod.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "partial-pod-list" {
				kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "more"}}, nil
				})
			}
			_, third, tp := guardedVMIFixture(t, 3)
			err = authorizeTestVMI(t, c, third, tp)
			if scenario == "expired-gone" {
				if err != nil {
					t.Fatalf("crash before POST leaked capacity: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "over grant") {
				t.Fatalf("unsafe expiry for %s: %v", scenario, err)
			}
			for _, a := range vmi.Actions() {
				if a.GetVerb() == "create" && scenario != "expired-gone" {
					t.Fatal("unexpired claims allowed a POST")
				}
			}
		})
	}
}

func TestVMIFinishClaimIgnoresCallerCancellation(t *testing.T) {
	claim := vmiClaim{ReservedAt: time.Now().UTC(), RepositoryID: 7, PodName: "resource", PodUID: "pod", WorkflowUID: "wf", Name: "vm", Hash: strings.Repeat("a", 64), Demand: argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 2 << 30}}
	data, err := json.Marshal(vmiLedger{Version: 1, Claims: map[string]vmiClaim{"claim": claim}})
	if err != nil {
		t.Fatal(err)
	}
	stored := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: vmiQuotaConfigMap, Namespace: testNamespace, ResourceVersion: "1", Labels: map[string]string{"oberth.ci/role": "vmi-reservations"}, Annotations: map[string]string{vmiLedgerInitialized: "1"}}, Data: map[string]string{"reservations.json": string(data)}}
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == "PUT" {
			var updated corev1.ConfigMap
			if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			stored = &updated
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stored)
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &Controller{kube: client, config: Config{Namespace: testNamespace}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = c.finishVMIClaim(ctx, "claim", "observed-vmi-uid", false); err != nil {
		t.Fatalf("canceled caller prevented durable finish: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	ledger, err := readVMILedger(stored)
	if err != nil || ledger.Claims["claim"].UID != "observed-vmi-uid" {
		t.Fatalf("UID not persisted: %+v %v", ledger, err)
	}
}

func TestVMIClaimMalformedOrMismatchedVMIIsolatedByRepository(t *testing.T) {
	for _, scenario := range []string{"unparseable", "unattributed-unparseable", "hash-mismatch", "owner-mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			kube, vmi := casQuotaClient(), quotaDynamic()
			config, wf, pod := guardedVMIFixture(t, 1)
			c := quotaController(config, kube, vmi)
			normal := c.vmiCreate
			c.vmiCreate = func(ctx context.Context, ns string, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
				_, err := normal(ctx, ns, obj)
				if err != nil {
					return nil, err
				}
				return nil, context.DeadlineExceeded
			}
			if err := authorizeTestVMI(t, c, wf, pod); err == nil {
				t.Fatal("lost response accepted")
			}
			c = quotaController(config, kube, vmi)
			object, err := vmi.Resource(vmiResource).Namespace(wf.Namespace).Get(t.Context(), wf.Name+"-amd64", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "unparseable", "unattributed-unparseable":
				err = unstructured.SetNestedField(object.Object, "invalid", "spec", "domain", "memory", "guest")
				if scenario == "unattributed-unparseable" {
					object.SetLabels(nil)
				}
			case "hash-mismatch":
				a := object.GetAnnotations()
				a[vmiCreateHashAnnotation] = strings.Repeat("b", 64)
				object.SetAnnotations(a)
			case "owner-mismatch":
				object.SetOwnerReferences(nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = vmi.Resource(vmiResource).Namespace(wf.Namespace).Update(t.Context(), object, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			_, other, op := guardedVMIRepositoryFixture(t, 2, 8)
			if err = authorizeTestVMI(t, c, other, op); err != nil {
				t.Fatalf("bad repo 7 blocked repo 8: %v", err)
			}
			_, mine, mp := guardedVMIFixture(t, 3)
			if err = authorizeTestVMI(t, c, mine, mp); err == nil {
				t.Fatal("uncertain demand became free capacity for its own repo")
			}
		})
	}
}

func TestVMILedgerIsInitializedOnceWithoutCreatePermission(t *testing.T) {
	kube := casQuotaClient()
	c := &Controller{kube: kube, config: Config{Namespace: testNamespace}}
	cm, err := c.getVMILedger(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cm.Annotations[vmiLedgerInitialized] != "1" || cm.Data["reservations.json"] == "" {
		t.Fatal("metadata-only chart ledger was not initialized")
	}
	cm.Data = nil
	if _, err = kube.CoreV1().ConfigMaps(testNamespace).Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.reserveVMIClaim(t.Context(), "missing", vmiClaim{}, vmiQuotaBinding{}); err == nil {
		t.Fatal("lost initialized data silently reset the ledger")
	}
	for _, a := range kube.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "configmaps" {
			t.Fatal("server tried to create the ledger")
		}
	}
	missing := &Controller{kube: fake.NewClientset(), config: Config{Namespace: testNamespace}}
	if _, err = missing.getVMILedger(t.Context()); err == nil {
		t.Fatal("missing chart ledger recreated by server")
	}
}
