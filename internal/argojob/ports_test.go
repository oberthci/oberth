package argojob_test

import (
	"context"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stateport/contracttest"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
)

func TestProgressPortContract(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"store", "fake"} {
		for _, dynamic := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/static", true: "/dynamic"}[dynamic], func(t *testing.T) {
				contracttest.Progress(t, func(t *testing.T) contracttest.ProgressFixture {
					r := requestForProgress()
					document := `{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"main","templates":[{"name":"main","dag":{"tasks":[{"name":"build","template":"build"}]}},{"name":"build","container":{"image":"example.test/build:1","command":["true"]}}]}}`
					path := "build"
					if dynamic {
						document = strings.Replace(document, `"template":"build"`, `"template":"build","withParam":"{{workflow.parameters.items}}"`, 1)
						document = strings.Replace(document, `"entrypoint":"main"`, `"entrypoint":"main","arguments":{"parameters":[{"name":"items","value":"[\"a\"]"}]}`, 1)
						path = "build(0:a)"
					}
					p := compositionDocument(t, admissionInput{id: "progress-run", request: r, document: []byte(document)})
					var port interface {
						argojob.ProgressStore
						contracttest.NativeProgress
						argojob.Runs
					}
					if kind == "fake" {
						port = memoryAdmissionFor(t, p, r)
					} else {
						real := compositionStore(t)
						registerCompositionRepository(t, real, r.Repo)
						_, err := real.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
						compositionOK(t, err)
						port = real
					}
					return contracttest.ProgressFixture{Store: port, RunID: p.View().RunID, Path: path, Revision: func(ctx context.Context) (int64, error) {
						r, err := port.Run(ctx, p.View().RunID)
						return r.Revision, err
					}, Steps: func(ctx context.Context) ([]stepmodel.RunStep, error) { return port.Steps(ctx, p.View().RunID) }}
				})
			})
		}
	}
}
