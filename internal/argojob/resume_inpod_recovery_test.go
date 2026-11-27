package argojob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type inPodNative struct {
	Workflows
	w           *wf.Workflow
	patches     int
	reads       int
	lostPatch   bool
	beforePatch func()
}

type inPodStaleCache struct{ w *wf.Workflow }

func (c inPodStaleCache) Workflow(string) (*wf.Workflow, bool) { return c.w.DeepCopy(), true }
func (inPodStaleCache) Pods(string) []*core.Pod                { return nil }
func (inPodStaleCache) OnChange(func(string))                  {}

func (n *inPodNative) Get(context.Context, string, meta.GetOptions) (*wf.Workflow, error) {
	n.reads++
	return n.w.DeepCopy(), nil
}
func (n *inPodNative) Patch(_ context.Context, q PatchRequest) (*wf.Workflow, error) {
	n.patches++
	if n.beforePatch != nil {
		n.beforePatch()
	}
	patch, err := jsonpatch.DecodePatch(q.Body)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(n.w)
	if err != nil {
		return nil, err
	}
	body, err = patch.Apply(body)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(body, &n.w); err != nil {
		return nil, err
	}
	if n.lostPatch {
		return nil, errors.New("patch reply lost")
	}
	return n.w.DeepCopy(), nil
}

type inPodTransport struct {
	Pods
	pod            *core.Pod
	native         *inPodNative
	state          string
	runs, releases int
	inspections    int
	lostReply      bool
	digest         string
}

func (p *inPodTransport) Get(context.Context, string, meta.GetOptions) (*core.Pod, error) {
	return p.pod.DeepCopy(), nil
}
func (p *inPodTransport) List(context.Context, meta.ListOptions) (*core.PodList, error) {
	return &core.PodList{Items: []core.Pod{*p.pod.DeepCopy()}}, nil
}
func (p *inPodTransport) Delete(context.Context, string, meta.DeleteOptions) error {
	return fmt.Errorf("in-Pod resume deleted a Pod")
}
func (p *inPodTransport) Exec(_ context.Context, q ExecRequest) error {
	if q.Command[3] != string(p.pod.UID) {
		return fmt.Errorf("unfenced exec")
	}
	frame := holdFrame{Type: p.state, OriginalExit: 7, HeldSince: time.Unix(90, 0), TTL: 1200, RemainingMS: 60000}
	switch q.Command[1] {
	case "exec":
		p.inspections++
		frame.Type = "exit"
	case "rerun":
		if p.native.w.Labels[nativebinding.RunLabel] != "r2" {
			return fmt.Errorf("dispatch before relabel")
		}
		data, err := io.ReadAll(q.In)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != p.digest {
			return fmt.Errorf("wrong snapshot digest")
		}
		p.runs++
		p.state, frame.Type = "rerunning", "rerunning"
		if p.lostReply {
			return errors.New("rerun reply lost")
		}
	case "release":
		p.releases++
		frame.Type = "closed"
	}
	body, _ := json.Marshal(frame)
	_, err := fmt.Fprintln(q.Err, "oberth-hold: "+string(body))
	return err
}

