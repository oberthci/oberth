package argoworkflow

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func TestOfflineNeedsUseTheExactPushCommands(t *testing.T) {
	for _, file := range []string{"cache.yaml", "release.yaml", "resource.yaml"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../docs/examples", file))
			if err != nil {
				t.Fatal(err)
			}
			r, p, _ := documentationRequest(t, file, raw)
			r.Grants, p.PVCClaims, p.Capability = nil, nil, nil
			w, err := Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			push := admissionFindings(t, p.admit(t.Context(), w, r, admissioncontract.PrepareHooks{}))
			r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
			prepared, err := Prepare(t.Context(), r, p, admissioncontract.PrepareHooks{OfflineValidation: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range push {
				if !f.Installation || f.Command == "" || !slices.ContainsFunc(prepared.View().Findings, func(need admissioncontract.Finding) bool {
					return offlineNeedMatchesPush(need, f)
				}) {
					t.Fatal("push remedy differs from offline need", f, prepared.View().Findings)
				}
			}
		})
	}
}

func offlineNeedMatchesPush(need, push admissioncontract.Finding) bool {
	if push.Code == "store_config" {
		return need.Code == "store_unverified" && need.Path == push.Path && need.Command == push.Command
	}
	return need == push
}

func TestCapabilityCommandIncludesAllRequestedActions(t *testing.T) {
	w, fixture := resourceWorkflow("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: note, namespace: app}")
	other := *w.Spec.Templates[0].DeepCopy()
	other.Name, other.Resource.Action = "cleanup", "delete"
	w.Spec.Templates = append(w.Spec.Templates, other)
	r := requestFor(t, w)
	r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
	fixture.Capability = nil
	prepared, err := Prepare(t.Context(), r, fixture.Policy, admissioncontract.PrepareHooks{})
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, f := range prepared.View().Findings {
		if f.Code == "capability" {
			commands = append(commands, f.Command)
		}
	}
	if len(commands) != 2 || commands[0] != commands[1] {
		t.Fatal("capability commands would overwrite each other", commands)
	}
}
