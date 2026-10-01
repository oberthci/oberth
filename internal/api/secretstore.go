package api

// SecretStoreVerifyRequest selects checks, never connection settings or credentials.
type SecretStoreVerifyRequest struct {
	Paths       []string `json:"paths,omitempty"`
	ReleaseTier bool     `json:"release_tier,omitempty"`
	Repo        string   `json:"repo,omitempty"`
	Tier        string   `json:"tier,omitempty"`
	Keys        bool     `json:"keys,omitempty"`
	Expect      []string `json:"expect,omitempty"`
	Timeout     int      `json:"timeout,omitempty"`
}

// SecretStoreVerifyResponse contains only the verifier's bounded, value-free
// diagnostic output. A failed verification is an MCP tool error, not success.
type SecretStoreVerifyResponse struct {
	Verified bool   `json:"verified"`
	Output   string `json:"output"`
}

func (response SecretStoreVerifyResponse) MCPToolText() string  { return response.Output }
func (response SecretStoreVerifyResponse) MCPToolIsError() bool { return !response.Verified }
