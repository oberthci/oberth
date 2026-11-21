package argojob

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"strconv"
	"sync"
	"time"
)

const (
	DefaultExecTimeout = time.Minute
	MaxExecTimeout     = 15 * time.Minute
	DefaultExecOutput  = 64 << 10
	MaxExecOutput      = 4 << 20
)

type HoldExecRequest struct {
	Binding   nativebinding.RunBinding
	Hold      model.LiveHold
	Command   []string
	Timeout   time.Duration
	MaxOutput int
}
type ExecResult struct {
	Stdout, Stderr string
	ExitCode       int
	Truncated      bool
}
type outputBudget struct {
	mu        sync.Mutex
	remaining int
}
type boundedOutput struct {
	budget    *outputBudget
	mu        sync.Mutex
	body      bytes.Buffer
	limit     int
	truncated bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	size := len(p)
	n := min(size, max(0, w.limit-w.body.Len()))
	if w.budget != nil {
		w.budget.mu.Lock()
		n = min(n, w.budget.remaining)
		w.budget.remaining -= n
		w.budget.mu.Unlock()
	}
	_, _ = w.body.Write(p[:n])
	w.truncated = w.truncated || n < size
	return size, nil
}

type holdTail struct {
	mu   sync.Mutex
	body []byte
	out  *boundedOutput
}

func (w *holdTail) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.body = append(w.body, p...)
	if spill := len(w.body) - 4096; spill > 0 {
		_, _ = w.out.Write(w.body[:spill])
		w.body = append([]byte(nil), w.body[spill:]...)
	}
	return n, nil
}
func (e *Executor) HoldExec(ctx context.Context, r HoldExecRequest) (ExecResult, error) {
	r.Timeout, r.MaxOutput = cmp.Or(r.Timeout, DefaultExecTimeout), cmp.Or(r.MaxOutput, DefaultExecOutput)
	if r.Timeout <= 0 || r.Timeout > MaxExecTimeout || r.MaxOutput < 0 || r.MaxOutput > MaxExecOutput || len(r.Command) == 0 || len(r.Command) > 256 {
		return ExecResult{}, limited()
	}
	if err := e.verifyHold(ctx, r.Binding, r.Hold); err != nil {
		return ExecResult{}, err
	}
	command := []string{holdBinary, "exec", "--pod-uid", r.Hold.PodUID, "--timeout-ms", strconv.FormatInt(r.Timeout.Milliseconds(), 10), "--"}
	return e.relayHold(ctx, r.Hold, append(command, r.Command...), r.MaxOutput)
}
func (e *Executor) relayHold(ctx context.Context, h model.LiveHold, command []string, limit int) (ExecResult, error) {
	budget := &outputBudget{remaining: limit}
	stdout, stderr := &boundedOutput{limit: limit, budget: budget}, &boundedOutput{limit: limit, budget: budget}
	tail := &holdTail{out: stderr}
	err := e.config.Pods.Exec(ctx, ExecRequest{Pod: h.PodName, Container: "main", Command: command, Out: stdout, Err: tail})
	f, payload, ok := holdReply(tail.body)
	_, _ = stderr.Write(payload)
	result := ExecResult{Stdout: stdout.body.String(), Stderr: stderr.body.String(), Truncated: stdout.truncated || stderr.truncated}
	if !ok {
		return result, notHeld(err)
	}
	result.ExitCode = holdFrameCode(f)
	if f.Type == "exit" || f.Type == "closed" && command[1] == "release" {
		return result, nil
	}
	if f.Type == "timeout" {
		return result, execFailure(result, publicerr.Unavailable, context.DeadlineExceeded)
	}
	if f.Type == "start_failed" {
		return result, execFailure(result, publicerr.InvalidInput, err)
	}
	return result, notHeld(err)
}
func holdReply(data []byte) (holdFrame, []byte, bool) {
	at := bytes.LastIndex(data, []byte("oberth-hold: "))
	var f holdFrame
	if at < 0 || json.Unmarshal(bytes.TrimSpace(data[at+len("oberth-hold: "):]), &f) != nil {
		return f, data, false
	}
	return f, data[:at], f.Type == "held" || f.Type == "exit" || f.Type == "closed" || f.Type == "timeout" || f.Type == "refused" || f.Type == "start_failed"
}

func execFailure(result ExecResult, code publicerr.Code, cause error) error {
	reason, remedy := execStartReason, execStartRemedy
	if code == publicerr.Unavailable {
		reason, remedy = execTimeoutReason, execTimeoutRemedy
	}
	e := publicerr.New(code, reason, remedy, cause)
	raw, _ := json.Marshal(result)
	e.Detail = string(raw)
	return e
}
