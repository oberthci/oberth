package argojob

import (
	"bytes"
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/nativebinding"
	"log/slog"
	"strings"
	"testing"
)

func TestObservationErrorsLogOncePerChangeAndRecovery(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	e := New(Config{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	defer e.queue.ShutDown()
	tr := &tracking{binding: nativebinding.RunBinding{RunID: "id"}}
	for _, err := range []error{errors.New("first cause"), errors.New("first cause"), errors.New("second cause"), errors.New("second cause"), nil, nil, errors.New("first cause")} {
		e.logObservation(context.Background(), tr, err)
	}
	if strings.Count(buf.String(), `"level":"ERROR"`) != 3 || strings.Count(buf.String(), `"level":"INFO"`) != 1 || strings.Count(buf.String(), `"run_id":"id"`) != 4 || strings.Count(buf.String(), `"msg":"native observation recovered"`) != 1 || strings.Contains(buf.String(), `"cause":""`) || strings.Contains(buf.String(), `"cause":null`) {
		t.Fatal(buf.String())
	}
}
func TestForeignWorkflowIsReportedOnceAndUntracked(t *testing.T) {
	t.Parallel()
	reporter := &reviewR2Reporter{}
	e, tr, native, _ := reviewR2Executor(t, reporter)
	native.w.Labels[nativebinding.EngineLabel] = "foreign"
	for range 10 {
		e.process(t.Context(), tr.binding.Workflow.Name)
	}
	if reporter.calls != 1 || len(e.tracked) != 0 || native.reads != 1 {
		t.Fatal("foreign Workflow kept retrying", reporter.calls, len(e.tracked), native.reads)
	}
}
