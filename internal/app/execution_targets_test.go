package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	fakerest "k8s.io/client-go/rest/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// The JSON fixture contains synthetic public controller/admission identities
// shared with argojob's pinned-profile controls, never installation authority.
type targetPolicyGit struct {
	err      error
	baseline string
	source   []byte
	wait     <-chan struct{}
	entered  chan struct{}
}

func (g *targetPolicyGit) ReadOptionalBlob(_ context.Context, _, _, file string, _ int) ([]byte, bool, error) {
	if file == argoworkflow.BuildFile && g.err == nil {
		return append([]byte(nil), g.source...), true, nil
	}
	return nil, false, g.err
}
func (g *targetPolicyGit) ReadDefaultBlob(ctx context.Context, _ string, _ string, _ int) (string, []byte, bool, error) {
	if g.wait != nil {
		if g.entered != nil {
			select {
			case g.entered <- struct{}{}:
			default:
			}
		}
		select {
		case <-g.wait:
		case <-ctx.Done():
			return "", nil, false, ctx.Err()
		}
	}
	return g.baseline, nil, false, g.err
}

type targetWorkflowFake struct {
	object               *wfv1.Workflow
	posts, gets, deletes int
	err                  error
}

func (w *targetWorkflowFake) Create(_ context.Context, object *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	w.posts++
	if w.err != nil {
		return nil, w.err
	}
	copy := object.DeepCopy()
	copy.UID = "remote-workflow-uid"
	copy.ResourceVersion = "1"
	w.object = copy
	return copy.DeepCopy(), nil
}
func (w *targetWorkflowFake) Get(context.Context, string, metav1.GetOptions) (*wfv1.Workflow, error) {
	w.gets++
	if w.err != nil {
		return nil, w.err
	}
	if w.object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "absent")
	}
	return w.object.DeepCopy(), nil
}
func (w *targetWorkflowFake) Delete(_ context.Context, _ string, options metav1.DeleteOptions) error {
	if w.object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "absent")
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != w.object.UID {
		return errors.New("missing Workflow UID fence")
	}
	w.deletes++
	w.object = nil
	return nil
}

type targetKubeFake struct{ *kubefake.Clientset }
type targetDiscovery struct {
	discovery.DiscoveryInterface
	client rest.Interface
}

