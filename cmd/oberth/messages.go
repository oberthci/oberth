package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func messages() map[string][2]string {
	return map[string][2]string{
		"config": {"Cannot find the user configuration directory.", "Set XDG_CONFIG_HOME to a writable directory."},
		"usage":  {"Command arguments are invalid.", "Run oberth help for command names and arguments."},
		"failed": {"Command failed.", "Check the connection and destination, then retry."},
	}
}

func (a application) usage(problems []string) int {
	m := messages()["usage"]
	_, _ = fmt.Fprintln(a.diagnostic, m[0])
	for _, p := range problems {
		_ = writeWrapped(a.diagnostic, p)
	}
	_, _ = fmt.Fprintln(a.diagnostic, m[1])
	return 2
}

func (a application) failed(err error) int { return a.failedDetail(err, false) }

func (a application) failedDetail(err error, detail bool) int {
	refusal := errorRefusal(err)
	_ = a.writeRefusal(refusal, detail)
	if errors.Is(err, context.Canceled) {
		return 130
	}
	return 1
}
func errorRefusal(err error) refusalMessage {
	m := messages()["failed"]
	refusal := refusalMessage{Reason: m[0], Remedy: m[1]}
	var public interface {
		Reason() string
		Remedy() string
	}
	if errors.As(err, &public) {
		refusal.Reason, refusal.Remedy = public.Reason(), public.Remedy()
	}
	var explained *publicerr.Error
	if errors.As(err, &explained) {
		raw, _ := json.Marshal(explained)
		_ = json.Unmarshal(raw, &refusal)
	}
	var admission *argoworkflow.AdmissionError
	if errors.As(err, &admission) {
		refusal.Detail = admission.Detail
	}
	var file interface{ Path() string }
	if errors.As(err, &file) {
		refusal.Context = append(refusal.Context, "File: "+file.Path())
	}
	return refusal
}
