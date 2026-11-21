package argojob

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

func (r holdRuntime) client(ctx context.Context, args []string, streams HoldIO) int {
	request, probe, err := r.clientRequest(args)
	if err != nil {
		return holdDiagnostic(streams.Err, 120, err)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(r.root, "hold.sock"))
	if err != nil {
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
	return readHoldClient(conn, streams, probe)
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
	request := holdRequest{Version: 1}
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
		switch args[i] {
		case "--pod-uid":
			i++
			request.UID = args[i]
		case "--timeout-ms":
			i++
			n, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil {
				return request, false, err
			}
			request.TimeoutMS = n
		default:
			return request, false, fmt.Errorf("unknown option %s", strings.TrimSpace(args[i]))
		}
	}
	if !validHoldRequest(request) {
		return request, false, fmt.Errorf("invalid request")
	}
	return request, false, nil
}