func (d targetDiscovery) RESTClient() rest.Interface { return d.client }
func (c *targetKubeFake) Discovery() discovery.DiscoveryInterface {
	client := &fakerest.RESTClient{NegotiatedSerializer: scheme.Codecs.WithoutConversion(), Client: fakerest.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/version" {
			return nil, errors.New("unexpected discovery")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"gitVersion":"v1.35.7-fixture"}`))}, nil
	})}
	return targetDiscovery{c.Clientset.Discovery(), client}
}

type targetRouteFixture struct {
	jobs, local    *ArgoJobs
	db             *store.Store
	request        service.WorkflowRequest
	definition     ExecutionTarget
	remote         *targetWorkflowFake
	kube           *targetKubeFake
	localKube      *kubefake.Clientset
	localWorkflows *targetWorkflowFake
	policy         *targetPolicyGit
	execs          *int
}

func newTargetRouteFixture(t *testing.T) *targetRouteFixture {
	return newTargetRouteFragmentsFixture(t, false)
}

func newTargetRouteFragmentsFixture(t *testing.T, withFragment bool) *targetRouteFixture {
	t.Helper()
	ctx := t.Context()
	localFixture := newRaceFixture(t)
	db, err := storetest.Open(ctx, filepath.Join(t.TempDir(), "state.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := storetest.CreateUpstream(db, ctx, model.UpstreamSpec{Name: "github", Kind: "github", BaseURL: "https://github.com/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := storetest.EnqueueRun(db, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature", SHA: strings.Repeat("a", 40), Actor: "fixture", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.SetRunWorkflowName(ctx, result.ID, "remote-run")
	if err != nil {
		t.Fatal(err)
	}
	request := service.WorkflowRequest{Run: run, WorkflowName: run.WorkflowName, Repository: repo, SourceDir: localFixture.sourceDir, UpstreamName: u.Name, UpstreamOrg: u.Org()}
	body, err := os.ReadFile("testdata/native-target.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Config  json.RawMessage
		Source  []byte
		Objects []json.RawMessage
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	var configFields map[string]json.RawMessage
	if err := json.Unmarshal(fixture.Config, &configFields); err != nil {
		t.Fatal(err)
	}
	var selection map[string]any
	if err := decodeCurrentTargetFixture(configFields["ExecutionPlacement"], &selection); err != nil {
		t.Fatal(err)
	}
	configFields["ExecutionPlacement"] = json.RawMessage(`null`)
	body, _ = json.Marshal(configFields)
	var config argojob.Config
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	config, err = argojob.DefaultedConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte(strings.Replace(string(fixture.Source), "metadata:\n  annotations:\n    oberth.ci/size: L\n", "", 1))
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	leaf := workflow.Spec.Templates[0]
	leaf.Name = "unit"
	workflow.Spec.Templates = []wfv1.Template{{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "test", Template: "test"}}}}, {Name: "test", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "unit", Template: "unit"}}}}, leaf}
	fragmentMap := map[argoworkflow.FragmentKey]argoworkflow.Fragment{}
	if withFragment {
		key := argoworkflow.FragmentKey{Repo: "github/org/shared", Version: "v1.0.0"}
		fragmentLeaf := leaf
		fragmentLeaf.Name = "leaf"
		fragmentSource, err := json.Marshal(&wfv1.Workflow{TypeMeta: metav1.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow"}, Spec: wfv1.WorkflowSpec{Entrypoint: "leaf", Templates: []wfv1.Template{fragmentLeaf}}})
		if err != nil {
			t.Fatal(err)
		}
		fragmentMap[key] = argoworkflow.Fragment{Key: key, SHA: strings.Repeat("d", 40), Source: fragmentSource}
		workflow.Spec.Templates = workflow.Spec.Templates[:2]
		workflow.Spec.Templates[1].DAG.Tasks[0].Template = ""
		workflow.Spec.Templates[1].DAG.Tasks[0].TemplateRef = &wfv1.TemplateRef{Name: key.String(), Template: "leaf"}
	}
	source, err = json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(request.SourceDir, argoworkflow.BuildFile), source, 0600); err != nil {
		t.Fatal(err)
	}
	policy := &targetPolicyGit{baseline: strings.Repeat("b", 40), source: source}
	authority, err := workflowpolicy.ResolveBranchAuthority(ctx, policy, "github/org/repo", run.SHA)
	if err != nil {
		t.Fatal(err)
	}
	baseline, digest := authority.Identity()
	selection["schema"] = "oberth.execution-placement/branch-v2"
	delete(selection, "preflight")
	selection["repository"] = "github/org/repo"
	selection["repository_id"] = repo.ID
	selection["ref"] = run.Ref
	selection["sha"] = run.SHA
	selection["source_digest"] = targetBytesHash(source)
	selection["normal"] = map[string]string{"run_id": run.ID, "workflow": run.WorkflowName}
	selection["branch_policy_baseline"] = baseline
	selection["policy_digest"] = digest
	resolved, err := argoworkflow.ResolveRunSource(source, fragmentMap, argoworkflow.RunSourceBinding{RunID: run.ID, RepositoryID: repo.ID, SHA: run.SHA, File: argoworkflow.BuildFile, Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		t.Fatal(err)
	}
	if withFragment {
		selection["fragments"] = []map[string]string{}
		for key, fragment := range fragmentMap {
			selection["fragments"] = append(selection["fragments"].([]map[string]string), map[string]string{"key": key.String(), "commit": fragment.SHA, "digest": targetBytesHash(fragment.Source)})
		}
	}
	selection["graph_digest"] = resolved.GraphIdentity()
	selection["fragment_lock_digest"] = targetHash(resolved.Lock())
	selection["dependency_identity"] = targetHash(config)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	selection["native_identity"] = hex.EncodeToString(hash.Sum(nil))
	objects := []runtime.Object{}
	for _, raw := range fixture.Objects {
		object, _, err := scheme.Codecs.UniversalDeserializer().Decode(raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cm, ok := object.(*corev1.ConfigMap); ok && cm.Name == selection["admission_config_map"] {
			cm.Data["policy_digest"] = digest
			selection["admission_digest"] = targetHash(cm.Data)
		}
		objects = append(objects, object)
	}
	body, _ = json.Marshal(selection)
	config.ExecutionPlacement, err = argojob.ParseExecutionPlacement(body)
	if err != nil {
		t.Fatal(err)
	}
	objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: config.PipelineServiceAccount, Namespace: config.Namespace}})
	kube := &targetKubeFake{kubefake.NewClientset(objects...)}
	kube.PrependReactor("create", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		claim := action.(ktesting.CreateAction).GetObject().(*corev1.PersistentVolumeClaim)
		claim.UID = "remote-source-uid"
		claim.ResourceVersion = "1"
		return false, nil, nil
	})
	kube.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = "remote-seed-uid"
		pod.ResourceVersion = "1"
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))}, corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: ptr.To(int64(300))})
		return false, nil, nil
	})
	kube.PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		get := action.(ktesting.GetAction)
		object, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), get.GetNamespace(), get.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := object.(*corev1.Pod).DeepCopy()
		if pod.Labels["oberth.ci/component"] == "source-seed" {
			pod.Status.Phase = corev1.PodRunning
			pod.Spec.NodeName = "operator-scheduled-node"
		}
		return true, pod, nil
	})
	remote := &targetWorkflowFake{}
	localWorkflows := &targetWorkflowFake{}
	localKube := kubefake.NewClientset()
	localController, err := argojob.NewController(localWorkflows, localKube, localFixture.jobs.config)
	if err != nil {
		t.Fatal(err)
	}
	local, err := NewArgoJobs(localController, localFixture.jobs.config, db, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	local.SetResourceBlobs(policy)
	if withFragment {
		retained := retainedFragments{}
		for _, fragment := range fragmentMap {
			retained = append(retained, retainedFragment{Fragment: fragment, Source: fragment.Source})
		}
		local.fragments = retained
	}
	execs := new(int)
	lastDigest := ""
	exec := func(_ context.Context, namespace, pod, _ string, command []string, stdin io.Reader, stdout, _ io.Writer) error {
		*execs++
		if namespace != config.Namespace || !strings.Contains(pod, "-src") {
			t.Fatal("exec left selected source bundle", namespace, pod)
		}
		if stdin != nil {
			h := sha256.New()
			n, err := io.Copy(h, stdin)
			if err != nil {
				return err
			}
			if n > 0 {
				lastDigest = hex.EncodeToString(h.Sum(nil))
			}
		}
		if strings.Contains(strings.Join(command, " "), "sha256sum") {
			_, err := fmt.Fprintln(stdout, lastDigest)
			return err
		}
		return nil
	}
	definition := ExecutionTarget{ID: "remote", Config: config, Transport: &rest.Config{Host: "https://api.example.org", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("fixture-ca")}}, Kubernetes: kube, Workflows: func(namespace string) argojob.WorkflowClient {
		if namespace != config.Namespace {
			t.Fatal("wrong Workflow namespace")
		}
		return remote
	}, Exec: exec}
	jobs, err := WithExecutionTargets(local, db, []ExecutionTarget{definition})
	if err != nil {
		t.Fatal(err)
	}
	jobs.SetWorkflowCapabilityLoader(db.ActiveWorkflowCapability)
	return &targetRouteFixture{jobs: jobs, local: local, db: db, request: request, definition: definition, remote: remote, kube: kube, localKube: localKube, localWorkflows: localWorkflows, policy: policy, execs: execs}
}

func (f *targetRouteFixture) noLocalEffects(t *testing.T) {
	t.Helper()
	if f.localWorkflows.posts != 0 || f.localWorkflows.gets != 0 || f.localWorkflows.deletes != 0 || len(f.localKube.Actions()) != 0 {
		t.Fatal("remote lifecycle leaked to local bundle", f.localWorkflows, f.localKube.Actions())
	}
}

func TestExecutionTargetNativeCreatePlanningAndRestart(t *testing.T) {
	f := newTargetRouteFixture(t)
	ctx := t.Context()
	request := f.request
	if size, err := f.jobs.ResolvePipelineSource(ctx, request); err != nil {
		t.Fatal(size, err)
	}
	if steps, err := f.jobs.PlannedSteps(ctx, request); err != nil || len(steps) != 1 {
		t.Fatal(steps, err)
	}
	if credentialed, err := f.jobs.PipelineCredentialed(ctx, request); err != nil || credentialed {
		t.Fatal(credentialed, err)
	}
	receipt, err := f.jobs.ResolvePipelineSource(ctx, request)
	if err != nil || receipt == nil {
		t.Fatal(receipt, err)
	}
	request.SourceReceipt = receipt
	if len(f.kube.Actions()) != 0 || *f.execs != 0 {
		t.Fatal("planning allocated target resources")
	}
	if err := f.jobs.CreateWorkflow(ctx, request); err != nil {
		t.Fatal(err)
	}
	if f.remote.posts != 1 || *f.execs != 2 {
		t.Fatal("native Workflow/source delivery missing", f.remote.posts, *f.execs)
	}
	binding, found, err := f.db.ExecutionTarget(ctx, request.Run.ID, request.WorkflowName)
	if err != nil || !found || binding.Target != "remote" {
		t.Fatal(binding, found, err)
	}
	if observation, err := f.jobs.ObserveScheduling(ctx, request.Run); err != nil || observation == nil {
		t.Fatal("target scheduling observation lost", observation, err)
	}
	if owns, err := f.jobs.Owns(ctx, request.WorkflowName); err != nil || !owns {
		t.Fatal(owns, err)
	}
	// Restart with different mutable executor settings. The retained config wins.
	changed := f.definition

	changed.Config.RunnerImagePrefixes = []string{"unrelated:"}
	restarted, err := WithExecutionTargets(f.local, f.db, []ExecutionTarget{changed})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.db.Run(ctx, request.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := restarted.Adopt(ctx, run); err != nil || !found {
		t.Fatal(found, err)
	}
	if namespace, err := restarted.WorkflowNamespaceForRun(ctx, run); err != nil || namespace != f.definition.Config.Namespace {
		t.Fatal(namespace, err)
	}

	f.remote.object.Status.Phase = wfv1.WorkflowSucceeded
	start, end := metav1.NewTime(time.Now().Add(-2*time.Second)), metav1.NewTime(time.Now())
	f.remote.object.Status.Nodes = wfv1.Nodes{
		"root": {ID: "root", Name: request.WorkflowName, DisplayName: request.WorkflowName, TemplateName: "main", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
		"test": {ID: "test", Name: request.WorkflowName + ".test", DisplayName: "test", TemplateName: "test", BoundaryID: "root", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
		"unit": {ID: "unit", Name: request.WorkflowName + ".test.unit", DisplayName: "unit", TemplateName: "unit", BoundaryID: "test", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded, StartedAt: start, FinishedAt: end},
	}
	child, err := restarted.routes.forName(ctx, request.WorkflowName, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	logs := 0
	child.controller.(*argojob.Controller).WithWatchHub(terminalTargetWatchSnapshot{workflow: f.remote.object.DeepCopy()}).WithPodLogReader(func(_ context.Context, namespace, pod, container string) (io.ReadCloser, error) {
		logs++
		if namespace != f.definition.Config.Namespace || container != "main" || pod == "" {
			t.Fatal("log left selected native bundle")
		}
		return io.NopCloser(strings.NewReader("remote native log\n")), nil
	})
	if result, err := restarted.TerminalResult(ctx, request.WorkflowName); err != nil || result.Status != model.RunPassed {
		t.Fatal(result, err)
	}
	var output bytes.Buffer
	if result, err := restarted.WaitResumed(ctx, request.WorkflowName, &output, []runprogress.Event{}, 123); err != nil || result.Status != model.RunPassed {
		t.Fatal(result, err)
	}
	if logs != 1 || !strings.Contains(output.String(), "remote native log") {
		t.Fatal("retained native log path lost", logs, output.String())
	}
	if found, err := restarted.Adopt(ctx, run); err != nil || !found {
		t.Fatal(found, err)
	}
	steps := argojob.StepResults(f.remote.object)
	if len(steps) != 1 {
		t.Fatal("native step derivation lost", steps)
	}
	step := steps[0]
	events := []runprogress.Event{{Version: runprogress.Version, Stage: step.Stage, Step: step.Step, Ordinal: step.Ordinal, Status: step.Status, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt}}
	output.Reset()
	if _, err := restarted.WaitResumed(ctx, request.WorkflowName, &output, events, 123); err != nil {
		t.Fatal(err)
	}
	if logs != 1 || output.Len() != 0 {
		t.Fatal("restart replayed retained log bytes", logs, output.String())
	}
	if f.remote.posts != 1 {
		t.Fatal("restart submitted duplicate Workflow")
	}
	f.noLocalEffects(t)
}

func TestExecutionTargetMissingChangedTransportAndPolicyRefuseBeforeEffects(t *testing.T) {
	for _, mode := range []string{"policy unavailable", "source changed", "target removed", "endpoint changed", "native policy changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newTargetRouteFixture(t)
			ctx := t.Context()
			switch mode {
			case "policy unavailable":
				f.policy.err = errors.New("unavailable")
			case "source changed":
				file := filepath.Join(f.request.SourceDir, argoworkflow.BuildFile)
				if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := f.jobs.ResolvePipelineSource(ctx, f.request); err != nil {
					t.Fatal(err)
				}
			}
			jobs := f.jobs
			if mode == "target removed" {
				var err error
				jobs, err = WithExecutionTargets(f.local, f.db, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "endpoint changed" {
				changed := f.definition
				changed.Transport = rest.CopyConfig(changed.Transport)
				changed.Transport.Host = "https://replacement.example.org"
				jobs.routes.definitions["remote"] = changed
			}
			if mode == "native policy changed" {
				f.policy.baseline = strings.Repeat("c", 40)
			}
			if err := jobs.CreateWorkflow(ctx, f.request); err == nil {
				t.Fatal("mismatch permitted native execution")
			}
			if f.remote.posts != 0 || *f.execs != 0 {
				t.Fatal("refusal allocated resources")
			}
			for _, action := range f.kube.Actions() {
				if action.GetVerb() == "create" {
					t.Fatal("refusal created target resource")
				}
			}
			f.noLocalEffects(t)
		})
	}
}

func TestExecutionTargetRetainsAuthenticFragmentBytesAcrossRestart(t *testing.T) {
	f := newTargetRouteFragmentsFixture(t, true)
	ctx := t.Context()
	if _, err := f.jobs.ResolvePipelineSource(ctx, f.request); err != nil {
		t.Fatal(err)
	}
	// A moved tag in the current loader cannot replace the retained raw source.
	f.local.fragments = retainedFragments{}
	restarted, err := WithExecutionTargets(f.local, f.db, []ExecutionTarget{f.definition})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := restarted.ResolvePipelineSource(ctx, f.request)
	if err != nil || receipt == nil {
		t.Fatal(receipt, err)
	}
	request := f.request
	request.SourceReceipt = receipt
	if err := restarted.CreateWorkflow(ctx, request); err != nil {
		t.Fatal(err)
	}
	if f.remote.posts != 1 || !strings.Contains(f.remote.object.Annotations[argojob.FragmentsAnnotation], strings.Repeat("d", 40)) {
		t.Fatal("native submission lost original fragment lock")
	}
	f.noLocalEffects(t)
}

func TestExecutionTargetCancellationAndDeniedCleanupRetainOriginalBundle(t *testing.T) {
	f := newTargetRouteFixture(t)
	ctx := t.Context()
	if err := f.jobs.CreateWorkflow(ctx, f.request); err != nil {
		t.Fatal(err)
	}
	restarted, err := WithExecutionTargets(f.local, f.db, []ExecutionTarget{f.definition})
	if err != nil {
		t.Fatal(err)
	}
	f.remote.err = apierrors.NewForbidden(schema.GroupResource{Resource: "workflows"}, f.request.WorkflowName, errors.New("denied"))
	if err := restarted.Stop(ctx, f.request.WorkflowName, f.request.Run.ID); err == nil {
		t.Fatal("denied remote cleanup became success")
	}
	if cancelled, err := f.db.ExecutionTargetCancelled(ctx, f.request.Run.ID); err != nil || cancelled {
		t.Fatal("Stop must retain live result custody", cancelled, err)
	}
	if _, err := restarted.TerminalResult(ctx, f.request.WorkflowName); err == nil {
		t.Fatal("unresolved cleanup allowed native result")
	}
	f.remote.err = nil
	if done, err := restarted.SettleCreate(ctx, f.request.WorkflowName, f.request.Run.ID, time.Now().Add(time.Hour)); err != nil || done {
		t.Fatal("accepted delete counted as late-commit absence", done, err)
	}
	if f.remote.deletes != 0 || f.remote.object.Spec.Shutdown != wfv1.ShutdownStrategyStop {
		t.Fatal("native Stop did not retain the original Workflow")
	}
	if rows, err := f.db.ExecutionTargetResources(ctx, f.request.Run.ID); err != nil || len(rows) < 3 {
		t.Fatal("source/seed/Workflow custody discarded", rows, err)
	}
	f.noLocalEffects(t)
}

func TestExecutionTargetGitOutageDoesNotSerializeLocalAdmission(t *testing.T) {
	f := newTargetRouteFixture(t)
	ctx := t.Context()
	result, err := storetest.EnqueueRun(f.db, ctx, model.RunSpec{RepoID: f.request.Repository.ID, RefKind: model.RefBranch, Ref: "local", SHA: strings.Repeat("c", 40), Actor: "fixture", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.db.SetRunWorkflowName(ctx, result.ID, "local-workflow")
	if err != nil {
		t.Fatal(err)
	}
	local := f.request
	local.Run = run
	local.WorkflowName = run.WorkflowName
	release := make(chan struct{})
	f.policy.wait = release
	f.policy.entered = make(chan struct{}, 1)
	remoteDone := make(chan error, 1)
	go func() { _, err := f.jobs.ResolvePipelineSource(ctx, f.request); remoteDone <- err }()
	select {
	case <-f.policy.entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("native policy refresh not entered")
	}
	localDone := make(chan error, 1)
	go func() { _, err := f.jobs.ResolvePipelineSource(ctx, local); localDone <- err }()
	select {
	case err := <-localDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("remote Git refresh blocked unrelated local admission")
	}
	close(release)
	if err := <-remoteDone; err != nil {
		t.Fatal(err)
	}
}

// Keep the canonical optional contracts visible at compile time.
var (
	_ service.JobController          = (*ArgoJobs)(nil)
	_ service.JobAdopter             = (*ArgoJobs)(nil)
	_ service.JobCreateSettler       = (*ArgoJobs)(nil)
	_ service.PipelineSourceResolver = (*ArgoJobs)(nil)
)

// Keep historical synthetic controller captures unchanged; exercise the current
// configuration schema after removing the retired placement-only fields.
func decodeCurrentTargetFixture(body []byte, selection *map[string]any) error {
	if err := json.Unmarshal(body, selection); err != nil {
		return err
	}
	for _, retired := range []string{"node_name", "node_uid", "provider_id", "zone", "pool", "taint_key", "taint_value"} {
		delete(*selection, retired)
	}
	return nil
}

// This target fixture supplies the same required observation seam as
// native wait fixtures. A9 removes the target implementation and this test.
type terminalTargetWatchSnapshot struct{ workflow *wfv1.Workflow }

func (h terminalTargetWatchSnapshot) Workflow(string) (*wfv1.Workflow, bool) { return h.workflow, true }
func (terminalTargetWatchSnapshot) PodsOf(string) []*corev1.Pod              { return nil }
func (terminalTargetWatchSnapshot) Pods() []*corev1.Pod                      { return nil }
func (terminalTargetWatchSnapshot) Healthy() bool                            { return true }
func (terminalTargetWatchSnapshot) Synced() bool                             { return true }
func (terminalTargetWatchSnapshot) UnhealthySince() (time.Time, bool)        { return time.Time{}, false }
func (terminalTargetWatchSnapshot) Subscribe(string) (<-chan struct{}, func()) {
	return make(chan struct{}, 1), func() {}
}

func (w *targetWorkflowFake) Patch(_ context.Context, _ string, kind types.PatchType, data []byte, _ metav1.PatchOptions, _ ...string) (*wfv1.Workflow, error) {
	if w.err != nil {
		return nil, w.err
	}
	if w.object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "missing")
	}
	var patch struct {
		Metadata struct {
			UID             types.UID `json:"uid"`
			ResourceVersion string    `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			Shutdown wfv1.ShutdownStrategy `json:"shutdown"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &patch); err != nil {
		return nil, err
	}
	if kind != types.MergePatchType || patch.Metadata.UID != w.object.UID || patch.Metadata.ResourceVersion != "" && patch.Metadata.ResourceVersion != w.object.ResourceVersion {
		return nil, errors.New("missing Workflow UID/resourceVersion fence")
	}
	if patch.Spec.Shutdown != wfv1.ShutdownStrategyStop {
		return nil, errors.New("expected Stop")
	}
	w.object.Spec.Shutdown = patch.Spec.Shutdown
	return w.object.DeepCopy(), nil
}
