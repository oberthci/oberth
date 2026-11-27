package main

import (
	"context"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type holdNative interface {
	Find(context.Context, stateport.Run) (nativebinding.RunBinding, bool, error)
	Holds(context.Context, nativebinding.RunBinding) ([]model.LiveHold, error)
	HoldExec(context.Context, argojob.HoldExecRequest) (argojob.ExecResult, error)
	Abandon(context.Context, nativebinding.RunBinding, model.LiveHold) error
}

type heldRuns struct{ native holdNative }

func (h heldRuns) binding(ctx context.Context, r stateport.Run) (nativebinding.RunBinding, error) {
	b, found, err := h.native.Find(ctx, r)
	if err == nil && !found {
		err = publicerr.New(publicerr.Conflict, "The run has no native workflow.", "List the run's active holds before acting.", nil)
	}
	return b, err
}
func (h heldRuns) List(ctx context.Context, r stateport.Run) ([]model.LiveHold, error) {
	b, err := h.binding(ctx, r)
	if err != nil {
		return nil, err
	}
	return h.native.Holds(ctx, b)
}
func (h heldRuns) Abandon(ctx context.Context, r stateport.Run, hold model.LiveHold) error {
	b, err := h.binding(ctx, r)
	if err != nil {
		return err
	}
	return h.native.Abandon(ctx, b, hold)
}
func (h heldRuns) Exec(ctx context.Context, r stateport.Run, hold model.LiveHold, q service.HoldInspection) (apicontract.ExecResult, error) {
	b, err := h.binding(ctx, r)
	if err != nil {
		return apicontract.ExecResult{}, err
	}
	out, err := h.native.HoldExec(ctx, argojob.HoldExecRequest{Binding: b, Hold: hold, Command: q.Args.Argv, Timeout: time.Duration(q.Args.TimeoutS) * time.Second, BeforeExec: q.BeforeExec})
	return apicontract.ExecResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode, Truncated: out.Truncated}, err
}
