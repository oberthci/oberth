package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// PreparedLongPoll observes existing work once. Its context never owns that work.
type PreparedLongPoll func(context.Context) (any, error)

// LongPollPreparer performs strict argument validation and the first target
// lookup before HTTP streaming begins. Preparation must not duplicate the first
// observation when the returned single-use call runs.
type LongPollPreparer interface {
	PrepareLongPoll(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error)
}

// acceptsMCPStream requires an explicit acceptable SSE media type. Existing
// JSON-only and headerless clients retain their response format. An explicit
// exclusion wins over duplicate/wildcard ranges; malformed quality fails closed.
func acceptsMCPStream(header http.Header) bool {
	accepted := false
	for _, value := range header.Values("Accept") {
		for _, item := range strings.Split(value, ",") {
			media, params, err := mime.ParseMediaType(strings.TrimSpace(item))
			if err != nil || !strings.EqualFold(media, "text/event-stream") {
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

func supportsMCPFlush(writer http.ResponseWriter) bool {
	for {
		if _, ok := writer.(interface{ FlushError() error }); ok {
			return true
		}
		if _, ok := writer.(http.Flusher); ok {
			return true
		}
		unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		writer = unwrapper.Unwrap()
	}
}

// writeMCPFrame bounds each network write/flush independently of the existing
// long-poll ceiling, then restores that ceiling. The caller owns the writer
// exclusively, including while ResponseController unwraps HTTP middleware.
func writeMCPFrame(writer http.ResponseWriter, frame string, deadline time.Time, writeTimeout time.Duration) (result error) {
	controller := http.NewResponseController(writer)
	short := time.Now().Add(writeTimeout)
	if deadline.Before(short) {
		short = deadline
	}
	if err := controller.SetWriteDeadline(short); err != nil {
		return err
	}
	defer func() {
		if err := controller.SetWriteDeadline(deadline); err != nil {
			result = errors.Join(result, err)
		}
	}()
	if n, err := io.WriteString(writer, frame); err != nil {
		return err
	} else if n != len(frame) {
		return io.ErrShortWrite
	}
	return controller.Flush()
}

func (server *Server) streamLongPoll(writer http.ResponseWriter, request *http.Request, response rpcResponse, prepared PreparedLongPoll, deadline time.Time) error {
	return server.streamLongPollObserved(writer, request, response, prepared, deadline, nil)
}

func (server *Server) streamLongPollObserved(writer http.ResponseWriter, request *http.Request, response rpcResponse, prepared PreparedLongPoll, deadline time.Time, trace *mcpWaitObservation) error {
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	interval := server.mcpHeartbeatInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	writeTimeout := server.mcpStreamWriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 5 * time.Second
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	const heartbeat = ": keepalive\n\n"
	if err := writeMCPFrame(writer, heartbeat, deadline, writeTimeout); err != nil {
		trace.outcome(mcpWaitWriteOutcome(err, "initial"))
		return err
	}
	trace.frame("initial")
	stop := make(chan struct{})
	joined := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				joined <- nil
				return
			case <-ctx.Done():
				joined <- ctx.Err()
				return
			case <-ticker.C:
				if err := writeMCPFrame(writer, heartbeat, deadline, writeTimeout); err != nil {
					trace.outcome(mcpWaitWriteOutcome(err, "heartbeat"))
					cancel() // Stop this observer, never its durable run or promotion.
					joined <- err
					return
				}
				trace.frame("heartbeat")
			}
		}
	}()
	// The observer stays in the request goroutine. A disconnected transport cannot
	// leave an unjoined tool goroutine behind. Only the heartbeat writes meanwhile.
	var heartbeatErr error
	stopped := false
	join := func() {
		if !stopped {
			close(stop)
			heartbeatErr = <-joined
			stopped = true
		}
	}
	defer join() // Includes a panic in the observer: no writer outlives the request.
	payload, callErr := prepared(ctx)
	join()
	if heartbeatErr != nil || ctx.Err() != nil {
		if trace != nil && trace.event.Outcome == "panic" {
			if request.Context().Err() != nil {
				trace.outcome("request-context-canceled")
			} else {
				trace.outcome("observer-context-canceled")
			}
		}
		return errors.Join(heartbeatErr, ctx.Err())
	}
	if callErr != nil {
		if trace == nil {
			response.Result = server.classifyToolFailure(callErr)
		} else {
			response.Result = server.classifyWaitFailure(callErr, trace)
		}
	} else {
		response.Result = toolSuccess(payload)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		trace.outcome("marshal-failed")
		return err
	}
	// The heartbeat goroutine has joined: exactly one owner writes the final RPC.
	err = writeMCPFrame(writer, "event: message\ndata: "+string(encoded)+"\n\n", deadline, writeTimeout)
	if err != nil {
		trace.outcome(mcpWaitWriteOutcome(err, "final"))
		return err
	}
	trace.frame("final")
	trace.outcome("final-frame-flushed")
	return nil
}
