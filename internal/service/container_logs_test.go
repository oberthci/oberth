package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	for _, container := range []string{"main", "wait", "init", "oberth-executor-token", "init-artifact-oberth"} {
		if err := logs.StoreContainerLog(run.ID, runlog.ContainerLog{NodeID: "node", PodUID: "uid", PodName: "opaque", ContainerName: container, Reader: strings.NewReader(retainedLogMarkerFixture + container + " body\n")}, 1024); err != nil {
			t.Fatal(err)
		}
	}
	value, err := f.api.CallTool(t.Context(), actor, "run_logs", json.RawMessage(`{"id":"`+run.ID+`","containers":true}`))
	if err != nil || len(value.(ContainerLogsResponse).Containers) != 1 {
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
	shaRequest, err := json.Marshal(map[string]any{"repo": f.repo.Name, "sha": run.SHA, "node_id": "node", "container": "wait"})
	if err != nil {
		t.Fatal(err)
	}
	value, err = f.api.CallTool(t.Context(), actor, "logs", shaRequest)
	if err != nil || value.(ContainerLogResponse).Output != retainedLogMarkerFixture+"wait body\n" {
		t.Fatal(value, err)
	}
	shaRequest, err = json.Marshal(map[string]any{"repo": f.repo.Name, "sha": run.SHA, "containers": true})
	if err != nil {
		t.Fatal(err)
	}
	value, err = f.api.CallTool(t.Context(), actor, "logs", shaRequest)
	if err != nil || len(value.(ContainerLogsResponse).Containers) != 1 {
		t.Fatal(value, err)
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

func TestNativeStepNameReadersAndFailureExcerpt(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", true)
	actor := api.Actor{Identity: "operator"}
	const body = "[other/fake] ordinary text\nError: native failure\n"
	entry := runlog.ContainerLog{NodeID: "test.unit", PodUID: "native-pod", ContainerName: "main", Stage: "test", Step: "unit", Reader: strings.NewReader(body)}
	if err := f.retention.Logs.StoreContainerLog(run.ID, entry, 4096); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"run_logs", "logs"} {
		args := map[string]any{"id": run.ID}
		if name == "logs" {
			args = map[string]any{"repo": f.repo.Name, "sha": run.SHA, "step": "unit"}
		}
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		value, err := f.api.CallTool(t.Context(), actor, name, raw)
		if err != nil || value.(LogResponse).Output != body {
			t.Fatal(name, value, err)
		}
	}
	value, err := f.api.RunLogFiltered(t.Context(), actor, run.ID, "test", "unit", runlog.Filter{})
	if err != nil || value.(LogResponse).Output != body {
		t.Fatal(value, err)
	}
	excerpt := stepFailureExcerpt(f.retention.Logs, run.ID, "test", "test.unit", 1024)
	if string(excerpt) != body {
		t.Fatal("CI excerpt lost native step", string(excerpt))
	}
}

func TestNativeActiveStreamServesStoredCopyFirst(t *testing.T) {
	f := newRetentionFixture(t)
	run := f.run(t, "main", "ci", false)
	source := &fakeContainerLogSource{}
	f.api.containerLogSource = source
	entry := runlog.ContainerLog{NodeID: "node", PodUID: "pod", ContainerName: "main", Reader: strings.NewReader("stored before PodGC\n")}
	if err := f.retention.Logs.StoreContainerLog(run.ID, entry, 4096); err != nil {
		t.Fatal(err)
	}
	stream, err := f.api.OpenRunContainerLog(t.Context(), api.Actor{Identity: "operator"}, run.ID, "node", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	body, err := io.ReadAll(stream)
	if err != nil || string(body) != "stored before PodGC\n" || source.opened != 0 {
		t.Fatal(string(body), source.opened, err)
	}
}

func TestFinishedUncapturedContainerLogReadReason(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			f := newRetentionFixture(t)
			run := f.run(t, "main", "ci", terminal)
			actor := api.Actor{Identity: "operator"}
			entry := runlog.ContainerLog{NodeID: "unread", PodUID: "uid", PodName: "opaque", ContainerName: "main"}
			if err := f.retention.Logs.TrackContainerLogs(run.ID, []runlog.ContainerLog{entry}); err != nil {
				t.Fatal(err)
			}
			value, err := f.api.RunContainerLogs(t.Context(), actor, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			info := value.(ContainerLogsResponse).Containers[0]
			want := ""
			if terminal {
				want = "oberth: not captured: observation ended"
			}
			if info.Stored || info.Reason != want {
				t.Fatal(info)
			}
			value, err = f.api.RunContainerLogFiltered(t.Context(), actor, run.ID, "unread", "main", runlog.Filter{})
			if terminal {
				if err != nil || value.(ContainerLogResponse).Output != want+"\n" {
					t.Fatal(value, err)
				}
			} else if !errors.Is(err, ErrStepLogPending) {
				t.Fatal(value, err)
			}
			stream, err := f.api.OpenRunContainerLog(t.Context(), actor, run.ID, "unread", "main", false)
			if terminal {
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(stream)
				_ = stream.Close()
				if readErr != nil || string(body) != want+"\n" {
					t.Fatal(string(body), readErr)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if _, err := f.api.OpenRunContainerLog(t.Context(), actor, run.ID, "unknown", "main", false); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invented unknown container", err)
			}
			entries, err := f.retention.Logs.ContainerLogs(t.Context(), run.ID)
			if err != nil || entries[0].Captured || entries[0].Pending || entries[0].Reason != "" {
				t.Fatal("read changed receipt", entries, err)
			}
		})
	}
}
