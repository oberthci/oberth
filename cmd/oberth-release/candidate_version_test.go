package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestCandidateVersionsProduceDistinctLoadableCharts(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	versions := map[string]bool{}
	for _, commit := range []string{strings.Repeat("a", 14) + "1" + strings.Repeat("b", 25), strings.Repeat("a", 14) + "2" + strings.Repeat("b", 25), strings.Repeat("f", 40)} {
		dir := workflowFixture(t)
		values := workflowValues(dir)
		values["CANDIDATE_COMMIT"], values["TARGET"] = commit, "build-bytes"
		configureBaseFixture(t, dir)
		if output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName("prepare-base"), values); err != nil {
			t.Fatal(err, string(output))
		}
		if output, err := executeWorkflowTemplate(t, wf, wf.GetTemplateByName("go"), values); err != nil {
			t.Fatal(err, string(output))
		}
		version := candidateBuildVersion(t, dir)
		number, err := strconv.ParseUint(commit[:15], 16, 64)
		if err != nil || version != "v0.0."+strconv.FormatUint(number, 10) || !workflowpolicy.IsReleaseVersion(version) || versions[version] {
			t.Fatal("candidate version lost commit prefix or stable grammar", version)
		}
		o := options{dir: t.TempDir(), chart: "../../charts/oberth", version: version}
		if err := chart(o, map[string]string{"oberth": "sha256:" + strings.Repeat("a", 64), "oberth-source-plugin": "sha256:" + strings.Repeat("b", 64)}); err != nil {
			t.Fatal(err)
		}
		if _, err := loader.LoadFile(filepath.Join(o.dir, "oberth.tgz")); err != nil {
			t.Fatal("candidate chart cannot be loaded", err)
		}
		versions[version] = true
	}
}
