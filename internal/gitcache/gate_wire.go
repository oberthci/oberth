package gitcache

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"

	"github.com/oberthci/oberth/internal/strictjson"
)

const maxGateFrame = 8 << 20

type gateRequest struct {
	Quarantine string      `json:"quarantine"`
	Phase      string      `json:"phase,omitempty"`
	Refusal    string      `json:"refusal,omitempty"`
	Updates    []RefUpdate `json:"updates,omitempty"`
}
type gateReply struct {
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func readGateFrame(r io.Reader, target any) error {
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return err
	}
	if size > maxGateFrame {
		return &LimitError{}
	}
	data := make([]byte, int(size))
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return strictjson.DecodeStrictObject(data, target, maxGateFrame)
}
func writeGateFrame(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxGateFrame {
		return &LimitError{}
	}
	if err = binary.Write(w, binary.BigEndian, uint32(len(data))); err != nil {
		return err
	}
	_, err = io.Copy(w, bytes.NewReader(data))
	return err
}

func callGate(ctx context.Context) error {
	return callGateRequest(ctx, gateRequest{Quarantine: os.Getenv("GIT_OBJECT_DIRECTORY")})
}
func callGateRequest(ctx context.Context, request gateRequest) error {
	address := os.Getenv("OBERTH_RECEIVE_GATE")
	if address == "" {
		return &gateError{code: "unavailable"}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return &gateError{code: "unavailable", cause: err}
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err = writeGateFrame(conn, request); err != nil {
		return &gateError{code: "unavailable", cause: err}
	}
	var reply gateReply
	if err = readGateFrame(conn, &reply); err != nil {
		return &gateError{code: "unavailable", cause: err}
	}
	if reply.Code != "accepted" {
		return &gateError{code: reply.Code, detail: reply.Detail, reported: true}
	}
	if ctx.Err() != nil {
		return &gateError{code: "unavailable", cause: context.Cause(ctx)}
	}
	return nil
}

type gateError struct {
	code, detail string
	cause        error
	reported     bool
	// deadline marks the refusal of an admission that ran out of time.
	deadline bool
}

func (e *gateError) Reason() string {
	switch e.code {
	case "refused":
		return reasonGateRefused
	}
	return reasonGateUnavailable
}
func (e *gateError) Remedy() string {
	if e.code == "refused" {
		return remedyGateRefused
	}
	return remedyGateRetry
}
func (e *gateError) Error() string {
	message := e.Reason() + ". " + e.Remedy()
	if e.detail != "" {
		return e.detail
	}
	return message
}
func (e *gateError) Unwrap() error { return e.cause }
