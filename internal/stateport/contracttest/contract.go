// Package contracttest specifies durable backup and run facts for their memory fakes and C7.
// Spec: docs/plans/c7-durable-state.md#baseline-and-backup
package contracttest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// Fixture supplies an independent content/head oracle and a test-only reader
// of the restored database, separate from the production port's own reports.
type Fixture struct {
	Port        stateport.Backup
	Options     stateport.Options
	WantContent string
	WantHead    stateport.AuditHead
	Read        func(*testing.T, string) (string, stateport.AuditHead, bool)
}

type factory func(*testing.T, int) Fixture

func prepare(t *testing.T, create factory, index int) Fixture {
	t.Helper()
	f := create(t, index)
	if f.Read == nil {
		t.Fatal("fixture requires an independent destination reader")
	}
	if err := f.Options.Validate(); err != nil {
		t.Fatal(err)
	}
	return f
}

// RunContract uses isolated verified databases and caller-supplied options.
// Index 0 supplies the base; indices 1/2/3 change only schema identity/schema
// number/audit domain respectively, keeping known content and head identical.
func RunContract(t *testing.T, create func(*testing.T, int) Fixture) {
	t.Helper()
	t.Run("repeatable_reads", func(t *testing.T) {
		f := prepare(t, create, 0)
		body, report := f.snapshot(t)
		for range 2 {
			got, err := f.Port.Inspect(t.Context(), f.Options, bytes.NewReader(body))
			if err != nil || got != report {
				t.Fatalf("inspect = %+v, %v; want %+v", got, err, report)
			}
		}
		_, again := f.snapshot(t)
		if again != report {
			t.Fatal("reads changed the database")
		}
	})
	t.Run("restore_conflict", func(t *testing.T) {
		f := prepare(t, create, 0)
		body, report := f.snapshot(t)
		req := f.restoreRequest(t)
		got, err := f.Port.Restore(t.Context(), req, bytes.NewReader(body))
		if err != nil || got != report {
			t.Fatalf("restore = %+v, %v", got, err)
		}
		f.assertDestination(t, req.Destination, true)
		_, err = f.Port.Restore(t.Context(), req, bytes.NewReader(body))
		assertCode(t, err, publicerr.Conflict)
		f.assertDestination(t, req.Destination, true)
	})
	t.Run("canceled_context", func(t *testing.T) {
		f := prepare(t, create, 0)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := f.Port.Snapshot(ctx, f.Options, io.Discard)
		assertCanceled(t, err)
		_, err = f.Port.Inspect(ctx, f.Options, bytes.NewReader(nil))
		assertCanceled(t, err)
		request := f.restoreRequest(t)
		_, err = f.Port.Restore(ctx, request, bytes.NewReader(nil))
		assertCanceled(t, err)
		f.assertDestination(t, request.Destination, false)
	})
	t.Run("invalid_options", func(t *testing.T) { invalidOptions(t, create) })
	t.Run("corrupt_backups", func(t *testing.T) { badBackups(t, create) })
	t.Run("foreign_archives", func(t *testing.T) { foreignArchives(t, create) })
	t.Run("canceled_streams", func(t *testing.T) { canceledStreams(t, prepare(t, create, 0)) })
	t.Run("stream_causes", func(t *testing.T) { streamErrors(t, prepare(t, create, 0)) })
	t.Run("concurrent_restores", func(t *testing.T) { concurrentRestores(t, prepare(t, create, 0)) })
	t.Run("missing_inputs", func(t *testing.T) { missingInputs(t, prepare(t, create, 0)) })
}

func (f Fixture) snapshot(t *testing.T) ([]byte, stateport.BackupReport) {
	t.Helper()
	var output bytes.Buffer
	report, err := f.Port.Snapshot(t.Context(), f.Options, &output)
	if err != nil {
		t.Fatal(err)
	}
	if err := report.Validate(); err != nil || output.Len() == 0 || report.Schema != f.Options.Schema || report.AuditHead != f.WantHead {
		t.Fatalf("snapshot verification/content/schema missing: %+v, %v", report, err)
	}
	return output.Bytes(), report
}

func (f Fixture) restoreRequest(t *testing.T) stateport.RestoreRequest {
	t.Helper()
	return stateport.RestoreRequest{Options: f.Options, Destination: filepath.Join(t.TempDir(), "restored.sqlite")}
}

func assertCode(t *testing.T, err error, code publicerr.Code) {
	t.Helper()
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || refusal.Code() != code || refusal.Reason() == "" || refusal.Remedy() == "" {
		t.Fatalf("error = %v; want helpful %s refusal", err, code)
	}
}

func assertCanceled(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
}

func (f Fixture) assertDestination(t *testing.T, path string, want bool) {
	t.Helper()
	content, head, found := f.Read(t, path)
	if found != want {
		t.Fatalf("destination present = %v, want %v", found, want)
	}
	if found && (content != f.WantContent || head != f.WantHead) {
		t.Fatalf("restored content/head differs: %q %+v", content, head)
	}
}
