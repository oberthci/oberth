package argojob

import (
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type custodyJournal struct {
	db *store.Store
	b  model.ExecutionTargetBinding
}

func (j custodyJournal) Binding() model.ExecutionTargetBinding { return j.b }
func (j custodyJournal) Prepare(ctx context.Context, r model.ExecutionTargetResource) (bool, error) {
	return j.db.PrepareExecutionTargetResource(ctx, r)
}
func (j custodyJournal) Observe(ctx context.Context, k, ns, n, uid string) error {
	return j.db.ObserveExecutionTargetUID(ctx, j.b.RunID, k, ns, n, uid)
}
func (j custodyJournal) Resources(ctx context.Context) ([]model.ExecutionTargetResource, error) {
	return j.db.ExecutionTargetResources(ctx, j.b.RunID)
}
func (j custodyJournal) Cancelled(ctx context.Context) (bool, error) {
	return j.db.ExecutionTargetCancelled(ctx, j.b.RunID)
}
func (j custodyJournal) RetainOwned(ctx context.Context, r model.ExecutionTargetResource, uid string) error {
	return j.db.RetainExecutionTargetOwnedResource(ctx, r, uid)
}

func newCustodyJournal(t *testing.T, name string) custodyJournal {
	t.Helper()
	db, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	u, err := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "upstream", Kind: "github", BaseURL: "https://example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := storetest.EnqueueRun(db, t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature", SHA: strings.Repeat("a", 40), Actor: "fixture", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := db.SetRunWorkflowName(t.Context(), result.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	b := model.ExecutionTargetBinding{RefKind: string(run.RefKind), Trigger: run.Trigger, ScheduleEntry: run.ScheduleEntry, RunID: run.ID, Workflow: name, Target: "remote", Repository: "upstream/org/repo", RepositoryID: repo.ID, Ref: run.Ref, SHA: run.SHA, TestedSHA: run.SHA, Namespace: "remote", Snapshot: json.RawMessage(`{}`)}
	if err := db.BindExecutionTarget(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if err := db.BindExecutionTargetAdmission(t.Context(), run.ID, []byte(`{"native":"admitted"}`)); err != nil {
		t.Fatal(err)
	}
	return custodyJournal{db, b}
}

// A transport can commit and lose its response. The fake exposes the late
// object only when told; a second POST is counted as a test failure.
type custodyWorkflows struct {
	object          *wfv1.Workflow
	late            *wfv1.Workflow
	posts, deletes  int
	postErr, getErr error
}

func (w *custodyWorkflows) Create(_ context.Context, object *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	w.posts++
	copy := object.DeepCopy()
	copy.UID = "original-workflow"
	copy.ResourceVersion = "1"
	if w.postErr != nil {
		w.late = copy
		return nil, w.postErr
	}
	w.object = copy
	return copy.DeepCopy(), nil
}
func (w *custodyWorkflows) Get(context.Context, string, metav1.GetOptions) (*wfv1.Workflow, error) {
	if w.getErr != nil {
		return nil, w.getErr
	}
	if w.object == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, "missing")
	}
	return w.object.DeepCopy(), nil
}
func (w *custodyWorkflows) Delete(_ context.Context, _ string, o metav1.DeleteOptions) error {
	w.deletes++
	if w.object == nil {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "missing")
	}
	if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != w.object.UID {
		return errors.New("missing original UID fence")
	}
	w.object = nil
	return nil
}

func custodyWorkflow(t *testing.T, j custodyJournal) *wfv1.Workflow {
	t.Helper()
	r := testRequest(pipeline.TriggerCI, greedyDocument)
	r.RunID = j.b.RunID
	r.Name = j.b.Workflow
	r.RepositoryID = j.b.RepositoryID
	wf, err := Build(testConfig(), r)
	if err != nil {
		t.Fatal(err)
	}
	wf.Namespace = j.b.Namespace
	return wf
}

