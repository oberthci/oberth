package client

import "net/http"

func (b *bridge) headers() http.Header {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := http.Header{}
	if b.session != "" {
		h.Set("Mcp-Session-Id", b.session)
	}
	if b.version != "" {
		h.Set("MCP-Protocol-Version", b.version)
	}
	return h
}