func inPodRecovery(t *testing.T) (Config, stateport.Run, ResumePlan, *inPodNative, *inPodTransport) {
	t.Helper()
	leaf := wf.Template{Name: "build", Container: &core.Container{Command: []string{"true"}}}
	if err := wrapHold(&leaf, nativebinding.Preparation{HelperImage: "oberth.ci/helper:1"}); err != nil {
		t.Fatal(err)
	}
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "shared", Namespace: "ci", UID: "uid", ResourceVersion: "7"}, Spec: wf.WorkflowSpec{Entrypoint: "build", Templates: []wf.Template{leaf}}, Status: wf.WorkflowStatus{Phase: wf.WorkflowRunning, Nodes: wf.Nodes{"node": {ID: "node", Name: "shared", TemplateName: "build", Type: wf.NodeTypePod, Phase: wf.NodeRunning}}}}
	v := admissioncontract.PreparedView{RunID: "r1", Repository: "origin/team/repo", Ref: "refs/heads/topic", CommitSHA: strings.Repeat("a", 40), Digest: strings.Repeat("b", 64), SpecDigest: strings.Repeat("c", 64), Trigger: model.TriggerBranch, Identity: admissioncontract.IdentityEvidence{Namespace: "ci"}, Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "", Name: "build", HoldEligible: true, HoldArmed: true}}}}
	stampSubmission(w, v, 1)
	controller := true
	pod := &core.Pod{ObjectMeta: meta.ObjectMeta{Name: "pod", Namespace: "ci", UID: "pod-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": "shared"}, Annotations: map[string]string{"workflows.argoproj.io/node-id": "node", "workflows.argoproj.io/node-name": "shared"}, OwnerReferences: []meta.OwnerReference{{APIVersion: wf.SchemeGroupVersion.String(), Kind: "Workflow", Name: "shared", UID: "uid", Controller: &controller}}}, Status: core.PodStatus{Phase: core.PodRunning, ContainerStatuses: []core.ContainerStatus{{Name: "main", Ready: true, State: core.ContainerState{Running: &core.ContainerStateRunning{}}}}}}
	fix := strings.Repeat("f", 40)
	v.RunID, v.CommitSHA = "r2", fix
	r := stateport.Run{RepositoryID: 1, Revision: 1, RunSummary: stepmodel.RunSummary{ID: "r2", Repo: v.Repository, Ref: v.Ref, CommitSHA: &fix, Status: stepmodel.RunQueued}, Admission: v, Origin: model.RunOrigin{RunCause: model.RunCause{AssistRequested: true}, Assisted: true, ResumedFrom: "r1", ResumeUID: "uid", WorkflowName: "shared"}}
	bundle, _ := rerunBundle(t, minimalRerunFiles(), nil, fix)
	reader := bytes.NewReader(bundle)
	var manifest rerunManifest
	if err := readHoldFrame(reader, &manifest); err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(reader)
	held := &nativebinding.ResumeHold{RunID: "r2", PodName: "pod", PodUID: "pod-uid", NodeID: "node", Step: "", OriginalExit: 7, HeldSince: time.Unix(90, 0), Source: sourcecontract.Binding{Revision: sourcecontract.Revision{Repository: v.Repository, Commit: fix}}}
	export := func(context.Context, sourcecontract.Binding) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	}
	transfer, err := resumeExport(t.Context(), export, *held)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	_, _ = io.Copy(hash, transfer)
	_ = transfer.Close()
	held.Digest = hex.EncodeToString(hash.Sum(nil))
	updated := w.DeepCopy()
	relabel(updated, "r2", fix)
	plan := ResumePlan{Workflow: updated, UID: w.UID, ResourceVersion: w.ResourceVersion, ObservedPods: []ResumePod{{Name: pod.Name, UID: pod.UID}}, Remaining: []string{""}, InPod: held}
	owner := statefake.NewRuns(func() time.Time { return time.Unix(100, 0) })
	owner.SeedRun(r)
	native := &inPodNative{w: w}
	transport := &inPodTransport{pod: pod, native: native, state: "held", digest: held.Digest}
	config := Config{Workflows: native, Pods: transport, Runs: owner, ResumeExport: export, Now: func() time.Time { return time.Unix(100, 0) }, Reporter: &reviewR2Reporter{}}
	return config, r, plan, native, transport
}

func TestInPodResumeRecovery(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"insert", "relabel", "lost patch reply", "lost rerun reply", "already rerunning", "closed container", "stale informer"} {
		t.Run(point, func(t *testing.T) { checkInPodRecovery(t, point) })
	}
}

func recoveryPoint(point string, native *inPodNative, pods *inPodTransport, plan ResumePlan) int {
	if point == "relabel" || point == "already rerunning" || point == "closed container" {
		native.w = plan.Workflow.DeepCopy()
	}
	if point == "lost patch reply" {
		native.lostPatch = true
	}
	if point == "lost rerun reply" {
		pods.lostReply = true
	}
	if point == "already rerunning" {
		pods.state = "rerunning"
		return 0
	}
	if point == "closed container" {
		n := native.w.Status.Nodes["node"]
		n.Phase = wf.NodeSucceeded
		native.w.Status.Nodes["node"] = n
		return 0
	}
	return 1
}

