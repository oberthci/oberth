package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"time"
)

// These diagnostics deliberately have no actor, arguments, RPC ID, target,
// headers, payload or error-string fields. The nonce is server generated and
// is only a correlation hint, never an authority or client delivery receipt.
type mcpWaitEvent struct {
	Event           string `json:"event"`
	UTC             string `json:"utc"`
	Nonce           string `json:"nonce"`
	Tool            string `json:"tool"`
	Mode            string `json:"mode"`
	Outcome         string `json:"outcome,omitempty"`
	ElapsedMS       int64  `json:"elapsed_ms"`
	InitialFrames   uint32 `json:"initial_frames"`
	HeartbeatFrames uint32 `json:"heartbeat_frames"`
	FinalFrames     uint32 `json:"final_frames"`
}

type mcpWaitObservation struct {
	server   *Server
	start    time.Time
	event    mcpWaitEvent
	finished bool
}

func (server *Server) beginMCPWait(writer http.ResponseWriter, tool string, stream bool) *mcpWaitObservation {
	var random [16]byte
	_, _ = rand.Read(random[:])
	mode := "json"
	if stream {
		mode = "sse"
	}
	trace := &mcpWaitObservation{server: server, start: time.Now(), event: mcpWaitEvent{Tool: tool, Mode: mode, Nonce: hex.EncodeToString(random[:])}}
	writer.Header().Set("X-Oberth-Wait-ID", trace.event.Nonce)
	trace.emit("start")
	trace.event.Outcome = "panic"
	return trace
}

func (trace *mcpWaitObservation) emit(event string) {
	value := trace.event
	value.Event = event
	value.UTC = time.Now().UTC().Format(time.RFC3339Nano)
	value.ElapsedMS = time.Since(trace.start).Milliseconds()
	if trace.server.mcpWaitLog != nil {
		trace.server.mcpWaitLog(value)
		return
	}
	raw, _ := json.Marshal(value)
	log.Printf("mcp_wait %s", raw)
}

func (trace *mcpWaitObservation) finish() {
	if trace == nil || trace.finished {
		return
	}
	trace.finished = true
	trace.emit("terminal")
}
func (trace *mcpWaitObservation) outcome(value string) {
	if trace != nil {
		trace.event.Outcome = value
	}
}
func (trace *mcpWaitObservation) frame(kind string) {
	if trace == nil {
		return
	}
	var count *uint32
	switch kind {
	case "initial":
		count = &trace.event.InitialFrames
	case "heartbeat":
		count = &trace.event.HeartbeatFrames
	case "final":
		count = &trace.event.FinalFrames
	default:
		return
	}
	if *count < 65535 {
		*count++
	}
}
func mcpWaitWriteOutcome(err error, phase string) string {
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return "write-deadline-exceeded"
	}
	switch phase {
	case "initial":
		return "initial-write-failed"
	case "heartbeat":
		return "heartbeat-write-failed"
	default:
		return "final-write-failed"
	}
}

// Preserve public response classification without the legacy raw internal-error
// log. Unknown failures refer to this request's fixed public nonce instead.
func (server *Server) classifyWaitFailure(err error, trace *mcpWaitObservation) map[string]any {
	code, message := server.classifyError(err)
	if code == http.StatusInternalServerError {
		message = "internal error (reference " + trace.event.Nonce + ")"
	}
	return toolFailure(errors.New(message))
}

func (server *Server) handleMCPLongPoll(writer http.ResponseWriter, request *http.Request, response rpcResponse, call callParams) {
	deadline := time.Now().Add(server.maximumToolWait + writeDeadlineMargin)
	deadlineErr := http.NewResponseController(writer).SetWriteDeadline(deadline)
	preparer, canPrepare := server.tools.(LongPollPreparer)
	stream := canPrepare && deadlineErr == nil && acceptsMCPStream(request.Header) && supportsMCPFlush(writer)
	if stream {
		prepared, err := preparer.PrepareLongPoll(request.Context(), actorFrom(request.Context()), call.Name, call.Arguments)
		// Preparation selects the actual response transport. Refusals retain
		// the existing JSON response, even if the client requested SSE.
		trace := server.beginMCPWait(writer, call.Name, err == nil)
		defer trace.finish()
		if err != nil {
			response.Result = server.classifyWaitFailure(err, trace)
			trace.outcome("preparation-refused")
			writeMCPWaitJSON(writer, response, trace, true)
			return
		}
		_ = server.streamLongPollObserved(writer, request, response, prepared, deadline, trace)
		return
	}
	trace := server.beginMCPWait(writer, call.Name, false)
	defer trace.finish()
	result, err := server.tools.CallTool(request.Context(), actorFrom(request.Context()), call.Name, call.Arguments)
	if err != nil {
		response.Result = server.classifyWaitFailure(err, trace)
	} else {
		response.Result = toolSuccess(result)
	}
	canceled := request.Context().Err() != nil
	if canceled {
		trace.outcome("request-context-canceled")
	}
	writeMCPWaitJSON(writer, response, trace, canceled)
}

func writeMCPWaitJSON(writer http.ResponseWriter, response rpcResponse, trace *mcpWaitObservation, preserveOutcome bool) {
	writer.Header().Set("Content-Type", "application/json")
	raw, err := json.Marshal(response)
	if err != nil {
		trace.outcome("marshal-failed")
		return
	}
	raw = append(raw, '\n')
	n, err := writer.Write(raw)
	if err != nil || n != len(raw) {
		trace.outcome(mcpWaitWriteOutcome(err, "final"))
		return
	}
	trace.frame("final")
	if supportsMCPFlush(writer) {
		if err := http.NewResponseController(writer).Flush(); err != nil {
			trace.outcome(mcpWaitWriteOutcome(err, "final"))
			return
		}
		if !preserveOutcome {
			trace.outcome("final-frame-flushed")
		}
	} else if !preserveOutcome {
		trace.outcome("final-frame-written")
	}
}
