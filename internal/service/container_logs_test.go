package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

const retainedLogMarkerFixture = "@@oberth-step-progress@@ " + `{"version":1,"burn":"test","step":"one","ordinal":0,"status":"queued"}` + "\n"

type fakeContainerLogSource struct {
	opened  int
	context context.Context
}

func (*fakeContainerLogSource) ContainerLogs(context.Context, model.Run) ([]runlog.ContainerLog, error) {
	return []runlog.ContainerLog{{NodeID: "node", PodName: "opaque", PodUID: "uid", ContainerName: "main"}}, nil
}
func (source *fakeContainerLogSource) OpenContainerLog(ctx context.Context, _ model.Run, node, container string, follow bool) (io.ReadCloser, error) {
	source.opened++
	source.context = ctx
	return io.NopCloser(strings.NewReader(node + "/" + container)), nil
}

func TestContainerLogServiceStoredSelectionAndMCPDiscovery(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", true)
	actor := api.Actor{Identity: "operator"}
	logs := f.retention.Logs
	for _, container := range []string{"main", "wait"} {
		if err := logs.StoreContainerLog(run.ID, runlog.ContainerLog{NodeID: "node", PodUID: "uid", PodName: "opaque", ContainerName: container, Body: []byte(retainedLogMarkerFixture + container + " body\n")}, 1024); err != nil {
			t.Fatal(err)
		}
	}
	value, err := f.api.CallTool(t.Context(), actor, "run_logs", json.RawMessage(`{"id":"`+run.ID+`","containers":true}`))
	if err != nil || len(value.(ContainerLogsResponse).Containers) != 2 {
		t.Fatal(value, err)
	}
	value, err = f.api.CallTool(t.Context(), actor, "run_logs", json.RawMessage(`{"id":"`+run.ID+`","node_id":"node","container":"wait"}`))
	if err != nil {
		t.Fatal(err)
	}
	response := value.(ContainerLogResponse)
	if response.Output != retainedLogMarkerFixture+"wait body\n" || response.Container != "wait" {
		t.Fatal(response)
	}
	reader, err := f.api.OpenRunContainerLog(t.Context(), actor, run.ID, "node", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), "main body") {
		t.Fatal(string(body), err)
	}
}
func TestContainerLogServicePendingAndRequestContext(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", false)
	actor := api.Actor{Identity: "operator"}
	source := &fakeContainerLogSource{}
	f.api.containerLogSource = source
	if err := f.retention.Logs.TrackContainerLogs(run.ID, []runlog.ContainerLog{{NodeID: "node", PodUID: "uid", PodName: "opaque", ContainerName: "main"}}); err != nil {
		t.Fatal(err)
	}
	_, err := f.api.RunContainerLogFiltered(t.Context(), actor, run.ID, "node", "main", runlog.Filter{})
	if !errors.Is(err, ErrStepLogPending) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader, err := f.api.OpenRunContainerLog(ctx, actor, run.ID, "node", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cancel()
	if source.opened != 1 || source.context.Err() != context.Canceled {
		t.Fatal("stream lost request lifetime", source.opened, source.context.Err())
	}
}

func TestContainerLogServiceHistoricMarkersArePlain(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", true)
	actor := api.Actor{Identity: "operator"}
	body := retainedLogMarkerFixture + "::oberth-log:: arbitrary text\n[build/test] retained\n"
	if _, err := f.retention.Logs.AppendRunLog(run.ID, []byte(body)); err != nil {
		t.Fatal(err)
	}
	value, err := f.api.CallTool(t.Context(), actor, "run_logs", json.RawMessage(`{"id":"`+run.ID+`","raw":true}`))
	if err != nil || value.(LogResponse).Output != "[test/unit] retained output\n"+body {
		t.Fatal(value, err)
	}
}