func TestTargetWorkflowAmbiguousPOSTRestartLateCommitAndUIDFence(t *testing.T) {
	j := newCustodyJournal(t, "remote-workflow")
	remote, local := &custodyWorkflows{postErr: context.DeadlineExceeded}, &custodyWorkflows{}
	kube := fake.NewClientset()
	clients, err := NewTargetClients(remote, kube, j, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	wf := custodyWorkflow(t, j)
	if _, err := clients.WorkflowClient().Create(t.Context(), wf, metav1.CreateOptions{}); !errors.Is(err, ErrTargetUncertain) {
		t.Fatal("POST ambiguity lost", err)
	}
	var ambiguous interface{ AmbiguousCreate() bool }
	if !errors.As(ErrTargetUncertain, &ambiguous) || !ambiguous.AmbiguousCreate() {
		t.Fatal("scheduler cannot retain admission")
	}
	// Empty process state, same canonical store. GET NotFound is not a retry.
	clients, err = NewTargetClients(remote, kube, j, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients.WorkflowClient().Create(t.Context(), wf, metav1.CreateOptions{}); !errors.Is(err, ErrTargetUncertain) || remote.posts != 1 {
		t.Fatal("ambiguous POST repeated", err, remote.posts)
	}
	if err := j.db.CancelExecutionTarget(t.Context(), j.b.RunID); err != nil {
		t.Fatal(err)
	}
	clients.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	remote.object = remote.late
	done, err := clients.Settle(t.Context(), time.Time{})
	if err != nil || done || remote.deletes != 0 || remote.object.Spec.Shutdown != wfv1.ShutdownStrategyStop {
		t.Fatal("late commit not retained/stopped", done, err)
	}
	if done, err := clients.Settle(t.Context(), time.Time{}); done || err != nil {
		t.Fatal("running Stop released target custody", done, err)
	}
	remote.object.Status.Phase = wfv1.WorkflowFailed
	done, err = clients.Settle(t.Context(), time.Time{})
	if err != nil || !done || remote.object == nil {
		t.Fatal("Argo completion not settled or Workflow deleted", done, err)
	}
	if local.posts != 0 || local.deletes != 0 {
		t.Fatal("local fallback touched")
	}
	remote.object = remote.late.DeepCopy()
	remote.object.UID = "replacement"
	done, err = clients.Settle(t.Context(), time.Time{})
	if done || err == nil || remote.deletes != 0 {
		t.Fatal("replacement deleted or accepted")
	}
}

func TestTargetSeedAndStoragePOSTsRemainOwnedWithoutWorkflow(t *testing.T) {
	for _, kind := range []string{"Pod", "PersistentVolumeClaim"} {
		t.Run(kind, func(t *testing.T) {
			j := newCustodyJournal(t, "seed-workflow")
			kube := fake.NewClientset()
			remote := &custodyWorkflows{}
			clients, _ := NewTargetClients(remote, kube, j, testConfig())
			resource := "pods"
			if kind == "PersistentVolumeClaim" {
				resource = "persistentvolumeclaims"
			}
			posts, deletes := 0, 0
			var late runtime.Object
			kube.PrependReactor("create", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
				posts++
				if posts > 1 {
					t.Fatal("second ambiguous seed/source POST")
				}
				object := action.(ktesting.CreateAction).GetObject().DeepCopyObject()
				switch p := object.(type) {
				case *corev1.Pod:
					p.UID = "original-source"
				case *corev1.PersistentVolumeClaim:
					p.UID = "original-source"
				}
				late = object
				return true, nil, context.DeadlineExceeded
			})
			kube.PrependReactor("delete", resource, func(action ktesting.Action) (bool, runtime.Object, error) {
				deletes++
				options := action.(ktesting.DeleteAction).GetDeleteOptions()
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "original-source" {
					t.Fatal("source deletion lacked original UID")
				}
				return false, nil, nil
			})
			create := func() error {
				if kind == "Pod" {
					_, err := clients.Kubernetes().CoreV1().Pods(j.b.Namespace).Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: j.b.Namespace, Name: sourceClaimName(j.b.Workflow)}}, metav1.CreateOptions{})
					return err
				}
				_, err := clients.Kubernetes().CoreV1().PersistentVolumeClaims(j.b.Namespace).Create(t.Context(), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: j.b.Namespace, Name: sourceClaimName(j.b.Workflow)}}, metav1.CreateOptions{})
				return err
			}
			if err := create(); !errors.Is(err, ErrTargetUncertain) {
				t.Fatal(err)
			}
			clients, _ = NewTargetClients(remote, kube, j, testConfig())
			if err := create(); !errors.Is(err, ErrTargetUncertain) || posts != 1 {
				t.Fatal("restart lost source custody", err)
			}
			if err := j.db.CancelExecutionTarget(t.Context(), j.b.RunID); err != nil {
				t.Fatal(err)
			}
			if err := kube.Tracker().Add(late); err != nil {
				t.Fatal(err)
			}
			clients.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
			if done, err := clients.Settle(t.Context(), time.Time{}); done || err != nil || deletes != 1 {
				t.Fatal("late source not UID-fenced", done, err)
			}
			if done, err := clients.Settle(t.Context(), time.Time{}); !done || err != nil {
				t.Fatal("source absence not confirmed", done, err)
			}
			if remote.posts != 0 {
				t.Fatal("Workflow allocated while source outcome uncertain")
			}
		})
	}
}

