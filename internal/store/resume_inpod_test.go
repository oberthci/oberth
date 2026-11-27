package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func inPodAcceptance(t *testing.T, s *Store) (stateport.Run, stateport.Acceptance) {
	t.Helper()
	_, seed := acceptedMode(t, s, "seed", true)
	input := reacceptance(t, seed, "held-run")
	v := input.Prepared.View()
	v.Plan.Entries[0].HoldArmed, v.Plan.Entries[0].HoldEligible = true, true
	v.SourceBindings = []sourcecontract.Binding{{Template: "build", Artifact: "source", Path: "/work/src", RepositoryID: 1, Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, Revision: sourcecontract.Revision{Repository: v.Repository, Commit: v.CommitSHA}}}
	var err error
	input.Prepared, err = admissioncontract.Seal(v)
	requireOK(t, err)
	r, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	requireOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, runBinding(r)))
	requireOK(t, s.SaveProgress(t.Context(), r.ID, stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepRunning}}}))
	r, err = s.Run(t.Context(), r.ID)
	requireOK(t, err)
	input = reacceptance(t, input, "successor")
	fix := strings.Repeat("f", 40)
	input.Request.ObjectSHA, input.Request.CommitSHA = fix, &fix
	v = input.Prepared.View()
	v.ObjectSHA, v.CommitSHA = fix, fix
	v.SourceBindings[0].Revision.Commit = fix
	input.Prepared, err = admissioncontract.Seal(v)
	requireOK(t, err)
	h := &nativebinding.ResumeHold{RunID: "successor", PodName: "pod", PodUID: "pod-uid", NodeID: "node", Step: "build", OriginalExit: 7, HeldSince: s.options.Clock(), Digest: strings.Repeat("d", 64), Source: v.SourceBindings[0]}
	plan := nativebinding.ResumePlan{UID: "uid", ResourceVersion: "7", Remaining: []string{"build"}, Workflow: &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: r.Origin.WorkflowName, Labels: map[string]string{nativebinding.RunLabel: "successor"}}}, InPod: h}
	body, err := json.Marshal(plan)
	requireOK(t, err)
	input.Resume = &model.RunOrigin{ResumedFrom: r.ID, ResumeUID: r.Origin.NativeUID(), WorkflowName: r.Origin.WorkflowName, Assisted: true, ResumePlan: body}
	return r, input
}

func TestInPodHandoffAtomicAndPredecessorFrozen(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	old, input := inPodAcceptance(t, s)
	next, err := acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	final, err := s.Run(t.Context(), old.ID)
	requireOK(t, err)
	if final.Status != stepmodel.RunFailed || final.DisplayReason != "resumed in place" || !final.Origin.Assisted || !strings.Contains(final.Detail, "exit 7") || !next.Origin.Assisted || !next.Origin.Resumed() {
		t.Fatal("handoff facts", final, next)
	}
	checkHeldFinalEvidence(t, s, old.ID)
	checkHandedOffWrites(t, s, final)
	checkInPodAudit(t, s, next.ID)
}

func checkHeldFinalEvidence(t *testing.T, s *Store, id string) {
	t.Helper()
	steps, err := s.Steps(t.Context(), id)
	requireOK(t, err)
	if len(steps) != 1 || steps[0].Status != stepmodel.StepFailed {
		t.Fatal("held failure not final", steps)
	}
	nodes, err := s.Nodes(t.Context(), id)
	requireOK(t, err)
	if len(nodes) != 1 || nodes[0].Exit == nil || *nodes[0].Exit != 7 || nodes[0].Status != stepmodel.StepFailed {
		t.Fatal("held native failure not durable", nodes)
	}
}

func checkHandedOffWrites(t *testing.T, s *Store, final stateport.Run) {
	t.Helper()
	if err := s.SaveProgress(t.Context(), final.ID, stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Status: stepmodel.StepPassed}}}); err == nil {
		t.Fatal("stale tracker overwrote R1")
	}
	if err := s.AdvanceNative(t.Context(), final.ID); err == nil {
		t.Fatal("stale tracker advanced R1")
	}
	if _, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: final.ID, Nodes: 1}); err == nil {
		t.Fatal("stale tracker opened R1 evidence")
	}
	after, err := s.Run(t.Context(), final.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(final, after) {
		t.Fatal("R1 history changed")
	}
}

type heldTransferAudit struct {
	Mode         string
	ExportDigest string `json:"export_digest"`
	PodUID       string `json:"pod_uid"`
	NodeID       string `json:"node_id"`
	FailedLog    struct {
		Run, Node, Container string
		BeforeMarker         string `json:"before_marker"`
	} `json:"failed_attempt_log"`
}

