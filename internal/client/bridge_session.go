package client

import (
	"context"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
)

// Shutdown gets a short independent deadline, including after an interrupt.
func (b *bridge) closeSession(parent context.Context) {
	headers := b.headers()
	if b.client == nil || headers.Get("Mcp-Session-Id") == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	resp, err := b.client.send(ctx, http.MethodDelete, nil, headers)
	if err != nil {
		b.diagnostic(requestFailure(apicontract.Definition{}, err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		b.diagnostic(failure("bridge_close", nil))
	}
}
