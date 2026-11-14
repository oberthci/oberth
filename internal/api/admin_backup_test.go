package api

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"
)

type backupDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

func (r *backupDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func TestAdminBackupRollingDeadline(t *testing.T) {
	writer := &backupDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	content := bytes.Repeat([]byte("x"), 100<<10)
	if err := streamAdminBackup(writer, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, writer.Body.Bytes()) {
		t.Fatal("truncated stream")
	}
	if len(writer.deadlines) < 4 || !writer.deadlines[0].IsZero() {
		t.Fatal("global write timeout not cleared", writer.deadlines)
	}
	for _, deadline := range writer.deadlines[1:] {
		if deadline.Before(time.Now()) || time.Until(deadline) > adminBackupWriteIdle {
			t.Fatal("stream idle timeout not applied", deadline)
		}
	}
}
