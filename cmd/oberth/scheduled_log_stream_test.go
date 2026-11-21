//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"crypto/tls"
	"net/url"
	"strings"
	"testing"

	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"github.com/oberthci/oberth/internal/stateport"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestScheduledChildIDThroughComposedLogStream(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareScheduledObservation(t)
	f.prepareAdministrator(t)
	close(f.allowList)
	cancel, done := f.run(t)
	defer cancel()
	f.waitStarted(t, done)
	parent := f.scheduledParent(t, commit)
	ctx := argologging.WithLogger(t.Context(), scheduledFixtureLogger)
	child := common.ConvertCronWorkflowToWorkflowWithProperties(ctx, parent, parent.Name+"-1795012440", f.clock.Now())
	child.Namespace = f.config.PipelineNamespace
	child = initializedScheduledObservation(t, child, f.clock.Now())
	child, err := f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Create(t.Context(), child, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.ID == child.Name && r.Origin.WorkflowUID == string(child.UID) })
	f.scheduledChildProof(t, run, commit)
	f.captureViewLog(t, run.ID)
	if err = f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Delete(t.Context(), child.Name, meta.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	f.enrollJoined(t)
	for _, certificate := range []tls.Certificate{f.adminCertificate, f.joinedCertificate(t)} {
		path := "/api/runs/" + url.PathEscape(run.ID) + "/containerlog/stream?node_id=view-node&container=main"
		if body := f.assertHTTPView(t, certificate, path, 200); body != "view output\n" {
			t.Fatal("scheduled log stream changed retained bytes", body)
		}
		shortened := strings.TrimSuffix(run.ID, "0")
		f.assertHTTPView(t, certificate, "/api/runs/"+shortened+"/containerlog/stream?node_id=view-node&container=main", 404)
	}
}
