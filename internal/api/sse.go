package api

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

func acceptsSSE(h http.Header) bool { return acceptsMedia(h.Values("Accept"), "text/event-stream") }
func acceptsMedia(values []string, want string) bool {
	accepted := false
	for _, header := range values {
		for _, value := range strings.Split(header, ",") {
			media, params, err := mime.ParseMediaType(strings.TrimSpace(value))
			if err != nil || media != want {
				continue
			}
			if raw, ok := params["q"]; ok {
				q, err := strconv.ParseFloat(raw, 64)
				if err != nil || !(q > 0 && q <= 1) {
					return false
				}
			}
			accepted = true
		}
	}
	return accepted
}
func (s *Server) frame(w http.ResponseWriter, frame string) error {
	ctrl := http.NewResponseController(w)
	if err := ctrl.SetWriteDeadline(s.config.Runtime.Now().Add(eventWriteTimeout)); err != nil {
		return err
	}
	defer func() { _ = ctrl.SetWriteDeadline(time.Time{}) }()
	if _, err := io.WriteString(w, frame); err != nil {
		return err
	}
	return ctrl.Flush()
}

type streamInvocation struct {
	caller   apicontract.Caller
	prepared prepared
	response rpcResponse
	trace    *Diagnostic
}

func (s *Server) streamCall(ctx context.Context, w http.ResponseWriter, r *http.Request, v streamInvocation) {
	c, p, resp := v.caller, v.prepared, v.response
	diagnostic := v.trace
	diagnostic.Outcome = Disconnected
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.frame(w, ": keepalive\n\n"); err != nil {
		return
	}
	countFrame(&diagnostic.Frames)
	budget := requestBudget(ctx)
	budget.streaming = true
	value, err := s.withHeartbeats(w, r.WithContext(ctx), c, observation{call: p.call, frame: func() { countFrame(&diagnostic.Frames) }})
	if budget.expired {
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if s.draining.Load() {
		resp.Error = drainRPC()
	} else {
		resp.Result = s.toolResponse(value, err, p.meta.OutputBytes, diagnostic)
	}
	data, err := encodeBounded(resp, rpcResponseBytes)
	if err != nil {
		return
	}
	if err := s.frame(w, "event: message\ndata: "+string(data)+"\n\n"); err == nil {
		countFrame(&diagnostic.Frames)
		diagnostic.Outcome = Final
	}
}

// withHeartbeats joins its single writer before returning ownership to the caller.
func (s *Server) withHeartbeats(w http.ResponseWriter, r *http.Request, c apicontract.Caller, o observation) (value any, err error) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := make(chan struct{})
	joined := make(chan struct{})
	var heartbeatErr error
	go func() {
		defer close(joined)
		heartbeatErr = s.heartbeat(ctx, w, heartbeatState{request: r, caller: c, stop: stop, cancel: cancel, frame: o.frame})
	}()
	var once sync.Once
	join := func() { once.Do(func() { close(stop) }); <-joined }
	if budget := requestBudget(ctx); budget != nil {
		budget.joinWriter = join
	}
	defer func() {
		join()
		if heartbeatErr != nil {
			value = nil
			err = heartbeatErr
		}
	}()

	return o.call(ctx)
}

type heartbeatState struct {
	request *http.Request
	caller  apicontract.Caller
	stop    <-chan struct{}
	cancel  context.CancelFunc
	frame   func()
}

func (s *Server) heartbeat(ctx context.Context, w http.ResponseWriter, state heartbeatState) error {
	r, c, stop, cancel := state.request, state.caller, state.stop, state.cancel
	beats, end := s.config.Runtime.Ticker(20 * time.Second)
	defer end()
	auth, endAuth := s.config.Runtime.Ticker(30 * time.Second)
	defer endAuth()
	for {
		select {
		case <-stop:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-s.drain:
			cancel()
			return refusal("restarting")
		case <-auth:
			if !s.stillAuthorized(r, c) {
				cancel()
				return refusal("auth")
			}
		case <-beats:
			if err := s.frame(w, ": keepalive\n\n"); err != nil {
				cancel()
				return err
			}
			if state.frame != nil {
				state.frame()
			}
		}
	}
}

func notification(method string, params any) string {
	data, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", method, params})
	return "event: message\ndata: " + string(data) + "\n\n"
}

type observation struct {
	call  func(context.Context) (any, error)
	frame func()
}

func countFrame(n *uint16) {
	if *n < ^uint16(0) {
		*n++
	}
}
