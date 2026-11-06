package app

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// The same checksum-verified upstream Pod constructor observation as argojob's
// observer tests. Only API/status and documented Argo post-patch mutations are
// modeled. No suite is executed and no real-cluster qualification is claimed.
func directControllerFixture(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	var provenance struct {
		Path, Version, Sum, GoModSum string
		Files                        map[string]string `json:"files"`
	}
	read := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if err := json.Unmarshal(read(filepath.Join("../argojob/testdata/nonroot-argo4.0.8", "provenance.json")), &provenance); err != nil {
		t.Fatal(err)
	}
	module := provenance.Path + " " + provenance.Version
	if !strings.Contains(string(read("../../go.mod")), "\t"+module+"\n") {
		t.Fatal("Argo module version changed; regenerate controller fixtures with actual Pod proof")
	}
	sums := "\n" + string(read("../../go.sum"))
	for _, line := range []string{module + " " + provenance.Sum, module + "/go.mod " + provenance.GoModSum} {
		if !strings.Contains(sums, "\n"+line+"\n") {
			t.Fatal("Argo checksum changed; controller fixtures are stale")
		}
	}
	data := read(filepath.Join("../argojob/testdata/nonroot-argo4.0.8", name))
	digest := sha256.Sum256(data)
	if provenance.Files[name] != hex.EncodeToString(digest[:]) {
		t.Fatalf("controller fixture %s failed integrity check", name)
	}
	var pod corev1.Pod
	if err := json.Unmarshal(data, &pod); err != nil {
		t.Fatal(err)
	}
	return &pod
}

