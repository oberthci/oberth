//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (f *composedFixture) pendingNativeLog(t *testing.T, run stateport.Run) func() {
	t.Helper()
	pod, err := f.kube.CoreV1().Pods(f.config.PipelineNamespace).Get(t.Context(), run.Origin.WorkflowName, meta.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	entry := runlog.ContainerLog{NodeID: pod.Annotations["workflows.argoproj.io/node-id"], Step: run.Admission.Plan.Entries[0].Path, PodUID: string(pod.UID), PodName: pod.Name, Namespace: pod.Namespace, Container: "main", Main: true}
	reservation, err := f.owners.c.logs.Reserve(t.Context(), run.ID, entry)
	if err != nil || reservation == nil {
		t.Fatal("native log was not reserved before completion", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- reservation.Capture(ctx, runlog.CaptureRequest{Source: reader})
		close(done)
	}()
	t.Cleanup(func() { cancel(); _ = reader.Close(); _ = writer.Close(); <-done })
	return func() {
		if _, err := io.WriteString(writer, "fake logs\n"); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal("native capture did not commit", err)
		}
	}
}

func (f *composedFixture) waitLogProjection(t *testing.T) {
	t.Helper()
	f.waitRun(t, func(r stateport.Run) bool {
		steps, err := f.owners.c.store.Steps(t.Context(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r.Status == stepmodel.RunPassed && len(steps) > 0 && len(steps[0].Attempts) > 0
	})
}

func watchLogReads(t *testing.T, app *application, open bool, finish func()) *int {
	t.Helper()
	original := app.config.Transport
	reads := new(int)
	app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if err != nil {
			return response, err
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		meta := logReplyMeta(t, data)
		if meta == nil {
			return response, nil
		}
		*reads++
		if *reads != 1 {
			return response, nil
		}
		if meta.Copying != open {
			t.Fatal("first read did not exercise requested capture order", open, meta)
		}
		if open {
			finish()
		}
		return response, nil
	})
	return reads
}

func logReplyMeta(t *testing.T, data []byte) *logcontract.Meta {
	t.Helper()
	payload, refused, _, err := apicontract.ToolPayload(data)
	if err != nil || refused {
		t.Fatal("composed call refused", err, string(data))
	}
	var result struct {
		Meta *logcontract.Meta `json:"meta"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	return result.Meta
}
