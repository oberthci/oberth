package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (r holdRuntime) client(ctx context.Context, args []string, streams HoldIO) int {
	request, probe, err := r.clientRequest(args)
	if err != nil {
		return holdDiagnostic(streams.Err, 120, err)
	}
	request, err = r.stageClientRequest(ctx, request, streams.In)
	if err != nil {
		if errors.Is(err, errRerunStageSpace) {
			return finishHoldClient(holdFrame{Type: "refused", Reason: rerunStageReason, Remedy: rerunStageRemedy}, streams, false)
		}
		return holdDiagnostic(streams.Err, 120, err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(r.root, "hold.sock"))
	if err != nil {
		if request.Op == "rerun" {
			_ = os.Remove(request.Stage)
		}
		return holdDiagnostic(streams.Err, 122, fmt.Errorf("hold is no longer available"))
	}
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	if err = writeHoldFrame(conn, request); err != nil {
		return holdDiagnostic(streams.Err, 124, err)
	}
	if request.Op == "rerun" {
		return finishRerunClient(conn, request, streams)
	}
	return readHoldClient(conn, streams, probe)
}

func (r holdRuntime) stageClientRequest(ctx context.Context, request holdRequest, input io.Reader) (holdRequest, error) {
	if request.Op != "rerun" {
		return request, nil
	}
	if request.UID != r.env("OBERTH_HOLD_POD_UID") {
		return request, fmt.Errorf("pod UID mismatch")
	}
	// The owner owns an admitted stage even when its reply is lost.
	stage, err := r.stageRerun(ctx, input, request.Digest)
	request.Stage = stage
	return request, err
}

func finishRerunClient(conn net.Conn, request holdRequest, streams HoldIO) int {
	var frame holdFrame
	if err := readHoldFrame(conn, &frame); err != nil {
		return holdDiagnostic(streams.Err, 124, err)
	}
	if frame.Type == "refused" || frame.Type == "closed" || frame.Type == "already_rerun" {
		_ = os.Remove(request.Stage)
	}
	return finishHoldClient(frame, streams, false)
}
func readHoldClient(conn net.Conn, streams HoldIO, probe bool) int {
	for {
		var f holdFrame
		if err := readHoldFrame(conn, &f); err != nil {
			return holdDiagnostic(streams.Err, 124, err)
		}
		switch f.Type {
		case "out", "err":
			if len(f.Data) > 64<<10 {
				return holdDiagnostic(streams.Err, 124, fmt.Errorf("invalid output frame"))
			}
			out := streams.Out
			if f.Type == "err" {
				out = streams.Err
			}
			if _, err := out.Write(f.Data); err != nil {
				return 124
			}
		default:
			return finishHoldClient(f, streams, probe)
		}
	}
}
func finishHoldClient(f holdFrame, streams HoldIO, probe bool) int {
	code := holdFrameCode(f)
	if !probe {
		body, _ := json.Marshal(f)
		_, _ = fmt.Fprintln(streams.Err, "oberth-hold:", string(body))
	}
	return code
}

func (r holdRuntime) clientRequest(args []string) (holdRequest, bool, error) {
	request := holdRequest{Version: holdProtocol}
	if len(args) == 0 {
		return request, false, fmt.Errorf("missing operation")
	}
	request.Op = args[0]
	probe := len(args) == 2 && args[1] == "--probe" && args[0] == "status"
	if probe {
		request.UID = r.env("OBERTH_HOLD_POD_UID")
		return request, true, nil
	}
	for i := 1; i < len(args); i++ {
		if args[i] == "--" {
			request.Argv = args[i+1:]
			break
		}
		if i+1 >= len(args) {
			return request, false, fmt.Errorf("missing option value")
		}
		if err := holdClientOption(&request, args[i], args[i+1]); err != nil {
			return request, false, err
		}
		i++
	}
	// The rerun stage is filled only after stdin has been verified.
	if request.Op == "rerun" {
		request.Stage = "pending"
	}
	if !validHoldRequest(request) {
		return request, false, fmt.Errorf("invalid request")
	}
	return request, false, nil
}

func holdClientOption(request *holdRequest, option, value string) error {
	switch option {
	case "--pod-uid":
		request.UID = value
	case "--timeout-ms":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		request.TimeoutMS = n
	case "--sha256":
		request.Digest = value
	default:
		return fmt.Errorf("unknown option %s", strings.TrimSpace(option))
	}
	return nil
}
