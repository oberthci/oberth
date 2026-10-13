package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func trustedKVMRequest(t *testing.T) (Config, Request) {
	t.Helper()
	config := testConfig()
	config.KVMEnabled = true
	request := testRequest(periapsis.TriggerCI, greedyDocument)
	request.UpstreamName = "codeberg"
	digest := sha256.Sum256(request.Source)
	now := time.Date(2026, 10, 13, 20, 0, 0, 0, time.UTC)
	request.TrustedKVM = &vmrunner.KVMIntent{RunID: request.RunID, RepoID: 1, Repository: "codeberg/skipops/oberth", CandidateSHA: request.SHA, BaselineSHA: strings.Repeat("b", 40),
		Contract:  vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: strings.Repeat("c", 64), GuestSHA256: strings.Repeat("d", 64), Artifacts: []string{"program"}},
		Recipe:    vmrunner.KVMRecipe{SuiteSHA256: strings.Repeat("c", 64), GuestSHA256: strings.Repeat("d", 64), HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("e", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"},
		Namespace: config.Namespace, WorkflowName: request.Name, WorkflowSpecDigest: hex.EncodeToString(digest[:]), InventoryDigest: strings.Repeat("f", 64), CaseCount: 2, AdmittedAt: now, Deadline: now.Add(time.Minute)}
	return config, request
}
func trustedKVMBuilt(t *testing.T) (Config, Request, *wfv1.Workflow, *wfv1.Template) {
	t.Helper()
	config, request := trustedKVMRequest(t)
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = "workflow-uid"
	wf.Status.StartedAt = metav1.NewTime(request.TrustedKVM.AdmittedAt)
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == trustedKVMTemplate {
			return config, request, wf, &wf.Spec.Templates[i]
		}
	}
	t.Fatal("missing trusted leaf")
	return Config{}, Request{}, nil, nil
}
func TestTrustedKVMBuildClosedLeafAndWrapper(t *testing.T) {
	config, request, wf, leaf := trustedKVMBuilt(t)
	if wf.Spec.Entrypoint != trustedKVMWrapper || len(leaf.Container.VolumeMounts) != 2 || leaf.Container.Image != request.TrustedKVM.Recipe.HarnessImage || leaf.Container.WorkingDir != "/trusted" {
		t.Fatalf("unexpected leaf: %#v", leaf)
	}
	for _, env := range leaf.Container.Env {
		if env.Name != "OBERTH_KVM" {
			t.Fatalf("inherited repository environment: %s", env.Name)
		}
	}
	if leaf.Container.VolumeMounts[0].Name != trustedKVMArtifacts || !leaf.Container.VolumeMounts[0].ReadOnly || leaf.Container.VolumeMounts[0].SubPath != "artifacts" {
		t.Fatal("input artifacts not isolated read-only")
	}
	var wrapper *wfv1.Template
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == trustedKVMWrapper {
			wrapper = &wf.Spec.Templates[i]
		}
	}
	if wrapper == nil || len(wrapper.Steps) != 2 || wrapper.Steps[0].Steps[0].Template != "main" || wrapper.Steps[1].Steps[0].Template != trustedKVMTemplate {
		t.Fatal("trusted leaf did not follow complete producer entrypoint")
	}
	// Dry-run and actual seeded source produce the same exact submission.
	seeded := request
	seeded.SourceVolume = SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: "artifacts"}
	actual, err := Build(config, seeded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wf.Spec, actual.Spec) {
		t.Fatal("dryrun differs from seeded admission")
	}
	wrong := request
	wrong.SHA = strings.Repeat("f", 40)
	if _, err := Build(config, wrong); err == nil {
		t.Fatal("candidate substitution admitted")
	}
	config.KVMEnabled = false
	if _, err := Build(config, request); !errors.Is(err, ErrKVMNotEnabled) {
		t.Fatalf("KVM switch bypassed: %v", err)
	}
}
func TestTrustedKVMRejectsProducerEscapeHatches(t *testing.T) {
	for _, field := range []string{"templateDefaults: {}", "onExit: main", "hooks: {exit: {template: main}}", "retryStrategy: {limit: 1}"} {
		t.Run(field, func(t *testing.T) {
			config, request := trustedKVMRequest(t)
			request.Source = []byte(strings.Replace(string(request.Source), "  entrypoint: main", "  "+field+"\n  entrypoint: main", 1))
			digest := sha256.Sum256(request.Source)
			request.TrustedKVM.WorkflowSpecDigest = hex.EncodeToString(digest[:])
			if _, err := Build(config, request); err == nil {
				t.Fatalf("accepted %s", field)
			}
		})
	}
	for _, field := range []string{"daemon: true", "retryStrategy: {limit: 1}", "memoize: {key: cached}"} {
		t.Run(field, func(t *testing.T) {
			config, request := trustedKVMRequest(t)
			request.Source = []byte(strings.Replace(string(request.Source), "      container:", "      "+field+"\n      container:", 1))
			digest := sha256.Sum256(request.Source)
			request.TrustedKVM.WorkflowSpecDigest = hex.EncodeToString(digest[:])
			if _, err := Build(config, request); err == nil {
				t.Fatalf("accepted %s", field)
			}
		})
	}
}

