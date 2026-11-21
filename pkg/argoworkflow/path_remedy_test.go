package argoworkflow

import (
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestPathRemedyFitsMountsAndArtifacts(t *testing.T) {
	t.Parallel()
	for _, artifact := range []bool{false, true} {
		for _, value := range []string{"/safe", "/argo/staging"} {
			checkPathRemedy(t, artifact, value)
		}
	}
}

func checkPathRemedy(t *testing.T, artifact bool, value string) {
	t.Helper()
	w, p := mountDocument("container", value)
	if artifact {
		w.Spec.Templates[0].Container.VolumeMounts = nil
		w.Spec.Templates[0].Inputs.Artifacts = []wf.Artifact{{Name: "file", Path: value, ArtifactLocation: wf.ArtifactLocation{Raw: &wf.RawArtifact{Data: "content"}}}}
	}
	_, err := Prepare(t.Context(), requestFor(t, w), p, identityHooks())
	if value == "/safe" {
		if err != nil {
			t.Fatal("positive control", err)
		}
		return
	}
	assertPathRemedy(t, err, artifact)
}

func assertPathRemedy(t *testing.T, err error, artifact bool) {
	t.Helper()
	for _, f := range admissionFindings(t, err) {
		if f.Code != "path" {
			continue
		}
		if len(f.Reason) > 80 || len(f.Remedy) > 120 {
			t.Fatal("unbounded path diagnostic", f)
		}
		if artifact && strings.HasPrefix(strings.ToLower(f.Remedy), "mount ") {
			t.Fatal("artifact remedy assumes a volume mount", f)
		}
		if !strings.Contains(f.Remedy, "somewhere else") {
			t.Fatal("missing destination remedy", f)
		}
		return
	}
	t.Fatal("missing path refusal", err)
}
