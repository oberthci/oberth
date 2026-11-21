package client

import (
	"context"
	"crypto/rand"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/restartretry"
)

func (b *bridge) followEvents(ctx context.Context) {
	if b.headers().Get("Mcp-Session-Id") == "" {
		return
	}
	cursor := ""
	budget := restartretry.Stream()
	for ctx.Err() == nil {
		err := b.eventStream(ctx, &cursor, &budget)
		if !restartretry.Transient(err) {
			if ctx.Err() == nil {
				b.diagnostic(err)
			}
			return
		}
		delay, ok := budget.Next(b.client.now())
		if !ok {
			return
		}
		jitter := b.config.Jitter
		if jitter == nil {
			jitter = streamJitter
		}
		delay = min(30*time.Second, delay+time.Duration(float64(delay)*max(0, min(1, jitter()))/4))
		if b.client.wait(ctx, delay) != nil {
			return
		}
	}
}

func streamJitter() float64 {
	var value [1]byte
	_, _ = rand.Read(value[:])
	return float64(value[0]) / 256
}

func (b *bridge) eventStream(ctx context.Context, cursor *string, budget *restartretry.Budget) error {
	h := b.headers()
	h.Set("Accept", "text/event-stream")
	if *cursor != "" {
		h.Set("Last-Event-ID", *cursor)
	}
	resp, err := b.client.send(ctx, http.MethodGet, nil, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests || restartretry.RetryableStatus(resp.StatusCode) {
			return io.ErrUnexpectedEOF
		}
		return failure("bridge_stream", nil)
	}
	kind, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || kind != "text/event-stream" {
		return failure("reply", err)
	}
	err = apicontract.ReadSSE(resp.Body, apicontract.RPCReplyBytes(apicontract.ResponseBytes), func(frame apicontract.SSEFrame) error {
		if len(frame.Data) == 0 {
			return nil
		}
		if err := b.emit(ctx, frame.Data); err != nil {
			return err
		}
		if frame.ID != "" {
			*cursor = frame.ID
		}
		*budget = restartretry.Stream()
		return nil
	})
	if err == nil {
		return io.EOF
	}
	return err
}
