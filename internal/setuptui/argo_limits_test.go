package setuptui

import (
	"strings"
	"testing"
)

func TestArgoLimitsReviewAndResume(t *testing.T) {
	var st WizardState
	var limits *question
	for _, q := range buildQuestions() {
		if q.id == "cluster.argo-limits" {
			limits = q
		}
	}
	if limits == nil {
		t.Fatal("missing Argo limit editor")
	}
	if err := limits.save(&st, []string{"9", "2"}); err != nil {
		t.Fatal(err)
	}
	var resumed WizardState
	resumed.fromSaved(st.toSaved())
	cfg := installConfig(&resumed)
	if cfg.ArgoParallelism == nil || *cfg.ArgoParallelism != 9 || cfg.ArgoNamespaceParallelism == nil || *cfg.ArgoNamespaceParallelism != 2 {
		t.Fatal("native Argo settings lost on resume")
	}
	command := BuildCommandLine(&resumed)
	if !strings.Contains(command, "--argo-parallelism=9") || !strings.Contains(command, "--argo-namespace-parallelism=2") {
		t.Fatal("equivalent command lost Argo limits", command)
	}
	if err := limits.save(&st, []string{"-1", "2"}); err == nil {
		t.Fatal("negative parallelism accepted")
	}
	if err := limits.save(&st, []string{"", ""}); err != nil || st.Config.ArgoParallelism != nil || st.Config.ArgoNamespaceParallelism != nil {
		t.Fatal("empty inputs do not preserve existing limits", err)
	}
}
