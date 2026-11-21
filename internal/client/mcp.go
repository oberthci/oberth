package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/restartretry"
)

// Config injects time, waiting and an optional HTTP transport for deterministic tests.
type Config struct {
	Diagnostic io.Writer
	Options    Options
	Transport  http.RoundTripper
	Now        func() time.Time
	Wait       func(context.Context, time.Duration) error
}

type Client struct {
	diagnostic          io.Writer
	http                *http.Client
	endpoint, tokenFile string
	now                 func() time.Time
	wait                func(context.Context, time.Duration) error
	certificate         string
	identityMu          sync.Mutex
	identityDigest      [32]byte
}

func New(cfg Config) (*Client, error) {
	if err := validateAddress(cfg.Options.URL); err != nil {
		return nil, err
	}
	rt := cfg.Transport
	if rt == nil {
		var err error
		rt, err = transport(cfg.Options)
		if err != nil {
			return nil, err
		}
	}
	h := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return failure("redirect", nil) }}
	u, _ := apicontract.ParseServerAddress(cfg.Options.URL)
	endpoint := u.String()
	certificate := ""
	if native, ok := rt.(*http.Transport); ok && native.TLSClientConfig != nil && native.TLSClientConfig.GetClientCertificate != nil {
		certificate = cfg.Options.ClientCert
	}
	if cfg.Diagnostic == nil {
		cfg.Diagnostic = io.Discard
	}
	return &Client{diagnostic: &diagnosticWriter{out: cfg.Diagnostic}, http: h, endpoint: endpoint, tokenFile: cfg.Options.TokenFile, now: cfg.Time, wait: cfg.WaitFor, certificate: certificate}, nil
}

// Close releases idle connections after a command.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Call returns the original tool payload and whether the server refused it.
// A safe read can reconnect only within this call's original deadline and outage window.
func (c *Client) Call(ctx context.Context, d apicontract.Definition, input json.RawMessage) (json.RawMessage, bool, error) {
	data, refused, err := c.call(ctx, d, input, rpcExchange{request: rpcRequest{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: "tools/call"}})
	if err != nil || refused {
		return data, refused, err
	}
	value, refused, _, err := apicontract.ToolPayload(data)
	return value, refused, err
}

func (c *Client) call(ctx context.Context, d apicontract.Definition, input json.RawMessage, wire rpcExchange) (json.RawMessage, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout(d, input))
	defer cancel()
	deadline, _ := ctx.Deadline()
	var budget *restartretry.Budget
	var feedback readFeedback
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, feedback.beforeSendFailure(d, err)
		}
		input = remainingInput(d, input, deadline.Sub(c.now()))
		data, refused, retry, err := c.attempt(ctx, d, input, wire)
		if budget != nil && err != nil {
			retry = restartretry.Transient(err)
		}
		if !retry || d.Mutation {
			return data, refused, feedback.failure(d, err)
		}
		feedback.lost = feedback.lost || (err != nil && retryRequest(err))
		if budget == nil {
			feedback.since = c.now()
			b := restartretry.New(c.now(), deadline)
			budget = &b
		}
		delay, ok := budget.Next(c.now())
		if !ok {
			return data, refused, feedback.failure(d, err)
		}
		if err = c.retryWait(ctx, delay, &feedback); err != nil {
			return nil, false, feedback.failure(d, err)
		}
	}
}

func callTimeout(d apicontract.Definition, input json.RawMessage) time.Duration {
	seconds := 0
	if d.InputType != nil {
		_, seconds = apicontract.TimeoutSeconds(d, input)
	}
	return max(time.Minute, d.Deadline+30*time.Second, time.Duration(seconds)*time.Second+30*time.Second)
}

func remainingInput(d apicontract.Definition, input json.RawMessage, left time.Duration) json.RawMessage {
	if !d.LongPoll {
		return input
	}
	name, requested := apicontract.TimeoutSeconds(d, input)
	if name == "" {
		return input
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil || args == nil {
		return input
	}
	seconds := max(1, int((left-30*time.Second)/time.Second))
	if requested > 0 {
		seconds = min(seconds, requested)
	}
	args[name], _ = json.Marshal(seconds)
	data, _ := json.Marshal(args)
	return data
}

func (c *Client) attempt(ctx context.Context, d apicontract.Definition, input json.RawMessage, wire rpcExchange) (json.RawMessage, bool, bool, error) {
	body, err := wire.toolRequest(d.Name, input)
	if err != nil {
		return nil, false, false, failure("reply", err)
	}
	if len(body) > apicontract.RequestBytes {
		return nil, false, false, failure("request_size", nil)
	}
	resp, err := c.send(ctx, http.MethodPost, body, wire.headers)
	if err != nil {
		retry, classified := exchangeFailure(d, resp, err)
		return nil, false, retry, classified
	}
	defer resp.Body.Close()
	data, err := apicontract.ReadReplyWithProgress(resp, apicontract.RPCReplyBytes(d.OutputBytes), wire.progress)
	if err != nil {
		return nil, false, restartretry.Transient(err), replyFailure(d, err)
	}
	if resp.StatusCode != http.StatusOK {
		return data, true, restartretry.RetryableStatus(resp.StatusCode), nil
	}

	_, _, retry, err := apicontract.ToolPayloadForID(data, wire.request.ID)
	if err != nil {
		return nil, false, false, replyFailure(d, err)
	}
	return data, false, retry, nil
}
