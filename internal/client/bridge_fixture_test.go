package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

type bridgePeer struct {
	t      *testing.T
	input  *io.PipeWriter
	frames chan json.RawMessage
	done   chan error
	cancel context.CancelFunc
}

func bridgeProfile(t *testing.T, rt http.RoundTripper) BridgeConfig {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "test")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(filepath.Join(dir, "installation.json"), []byte(`{"server_url":"https://127.0.0.1","uplink":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(filepath.Join(dir, "token"), []byte("valid")); err != nil {
		t.Fatal(err)
	}
	clock := fake.NewClock()
	return BridgeConfig{Base: base, Profile: "test", Config: Config{Transport: rt, Now: clock.Now, Wait: func(context.Context, time.Duration) error { return nil }}}
}

func newBridgePeer(t *testing.T, cfg BridgeConfig) *bridgePeer {
	t.Helper()
	return wrappedBridgePeer(t, cfg, nil)
}

func wrappedBridgePeer(t *testing.T, cfg BridgeConfig, wrap func(io.WriteCloser) io.WriteCloser) *bridgePeer {
	t.Helper()
	in, write := io.Pipe()
	read, out := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	p := &bridgePeer{t: t, input: write, frames: make(chan json.RawMessage, 128), done: make(chan error, 1), cancel: cancel}
	target := io.WriteCloser(out)
	if wrap != nil {
		target = wrap(target)
	}
	go func() {
		err := Bridge(ctx, in, target, cfg)
		_ = in.Close()
		_ = out.Close()
		p.done <- err
		close(p.done)
	}()
	go func() {
		s := bufio.NewScanner(read)
		s.Buffer(make([]byte, 4096), apicontract.RPCReplyBytes(apicontract.ResponseBytes)+1)
		for s.Scan() {
			data := bytes.Clone(s.Bytes())
			select {
			case p.frames <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = write.Close(); _ = read.Close(); <-p.done })
	return p
}
func (p *bridgePeer) send(id any, method string, params any) {
	p.t.Helper()
	frame := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		frame["id"] = id
	}
	if params != nil {
		frame["params"] = params
	}
	data, err := json.Marshal(frame)
	if err == nil {
		written := make(chan error, 1)
		go func() { _, writeErr := p.input.Write(append(data, '\n')); written <- writeErr }()
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case err = <-written:
		case <-timer.C:
			p.t.Fatal("open request blocked stdio input")
		}
	}
	if err != nil {
		select {
		case stopped := <-p.done:
			p.t.Fatalf("input failed: %v; bridge: %v", err, stopped)
		default:
			p.t.Fatal(err)
		}
	}
}
func (p *bridgePeer) frame() json.RawMessage {
	p.t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case data := <-p.frames:
		if !json.Valid(data) {
			p.t.Fatal("stdout contains non-protocol output")
		}
		return data
	case err := <-p.done:
		p.t.Fatalf("bridge stopped: %v", err)
	case <-timer.C:
		p.t.Fatal("bridge did not answer while another request was open")
	}
	return nil
}
func (p *bridgePeer) initialize() json.RawMessage {
	p.t.Helper()
	p.send("init", "initialize", map[string]any{"protocolVersion": apicontract.ProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}})
	data := p.frame()
	var reply struct {
		ID    string
		Error any
	}
	if json.Unmarshal(data, &reply) != nil || reply.ID != "init" || reply.Error != nil {
		p.t.Fatalf("initialize refused: %s", data)
	}
	return data
}
func (p *bridgePeer) tool(id any, name string, args any, token any) {
	params := map[string]any{"name": name, "arguments": args}
	if token != nil {
		params["_meta"] = map[string]any{"progressToken": token}
	}
	p.send(id, "tools/call", params)
}

type pipeResponse struct {
	header http.Header
	ready  chan struct{}
	once   sync.Once
	writer *io.PipeWriter
	status int
}

func (w *pipeResponse) Header() http.Header { return w.header }
func (w *pipeResponse) WriteHeader(status int) {
	w.once.Do(func() { w.status = status; close(w.ready) })
}
func (w *pipeResponse) Write(data []byte) (int, error) {
	w.WriteHeader(200)
	return w.writer.Write(data)
}
func (w *pipeResponse) Flush()                         { w.WriteHeader(200) }
func (*pipeResponse) SetWriteDeadline(time.Time) error { return nil }

func streamingTransport(handler http.Handler) roundTrip {
	return func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		w := &pipeResponse{header: http.Header{}, ready: make(chan struct{}), writer: writer}
		stop := context.AfterFunc(r.Context(), func() { _ = reader.CloseWithError(r.Context().Err()); _ = writer.CloseWithError(r.Context().Err()) })
		go func() { handler.ServeHTTP(w, r); w.WriteHeader(200); _ = writer.Close() }()
		select {
		case <-w.ready:
		case <-r.Context().Done():
			stop()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: w.status, Header: w.header.Clone(), Body: &responsePipe{PipeReader: reader, stop: stop}, Request: r}, nil
	}
}

type responsePipe struct {
	*io.PipeReader
	stop func() bool
}

func (p *responsePipe) Close() error { p.stop(); return p.PipeReader.Close() }

func readBridgeRequest(r *http.Request) rpcRequest {
	var req rpcRequest
	data, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	_ = json.Unmarshal(data, &req)
	r.Body = io.NopCloser(bytes.NewReader(data))
	return req
}
