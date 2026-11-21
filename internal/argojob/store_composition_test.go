package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	statecontract "github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/store"
)

func TestLandedStateSharedContract(t *testing.T) {
	t.Parallel()
	t.Run("store", func(t *testing.T) { statecontract.RunSuite(t, realRunFixture) })
	t.Run("fake", func(t *testing.T) { statecontract.RunSuite(t, memoryRunFixture) })
}

func TestSubmitBindsRealStoreAndAdoptsPersistedWorkflow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	options := store.DefaultOptions(compositionClock)
	s, err := store.Create(t.Context(), path, options)
	compositionOK(t, err)
	t.Cleanup(func() { compositionOK(t, s.Close()) })
	registerCompositionRepository(t, s, "origin/team/repo")
	commit := strings.Repeat("a", 40)
	request := model.CIRequest{ReceiveID: "native-receive", Repo: "origin/team/repo", Ref: "refs/heads/main", ObjectSHA: commit, CommitSHA: &commit, Trigger: model.TriggerBranch}
	request.Actor.Identity, request.Actor.AuthMethod = "operator", "cert"
	prepared := compositionAdmission(t, "native-run", request)
	_, err = s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: request.Repo, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: prepared}}})
	compositionOK(t, err)
	native := &nativefake.Workflows{}
	executor := argojob.New(argojob.Config{Workflows: native, Runs: s})
	binding, err := executor.Create(t.Context(), prepared)
	compositionOK(t, err)
	persisted, err := s.Run(t.Context(), binding.RunID)
	compositionOK(t, err)
	if persisted.Origin.WorkflowName != binding.Workflow.Name || persisted.Origin.WorkflowUID != binding.Workflow.UID || binding.Identity != persisted.Admission.Digest || binding.SpecSHA256 != persisted.Admission.SpecDigest {
		t.Fatal("durable native binding differs from admission", binding)
	}
	compositionOK(t, s.Close())
	reopened, err := store.Open(t.Context(), path, options)
	compositionOK(t, err)
	s = reopened
	persisted, err = s.Run(t.Context(), binding.RunID)
	compositionOK(t, err)
	binding.Workflow.Name, binding.Workflow.UID = persisted.Origin.WorkflowName, persisted.Origin.WorkflowUID
	restarted := argojob.New(argojob.Config{Workflows: native})
	if _, err = restarted.Adopt(t.Context(), binding); err != nil || native.Posts != 1 {
		t.Fatal("adoption refired native work", err, native.Posts)
	}
	native.Object.Annotations[nativebinding.IdentityAnnotation] = prepared.View().Digest
	if _, err = restarted.Adopt(t.Context(), binding); err == nil {
		t.Fatal("admission digest substituted for native spec digest")
	}

}

func TestProjectedEvidenceUsesRealStoreSequenceWithoutChangingRevision(t *testing.T) {
	t.Parallel()
	fixture := realRunFixture(t)
	s := fixture.Port.(*store.Store)
	commit := strings.Repeat("a", 40)
	request := model.CIRequest{ReceiveID: "evidence-receive", Repo: "origin/team/repo", Ref: "refs/heads/main", ObjectSHA: commit, CommitSHA: &commit, Trigger: model.TriggerBranch}
	request.Actor.Identity, request.Actor.AuthMethod = "operator", "cert"
	p := compositionAdmission(t, "evidence-run", request)
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: request.Repo, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: p}}})
	compositionOK(t, err)
	native := &nativefake.Workflows{}
	executor := argojob.New(argojob.Config{Workflows: native, Runs: s})
	binding, err := executor.Create(t.Context(), p)
	compositionOK(t, err)
	run, err := s.Run(t.Context(), binding.RunID)
	compositionOK(t, err)
	w := native.Object.DeepCopy()
	w.Status.Phase = wf.WorkflowSucceeded
	n := nativeNode("node", ".build", wf.NodeSucceeded, 1)
	n.Name = w.Name + ".build"
	w.Status.Nodes = wf.Nodes{n.ID: n}
	observation := project(t, argojob.Projection{Plan: p.View().Plan, Workflow: w, Pods: []*core.Pod{nativePod(w, n, 0)}})
	if !observation.Complete || len(observation.Nodes) != 1 {
		t.Fatal("fixture lacks complete native evidence")
	}
	before, err := s.Events(t.Context(), stateport.EventFilter{Limit: 500})
	compositionOK(t, err)
	sequence, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: run.ID, Nodes: len(observation.Nodes)})
	compositionOK(t, err)
	completion := stateport.EvidenceCompletion{RunID: run.ID, Sequence: sequence}
	if err = s.CompleteEvidence(t.Context(), stateport.Action{}, completion); err == nil {
		t.Fatal("store completed an unwritten sequence")
	}
	compositionOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: run.ID, Sequence: sequence, Nodes: observation.Nodes}))
	compositionOK(t, s.CompleteEvidence(t.Context(), stateport.Action{}, completion))
	after, err := s.Run(t.Context(), run.ID)
	compositionOK(t, err)
	events, err := s.Events(t.Context(), stateport.EventFilter{Limit: 500})
	compositionOK(t, err)
	if !after.EvidenceComplete || after.Revision != run.Revision || len(events.Events) != len(before.Events) {
		t.Fatal("evidence changed control facts or failed to complete", after)
	}
	compositionOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: run.ID, Revision: run.Revision, Status: observation.Status, Reason: observation.Reason}))
}

func TestNativeBindingAndEvidenceSharedContract(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"store", "fake"} {
		t.Run(kind, func(t *testing.T) {
			statecontract.NativeSuite(t, func(t *testing.T) statecontract.NativeFixture {
				r := requestForProgress()
				p := compositionAdmission(t, "native-contract", r)
				var s interface {
					statecontract.NativeStore
					argojob.Runs
				}
				if kind == "fake" {
					s = memoryAdmissionFor(t, p, r)
				} else {
					real := compositionStore(t)
					registerCompositionRepository(t, real, r.Repo)
					_, err := real.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
					compositionOK(t, err)
					s = real
				}
				b, err := argojob.New(argojob.Config{Workflows: &nativefake.Workflows{}, Runs: s}).Create(t.Context(), p)
				compositionOK(t, err)
				return statecontract.NativeFixture{Store: s, Binding: b}
			})
		})
	}
}
