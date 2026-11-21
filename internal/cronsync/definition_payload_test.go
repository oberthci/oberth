package cronsync_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/argoproj/argo-workflows/v4/util/instanceid"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"os"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/cronsync"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInstallerCronWorkflowPayload(t *testing.T) {
	for _, verb := range []string{"create", "update"} {
		for _, paused := range []bool{false, true} {
			name := "cron-workflow"
			if verb == "update" {
				name += "-update"
			}
			if paused {
				name += "-paused"
			}
			t.Run(name, func(t *testing.T) { pinCronSubmission(t, name, verb, paused) })
		}
	}
}

func pinCronSubmission(t *testing.T, name, verb string, paused bool) {
	t.Helper()
	f := newFixture(t)
	if verb == "update" {
		f.paused = !paused
		f.sync()
	}
	client := &capturedCronClient{Native: f.native, t: t}
	f.config.Client = client
	f.paused = paused
	var err error
	f.c, err = cronsync.New(f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.sync()
	if client.verb != verb || client.submitted == nil || client.submitted.Spec.Suspend != paused {
		t.Fatal("scheduled parent was not captured after pause intent", client.verb)
	}
	requireControllerIsolation(t.Context(), t, client.submitted)
	actual, err := json.MarshalIndent(client.submitted, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertCronPayload(t, name, actual)
}

func assertCronPayload(t *testing.T, name string, actual []byte) {
	t.Helper()
	want, err := os.ReadFile("../installer/testdata/" + name + "-payload.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), actual) {
		t.Fatal("installer payload differs from the client submission", name)
	}
}

type capturedCronClient struct {
	cronsync.Native
	t         *testing.T
	verb      string
	submitted *wf.CronWorkflow
}

func (c *capturedCronClient) Create(ctx context.Context, object *wf.CronWorkflow, options meta.CreateOptions) (*wf.CronWorkflow, error) {
	requireControllerIsolation(ctx, c.t, object)
	c.verb, c.submitted = "create", object.DeepCopy()
	return c.Native.Create(ctx, object, options)
}

func (c *capturedCronClient) Update(ctx context.Context, object *wf.CronWorkflow, options meta.UpdateOptions) (*wf.CronWorkflow, error) {
	requireControllerIsolation(ctx, c.t, object)
	c.verb, c.submitted = "update", object.DeepCopy()
	return c.Native.Update(ctx, object, options)
}

func requireControllerIsolation(ctx context.Context, t *testing.T, parent *wf.CronWorkflow) {
	t.Helper()
	// This is the conversion called by the pinned Cron controller's operator.
	child := common.ConvertCronWorkflowToWorkflowWithProperties(logging.TestContext(ctx), parent, "scheduled", parent.CreationTimestamp.Time)
	for _, object := range []meta.Object{parent, parent.Spec.WorkflowMetadata, child} {
		if err := instanceid.NewService("frontdoor").Validate(object); err != nil {
			t.Fatal(err)
		}
		if instanceid.NewService("").Validate(object) == nil {
			t.Fatal("default controller selects our scheduled object")
		}
	}
}
