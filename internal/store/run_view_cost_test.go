package store

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRunViewAndFullReadAgreeOnAdmissionPresence(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
		want bool
	}{{"null", nil, false}, {"empty blob", []byte{}, false}, {"present", []byte(`{}`), true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			r, _ := accepted(t, s, "admission-presence")
			_, err := s.writer.ExecContext(t.Context(), `UPDATE runs SET admission=? WHERE id=?`, tc.body, r.ID)
			requireOK(t, err)
			full, err := s.Run(t.Context(), r.ID)
			requireOK(t, err)
			view, err := s.LookupRun(t.Context(), nil, r.ID)
			requireOK(t, err)
			if full.HasAdmission != tc.want || view.HasAdmission != tc.want {
				t.Fatal("admission presence depends on its reader", full.HasAdmission, view.HasAdmission, tc.want)
			}
		})
	}
}

func TestRunViewDoesNotDecodeAdmission(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			s := testStore(t)
			r, _ := accepted(t, s, "record-one")
			if observed {
				requireOK(t, s.SaveProgress(t.Context(), r.ID, stateport.Progress{Steps: []stepmodel.RunStep{{Path: "build", Status: stepmodel.StepRunning}}}))
			}
			// The view needs the stored plan/projection, never the Workflow body.
			_, err := s.writer.ExecContext(t.Context(), `UPDATE runs SET admission=json_set(admission,'$.workflow',42) WHERE id=?`, r.ID)
			requireOK(t, err)
			record, err := s.LookupRun(t.Context(), nil, r.ID)
			requireOK(t, err)
			if !record.HasAdmission || record.Admission.Workflow != nil {
				t.Fatal("view changed admission presence", record.HasAdmission)
			}
			steps, err := s.Steps(t.Context(), r.ID)
			requireOK(t, err)
			if len(steps) != 1 || steps[0].Path != "build" {
				t.Fatal("view lost its recorded plan", steps)
			}
			if _, err := s.Run(t.Context(), r.ID); err == nil {
				t.Fatal("full read stopped validating the admission body")
			}
		})
	}
}

func BenchmarkRunView(b *testing.B) {
	for _, count := range []int{4, 41} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := benchmarkStore(b)
			id := seedRunView(b, s, count)
			b.ReportAllocs()
			for b.Loop() {
				r, err := s.LookupRun(b.Context(), nil, id)
				if err != nil {
					b.Fatal(err)
				}
				steps, err := s.Steps(b.Context(), r.ID)
				if err != nil || len(steps) != count {
					b.Fatal(err, len(steps))
				}
			}
		})
	}
}

func seedRunView(b *testing.B, s *Store, count int) string {
	b.Helper()
	id := strings.Repeat("a", 32)
	plan := admissioncontract.Plan{Exact: true}
	steps := []stepmodel.RunStep{}
	for i := range count {
		path := fmt.Sprintf("step-%02d", i)
		plan.Entries = append(plan.Entries, admissioncontract.PlanEntry{Path: path, Name: path})
		steps = append(steps, stepmodel.RunStep{Path: path, Name: path, Status: stepmodel.StepRunning})
	}
	view := admissioncontract.PreparedView{RunID: id, Plan: plan, Workflow: &wfv1.Workflow{ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{"payload": strings.Repeat("b", 56000)}}}}
	summary := stepmodel.RunSummary{ID: id, Repo: "origin/team/repo", Ref: "refs/heads/main", Status: stepmodel.RunRunning, CreatedAt: s.options.Clock()}
	_, err := s.writer.ExecContext(b.Context(), `INSERT INTO upstreams(name,org,kind,base_url,key_name) VALUES('origin','team','git','https://oberth.ci','source');
INSERT INTO repositories(id,upstream,name,canonical) VALUES(1,'origin','team/repo','origin/team/repo')`)
	if err != nil {
		b.Fatal(err)
	}
	_, err = s.writer.ExecContext(b.Context(), `INSERT INTO runs(id,repo_id,ref,status,release,origin,summary,admission,step_count,steps_planned_exact,steps_pending,steps_running)
VALUES(?,1,'refs/heads/main','running',0,'{}',?,?,?,1,0,?)`, id, encode(summary), encode(view), count, count)
	if err == nil {
		_, err = s.writer.ExecContext(b.Context(), `INSERT INTO native_steps VALUES(?,?)`, id, encode(steps))
	}
	if err != nil {
		b.Fatal(err)
	}
	return id
}
