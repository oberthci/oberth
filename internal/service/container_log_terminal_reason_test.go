package service

import (
	"errors"
	"io"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/runlog"
)

func TestTerminalUncapturedContainerReasonIsReadOnly(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "terminal"}[terminal], func(t *testing.T) {
			f := newRetentionFixture(t)
			run := f.run(t, "main", "ci", terminal)
			entry := runlog.ContainerLog{NodeID: "node", PodUID: "uid", PodName: "pod", ContainerName: "main"}
			if err := f.retention.Logs.TrackContainerLogs(run.ID, []runlog.ContainerLog{entry}); err != nil {
				t.Fatal(err)
			}
			actor := api.Actor{Identity: "reader"}
			value, err := f.api.RunContainerLogs(t.Context(), actor, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rows := value.(ContainerLogsResponse).Containers
			want := ""
			if terminal {
				want = "oberth: not captured: observation ended"
			}
			if len(rows) != 1 || rows[0].Stored || rows[0].Reason != want {
				t.Fatal(rows)
			}
			checkUncapturedContainerReads(t, f.api, run.ID, terminal, want)
			entries, err := f.retention.Logs.ContainerLogs(t.Context(), run.ID)
			if err != nil || len(entries) != 1 || entries[0].Captured || entries[0].Pending || entries[0].Reason != "" {
				t.Fatal("read changed the copy receipt", entries, err)
			}
		})
	}
}

func checkUncapturedContainerReads(t *testing.T, service *API, runID string, terminal bool, reason string) {
	t.Helper()
	actor := api.Actor{Identity: "reader"}
	value, err := service.RunContainerLogFiltered(t.Context(), actor, runID, "node", "main", runlog.Filter{})
	if terminal {
		if err != nil || value.(ContainerLogResponse).Output != reason+"\n" {
			t.Fatal(value, err)
		}
	} else if !errors.Is(err, ErrStepLogPending) {
		t.Fatal("active copy lost pending status", value, err)
	}
	reader, err := service.OpenRunContainerLog(t.Context(), actor, runID, "node", "main", false)
	if terminal {
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		body, err := io.ReadAll(reader)
		if err != nil || string(body) != reason+"\n" {
			t.Fatal(string(body), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("active copy fabricated a stored log", err)
	}
	if _, err := service.OpenRunContainerLog(t.Context(), actor, runID, "unknown", "main", false); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown selection fabricated a missing-copy reason", err)
	}
}