func TestTargetDeniedReadsReplacementAndUnresolvedResultNeverBecomeAbsence(t *testing.T) {
	j := newCustodyJournal(t, "blocked-workflow")
	remote := &custodyWorkflows{}
	kube := fake.NewClientset()
	clients, _ := NewTargetClients(remote, kube, j, testConfig())
	wf, err := clients.WorkflowClient().Create(t.Context(), custodyWorkflow(t, j), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	remote.getErr = apierrors.NewForbidden(schema.GroupResource{Resource: "workflows"}, wf.Name, errors.New("denied"))
	if done, err := clients.Settle(t.Context(), time.Time{}); done || err == nil {
		t.Fatal("denied read released custody")
	}
	if err := clients.VerifyResultCustody(t.Context()); err == nil {
		t.Fatal("unobserved native result accepted")
	}
	remote.getErr = nil
	remote.object.UID = types.UID("replacement")
	if err := clients.VerifyResultCustody(t.Context()); err == nil {
		t.Fatal("replacement accepted as original result")
	}
	remote.object = wf.DeepCopy()
	remote.object.Spec.Entrypoint = "changed"
	if _, err := clients.WorkflowClient().Get(t.Context(), wf.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("mutated native spec accepted")
	}
}

func TestTargetUnknownWorkflowUIDCannotDisposeUnprovenDescendant(t *testing.T) {
	j := newCustodyJournal(t, "unknown-parent")
	remote := &custodyWorkflows{postErr: context.DeadlineExceeded}
	kube := fake.NewClientset()
	clients, _ := NewTargetClients(remote, kube, j, testConfig())
	if _, err := clients.WorkflowClient().Create(t.Context(), custodyWorkflow(t, j), metav1.CreateOptions{}); err == nil {
		t.Fatal("missing transport error")
	}
	child := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unknown-child", Namespace: j.b.Namespace, UID: "child-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": j.b.Workflow}}}
	if err := kube.Tracker().Add(child); err != nil {
		t.Fatal(err)
	}
	clients.now = func() time.Time { return time.Now().Add(time.Hour) }
	if done, err := clients.Settle(t.Context(), time.Time{}); done || err == nil {
		t.Fatal("unknown original UID counted as resource absence")
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("unproven child deleted")
		}
	}
}