func checkInPodRecovery(t *testing.T, point string) {
	t.Helper()
	config, r, plan, native, pods := inPodRecovery(t)
	before := native.w.Status.DeepCopy()
	if point == "stale informer" {
		config.Cache = inPodStaleCache{native.w.DeepCopy()}
	}
	want := recoveryPoint(point, native, pods, plan)
	for range 2 {
		e := New(config)
		if err := e.applyInPod(t.Context(), r, plan); err != nil {
			t.Fatal(err)
		}
		e.queue.ShutDown()
		e.collector.queue.ShutDown()
	}
	if pods.runs != want || pods.releases != 0 || native.patches > 1 {
		t.Fatal("recovery repeated execution or lost hold", pods.runs, pods.releases, native.patches)
	}
	if point != "closed container" && !reflect.DeepEqual(*before, native.w.Status) {
		t.Fatal("handoff rewrote native status")
	}
}

func TestInspectionCannotInvokeRerunBeforeSecretStep(t *testing.T) {
	t.Parallel()
	config, r, plan, native, pods := inPodRecovery(t)
	native.w = plan.Workflow.DeepCopy()
	r.Admission.Plan.Entries = append(r.Admission.Plan.Entries, admissioncontract.PlanEntry{Path: "secret", Name: "publish", SecretRoute: true})
	owner := config.Runs.(*statefake.Runs)
	owner.SeedRun(r)
	e := New(config)
	defer e.queue.ShutDown()
	defer e.collector.queue.ShutDown()
	b := runBinding(r.Admission, r.RepositoryID, nativebinding.Object{Namespace: "ci", Name: "shared", UID: "uid"})
	q := HoldExecRequest{Binding: b, Hold: resumeLiveHold(r, plan), Command: []string{holdBinary, "rerun"}}
	if _, err := e.HoldExec(t.Context(), q); err == nil || pods.inspections != 0 {
		t.Fatal("inspection bypassed secret fence", err, pods.inspections)
	}
	r.Admission.Plan.Entries[1].SecretRoute = false
	owner.SeedRun(r)
	t.Run("secret-free inspection cannot rerun", TestInspectionCannotInvokeRerunEvenWithoutSecrets)
}

func TestInPodApplyDormantGate(t *testing.T) {
	t.Parallel()
	config, r, plan, native, pods := inPodRecovery(t)
	var err error
	r.Origin.ResumePlan, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	config.Runs.(*statefake.Runs).SeedRun(r)
	e := New(config)
	defer e.queue.ShutDown()
	defer e.collector.queue.ShutDown()
	if err = e.ApplyResume(t.Context(), r.ID); err == nil || !strings.Contains(err.Error(), "assist unavailable") {
		t.Fatal("dormant dispatch admitted", err)
	}
	if pods.runs != 0 || native.patches != 0 {
		t.Fatal("dormant gate allowed effects")
	}
}

func TestInPodApplyUIDAndVersionFences(t *testing.T) {
	t.Parallel()
	for _, point := range []string{"version", "Pod UID", "patch race"} {
		t.Run(point, func(t *testing.T) {
			config, r, plan, native, pods := inPodRecovery(t)
			switch point {
			case "version":
				native.w.ResourceVersion = "8"
			case "Pod UID":
				pods.pod.UID = "replaced"
			case "patch race":
				native.beforePatch = func() { native.w.ResourceVersion = "8" }
			}
			e := New(config)
			defer e.queue.ShutDown()
			defer e.collector.queue.ShutDown()
			if err := e.applyInPod(t.Context(), r, plan); err == nil {
				t.Fatal("changed object resumed")
			}
			if pods.runs != 0 || native.w.Labels[nativebinding.RunLabel] != "r1" {
				t.Fatal("failed fence changed owner or executed command")
			}
		})
	}
}
