package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

type smallStageTransport struct{ *inPodTransport }

type stageReporter struct {
	Reporter
	terminal Observation
}

func (r *stageReporter) Finished(ctx context.Context, id string, o Observation) error {
	r.terminal = o
	return r.Reporter.Finished(ctx, id, o)
}

func (p smallStageTransport) Exec(ctx context.Context, q ExecRequest) error {
	if q.Command[1] != "rerun" {
		return p.inPodTransport.Exec(ctx, q)
	}
	body, _ := json.Marshal(holdFrame{Type: "refused", Reason: rerunStageReason, Remedy: rerunStageRemedy})
	_, _ = fmt.Fprintln(q.Err, "oberth-hold: "+string(body))
	return errors.New("helper exited 120")
}

func TestInPodReportsSmallStageRefusal(t *testing.T) {
	t.Parallel()
	config, r, plan, _, pods := inPodRecovery(t)
	config.Pods = smallStageTransport{pods}
	reporter := &stageReporter{Reporter: config.Reporter}
	config.Reporter = reporter
	e := New(config)
	defer e.queue.ShutDown()
	defer e.collector.queue.ShutDown()
	err := e.applyInPod(t.Context(), r, plan)
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || refusal.Reason() != rerunStageReason || refusal.Remedy() != rerunStageRemedy {
		t.Fatal("small stage refusal was hidden", err)
	}
	if pods.runs != 0 || pods.releases != 1 {
		t.Fatal("small stage started a rerun or leaked its hold", pods.runs, pods.releases)
	}
	if !strings.Contains(reporter.terminal.Detail, rerunStageReason+" - "+rerunStageRemedy) {
		t.Fatal("persisted run detail hid the stage refusal", reporter.terminal)
	}
	if publicerr.CheckMessage(ReasonResumeStart, 40) != nil || publicerr.CheckMessage(refusal.Reason(), 40) != nil || publicerr.CheckMessage(refusal.Reason(), 80) != nil || publicerr.CheckMessage(refusal.Remedy(), 120) != nil {
		t.Fatal("stage refusal exceeds message bounds", refusal)
	}
}