// This materialization retains the checksum-pinned real v4.0.8 pre-patch Pod
// constructor observation. Current server fields replace the fixture's previous
// leaf; the same documented post-patch transformations as the nonroot fixture
// apply. It is a regression model, not fresh controller or live-node evidence.
func trustedKVMMaterializedPod(t *testing.T, wf *wfv1.Workflow, leaf *wfv1.Template) *corev1.Pod {
	t.Helper()
	pod := nonrootControllerFixture(t, "container-before-patch.json")
	node := wfv1.NodeStatus{ID: wf.Name + "-trusted", Name: wf.Name + "[1].trusted-tests", TemplateName: trustedKVMTemplate, Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded, BoundaryID: wf.Name}
	wf.Status.Nodes = wfv1.Nodes{wf.Name: {ID: wf.Name, Name: wf.Name, Type: wfv1.NodeTypeSteps, Phase: wfv1.NodeSucceeded}, node.ID: node}
	wf.Status.Phase = wfv1.WorkflowSucceeded
	pod.ObjectMeta = metav1.ObjectMeta{Name: PodName(wf, node), Namespace: wf.Namespace, UID: "pod-uid", Annotations: map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}
	pod.Spec.ServiceAccountName = leaf.ServiceAccountName
	pod.Spec.AutomountServiceAccountToken = ptr.To(false)
	pod.Spec.SecurityContext = wf.Spec.SecurityContext.DeepCopy()
	pod.Spec.NodeSelector = leaf.NodeSelector
	pod.Spec.NodeName = "worker-amd64"
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	var volumes []corev1.Volume
	for _, v := range pod.Spec.Volumes {
		if v.Name == "var-run-argo" || v.Name == "tmp-dir-argo" || v.Name == executorTokenVolume {
			volumes = append(volumes, v)
		}
	}
	pod.Spec.Volumes = append(volumes, leaf.Volumes...)
	template := leaf.DeepCopy()
	template.Metadata.Labels = wf.Spec.PodMetadata.Labels
	templateBody, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	runtimeEnv := []corev1.EnvVar{{Name: "ARGO_NODE_ID", Value: node.ID}, {Name: "ARGO_INCLUDE_SCRIPT_OUTPUT", Value: "false"}, {Name: "ARGO_DEADLINE", Value: wf.Status.StartedAt.Add(time.Duration(*wf.Spec.ActiveDeadlineSeconds) * time.Second).UTC().Format(time.RFC3339)}, {Name: "ARGO_PROGRESS_FILE", Value: "/var/run/argo/progress"}, {Name: "ARGO_PROGRESS_PATCH_TICK_DURATION", Value: "1m0s"}, {Name: "ARGO_PROGRESS_FILE_TICK_DURATION", Value: "3s"}}
	init := pod.Spec.InitContainers[0].DeepCopy()
	init.Env = []corev1.EnvVar{{Name: "ARGO_CONTAINER_NAME", Value: "init"}, {Name: "ARGO_TEMPLATE", Value: string(templateBody)}}
	init.Env = append(init.Env, runtimeEnv...)
	init.VolumeMounts = []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir, ReadOnly: true}, {Name: "var-run-argo", MountPath: "/var/run/argo"}}
	pod.Spec.InitContainers = []corev1.Container{*init}
	wait := pod.Spec.Containers[0].DeepCopy()
	wait.Env = append([]corev1.EnvVar{{Name: "ARGO_CONTAINER_NAME", Value: "wait"}}, runtimeEnv...)
	wait.VolumeMounts = []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir, ReadOnly: true}}
	for _, m := range leaf.Container.VolumeMounts {
		m.MountPath = "/mainctrfs" + m.MountPath
		m.ReadOnly = false
		wait.VolumeMounts = append(wait.VolumeMounts, m)
	}
	main := leaf.Container.DeepCopy()
	main.Name = "main"
	main.Env = append(main.Env, corev1.EnvVar{Name: "ARGO_CONTAINER_NAME", Value: "main"})
	main.Env = append(main.Env, runtimeEnv...)
	pod.Spec.Containers = []corev1.Container{*wait, *main}
	body, err := json.Marshal(pod.Spec)
	if err != nil {
		t.Fatal(err)
	}
	body, err = strategicpatch.StrategicMergePatch(body, []byte(leaf.PodSpecPatch), corev1.PodSpec{})
	if err != nil {
		t.Fatal(err)
	}
	var patchedSpec corev1.PodSpec
	if err := json.Unmarshal(body, &patchedSpec); err != nil {
		t.Fatal(err)
	}
	pod.Spec = patchedSpec
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name == "main" {
			c.Command = append([]string{"/var/run/argo/argoexec", "emissary", "--loglevel", "info", "--log-format", "text", "--gloglevel", "0", "--"}, c.Command...)
		} else {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "tmp-dir-argo", MountPath: "/tmp", SubPath: "0"})
		}
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"})
	}
	return pod
}
func trustedKVMObservedFixture(t *testing.T) (Config, Request, *wfv1.Workflow, *corev1.Pod) {
	t.Helper()
	config, request, wf, leaf := trustedKVMBuilt(t)
	pod := trustedKVMMaterializedPod(t, wf, leaf)
	report := vmrunner.KVMReport{Version: 1, IntentDigest: vmrunner.KVMIntentDigest(*request.TrustedKVM), InventoryDigest: request.TrustedKVM.InventoryDigest, ArtifactSetDigest: strings.Repeat("a", 64), CaseCount: 2}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://main", ImageID: "sha256:" + strings.Repeat("e", 64), State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://main", StartedAt: metav1.NewTime(request.TrustedKVM.AdmittedAt.Add(time.Second)), FinishedAt: metav1.NewTime(request.TrustedKVM.AdmittedAt.Add(2 * time.Second)), Message: string(body)}}}}
	return config, request, wf, pod
}
func TestTrustedKVMObserverMaterializationAndAttacks(t *testing.T) {
	config, request, wf, pod := trustedKVMObservedFixture(t)
	controller, err := NewController(&staticWorkflowClient{workflow: wf}, fake.NewSimpleClientset(pod), config)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := controller.TrustedKVMReceipt(context.Background(), request, string(wf.UID))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Binding.PodUID != string(pod.UID) {
		t.Fatal("receipt lost pod binding")
	}
	attacks := map[string]func(*corev1.Pod){
		"command": func(p *corev1.Pod) { p.Spec.Containers[1].Command = []string{"/bin/sh", "-c", "true"} },
		"args":    func(p *corev1.Pod) { p.Spec.Containers[1].Args = []string{"--other"} },
		"mount": func(p *corev1.Pod) {
			p.Spec.Containers[1].VolumeMounts = append(p.Spec.Containers[1].VolumeMounts, corev1.VolumeMount{Name: executorTokenVolume, MountPath: "/token"})
		},
		"private-mirror": func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: trustedKVMPrivate, MountPath: "/mainctrfs/trusted"})
		},
		"source-write": func(p *corev1.Pod) { p.Spec.Containers[1].VolumeMounts[0].ReadOnly = false },
		"owner":        func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other" },
		"node":         func(p *corev1.Pod) { p.Annotations["workflows.argoproj.io/node-id"] = "other" },
		"exit":         func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 7 },
		"restart":      func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"image":        func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "sha256:" + strings.Repeat("f", 64) },
		"cases": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.Message = strings.ReplaceAll(p.Status.ContainerStatuses[0].State.Terminated.Message, `"case_count":2`, `"case_count":1`)
		},
		"progress-path": func(p *corev1.Pod) {
			for i := range p.Spec.Containers[1].Env {
				if p.Spec.Containers[1].Env[i].Name == "ARGO_PROGRESS_FILE" {
					p.Spec.Containers[1].Env[i].Value = "/trusted/outcome"
				}
			}
		},
		"executor-image":      func(p *corev1.Pod) { p.Spec.InitContainers[1].Image = "other" },
		"executor-privileged": func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true) },
	}
	for name, change := range attacks {
		t.Run(name, func(t *testing.T) {
			bad := pod.DeepCopy()
			change(bad)
			controller, err := NewController(&staticWorkflowClient{workflow: wf}, fake.NewSimpleClientset(bad), config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controller.TrustedKVMReceipt(context.Background(), request, string(wf.UID)); err == nil {
				t.Fatal("accepted altered trusted evidence")
			}
		})
	}
	changed := wf.DeepCopy()
	changed.Spec.Templates[0].Container.Command = []string{"/bin/false"}
	controller, err = NewController(&staticWorkflowClient{workflow: changed}, fake.NewSimpleClientset(pod), config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.TrustedKVMWorkflowUID(context.Background(), request); err == nil {
		t.Fatal("annotation-only Workflow identity accepted")
	}
}

type trustedCleanupClient struct {
	workflow *wfv1.Workflow
	deleted  bool
}

func (c *trustedCleanupClient) Create(context.Context, *wfv1.Workflow, metav1.CreateOptions) (*wfv1.Workflow, error) {
	return nil, errors.New("unexpected Create")
}
func (c *trustedCleanupClient) Get(context.Context, string, metav1.GetOptions) (*wfv1.Workflow, error) {
	if c.deleted {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, c.workflow.Name)
	}
	return c.workflow.DeepCopy(), nil
}
func (c *trustedCleanupClient) Delete(_ context.Context, _ string, options metav1.DeleteOptions) error {
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != c.workflow.UID {
		return errors.New("missing exact UID delete")
	}
	c.deleted = true
	return nil
}
func TestTrustedKVMCleanupRequiresExactUIDAndObservedAbsence(t *testing.T) {
	config, request, wf, pod := trustedKVMObservedFixture(t)
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: sourceClaimName(wf.Name), Namespace: wf.Namespace, UID: "claim-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID}}}}
	client := &trustedCleanupClient{workflow: wf}
	kube := fake.NewSimpleClientset(pod, claim)
	controller, err := NewController(client, kube, config)
	if err != nil {
		t.Fatal(err)
	}
	controller.interval = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.CleanupTrustedKVM(ctx, wf.Name, request.RunID, ""); err == nil {
		t.Fatal("empty UID discharged uncertain submission")
	}
	if err := controller.CleanupTrustedKVM(ctx, wf.Name, request.RunID, "foreign"); err == nil {
		t.Fatal("foreign UID cleaned")
	}
	if err := controller.CleanupTrustedKVM(ctx, wf.Name, request.RunID, string(wf.UID)); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().Pods(wf.Namespace).Get(ctx, pod.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("owned pod remains")
	}
	if _, err := kube.CoreV1().PersistentVolumeClaims(wf.Namespace).Get(ctx, claim.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("claim remains")
	}
	foreign := pod.DeepCopy()
	foreign.UID = types.UID("foreign-pod")
	foreign.OwnerReferences[0].UID = "foreign-workflow"
	if _, err := kube.CoreV1().Pods(wf.Namespace).Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := controller.CleanupTrustedKVM(ctx, wf.Name, request.RunID, string(wf.UID)); err == nil {
		t.Fatal("foreign source consumer did not preserve cleanup obligation")
	}
	if _, err := kube.CoreV1().Pods(wf.Namespace).Get(ctx, foreign.Name, metav1.GetOptions{}); err != nil {
		t.Fatal("foreign Pod deleted")
	}
}

