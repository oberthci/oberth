package argojob

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestReleaseAccountConsumesAdmissionsStepChoice(t *testing.T) {
	key := identityname.Key{Repository: "origin/team/widget", File: ".oberth/release.yaml", Step: "publish"}
	in := admissioncontract.TransformInput{CredentialSteps: map[string]string{"publish": key.Step}}
	p := nativebinding.Preparation{Repository: key.Repository, File: key.File, Release: true}
	leaf := nativeLeaf{template: &wf.Template{Name: key.Step}, key: key.Step}
	grants := []admissioncontract.GrantEvidence{{Repository: key.Repository, File: key.File, Step: "*"}}
	want, err := identityname.Name(key, identityname.Step)
	if err != nil {
		t.Fatal(err)
	}
	got, err := leafAccount(leaf, in, p, grants)
	if err != nil || got != want {
		t.Fatal("native layer overrode admission's release step", got, err)
	}
	p.Release = false
	want, err = identityname.Name(grants[0].Key(), identityname.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	got, err = leafAccount(leaf, in, p, grants)
	if err != nil || got != want {
		t.Fatal("CI lost its selected store account", got, err)
	}
}