func checkInPodAudit(t *testing.T, s *Store, id string) {
	t.Helper()
	audits, _, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	found := false
	for _, a := range audits {
		if a.Action == "run.resume" && a.ResourceID == id {
			found = true
			var got heldTransferAudit
			requireOK(t, json.Unmarshal([]byte(a.Details), &got))
			want := heldTransferAudit{Mode: "in-pod", ExportDigest: strings.Repeat("d", 64), PodUID: "pod-uid", NodeID: "node"}
			want.FailedLog.Run, want.FailedLog.Node, want.FailedLog.Container = id, "node", "main"
			want.FailedLog.BeforeMarker = nativebinding.RerunMarker(strings.Repeat("f", 40), id)
			if !reflect.DeepEqual(got, want) {
				t.Fatal("held export audit differs", got, want)
			}
		}
	}
	if !found {
		t.Fatal("audit lacks held export and R1 log span")
	}
}

func TestInPodHandoffRollsBackOnAuditFailureAndStop(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"audit", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			old, input := inPodAcceptance(t, s)
			if kind == "audit" {
				input.Actions = []model.AuditAction{{Action: strings.Repeat("x", 1000)}}
			} else {
				_, err := s.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: old.ID, Revision: old.Revision})
				requireOK(t, err)
			}
			if _, err := acceptForTest(t.Context(), s, input); err == nil {
				t.Fatal("invalid handoff accepted")
			}
			next, err := s.ResumeSuccessor(t.Context(), old.ID)
			requireOK(t, err)
			if next != "" {
				t.Fatal("partial successor persisted")
			}
			after, err := s.Run(t.Context(), old.ID)
			requireOK(t, err)
			if kind == "audit" && !reflect.DeepEqual(old, after) {
				t.Fatal("audit failure closed R1")
			}
		})
	}
}

func TestInPodInspectedSecretRefusedAndTerminalInspectionClosed(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	old, input := inPodAcceptance(t, s)
	intent := stateport.HoldAction{RunID: old.ID, Revision: old.Revision, Step: "build", NodeID: "node", PodUID: "pod-uid", Kind: "exec", Argv: []string{"true"}, TimeoutS: 1}
	requireOK(t, s.Assist(t.Context(), stateport.Action{Actor: "operator"}, intent))
	old, err := s.Run(t.Context(), old.ID)
	requireOK(t, err)
	var plan nativebinding.ResumePlan
	requireOK(t, json.Unmarshal(input.Resume.ResumePlan, &plan))
	plan.SecretSteps = []string{"secret"}
	input.Resume.ResumePlan, err = json.Marshal(plan)
	requireOK(t, err)
	if _, err = acceptForTest(t.Context(), s, input); err == nil {
		t.Fatal("inspected state reached secret")
	}
	plan.SecretSteps = nil
	input.Resume.ResumePlan, err = json.Marshal(plan)
	requireOK(t, err)
	_, err = acceptForTest(t.Context(), s, input)
	requireOK(t, err)
	old, err = s.Run(t.Context(), old.ID)
	requireOK(t, err)
	intent.Revision = old.Revision
	if err = s.Assist(t.Context(), stateport.Action{Actor: "operator"}, intent); err == nil {
		t.Fatal("inspection after handoff admitted")
	}
	intent.Kind, intent.Argv, intent.TimeoutS = "abandon", nil, 0
	requireOK(t, s.Assist(t.Context(), stateport.Action{Actor: "operator"}, intent))
	after, err := s.Run(t.Context(), old.ID)
	requireOK(t, err)
	if !reflect.DeepEqual(old, after) {
		t.Fatal("cleanup release changed final R1")
	}
}

func TestInPodEntryValidationOwnsPartition(t *testing.T) {
	t.Parallel()
	authored := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{
		{Path: "held", HoldArmed: true, HoldEligible: true},
		{Path: "later", SecretRoute: true},
	}}
	plan := nativebinding.ResumePlan{InPod: &nativebinding.ResumeHold{Step: "held"}, Remaining: []string{"held", "later"}}
	// Native planning owns S1; acceptance still requires every frozen entry.
	if err := validateInPodEntries(authored, plan); err != nil {
		t.Fatal("partition validation duplicated native S1", err)
	}
	plan.Remaining = []string{"held"}
	if err := validateInPodEntries(authored, plan); err == nil {
		t.Fatal("acceptance omitted frozen entry")
	}
}
