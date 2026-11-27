package apicontract_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestRPCReplyCorrelatesStringID(t *testing.T) {
	data := []byte(`{"jsonrpc":"2.0","id":"view","result":{"content":[{"type":"text","text":"{}"}],"structuredContent":{},"isError":false}}`)
	if _, _, _, err := apicontract.ToolPayloadForID(data, json.RawMessage(`"view"`)); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := apicontract.ToolPayloadForID(data, json.RawMessage(`1`)); !errors.Is(err, apicontract.ErrReply) {
		t.Fatal("mismatched ID accepted")
	}
	if apicontract.ProtocolVersion != "2026-07-28" {
		t.Fatal("wrong protocol revision")
	}
}

func TestSSEFramesBoundedAndRequestScoped(t *testing.T) {
	var frames []apicontract.SSEFrame
	err := apicontract.ReadSSE(strings.NewReader(": keepalive\n\nid: 42\ndata: {\ndata: \"method\":\"notifications/events\"}\n\nid: 43\ndata: truncated"), 128, func(f apicontract.SSEFrame) error {
		frames = append(frames, f)
		return nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(frames) != 2 || !frames[0].Keepalive || string(frames[1].Data) != "{\n\"method\":\"notifications/events\"}" {
		t.Fatalf("request frame changed: %+v %v", frames, err)
	}
	for _, wire := range []string{strings.Repeat("x", 129), "data: " + strings.Repeat("x\ndata: ", 40)} {
		if err := apicontract.ReadSSE(strings.NewReader(wire), 128, func(apicontract.SSEFrame) error { t.Fatal("oversize emitted"); return nil }); !errors.Is(err, apicontract.ErrReplySize) {
			t.Fatalf("frame bound missing: %v", err)
		}
	}
}

func TestReplyKeepaliveAndObserverFailure(t *testing.T) {
	read := func() *http.Response {
		return &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(": keepalive\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n"))}
	}
	count := 0
	first, second := read(), read()
	defer first.Body.Close()
	defer second.Body.Close()
	data, err := apicontract.ReadReplyWithProgress(first, 256, func() error { count++; return nil })
	if err != nil || count != 1 || string(data) != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("keepalive/final reply changed: %s %v %d", data, err, count)
	}
	failure := errors.New("output closed")
	if _, err = apicontract.ReadReplyWithProgress(second, 256, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatal("observer failure ignored")
	}
}
