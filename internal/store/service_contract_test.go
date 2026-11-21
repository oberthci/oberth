package store

import (
	"context"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
)

func TestServiceRunContract(t *testing.T) { contracttest.RunSuite(t, serviceFixture) }

func serviceFixture(t *testing.T) contracttest.RunFixture {
	s := testStore(t)
	atomicRepository(t, s)
	return contracttest.RunFixture{Port: s, Admit: serviceAdmission,
		Nodes: func(t *testing.T, id string, nodes []stateport.Node) {
			for _, node := range nodes {
				_, err := s.writer.ExecContext(t.Context(), `INSERT INTO nodes VALUES(?,?,?,?,?,?)`, id, node.NodeID, node.Path, false, encode(node), 1)
				requireOK(t, err)
			}
		},
		Prune: func(t *testing.T) {
			cutoff := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			requireOK(t, s.RetainEvents(t.Context(), cutoff, cutoff))
		},
		Archive: func(t *testing.T, name string) {
			r, err := s.Repository(t.Context(), name, nil)
			requireOK(t, err)
			requireOK(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, r.ID))
		},
		Repository: func(t *testing.T, name string) {
			parts := strings.Split(name, "/")
			requireOK(t, s.RegisterUpstream(t.Context(), stateport.Action{Actor: "operator"}, stateport.Upstream{Name: parts[0], Org: parts[1], Kind: "git", BaseURL: "https://oberth.ci", KeyName: "source"}))
			_, err := s.RegisterRepository(t.Context(), stateport.Action{Actor: "operator"}, name)
			requireOK(t, err)
		},
		NativeAdvance: func(t *testing.T, id string) {
			_, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1})
			requireOK(t, err)
		},
		Audit: func(t *testing.T) []model.AuditAction {
			rows, _, err := s.Audit(t.Context(), 0, 200)
			requireOK(t, err)
			return rows
		},
		Publication: s.Publication,
		Emit: func(ctx context.Context, events []stateport.EventWrite) error {
			return s.RecordEvents(ctx, events)
		},
	}
}

func serviceAdmission(t *testing.T, id string, r model.CIRequest) *admissioncontract.PreparedAdmission {
	t.Helper()
	view := admissioncontract.PreparedView{RunID: id, Repository: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA, Trigger: r.Trigger, ScheduleEntry: r.ScheduleEntry, Workflow: &wfv1.Workflow{}, Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build", Name: "build", HoldEligible: true, HoldArmed: r.Assist}}}}
	p, err := admissioncontract.Seal(view)
	requireOK(t, err)
	return p
}
