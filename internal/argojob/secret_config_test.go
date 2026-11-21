package argojob_test

import (
	"errors"
	core "k8s.io/api/core/v1"
	"reflect"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestGrantedStepValidatesWholeDeliveryAtConstruction(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"zero", "Address", "ServerName", "CAPEM", "Mount", "AuthMount", "WriterRole", "StepRole", "ServerNamespace", "PipelineNamespace", "ServerAccount", "Audience"} {
		t.Run(name, func(t *testing.T) {
			cfg := secretDelivery()
			field := reflect.ValueOf(&cfg).Elem().FieldByName(name)
			switch {
			case name == "zero":
				cfg = runtimeconfig.SecretDelivery{}
			case field.IsValid():
				field.SetString("")
			default:
				reflect.ValueOf(&cfg.Access).Elem().FieldByName(name).SetString("")
			}
			executor := argojob.New(argojob.Config{SecretStore: cfg})
			input := admissioncontract.TransformInput{SecretGrants: map[string][]admissioncontract.GrantEvidence{"private": {selectedGrant()}}}
			err := executor.Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(t.Context(), secretWorkflow(), input)
			wantReason, wantRemedy := runtimeconfig.SecretStoreConfiguration()
			got := publicerr.Classify(err)
			if err == nil || got.Code() != publicerr.Unavailable || got.Reason() != wantReason || got.Remedy() != wantRemedy {
				t.Fatal("invalid delivery did not give configuration refusal", err)
			}
		})
	}
}

func TestAdmissionAudienceComesFromDelivery(t *testing.T) {
	t.Parallel()
	prepared, err := prepareSelectedSecrets(t, secretSelectionWorkflow("container", true), []admissioncontract.GrantEvidence{selectedGrant()}, argoworkflow.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	leaf := prepared.View().Workflow.Spec.Templates[1]
	cfg, _ := secretEnvironment(t, leaf.Container.Env)
	if leaf.Volumes[0].Projected.Sources[0].ServiceAccountToken.Audience != cfg.Access.Audience {
		t.Fatal("admission and delivery audiences differ")
	}
}

func TestAuthoredIdentityWithStoreAudienceKeepsItsOwnMount(t *testing.T) {
	t.Parallel()
	w := secretSelectionWorkflow("container", false)
	audience := secretDelivery().Access.Audience
	leaf := &w.Spec.Templates[1]
	leaf.Volumes = []core.Volume{{Name: "authored-login", VolumeSource: core.VolumeSource{Projected: &core.ProjectedVolumeSource{Sources: []core.VolumeProjection{{ServiceAccountToken: &core.ServiceAccountTokenProjection{Audience: audience, Path: "token", ExpirationSeconds: new(int64(600))}}}}}}}
	leaf.Container.VolumeMounts = []core.VolumeMount{{Name: "authored-login", MountPath: "/authored-login", ReadOnly: true}}
	grant := selectedGrant()
	grant.Kind, grant.Path, grant.Audience = "identity", "identity", audience
	prepared, err := prepareSelectedSecrets(t, w, []admissioncontract.GrantEvidence{grant}, argoworkflow.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	got := prepared.View().Workflow.Spec.Templates[1]
	if !reflect.DeepEqual(got.Volumes, leaf.Volumes) || !reflect.DeepEqual(got.Container.VolumeMounts, leaf.Container.VolumeMounts) {
		t.Fatal("authored identity projection or mount changed")
	}
	assertNoStoreInputs(t, got)
	grant.Audience = "other-audience"
	for _, grants := range [][]admissioncontract.GrantEvidence{nil, {grant}} {
		_, err := prepareSelectedSecrets(t, w, grants, argoworkflow.Policy{})
		var refused *argoworkflow.AdmissionError
		if !errors.As(err, &refused) || !slices.ContainsFunc(refused.Findings, func(f admissioncontract.Finding) bool { return f.Code == "grant" }) {
			t.Fatal("authored projection bypassed its exact audience grant", err)
		}
	}
}
