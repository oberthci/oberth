package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/apicontract"
)

// BridgeConfig selects one installed profile; transport overrides do not apply.
// Display reuses the CLI renderer. Diagnostics never share the protocol writer.
type BridgeConfig struct {
	Config        Config
	Base, Profile string
	Display       func(string, json.RawMessage) (string, bool)
	Diagnostic    io.Writer
	Jitter        func() float64
}

type bridge struct {
	ctx         context.Context
	cancel      context.CancelFunc
	client      *Client
	config      BridgeConfig
	out         io.Writer
	output      sync.Mutex
	outputErr   error
	mu          sync.Mutex
	active      map[string]context.CancelFunc
	session     string
	version     string
	initialized sync.Once
	workers     sync.WaitGroup
	eventCtx    context.Context
	events      sync.WaitGroup
}

// Bridge speaks newline JSON-RPC until EOF or cancellation, then joins requests.
// Blocking input and output must be closable so cancellation can interrupt I/O.
func Bridge(ctx context.Context, in io.Reader, out io.Writer, config BridgeConfig) error {
	if config.Diagnostic != nil {
		config.Diagnostic = &diagnosticWriter{out: config.Diagnostic}
	}
	remote, err := bridgeClient(config)
	if err != nil {
		return err
	}
	if remote != nil {
		defer remote.Close()
	}
	life, cancel := context.WithCancel(ctx)
	events, stopEvents := context.WithCancel(life)
	b := &bridge{eventCtx: events, ctx: life, cancel: cancel, client: remote, config: config, out: out, active: map[string]context.CancelFunc{}}
	stop := context.AfterFunc(life, func() {
		for _, stream := range []any{in, out} {
			if c, ok := stream.(io.Closer); ok {
				_ = c.Close()
			}
		}
	})
	err = b.read(life, in)
	stopEvents()
	if err != nil {
		cancel()
	}
	b.workers.Wait()
	b.events.Wait()
	b.closeSession(ctx)
	cancel()
	stop()
	if b.outputErr != nil {
		return b.outputErr
	}
	if ctx.Err() != nil {
		return failure(failureClass(ctx.Err()), ctx.Err())
	}
	return err
}

func bridgeClient(config BridgeConfig) (*Client, error) {
	if config.Profile == "" {
		return nil, failure("bridge_profile", nil)
	}
	p, dir, err := loadProfile(config.Base, config.Profile)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, failure("bridge_profile", nil)
	}
	if p.AdminKey != "" || p.AdminCert != "" || !p.Uplink {
		return nil, failure("bridge_admin", nil)
	}
	config.Config.Options = inheritProfile(Options{}, p, dir)
	config.Config.Diagnostic = config.Diagnostic
	return New(config.Config)
}

func (b *bridge) read(ctx context.Context, in io.Reader) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), apicontract.RequestBytes+1)
	for scanner.Scan() && b.ctx.Err() == nil {
		var req rpcRequest
		if !utf8.Valid(scanner.Bytes()) || json.Unmarshal(scanner.Bytes(), &req) != nil {
			b.rpcError(nil, -32700, failure("bridge_input", nil))
			continue
		}
		id, valid := rpcID(req.ID)
		if req.JSONRPC != "2.0" || req.Method == "" || !valid {
			b.rpcError(nil, -32600, failure("bridge_input", nil))
			continue
		}
		req.ID = id
		b.accept(ctx, req)
	}
	if err := scanner.Err(); err != nil && b.ctx.Err() == nil {
		return failure("bridge_input", err)
	}
	return nil
}

func rpcID(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		id, _ := json.Marshal(text)
		return id, true
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		id, _ := json.Marshal(number)
		return id, true
	}
	return nil, false
}

func (b *bridge) emit(ctx context.Context, data []byte) error {
	var frame bytes.Buffer
	if !json.Valid(data) {
		return failure("reply", nil)
	}
	if bytes.ContainsAny(data, "\r\n") {
		if err := json.Compact(&frame, data); err != nil {
			return failure("reply", err)
		}
	} else {
		frame.Write(data)
	}
	b.output.Lock()
	defer b.output.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	wire := append(frame.Bytes(), '\n')
	n, err := b.out.Write(wire)
	if err == nil && n != len(wire) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b.outputErr = err
		b.cancel()
	}
	return err
}

func (b *bridge) accept(parent context.Context, req rpcRequest) {
	if req.Method == "notifications/cancelled" {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &params) == nil {
			id, _ := rpcID(params.RequestID)
			b.mu.Lock()
			if cancel := b.active[string(id)]; cancel != nil {
				cancel()
			}
			b.mu.Unlock()
		}
		return
	}
	if req.Method == "notifications/initialized" && len(req.ID) == 0 {
		if b.client != nil {
			b.initialized.Do(func() { b.workers.Go(func() { b.handle(parent, req) }) })
		}
		return
	}
	if len(req.ID) == 0 {
		return
	}
	if req.Method == "initialize" {
		b.handle(parent, req)
		return
	}
	ctx, cancel := context.WithCancel(parent)
	b.mu.Lock()
	_, duplicate := b.active[string(req.ID)]
	if duplicate || len(b.active) >= 64 {
		b.mu.Unlock()
		cancel()
		b.rpcError(req.ID, -32600, failure("bridge_busy", nil))
		return
	}
	b.active[string(req.ID)] = cancel
	b.mu.Unlock()
	b.workers.Go(func() {
		defer cancel()
		defer func() { b.mu.Lock(); delete(b.active, string(req.ID)); b.mu.Unlock() }()
		b.handle(ctx, req)
	})
}
