package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestManualRerunRefusesReleaseRouteBeforeAuthority(t *testing.T) {
	f := newFixture(t)
	const file = ".oberth/walk.yaml"
	q := request()
	q.Original = &stateport.Run{Admission: admissioncontract.PreparedView{SelectedFile: file}}
	f.d.Routes = workflowpolicy.ReleaseRoutes{q.Repository: {{Pattern: "v*", File: file}}}
	loads := 0
	load := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		loads++
		return load(ctx, scope)
	}
	_, err := f.new(t).Admit(t.Context(), q, f.source)
	if err == nil || !strings.Contains(err.Error(), "release files require an admin tag") || loads != 0 {
		t.Fatal("manual rerun reached release authority or lost its refusal", err, loads)
	}
}