func TestTrustedKVMRefusalRetainsActualRestartAndExit(t *testing.T) {
	config, request, wf, pod := trustedKVMObservedFixture(t)
	wf.Status.Phase = wfv1.WorkflowFailed
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses[0].RestartCount = 1
	pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 7
	controller, err := NewController(&staticWorkflowClient{workflow: wf}, fake.NewSimpleClientset(pod), config)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := controller.TrustedKVMRefusal(context.Background(), request, string(wf.UID))
	if err != nil {
		t.Fatal(err)
	}
	if actual.RestartCount != 1 || actual.ExitCode != 7 || actual.PodUID != string(pod.UID) {
		t.Fatalf("negative kubelet record lost: %#v", actual)
	}
	if _, err := controller.TrustedKVMReceipt(context.Background(), request, string(wf.UID)); err == nil {
		t.Fatal("negative kubelet record accepted as pass")
	}
}
func TestTrustedKVMPrecreateRefusalAndUnknownAuxiliary(t *testing.T) {
	config, request, wf, _ := trustedKVMObservedFixture(t)
	client := &trustedCleanupClient{workflow: wf, deleted: true}
	kube := fake.NewSimpleClientset()
	controller, err := NewController(client, kube, config)
	if err != nil {
		t.Fatal(err)
	}
	controller.interval = time.Millisecond
	_, err = controller.Create(context.Background(), request)
	if !errors.Is(err, ErrTrustedKVMCreateRefused) {
		t.Fatalf("pre-POST no-seeder refusal not classified after observed absence: %v", err)
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: sourceClaimName(request.Name), Namespace: config.Namespace, UID: "foreign"}}
	if _, err = kube.CoreV1().PersistentVolumeClaims(config.Namespace).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err = controller.confirmTrustedKVMNoCreate(ctx, request.Name); err == nil {
		t.Fatal("unbound auxiliary absence assumed")
	}
	if _, err = kube.CoreV1().PersistentVolumeClaims(config.Namespace).Get(context.Background(), claim.Name, metav1.GetOptions{}); err != nil {
		t.Fatal("read-only refusal observer deleted a foreign claim")
	}
}
func TestTrustedKVMRefusesReservedRuntimeVolumes(t *testing.T) {
	for _, name := range []string{trustedKVMArtifacts, trustedKVMPrivate, "var-run-argo", "tmp-dir-argo", executorTokenVolume} {
		for _, kind := range []string{"volume", "claim"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				config, request := trustedKVMRequest(t)
				field := "  volumes:\n  - name: " + name + "\n    emptyDir: {}\n"
				if kind == "claim" {
					field = "  volumeClaimTemplates:\n  - metadata:\n      name: " + name + "\n    spec:\n      accessModes: [ReadWriteOnce]\n      resources:\n        requests:\n          storage: 1Gi\n"
				}
				request.Source = []byte(strings.Replace(string(request.Source), "  entrypoint: main", field+"  entrypoint: main", 1))
				digest := sha256.Sum256(request.Source)
				request.TrustedKVM.WorkflowSpecDigest = hex.EncodeToString(digest[:])
				if _, err := Build(config, request); err == nil {
					t.Fatal("reserved runtime volume admitted")
				}
			})
		}
	}
}