func directMaterializedPod(t *testing.T, wf *wfv1.Workflow, leaf *wfv1.Template) *corev1.Pod {
	t.Helper()
	pod := directControllerFixture(t, "container-before-patch.json")
	node := wfv1.NodeStatus{ID: wf.Name + "-trusted", Name: wf.Name + "[1].trusted-tests", TemplateName: "oberth-trusted-tests", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded, BoundaryID: wf.Name}
	wf.Status.Nodes = wfv1.Nodes{wf.Name: {ID: wf.Name, Name: wf.Name, Type: wfv1.NodeTypeSteps, Phase: wfv1.NodeSucceeded}, node.ID: node}
	wf.Status.Phase = wfv1.WorkflowSucceeded
	pod.ObjectMeta = metav1.ObjectMeta{Name: argojob.PodName(wf, node), Namespace: wf.Namespace, UID: "pod-uid", Annotations: map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}
	pod.Spec.ServiceAccountName = leaf.ServiceAccountName
	pod.Spec.AutomountServiceAccountToken = ptr.To(false)
	pod.Spec.SecurityContext = leaf.SecurityContext.DeepCopy()
	pod.Spec.NodeSelector = leaf.NodeSelector
	pod.Spec.NodeName = "worker-amd64"
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	var volumes []corev1.Volume
	for _, v := range pod.Spec.Volumes {
		if v.Name == "var-run-argo" || v.Name == "tmp-dir-argo" || v.Name == "exec-sa-token" {
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
	init.VolumeMounts = []corev1.VolumeMount{{Name: "exec-sa-token", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}, {Name: "var-run-argo", MountPath: "/var/run/argo"}}
	pod.Spec.InitContainers = []corev1.Container{*init}
	wait := pod.Spec.Containers[0].DeepCopy()
	wait.Env = append([]corev1.EnvVar{{Name: "ARGO_CONTAINER_NAME", Value: "wait"}}, runtimeEnv...)
	wait.VolumeMounts = []corev1.VolumeMount{{Name: "exec-sa-token", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}}
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

type directPolicyFixture struct {
	fragmentPolicyErr   error
	fragmentPolicyReads int
	remote              string
	pushes              int
	source, contract    []byte
	baseline            string
}

func (p *directPolicyFixture) ReadDefaultBlob(_ context.Context, _ string, file string, _ int) (string, []byte, bool, error) {
	if file == argoworkflow.FragmentFile {
		p.fragmentPolicyReads++
		return p.baseline, nil, false, p.fragmentPolicyErr
	}
	if file == ".oberth/tests.yaml" {
		return p.baseline, bytes.Clone(p.contract), p.contract != nil, nil
	}
	return p.baseline, nil, false, nil
}
func (p *directPolicyFixture) ReadOptionalBlob(_ context.Context, _, _, file string, _ int) ([]byte, bool, error) {
	if file == ".oberth/tests.yaml" {
		return bytes.Clone(p.contract), p.contract != nil, nil
	}
	if file == argoworkflow.BuildFile {
		return bytes.Clone(p.source), true, nil
	}
	return nil, false, nil
}

type directNativeWorkflows struct {
	afterCreate      func()
	lostReply        bool
	readsUnavailable bool
	targetWorkflowFake
	mu   sync.Mutex
	t    *testing.T
	kube *targetKubeFake
	node string
}

func (w *directNativeWorkflows) Create(ctx context.Context, input *wfv1.Workflow, options metav1.CreateOptions) (*wfv1.Workflow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	wf, err := w.targetWorkflowFake.Create(ctx, input, options)
	if err != nil {
		return nil, err
	}
	wf.UID = types.UID("workflow-" + wf.Name)
	wf.Status.StartedAt = metav1.Now()
	wf.Status.FinishedAt = metav1.Now()
	wf.Status.Phase = wfv1.WorkflowSucceeded
	for i := range wf.Spec.Templates {
		leaf := &wf.Spec.Templates[i]
		if leaf.Name != "oberth-trusted-tests" {
			continue
		}
		body, err := base64.RawURLEncoding.DecodeString(leaf.Container.Args[1])
		if err != nil {
			w.t.Fatal(err)
		}
		var intent protectedtests.Intent
		if err = json.Unmarshal(body, &intent); err != nil {
			w.t.Fatal(err)
		}
		wf.Status.StartedAt = metav1.NewTime(intent.AdmittedAt)
		pod := directMaterializedPod(w.t, wf, leaf)
		pod.ResourceVersion = "1"
		pod.Spec.NodeName = w.node
		pod.Spec.Tolerations = append(append([]corev1.Toleration{}, leaf.Tolerations...), corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))}, corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))})
		report := protectedtests.Report{Version: 1, IntentDigest: protectedtests.IntentDigest(intent), InventoryDigest: intent.InventoryDigest, CaseCount: intent.CaseCount, ArtifactSetDigest: strings.Repeat("a", 64)}
		raw, _ := json.Marshal(report)
		pod.Status.Phase = corev1.PodSucceeded
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://main", ImageID: strings.Split(intent.Recipe.HarnessImage, "@")[1], State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: "containerd://main", StartedAt: metav1.NewTime(intent.AdmittedAt), FinishedAt: metav1.Now(), Message: string(raw)}}}}
		if err = w.kube.Tracker().Add(pod); err != nil {
			w.t.Fatal(err)
		}
	}
	w.object = wf.DeepCopy()
	if w.afterCreate != nil {
		w.afterCreate()
	}
	if w.lostReply {
		w.readsUnavailable = true
		return nil, errors.New("transport reply lost")
	}
	return wf, nil
}
func (w *directNativeWorkflows) Get(ctx context.Context, name string, o metav1.GetOptions) (*wfv1.Workflow, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readsUnavailable {
		return nil, errors.New("original API temporarily unavailable")
	}
	return w.targetWorkflowFake.Get(ctx, name, o)
}
func (w *directNativeWorkflows) Delete(ctx context.Context, name string, o metav1.DeleteOptions) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ns := w.object.Namespace
	if err := w.targetWorkflowFake.Delete(ctx, name, o); err != nil {
		return err
	}
	pods, err := w.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		for _, owner := range p.OwnerReferences {
			if owner.Name == name {
				if err = w.kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ns, p.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type directPlacementFixture struct {
	databasePath  string
	executor      *TrustedPreflights
	jobs          *ArgoJobs
	database      *store.Store
	request       service.WorkflowRequest
	requirement   vmrunner.Requirement
	native        *argojob.Controller
	workflows     *directNativeWorkflows
	environment   *targetRouteFixture
	fragmentBlobs *fakeFragmentBlobs
}

func newDirectPlacementFixture(t *testing.T) *directPlacementFixture {
	return newDirectPlacementFixtureExpiring(t, 0)
}

func newDirectPlacementFixtureExpiring(t *testing.T, lifetime time.Duration) *directPlacementFixture {
	return newDirectPlacementFixtureInputs(t, lifetime, nil, nil)
}

func newDirectPlacementFixtureInputs(t *testing.T, lifetime time.Duration, original []byte, fragments map[argoworkflow.FragmentKey]argoworkflow.Fragment) *directPlacementFixture {
	t.Helper()
	ctx := t.Context()
	environment := newTargetRouteFixture(t)
	databasePath := filepath.Join(t.TempDir(), "direct.db")
	db, err := storetest.Open(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	upstream, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "github", Kind: "github", BaseURL: "https://github.com/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	logs, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := service.NewScheduler(service.SchedulerConfig{Store: db, Git: &directPolicyFixture{}, Logs: logs, Jobs: environment.local, Auditor: db, WorkspaceRoot: t.TempDir(), MutationGate: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := bootstrap.SetAdmissionHold(ctx, "operator", model.AdmissionHoldRequest{On: true, Reason: "bind exact direct placement", Allow: []string{}})
	if err != nil || !hold.On || len(hold.Allow) != 0 {
		t.Fatal("explicit empty-allow bootstrap hold", hold, err)
	}
	enqueued, err := bootstrap.EnqueueCI(ctx, service.CIRequest{EventID: "direct-bootstrap", Repository: repo, Branch: "feature", SHA: sha, Actor: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimNextRun(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unbound bootstrap escaped hold", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = storetest.Open(ctx, databasePath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	retainedHold, err := db.AdmissionHold(ctx)
	if err != nil || !retainedHold.On || len(retainedHold.Allow) != 0 || !retainedHold.ExpiresAt.Equal(hold.ExpiresAt) {
		t.Fatal("restart lost bootstrap hold", retainedHold, err)
	}
	if _, err = db.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimNextRun(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("restart escaped bootstrap hold", err)
	}
	run, err := db.Run(ctx, enqueued.ID)
	if err != nil || run.Status != model.RunQueued {
		t.Fatal("canonical queued acceptance lost", run, err)
	}
	workspace := t.TempDir()
	sourceDir := filepath.Join(workspace, run.ID, "src")
	if err = os.CopyFS(sourceDir, os.DirFS(environment.request.SourceDir)); err != nil {
		t.Fatal(err)
	}
	request := service.WorkflowRequest{Run: run, Repository: repo, WorkflowName: "native-parent", UpstreamName: upstream.Name, UpstreamOrg: upstream.Org(), SourceDir: sourceDir}
	source, err := os.ReadFile(filepath.Join(request.SourceDir, argoworkflow.BuildFile))
	if err != nil {
		t.Fatal(err)
	}
	if original != nil {
		source = bytes.Clone(original)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	for i := range workflow.Spec.Templates {
		if leaf := workflow.Spec.Templates[i].Container; leaf != nil && original == nil {
			leaf.Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("128Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("128Mi")}}
		}
	}
	if original == nil {
		source, err = json.Marshal(workflow)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(request.SourceDir, argoworkflow.BuildFile), source, 0600); err != nil {
		t.Fatal(err)
	}
	guest := strings.Repeat("b", 64)
	manifest := []byte(fmt.Sprintf("version: 1\nprofile: kvm-suite-amd64-v1\nguest_sha256: %s\nartifacts: [program]\ncases: [one]\n", guest))
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.yaml", Size: int64(len(manifest)), Mode: 0444, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err = tw.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	recipe := protectedtests.Recipe{SuiteSHA256: targetBytesHash(archive.Bytes()), GuestSHA256: guest, HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("c", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}
	contract := protectedtests.Contract{Version: 2, Profile: protectedtests.KVMSuiteAMD64Profile, SuiteSHA256: recipe.SuiteSHA256, GuestSHA256: guest, Artifacts: []string{"program"}}
	suite, err := protectedtests.VerifySuiteBundle(contract, archive.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	inventory, count := suite.Inventory()
	config := environment.definition.Config
	raw, err := json.Marshal(config.ExecutionPlacement)
	if err != nil {
		t.Fatal(err)
	}
	var selection map[string]any
	if err = json.Unmarshal(raw, &selection); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"normal", "preflight", "branch_policy_baseline", "graph_digest", "fragment_lock_digest", "fragments"} {
		delete(selection, key)
	}
	config.ExecutionPlacement = nil
	capability := &argoworkflow.Capability{Namespace: config.Namespace, ServiceAccount: "operator-runner", ExtendedResources: map[string]int64{protectedtests.GenericDeviceResource: 1}}
	if _, err = db.SetWorkflowCapabilityGrant(ctx, repo.ID, "operator", capability); err != nil {
		t.Fatal(err)
	}
	if err = environment.kube.Tracker().Add(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: capability.ServiceAccount, Namespace: config.Namespace}}); err != nil {
		t.Fatal(err)
	}
	policy := protectedtests.PlacementPolicy{BaselineSHA: strings.Repeat("d", 40), Contract: contract, Recipe: recipe, InventoryDigest: inventory, CaseCount: count, CapabilityDigest: protectedtests.CapabilityDigest(*capability), SupplementalGroup: config.TrustedKVMGroup, MaxPhaseSeconds: 3600}
	selection["schema"] = "oberth.execution-placement/protected-v2"
	selection["parent_run_id"] = run.ID
	selection["normal_workflow"] = request.WorkflowName
	selection["preflight_workflow"] = request.WorkflowName + "-trusted"
	selection["protected_policy"] = policy
	selection["repository_id"] = repo.ID
	selection["source_digest"] = targetBytesHash(source)
	selection["dependency_identity"] = targetHash(config)
	var loader FragmentLoader
	var fragmentBlobs *fakeFragmentBlobs
	if fragments != nil {
		resolved, e := argoworkflow.ResolveRunSource(source, fragments, argoworkflow.RunSourceBinding{RunID: run.ID, RepositoryID: repo.ID, SHA: sha, File: argoworkflow.BuildFile, Trigger: argoworkflow.RunTriggerBranch})
		if e != nil {
			t.Fatal(e)
		}
		selection["graph_digest"], selection["fragment_lock_digest"] = resolved.GraphIdentity(), targetHash(resolved.Lock())
		inventory := []map[string]string{}
		fragmentBlobs = &fakeFragmentBlobs{tags: map[string]string{}, blobs: map[string]string{}}
		registry := fakeRegistry{known: map[string]bool{}}
		for _, fragment := range resolved.Lock() {
			inventory = append(inventory, map[string]string{"key": fragment.Key.String(), "commit": fragment.SHA, "digest": fragment.Digest})
			fragmentBlobs.tags[fragment.Key.String()] = fragment.SHA
			fragmentBlobs.blobs[fragment.SHA] = string(fragments[fragment.Key].Source)
			registry.known[fragment.Key.Repo] = true
		}
		selection["fragments"] = inventory
		loader, err = NewGitFragmentLoader(fragmentBlobs, registry, nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	if lifetime > 0 {
		selection["expires"] = time.Now().UTC().Add(lifetime).Format(time.RFC3339Nano)
	}
	raw, _ = json.Marshal(selection)
	config.ExecutionPlacement, err = argojob.ParseExecutionPlacement(raw)
	if err != nil {
		t.Fatal(err)
	}
	workflows := &directNativeWorkflows{t: t, kube: environment.kube, node: selection["node_name"].(string)}
	controller, err := argojob.NewController(workflows, environment.kube, config)
	if err != nil {
		t.Fatal(err)
	}
	controller.WithSourceSeeder(argojob.NewSourceSeeder(environment.kube, environment.definition.Exec, config)).WithProtectedPlacementJournal(db, environment.definition.Transport).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability).WithPodLogReader(func(context.Context, string, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	})
	jobs, err := NewArgoJobs(controller, config, db, emptySecretAccess{}, loader)
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
	contractBody, _ := json.Marshal(contract)
	jobs.SetResourceBlobs(&directPolicyFixture{source: source, contract: contractBody, baseline: policy.BaselineSHA})
	executor, err := NewGenericProtectedPreflights(jobs, db, []vmrunner.KVMRecipe{recipe}, workspace, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// The source fixture proves durable canonical acceptance and config readback.
	// Real API authorization denials throughout bootstrap remain a native gate.
	readback, err := json.Marshal(config.ExecutionPlacement)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{}
	observed := map[string]any{}
	if json.Unmarshal(raw, &expected) != nil || json.Unmarshal(readback, &observed) != nil || !reflect.DeepEqual(expected, observed) {
		t.Fatal("installed placement differs from reviewed input")
	}
	if _, err = db.SetAdmissionHold(ctx, "operator", model.AdmissionHoldRequest{Reason: "exact placement read back"}); err != nil {
		t.Fatal(err)
	}
	request.Run, err = db.ClaimNextRun(ctx)
	if err != nil || request.Run.ID != run.ID {
		t.Fatal("activation changed accepted run", err)
	}
	executor.fetch = func(context.Context, string, string, int64) ([]byte, error) { return bytes.Clone(archive.Bytes()), nil }
	return &directPlacementFixture{databasePath: databasePath, executor: executor, jobs: jobs, database: db, request: request, requirement: vmrunner.Requirement{CandidateSHA: sha, BaselineSHA: policy.BaselineSHA, Contract: &contract}, native: controller, workflows: workflows, environment: environment, fragmentBlobs: fragmentBlobs}
}

func TestDirectProtectedPlacementNativeAdapterReceiptAndParent(t *testing.T) {
	f := newDirectPlacementFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
		t.Fatal(err)
	}
	execution, err := f.database.KVMPreflight(ctx, f.request.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Receipt == nil || !execution.Cleaned || execution.WorkflowUID == "" || execution.Intent.RunID != f.request.Run.ID {
		t.Fatal("native preflight lost receipt/custody/cleanup")
	}
	if err = f.executor.CheckProtectedPlacementAuthority(ctx, f.request.Run.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.jobs.CreateWorkflow(ctx, f.request); err != nil {
		t.Fatal(err)
	}
	if f.workflows.posts != 2 || f.workflows.deletes != 1 {
		t.Fatalf("phases not delivered once: posts=%d deletes=%d", f.workflows.posts, f.workflows.deletes)
	}
	if _, err = f.database.VerifyAuditState(ctx); err != nil {
		t.Fatal(err)
	}
}

func (p *directPolicyFixture) Checkout(context.Context, string, string, string) error {
	return fmt.Errorf("unexpected checkout on publication-only fixture")
}
func (p *directPolicyFixture) RemoteRef(context.Context, string, string) (string, bool, error) {
	return p.remote, p.remote != "", nil
}
func (p *directPolicyFixture) SyncBranch(_ context.Context, _, _, sha string) error {
	p.pushes++
	p.remote = sha
	return nil
}
func (p *directPolicyFixture) SyncTag(context.Context, string, string, string) error {
	return fmt.Errorf("unexpected tag publication")
}
func (p *directPolicyFixture) PushPromotion(context.Context, string, string, string) error {
	return fmt.Errorf("unexpected promotion publication")
}

func TestDirectProtectedPlacementSchedulerPublicationChecksLiveEnvironment(t *testing.T) {
	for _, mode := range []string{"unchanged", "cluster", "node", "transport", "removed policy and replaced cluster"} {
		t.Run(mode, func(t *testing.T) {
			f := newDirectPlacementFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := f.database.SetRunWorkflowName(ctx, f.request.Run.ID, f.request.WorkflowName); err != nil {
				t.Fatal(err)
			}
			if err := f.jobs.CreateWorkflow(ctx, f.request); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "cluster", "removed policy and replaced cluster":
				if mode == "removed policy and replaced cluster" {
					f.jobs.resourceBlobs.(*directPolicyFixture).contract = nil
				}
				ns, err := f.environment.kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				ns.UID = "replacement-cluster"
				if _, err = f.environment.kube.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "node":
				node, err := f.environment.kube.CoreV1().Nodes().Get(ctx, f.workflows.node, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				node.UID = "replacement-node"
				if _, err = f.environment.kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			case "transport":
				f.native.WithProtectedPlacementJournal(f.database, &rest.Config{Host: "https://replacement.example.org", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("fixture-ca")}})
			}
			policy := f.jobs.resourceBlobs.(*directPolicyFixture)
			logs, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := service.NewScheduler(service.SchedulerConfig{Store: f.database, Git: policy, Logs: logs, Jobs: f.jobs, ReleaseJobs: f.jobs, TrustedPreflight: f.executor, Auditor: f.database, WorkspaceRoot: t.TempDir(), MutationGate: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			pub, err := f.database.BeginPublication(ctx, model.PublicationSpec{RepoID: f.request.Repository.ID, RunID: f.request.Run.ID, RefKind: model.RefBranch, Ref: f.request.Run.Ref, ResultSHA: f.request.Run.SHA, Actor: f.request.Run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			result, err := scheduler.DeliverPublication(ctx, pub.ID)
			if mode == "unchanged" {
				if err != nil || result.Publication.Status != model.PublicationDelivered || policy.pushes != 1 {
					t.Fatalf("unchanged publication: err=%v status=%s pushes=%d", err, result.Publication.Status, policy.pushes)
				}
			} else {
				if policy.pushes != 0 || result.Publication.Status == model.PublicationDelivered {
					t.Fatalf("changed %s published: err=%v status=%s pushes=%d", mode, err, result.Publication.Status, policy.pushes)
				}
				before := f.workflows.deletes
				if err = f.native.Cancel(ctx, f.request.WorkflowName, f.request.Run.ID); err == nil || f.workflows.deletes != before {
					t.Fatal("replacement environment deleted original obligation")
				}
			}
		})
	}
}

func TestDirectProtectedPlacementUncertainCreateReopenRetainsOriginalOwner(t *testing.T) {
	for _, mode := range []string{"group zero", "revoked grant", "legacy group zero", "removed recipes", "changed primary config", "replacement cluster", "replacement workflow UID", "replacement source UID", "foreign source owner"} {
		t.Run(mode, func(t *testing.T) {
			f := newDirectPlacementFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			f.workflows.lostReply = true
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err == nil {
				t.Fatal("uncertain create reported success")
			}
			execution, err := f.database.KVMPreflight(ctx, f.request.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !execution.Submitted || execution.Cleaned || execution.WorkflowUID != "" {
				t.Fatalf("lost uncertainty: %+v", execution)
			}
			if f.workflows.posts != 1 {
				t.Fatal("ambiguous create repeated POST")
			}
			if mode == "replacement workflow UID" {
				// First bind the authentic original UID. A replacement must not acquire its
				// deletion capability merely by repeating names and source annotations.
				if err = f.database.BindKVMPreflight(ctx, f.request.Run.ID, string(f.workflows.object.UID)); err != nil {
					t.Fatal(err)
				}
				f.workflows.object.UID = "replacement-workflow-uid"
			}
			if mode == "replacement source UID" || mode == "foreign source owner" {
				claim, e := f.environment.kube.CoreV1().PersistentVolumeClaims(f.jobs.config.Namespace).Get(ctx, f.request.WorkflowName+"-trusted-src", metav1.GetOptions{})
				if e != nil {
					t.Fatal(e)
				}
				if mode == "replacement source UID" {
					claim.UID = "replacement-source-uid"
				} else {
					claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "foreign", UID: "foreign"}}
				}
				if _, e = f.environment.kube.CoreV1().PersistentVolumeClaims(f.jobs.config.Namespace).Update(ctx, claim, metav1.UpdateOptions{}); e != nil {
					t.Fatal(e)
				}
			}
			if err = f.database.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := storetest.Open(ctx, f.databasePath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			f.workflows.readsUnavailable = false
			config := f.jobs.config
			if mode == "group zero" || mode == "legacy group zero" || mode == "removed recipes" {
				config.TrustedKVMGroup = 0
			}
			if mode == "removed recipes" {
				config.ExecutionPlacement = nil
			}
			if mode == "changed primary config" {
				config.SourceStorageClass = "changed-storage-class"
			}
			if mode == "revoked grant" {
				if _, err = db.SetWorkflowCapabilityGrant(ctx, f.request.Repository.ID, "operator", nil); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "replacement cluster" {
				ns, e := f.environment.kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
				if e != nil {
					t.Fatal(e)
				}
				ns.UID = "replacement-cluster"
				if _, e = f.environment.kube.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); e != nil {
					t.Fatal(e)
				}
			}
			native, err := argojob.NewController(f.workflows, f.environment.kube, config)
			if err != nil {
				t.Fatal(err)
			}
			native.WithProtectedPlacementJournal(db, f.environment.definition.Transport).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs, err := NewArgoJobs(native, config, db, emptySecretAccess{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs.SetResourceBlobs(f.jobs.resourceBlobs)
			constructor := NewGenericProtectedPreflights
			if mode == "legacy group zero" {
				constructor = NewTrustedPreflights
			}
			// Restart retains the canonical checkout root: its path is part of the
			// existing nonroot submission proof, just as in the real server.
			workspace := f.executor.workspaceRoot
			var executor *TrustedPreflights
			if mode == "removed recipes" {
				executor, err = NewProtectedPreflightRecovery(jobs, db, workspace, func(context.Context) error { return nil })
			} else {
				executor, err = constructor(jobs, db, f.executor.recipes, workspace, func(context.Context) error { return nil })
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "removed recipes" {
				if executor.Supports(*f.requirement.Contract) == nil || executor.RecipeAllowed(f.executor.recipes[0]) == nil {
					t.Fatal("removed recipes still authorize execution")
				}
				if executor.Execute(ctx, f.request, f.requirement, io.Discard) == nil {
					t.Fatal("recovery-only executor submitted work")
				}
				if f.workflows.posts != 1 {
					t.Fatal("removed recipes repeated POST")
				}
			}
			_, err = executor.Recover(ctx)

			after, loadErr := db.KVMPreflight(ctx, f.request.Run.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if mode == "replacement cluster" || mode == "replacement workflow UID" {
				if err == nil || after.Cleaned || f.workflows.deletes != 0 {
					t.Fatal("replacement acquired cleanup authority")
				}
			} else if mode == "replacement source UID" || mode == "foreign source owner" {
				if err == nil || after.Cleaned {
					t.Fatal("foreign volume discharged original obligation")
				}
				if _, e := f.environment.kube.CoreV1().PersistentVolumeClaims(f.jobs.config.Namespace).Get(ctx, f.request.WorkflowName+"-trusted-src", metav1.GetOptions{}); e != nil {
					t.Fatal("foreign source claim was deleted", e)
				}
			} else if err != nil || !after.Cleaned || after.Receipt != nil || after.Refusal == nil || f.workflows.deletes != 1 {
				t.Fatalf("recovery failed original cleanup: err=%v cleaned=%t deletes=%d", err, after.Cleaned, f.workflows.deletes)
			}
			if f.workflows.posts != 1 {
				t.Fatal("recovery submitted another Workflow")
			}
			if _, err = db.VerifyAuditState(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDirectProtectedPlacementParentDeliveryChecksLiveEnvironment(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			f := newDirectPlacementFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
				t.Fatal(err)
			}
			if changed {
				ns, err := f.environment.kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				ns.UID = "replacement-before-parent-delivery"
				if _, err = f.environment.kube.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			err := f.jobs.CreateWorkflow(ctx, f.request)
			if changed {
				if err == nil || f.workflows.posts != 1 {
					t.Fatalf("changed environment delivered parent: err=%v posts=%d", err, f.workflows.posts)
				}
			} else if err != nil || f.workflows.posts != 2 {
				t.Fatalf("unchanged parent delivery failed: err=%v posts=%d", err, f.workflows.posts)
			}
		})
	}
}

func TestDirectProtectedParentRecoveryRetainsOriginalUIDs(t *testing.T) {
	for _, mode := range []string{"acknowledged", "lost reply", "group zero lost", "revoked lost", "removed config lost", "changed config lost", "replacement parent UID", "replacement source UID", "foreign source owner", "replacement cluster"} {
		t.Run(mode, func(t *testing.T) {
			f := newDirectPlacementFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := f.database.SetRunWorkflowName(ctx, f.request.Run.ID, f.request.WorkflowName); err != nil {
				t.Fatal(err)
			}
			lost := strings.Contains(mode, "lost")
			f.workflows.lostReply = lost
			err := f.jobs.CreateWorkflow(ctx, f.request)
			if (err != nil) != lost {
				t.Fatalf("parent submission lost=%t err=%v", lost, err)
			}
			body, uid, err := f.database.PlacementWorkflow(ctx, f.request.Run.ID, f.request.WorkflowName)
			if err != nil || len(body) == 0 || (uid == "") != lost {
				t.Fatalf("missing original parent custody uid=%s err=%v", uid, err)
			}
			originalUID := string(f.workflows.object.UID)
			if mode == "replacement parent UID" {
				f.workflows.object.UID = "replacement-parent"
			}
			if mode == "replacement source UID" || mode == "foreign source owner" {
				claim, e := f.environment.kube.CoreV1().PersistentVolumeClaims(f.jobs.config.Namespace).Get(ctx, f.request.WorkflowName+"-src", metav1.GetOptions{})
				if e != nil {
					t.Fatal(e)
				}
				if mode == "replacement source UID" {
					claim.UID = "replacement-source"
				} else {
					claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "foreign", UID: "foreign"}}
				}
				if _, e = f.environment.kube.CoreV1().PersistentVolumeClaims(f.jobs.config.Namespace).Update(ctx, claim, metav1.UpdateOptions{}); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "replacement cluster" {
				ns, e := f.environment.kube.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
				if e != nil {
					t.Fatal(e)
				}
				ns.UID = "replacement-cluster"
				if _, e = f.environment.kube.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); e != nil {
					t.Fatal(e)
				}
			}
			if err = f.database.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := storetest.Open(ctx, f.databasePath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			f.workflows.readsUnavailable = false
			config := f.jobs.config
			switch mode {
			case "group zero lost":
				config.TrustedKVMGroup = 0
			case "changed config lost":
				config.SourceStorageClass = "changed-primary-storage"
			case "removed config lost":
				config.ExecutionPlacement = nil
			case "revoked lost":
				if _, err = db.SetWorkflowCapabilityGrant(ctx, f.request.Repository.ID, "operator", nil); err != nil {
					t.Fatal(err)
				}
			}
			native, err := argojob.NewController(f.workflows, f.environment.kube, config)
			if err != nil {
				t.Fatal(err)
			}
			native.WithProtectedPlacementJournal(db, f.environment.definition.Transport)
			clean := false
			if mode == "acknowledged" {
				err = native.Cancel(ctx, f.request.WorkflowName, f.request.Run.ID)
				clean = err == nil
			} else {
				for attempt := 0; attempt < 5; attempt++ {
					clean, err = native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, time.Now().Add(-time.Second))
					if clean || err != nil {
						break
					}
				}
			}
			negative := strings.HasPrefix(mode, "replacement") || mode == "foreign source owner"
			if negative {
				if err == nil || clean {
					t.Fatal("replacement discharged original parent obligation", err)
				}
				if (mode == "replacement parent UID" || mode == "replacement cluster") && f.workflows.deletes != 1 {
					t.Fatal("replacement parent/cluster obtained deletion")
				}
				if mode == "replacement source UID" || mode == "foreign source owner" {
					if _, e := f.environment.kube.CoreV1().PersistentVolumeClaims(config.Namespace).Get(ctx, f.request.WorkflowName+"-src", metav1.GetOptions{}); e != nil {
						t.Fatal("foreign source deleted", e)
					}
				}
			} else {
				if err != nil || !clean || f.workflows.deletes != 2 {
					t.Fatalf("parent cleanup err=%v clean=%t deletes=%d", err, clean, f.workflows.deletes)
				}
				_, retained, e := db.PlacementWorkflow(ctx, f.request.Run.ID, f.request.WorkflowName)
				if e != nil || retained != originalUID {
					t.Fatal("original UID lost", retained, e)
				}
			}
			if f.workflows.posts != 2 {
				t.Fatal("recovery repeated a Workflow POST")
			}
			if _, err = db.VerifyAuditState(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDirectProtectedParentLateCreateRetainsBoundaryAndSource(t *testing.T) {
	f := newDirectPlacementFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
		t.Fatal(err)
	}
	f.workflows.lostReply = true
	if err := f.jobs.CreateWorkflow(ctx, f.request); err == nil {
		t.Fatal("lost reply accepted")
	}
	f.workflows.readsUnavailable = false
	late := f.workflows.object.DeepCopy()
	f.workflows.object = nil
	if err := f.jobs.CreateWorkflow(ctx, f.request); err == nil || f.workflows.posts != 2 {
		t.Fatal("uncertain source reseeded/reposted", err)
	}
	future := time.Now().Add(time.Minute)
	clean, err := f.native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, future)
	if err != nil || clean {
		t.Fatal("early absence discharged late POST", clean, err)
	}
	if _, err = f.environment.kube.CoreV1().PersistentVolumeClaims(f.jobs.config.Namespace).Get(ctx, f.request.WorkflowName+"-src", metav1.GetOptions{}); err != nil {
		t.Fatal("early source deletion", err)
	}
	f.workflows.object = late
	clean, err = f.native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, future)
	if err != nil || clean || f.workflows.deletes != 2 {
		t.Fatal("late original Workflow not fenced", clean, err)
	}
	for attempt := 0; attempt < 5; attempt++ {
		clean, err = f.native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, time.Now().Add(-time.Second))
		if err != nil || clean {
			break
		}
	}
	if err != nil || !clean || f.workflows.posts != 2 {
		t.Fatal("late create cleanup failed", clean, err)
	}
	if err = f.jobs.CreateWorkflow(ctx, f.request); err == nil || f.workflows.posts != 2 {
		t.Fatal("settled phase recreated original run", err)
	}
}

func TestDirectProtectedParentSourceFailureRetainsCleanupObligation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			f := newDirectPlacementFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
				t.Fatal(err)
			}
			failUpload := func(ctx context.Context, ns, name, container string, command []string, input io.Reader, stdout, stderr io.Writer) error {
				if foreign {
					claim, err := f.environment.kube.CoreV1().PersistentVolumeClaims(ns).Get(ctx, f.request.WorkflowName+"-src", metav1.GetOptions{})
					if err != nil {
						return err
					}
					claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "foreign", UID: "foreign"}}
					if _, err = f.environment.kube.CoreV1().PersistentVolumeClaims(ns).Update(ctx, claim, metav1.UpdateOptions{}); err != nil {
						return err
					}
				}
				return errors.New("fixture source upload failed")
			}
			f.native.WithSourceSeeder(argojob.NewSourceSeeder(f.environment.kube, failUpload, f.jobs.config))
			err := f.jobs.CreateWorkflow(ctx, f.request)
			var pending interface{ AmbiguousCreate() bool }
			if err == nil || !errors.As(err, &pending) || !pending.AmbiguousCreate() || f.workflows.posts != 1 {
				t.Fatal("source failure lost durable settlement classification", err)
			}
			clean, err := f.native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, time.Now().Add(-time.Second))
			if foreign {
				if err == nil || clean {
					t.Fatal("foreign source ownership discharged", clean, err)
				}
			} else if err != nil || !clean {
				t.Fatal("original failed source not cleaned", clean, err)
			}
			if foreign {
				if _, err = f.environment.kube.CoreV1().PersistentVolumeClaims(f.jobs.config.Namespace).Get(ctx, f.request.WorkflowName+"-src", metav1.GetOptions{}); err != nil {
					t.Fatal("foreign source deleted", err)
				}
			}
		})
	}
}

func TestDirectProtectedExpiredPlacementRecoversAfterRestart(t *testing.T) {
	for _, phase := range []string{"preflight", "parent"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			f := newDirectPlacementFixtureExpiring(t, 30*time.Second)
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			raw, err := json.Marshal(f.jobs.config.ExecutionPlacement)
			if err != nil {
				t.Fatal(err)
			}
			var selection struct {
				Expires time.Time `json:"expires"`
			}
			if err = json.Unmarshal(raw, &selection); err != nil {
				t.Fatal(err)
			}
			if phase == "preflight" {
				f.workflows.lostReply = true
				if f.executor.Execute(ctx, f.request, f.requirement, io.Discard) == nil {
					t.Fatal("lost reply accepted")
				}
			} else {
				if err = f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
					t.Fatal(err)
				}
				if _, err = f.database.SetRunWorkflowName(ctx, f.request.Run.ID, f.request.WorkflowName); err != nil {
					t.Fatal(err)
				}
				f.workflows.lostReply = true
				if f.jobs.CreateWorkflow(ctx, f.request) == nil {
					t.Fatal("lost reply accepted")
				}
			}
			posts := f.workflows.posts
			if err = f.database.Close(); err != nil {
				t.Fatal(err)
			}
			timer := time.NewTimer(time.Until(selection.Expires))
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			db, err := storetest.Open(ctx, f.databasePath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			config := f.jobs.config
			config.ExecutionPlacement, err = argojob.ParseExecutionPlacementForStartup(raw)
			if err != nil {
				t.Fatal(err)
			}
			f.workflows.readsUnavailable = false
			native, err := argojob.NewController(f.workflows, f.environment.kube, config)
			if err != nil {
				t.Fatal(err)
			}
			native.WithProtectedPlacementJournal(db, f.environment.definition.Transport).WithWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs, err := NewArgoJobs(native, config, db, emptySecretAccess{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
			jobs.SetResourceBlobs(f.jobs.resourceBlobs)
			executor, err := NewGenericProtectedPreflights(jobs, db, f.executor.recipes, f.executor.workspaceRoot, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if executor.Execute(ctx, f.request, f.requirement, io.Discard) == nil {
				t.Fatal("expired placement admitted preflight")
			}
			if jobs.CreateWorkflow(ctx, f.request) == nil {
				t.Fatal("expired placement admitted parent")
			}
			if phase == "preflight" {
				if _, err = executor.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				execution, e := db.KVMPreflight(ctx, f.request.Run.ID)
				if e != nil || !execution.Cleaned || execution.Receipt != nil {
					t.Fatal("expired preflight custody lost", e)
				}
			} else {
				clean := false
				for attempt := 0; attempt < 5; attempt++ {
					clean, err = native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, time.Now().Add(-time.Second))
					if clean || err != nil {
						break
					}
				}
				if err != nil || !clean {
					t.Fatal("expired parent custody stranded", err)
				}
			}
			if f.workflows.posts != posts || f.workflows.deletes != posts {
				t.Fatal("expiry recreated or stranded original objects")
			}
			if _, err = db.VerifyAuditState(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type directParentAuditFailure struct {
	*store.Store
	bind bool
}

func (j directParentAuditFailure) PreparePlacementWorkflow(ctx context.Context, id, name string, body []byte) error {
	if !j.bind {
		return errors.New("parent intent audit unavailable")
	}
	return j.Store.PreparePlacementWorkflow(ctx, id, name, body)
}

func (j directParentAuditFailure) BindPlacementWorkflowUID(ctx context.Context, id, name, uid string) error {
	if j.bind {
		return errors.New("parent UID audit unavailable")
	}
	return j.Store.BindPlacementWorkflowUID(ctx, id, name, uid)
}

func TestDirectProtectedParentAuditFailureRetainsCleanup(t *testing.T) {
	for _, bind := range []bool{false, true} {
		t.Run(fmt.Sprintf("bind=%t", bind), func(t *testing.T) {
			f := newDirectPlacementFixture(t)
			ctx := t.Context()
			if err := f.executor.Execute(ctx, f.request, f.requirement, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := f.database.SetRunWorkflowName(ctx, f.request.Run.ID, f.request.WorkflowName); err != nil {
				t.Fatal(err)
			}
			f.native.WithProtectedPlacementJournal(directParentAuditFailure{f.database, bind}, f.environment.definition.Transport)
			err := f.jobs.CreateWorkflow(ctx, f.request)
			var pending interface{ AmbiguousCreate() bool }
			if !errors.As(err, &pending) || !pending.AmbiguousCreate() {
				t.Fatal("failed audit lost cleanup obligation", err)
			}
			wantPosts := 1
			if bind {
				wantPosts++
			}
			if f.workflows.posts != wantPosts {
				t.Fatal("unaudited intent crossed POST boundary")
			}
			body, uid, err := f.database.PlacementWorkflow(ctx, f.request.Run.ID, f.request.WorkflowName)
			if err != nil || (len(body) != 0) != bind || uid != "" {
				t.Fatal("failed audit retained false custody", err)
			}
			f.native.WithProtectedPlacementJournal(f.database, f.environment.definition.Transport)
			clean := false
			for attempt := 0; attempt < 5; attempt++ {
				clean, err = f.native.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, time.Now().Add(-time.Second))
				if clean || err != nil {
					break
				}
			}
			if err != nil || !clean || f.workflows.posts != wantPosts {
				t.Fatal("failed audit cleanup stranded or recreated work", err)
			}
		})
	}
}