func TestTargetConfigurationMigrationRetainsOriginalObjectIdentity(t *testing.T) {
	j := newCustodyJournal(t, "migrated-workflow")
	remote := &custodyWorkflows{}
	kube := fake.NewClientset()
	clients, err := NewTargetClients(remote, kube, j, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	original := custodyWorkflow(t, j)
	original.Annotations["oberth.ci/execution-placement"] = strings.Repeat("a", 64)
	created, err := clients.WorkflowClient().Create(t.Context(), original, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The migration retains this digest from immutable POST intents before
	// removing retired fields from the executor snapshot.
	j.b.CustodyDigest = clients.digest
	j.b.Snapshot = json.RawMessage(`{"Config":{"ExecutionPlacement":{"namespace":"remote"}}}`)
	migrated, err := NewTargetClients(remote, kube, j, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	observed, err := migrated.WorkflowClient().Get(t.Context(), original.Name, metav1.GetOptions{})
	if err != nil || observed.UID != created.UID {
		t.Fatal("original object not adopted", observed, err)
	}
	p := &ExecutionPlacement{digest: strings.Repeat("b", 64)}
	config := testConfig()
	config.Custody = j
	if err := p.verifyWorkflowIdentity(t.Context(), config, observed); err != nil {
		t.Fatal("retained authority lost", err)
	}
	forged := observed.DeepCopy()
	forged.Annotations["oberth.ci/execution-placement"] = p.digest
	if err := p.verifyWorkflowIdentity(t.Context(), config, forged); err == nil {
		t.Fatal("current configuration replaced original authority")
	}
	remote.object.UID = "replacement"
	if _, err := migrated.WorkflowClient().Get(t.Context(), original.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("migration accepted replacement UID")
	}
	remote.object = created.DeepCopy()
	if _, err := requestWorkflowShutdown(t.Context(), migrated.WorkflowClient(), created, wfv1.ShutdownStrategyStop); err != nil {
		t.Fatal("original fenced Stop failed", err)
	}
	if remote.posts != 1 || remote.deletes != 0 || remote.object.Spec.Shutdown != wfv1.ShutdownStrategyStop {
		t.Fatal("migration repeated POST, deleted or failed to Stop", remote.posts, remote.deletes)
	}
}

func TestTargetStopKeepsChildrenUntilExitHandlersFinish(t *testing.T) {
	j := newCustodyJournal(t, "exit-workflow")
	remote := &custodyWorkflows{}
	kube := fake.NewClientset()
	clients, err := NewTargetClients(remote, kube, j, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = clients.WorkflowClient().Create(t.Context(), custodyWorkflow(t, j), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// PVC kinds sort before Workflow in the journal. Their presence must never
	// let settlement clean source storage before it has checked onExit.
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "exit-source", Namespace: j.b.Namespace, UID: "source-uid"}}
	if err = kube.Tracker().Add(pvc); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(pvc)
	if _, err = j.Prepare(t.Context(), model.ExecutionTargetResource{RunID: j.b.RunID, Kind: "PersistentVolumeClaim", Namespace: pvc.Namespace, Name: pvc.Name, NotBefore: time.Now(), Object: body}); err != nil {
		t.Fatal(err)
	}
	if err = j.Observe(t.Context(), "PersistentVolumeClaim", pvc.Namespace, pvc.Name, string(pvc.UID)); err != nil {
		t.Fatal(err)
	}
	clients.now = func() time.Time { return time.Now().Add(time.Hour) }
	if done, err := clients.Settle(t.Context(), time.Time{}); err != nil || done {
		t.Fatal(done, err)
	}
	if remote.object == nil || remote.object.Spec.Shutdown != wfv1.ShutdownStrategyStop || remote.deletes != 0 {
		t.Fatal("Stop did not retain Workflow")
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("Stop deleted onExit storage", action)
		}
	}
	remote.object.Status.Phase = wfv1.WorkflowFailed
	if _, err = clients.Settle(t.Context(), time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err = kube.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(t.Context(), pvc.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("terminal cleanup kept source", err)
	}
	if remote.deletes != 0 || remote.object == nil {
		t.Fatal("terminal cleanup deleted Workflow")
	}
	if err := j.db.CancelExecutionTarget(t.Context(), j.b.RunID); err != nil {
		t.Fatal(err)
	}
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, ""} {
		remote.object.Spec.Shutdown = strategy
		if err := clients.VerifyResultCustody(t.Context()); err != nil {
			t.Fatal("cleanup lost original native terminal verdict", err)
		}
	}

}
