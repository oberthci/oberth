package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type retirementFixture struct {
	body, fragment string
	blockers       []string
	fail           bool
	changing       bool
	reads          int
}

func (f *retirementFixture) ListRepositories(context.Context) ([]model.Repository, error) {
	f.reads++
	id := int64(1)
	if f.changing && f.reads > 1 {
		id = 2
	}
	return []model.Repository{{ID: id, Name: "repository", UpstreamID: 1}}, nil
}
func (f *retirementFixture) Upstream(context.Context, int64) (model.Upstream, error) {
	return model.Upstream{Name: "forge", BaseURL: "https://forge.invalid/team"}, nil
}
func (f *retirementFixture) ActiveWorkflowCapability(context.Context, int64) (*argoworkflow.Capability, error) {
	return nil, nil
}
func (f *retirementFixture) LegacyRetirementBlockers(context.Context) ([]string, error) {
	return f.blockers, nil
}
func (f *retirementFixture) Ensure(context.Context, string) (gitcache.Repository, error) {
	return gitcache.Repository{}, nil
}
func (f *retirementFixture) RetirementSources(context.Context, string) ([]gitcache.RetirementSource, error) {
	if f.fail {
		return nil, errors.New("unreadable source")
	}
	return []gitcache.RetirementSource{{Ref: "refs/heads/main", SHA: strings.Repeat("a", 40), Path: ".oberth/build.yaml", Body: []byte(f.body)}}, nil
}
func (f *retirementFixture) Load(_ context.Context, key argoworkflow.FragmentKey, _ pipeline.Trigger, _ model.RefKind, _ string) (argoworkflow.Fragment, error) {
	return argoworkflow.Fragment{Key: key, SHA: strings.Repeat("b", 40), Source: []byte(f.fragment)}, nil
}

func TestLegacyRemovalNamesEveryBlockingBoundary(t *testing.T) {
	for _, scenario := range []string{"legacy", "pinned", "unreadable", "obligation", "changing", "migrated"} {
		t.Run(scenario, func(t *testing.T) {
			f := &retirementFixture{body: "kind: Workflow"}
			want := "repository"
			switch scenario {
			case "legacy":
				f.body = "metadata: {annotations: {oberth.ci/kvm-templates: main}}"
			case "pinned":
				f.body = "spec: {templates: [{templateRef: {name: 'forge/team/library@v1'}}]}"
				f.fragment = "metadata: {annotations: {oberth.ci/guest-test-templates: main}}"
				want = "pinned fragment"
			case "unreadable":
				f.fail = true
			case "obligation":
				f.blockers = []string{"execution run-fixture"}
				want = "run-fixture"
			case "changing":
				f.changing = true
				want = "changed"
			}
			err := (LegacyRemovalCheck{Catalog: f, Git: f, Fragments: f}).Check(t.Context())
			if scenario == "migrated" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s did not block removal: %v", scenario, err)
			}
		})
	}
}
