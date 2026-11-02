package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestMemoryLimitFallbackUsesLegacyAdmissionAndVisibleNotice(t *testing.T) {
	for _, shape := range []string{"request-only", "init", "sidecar", "all-limited"} {
		t.Run(shape, func(t *testing.T) {
			limit := "        limits: {memory: 32Mi}\n"
			if shape == "request-only" {
				limit = ""
			}
			helpers := ""
			if shape == "init" || shape == "sidecar" {
				key := "initContainers"
				if shape == "sidecar" {
					key = "sidecars"
				}
				helpers = fmt.Sprintf("    %s:\n    - name: step-11111111\n      image: example.invalid/repo-01@sha256:%s\n      resources:\n        requests: {cpu: 100m, memory: 16Mi}\n", key, strings.Repeat("a", 64))
			}
			source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  activeDeadlineSeconds: 3600
  entrypoint: step-abc12345
  templates:
  - name: step-abc12345
    container:
      image: example.invalid/repo-01@sha256:` + strings.Repeat("a", 64) + `
      resources:
        requests: {cpu: 100m, memory: 16Mi}
` + limit + helpers)
			resolved, err := argoworkflow.ResolveRunSource(source, nil, argoworkflow.RunSourceBinding{RunID: strings.Repeat("a", 32), RepositoryID: 1, SHA: strings.Repeat("b", 40), File: ".oberth/build.yaml", Trigger: argoworkflow.RunTriggerBranch})
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := resolved.Receipt(argoworkflow.Policy{RunnerImagePrefixes: []string{"example.invalid/"}})
			if err != nil {
				t.Fatal(err)
			}
			g := newWeightedAdmission(6)
			pods, reason := g.selectDeclaredDemand(receipt)
			r := g.reserve(1)
			release := acquireRequestWithinBudget(t, r, admissionRequest{RunID: strings.Repeat("a", 32), Size: periapsis.S, Pods: pods, Fallback: reason})
			defer release()
			runs := []model.Run{{ID: r.runID, DisplayReason: "Priority normal: ordinary run."}}
			(&API{admission: &Scheduler{admission: g}}).applyResourceMessages(t.Context(), runs)
			if shape == "all-limited" {
				if pods == nil || reason != "" || r.pods == nil || r.weight != 0 {
					t.Fatal("bounded pod lost numeric admission")
				}
			} else if pods != nil || r.pods != nil || r.weight != 1 || !strings.Contains(runs[0].ResourceMessage, "memory limit") || !strings.Contains(runs[0].ResourceMessage, "legacy size") {
				t.Fatalf("unbounded pod escaped legacy fallback and notice: %+v %q", pods, runs[0].ResourceMessage)
			}
			if runs[0].DisplayReason != "Priority normal: ordinary run." {
				t.Fatal("fallback changed display reason")
			}
		})
	}
}
