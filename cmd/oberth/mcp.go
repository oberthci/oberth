package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/mdnsdial"
)

const (
	mcpDefaultURL      = "https://watch.oberth.ci/mcp"
	mcpProtocolVersion = "2025-06-18"
	mcpRequestTimeout  = 60 * time.Second
	// mcpMaxResponseSize bounds one JSON-RPC reply. The server's inline text
	// views stop at 4 MiB, but a tool result carries that text twice (content
	// and structuredContent), each JSON-escaped, so 8 MiB rejected ordinary
	// full-size views as malformed JSON. A reply over this bound is reported
	// as oversized, never parsed as a truncated document (#1011).
	mcpMaxResponseSize     = 32 << 20
	mcpMaxErrorBody        = 4 << 10
	mcpJSONRPCVersion      = "2.0"
	mcpMethodToolsList     = "tools/list"
	mcpMethodToolsCall     = "tools/call"
	mcpContentTypeJSON     = "application/json"
	mcpAcceptStreamable    = "application/json, text/event-stream"
	mcpContentTypeSSE      = "text/event-stream"
	mcpTokenFilePermission = 0o600

	// maximumToolBytes mirrors the server-side limit for tool arguments.
	maximumToolBytes = 1 << 20
)

// mcpConfig holds resolved MCP client configuration.
type mcpConfig struct {
	URL      string
	Token    string
	CAFile   string
	Version  string
	TokenEnv bool // true when token came from the environment
}

// jsonRPCRequest is a JSON-RPC 2.0 request.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonRPCResponse is a JSON-RPC 2.0 response.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolsListResult is the result from tools/list.
type toolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

// mcpTool describes one MCP tool.
type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// toolsCallParams are the params for tools/call.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// toolsCallResult is the result from tools/call.
type toolsCallResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

// mcpContent describes one content block in a tool result.
type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func runMCP(ctx context.Context, arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return fmt.Errorf("%w: oberth mcp <tools|call> [arguments]", errUsage)
	}
	switch arguments[0] {
	case "tools":
		return runMCPTools(ctx, arguments[1:], output)
	case "call":
		return runMCPCall(ctx, arguments[1:], output)
	case "--help", "-h":
		_, err := fmt.Fprint(output, `Usage: oberth mcp <command>

Commands:
  tools     List available MCP tools
  call      Call an MCP tool

Environment:
  OBERTH_MCP_URL    MCP server URL (default: https://watch.oberth.ci/mcp)
  OBERTH_TOKEN      Bearer token for authentication

Flags:
  --url <url>          MCP server URL
  --token-file <path>  Path to a file containing the bearer token (0600)
  --ca-file <path>     PEM CA certificate for TLS verification
  --timeout <duration> Client deadline override (e.g. 90s or 11m); by default
                       at least 60s, or the tool timeout plus 30s for long polls
  --args-file <path>   Read tool arguments JSON from a file (or "-" for stdin);
                       bypasses the shell argv length ceiling for large payloads
  --output <path>      artifact_get only: stream the artifact's exact bytes to
                       this file instead of an inline text view; the size and
                       SHA-256 are verified and nothing is left on failure
`)
		return err
	default:
		return fmt.Errorf("%w: unknown mcp subcommand %q; use tools or call", errUsage, arguments[0])
	}
}

func runMCPTools(ctx context.Context, arguments []string, output io.Writer) error {
	cfg, err := parseMCPFlags("mcp tools", arguments, 0)
	if err != nil {
		return err
	}
	response, err := mcpRequest(ctx, cfg, mcpMethodToolsList, nil)
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("server error: %s (code %d)", response.Error.Message, response.Error.Code)
	}
	var result toolsListResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return fmt.Errorf("invalid tools/list result: %w", err)
	}
	for _, tool := range result.Tools {
		if tool.Description != "" {
			if _, err := fmt.Fprintf(output, "%-30s %s\n", tool.Name, tool.Description); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintln(output, tool.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func runMCPCall(ctx context.Context, arguments []string, output io.Writer) error {
	cfg, err := parseMCPFlags("mcp call", arguments, -1)
	if err != nil {
		return err
	}
	positional := cfg.positional
	if len(positional) == 0 {
		return fmt.Errorf("%w: oberth mcp call <tool> ['{\"json\":\"args\"}'] [--args-file <path>]", errUsage)
	}
	toolName := positional[0]
	var args json.RawMessage
	switch {
	case cfg.argsFile != "":
		// --args-file takes precedence over a positional JSON argument.
		if len(positional) > 1 {
			return fmt.Errorf("%w: --args-file and a positional JSON argument are mutually exclusive", errUsage)
		}
		var data []byte
		if cfg.argsFile == "-" {
			data, err = io.ReadAll(io.LimitReader(os.Stdin, maximumToolBytes+1))
		} else {
			data, err = os.ReadFile(cfg.argsFile) //nolint:gosec // G304: operator-supplied path.
		}
		if err != nil {
			return fmt.Errorf("read args file: %w", err)
		}
		if len(data) > maximumToolBytes {
			return fmt.Errorf("args file exceeds %d bytes", maximumToolBytes)
		}
		if !json.Valid(data) {
			return fmt.Errorf("args file is not valid JSON")
		}
		args = json.RawMessage(data)
	case len(positional) > 1:
		raw := positional[1]
		if !json.Valid([]byte(raw)) {
			return fmt.Errorf("tool arguments must be valid JSON: %s", raw)
		}
		args = json.RawMessage(raw)
	}
	if cfg.output != "" {
		if toolName != "artifact_get" {
			return fmt.Errorf("%w: --output is only supported for artifact_get", errUsage)
		}
		return saveMCPArtifact(ctx, cfg, args, output)
	}
	params := toolsCallParams{
		Name:      toolName,
		Arguments: args,
	}
	response, err := mcpRequest(ctx, cfg, mcpMethodToolsCall, params)
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("server error: %s (code %d)", response.Error.Message, response.Error.Code)
	}
	var result toolsCallResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		return fmt.Errorf("invalid tools/call result: %w", err)
	}
	for _, content := range result.Content {
		if content.Type == "text" && content.Text != "" {
			if _, err := fmt.Fprint(output, content.Text); err != nil {
				return err
			}
			if !strings.HasSuffix(content.Text, "\n") {
				if _, err := fmt.Fprintln(output); err != nil {
					return err
				}
			}
		}
	}
	if result.IsError {
		return errMCPToolError
	}
	return nil
}

// errMCPResponseTooLarge reports a reply past mcpMaxResponseSize instead of
// parsing its truncated prefix as malformed JSON.
var errMCPResponseTooLarge = fmt.Errorf("MCP response exceeds %d bytes; narrow it with pattern/offset/limit, "+
	"or save an artifact with artifact_get --output", mcpMaxResponseSize)

// errMCPToolError is returned when the tool reports isError. The caller
// maps this to exit code 2 (distinct from exit 1 for transport/auth errors).
var errMCPToolError = errors.New("tool returned an error")

// mcpFlagResult holds parsed flags and remaining positional arguments.
type mcpFlagResult struct {
	mcpConfig
	positional []string
	argsFile   string        // --args-file: path to a JSON file, or "-" for stdin
	output     string        // --output: artifact_get save-to-file destination
	timeout    time.Duration // --timeout: explicit client deadline; zero derives it
}

// parseMCPFlags parses the common MCP flags from arguments. wantPositional
// is the expected count of positional arguments, or -1 for any count.
func parseMCPFlags(name string, arguments []string, wantPositional int) (mcpFlagResult, error) {
	var result mcpFlagResult
	var tokenFile string
	var timeoutFlag string
	var timeoutSet bool
	var positional []string

	for i := 0; i < len(arguments); i++ {
		arg := arguments[i]
		switch {
		case arg == "--url" && i+1 < len(arguments):
			i++
			result.URL = arguments[i]
		case strings.HasPrefix(arg, "--url="):
			result.URL = strings.TrimPrefix(arg, "--url=")
		case arg == "--token-file" && i+1 < len(arguments):
			i++
			tokenFile = arguments[i]
		case strings.HasPrefix(arg, "--token-file="):
			tokenFile = strings.TrimPrefix(arg, "--token-file=")
		case arg == "--ca-file" && i+1 < len(arguments):
			i++
			result.CAFile = arguments[i]
		case strings.HasPrefix(arg, "--ca-file="):
			result.CAFile = strings.TrimPrefix(arg, "--ca-file=")
		case arg == "--timeout" && i+1 < len(arguments):
			i++
			timeoutFlag, timeoutSet = arguments[i], true
		case strings.HasPrefix(arg, "--timeout="):
			timeoutFlag, timeoutSet = strings.TrimPrefix(arg, "--timeout="), true
		case arg == "--args-file" && i+1 < len(arguments):
			i++
			result.argsFile = arguments[i]
		case strings.HasPrefix(arg, "--args-file="):
			result.argsFile = strings.TrimPrefix(arg, "--args-file=")
		case (arg == "--output" || arg == "-o") && i+1 < len(arguments):
			i++
			result.output = arguments[i]
		case strings.HasPrefix(arg, "--output="):
			result.output = strings.TrimPrefix(arg, "--output=")
		case arg == "--help" || arg == "-h":
			return result, fmt.Errorf("%w: %s [--url <url>] [--token-file <path>] [--ca-file <path>] [--timeout <duration>]", errUsage, name)
		case strings.HasPrefix(arg, "-"):
			return result, fmt.Errorf("%w: unknown flag %q", errUsage, arg)
		default:
			positional = append(positional, arg)
		}
	}

	if wantPositional >= 0 && len(positional) != wantPositional {
		return result, fmt.Errorf("%w: %s expects %d positional arguments, got %d", errUsage, name, wantPositional, len(positional))
	}
	result.positional = positional
	if timeoutSet {
		parsed, err := time.ParseDuration(timeoutFlag)
		if err != nil || parsed <= 0 {
			return result, fmt.Errorf("%w: --timeout requires a positive duration (e.g. 90s or 11m)", errUsage)
		}
		result.timeout = parsed
	}

	// Resolve URL.
	if result.URL == "" {
		result.URL = strings.TrimSpace(os.Getenv("OBERTH_MCP_URL"))
	}
	if result.URL == "" {
		result.URL = mcpDefaultURL
	}

	// Resolve token.
	if tokenFile != "" {
		info, err := os.Stat(tokenFile) //nolint:gosec // G703: operator-supplied --token-file path.
		if err != nil {
			return result, fmt.Errorf("token file: %w", err)
		}
		perm := info.Mode().Perm()
		if perm&0o077 != 0 {
			return result, fmt.Errorf("token file %s has mode %04o; expected 0600 (no group/other access)", tokenFile, perm)
		}
		data, err := os.ReadFile(tokenFile) //nolint:gosec // G304: operator-supplied path.
		if err != nil {
			return result, fmt.Errorf("read token file: %w", err)
		}
		result.Token = strings.TrimSpace(string(data))
		if result.Token == "" {
			return result, fmt.Errorf("token file %s is empty", tokenFile)
		}
	} else {
		result.Token = strings.TrimSpace(os.Getenv("OBERTH_TOKEN"))
	}
	if result.Token == "" {
		return result, errors.New("set OBERTH_TOKEN or use --token-file to authenticate")
	}

	result.Version = version
	return result, nil
}

// mcpRequest sends a JSON-RPC request to the MCP server and returns the
// parsed response. It handles both direct JSON responses and SSE-framed
// responses.
func mcpRequest(ctx context.Context, cfg mcpFlagResult, method string, params any) (_ *jsonRPCResponse, resultErr error) {
	request := jsonRPCRequest{
		JSONRPC: mcpJSONRPCVersion,
		ID:      1,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	limit, toolTimeout, err := mcpDeadline(cfg, params)
	if err != nil {
		return nil, err
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	defer func() {
		if resultErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
			resultErr = mcpDeadlineError(limit, toolTimeout, resultErr)
		}
	}()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mcpContentTypeJSON)
	httpReq.Header.Set("Accept", mcpAcceptStreamable)
	httpReq.Header.Set("MCP-Protocol-Version", mcpProtocolVersion)
	httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	ua := "oberth-cli/" + cfg.Version
	httpReq.Header.Set("User-Agent", ua)

	httpClient, err := mcpHTTPClientFunc(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	// The request context owns the limit, including streamed response bodies.
	// Copy the factory client so an old fixed HTTP timeout cannot cut it short.
	requestClient := *httpClient
	requestClient.Timeout = 0
	resp, err := requestClient.Do(httpReq)
	if err != nil {
		return nil, classifyMCPTransport(err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// success, parse below
	case http.StatusUnauthorized:
		return nil, errors.New("authentication failed: check OBERTH_TOKEN or --token-file")
	case http.StatusForbidden:
		return nil, errors.New("access denied: the token does not have permission for this operation")
	default:
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, mcpMaxErrorBody))
		return nil, fmt.Errorf("server returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errorBody)))
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, mcpContentTypeSSE) {
		return parseMCPSSEResponse(resp.Body)
	}
	return parseMCPJSONResponse(resp.Body)
}

// parseMCPJSONResponse reads a direct JSON-RPC response.
func parseMCPJSONResponse(body io.Reader) (*jsonRPCResponse, error) {
	data, err := io.ReadAll(io.LimitReader(body, mcpMaxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(data) > mcpMaxResponseSize {
		return nil, errMCPResponseTooLarge
	}
	var response jsonRPCResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC response: %w", err)
	}
	return &response, nil
}

// parseMCPSSEResponse reads an SSE-framed JSON-RPC response. The MCP
// Streamable HTTP spec sends the response as one or more SSE events; the
// final event with event type "message" contains the JSON-RPC response.
func parseMCPSSEResponse(body io.Reader) (*jsonRPCResponse, error) {
	limited := &io.LimitedReader{R: body, N: mcpMaxResponseSize + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 0, 64<<10), mcpMaxResponseSize)
	var lastData string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			lastData = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, errMCPResponseTooLarge
		}
		return nil, fmt.Errorf("read SSE stream: %w", err)
	}
	if limited.N <= 0 {
		return nil, errMCPResponseTooLarge
	}
	if lastData == "" {
		return nil, errors.New("SSE stream contained no data events")
	}
	var response jsonRPCResponse
	if err := json.Unmarshal([]byte(lastData), &response); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC response in SSE: %w", err)
	}
	return &response, nil
}

// mcpHTTPClientFunc is the factory for the MCP HTTP client. Tests replace
// it to inject an httptest client that trusts the test server's CA.
var mcpHTTPClientFunc = mcpHTTPClient //nolint:gochecknoglobals

// mcpHTTPClient builds an HTTP client with TLS verification and optional
// custom CA.
func mcpHTTPClient(caFile string) (*http.Client, error) {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not *http.Transport")
	}
	cloned := transport.Clone()
	// A .local server name resolves through the system resolver when Go's
	// own cannot (#066).
	cloned.DialContext = mdnsdial.Dialer{Base: &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}}.DialContext
	if cloned.TLSClientConfig == nil {
		cloned.TLSClientConfig = &tls.Config{}
	}
	cloned.TLSClientConfig.MinVersion = tls.VersionTLS12

	if caFile != "" {
		pemData, err := os.ReadFile(caFile) //nolint:gosec // G304: operator-supplied CA path.
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("CA file %s contains no certificates", caFile)
		}
		cloned.TLSClientConfig.RootCAs = pool
	}

	return &http.Client{
		Transport: cloned,
	}, nil
}

// classifyMCPTransport maps transport errors to readable messages.
func classifyMCPTransport(err error) error {
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return fmt.Errorf("TLS: server certificate does not match the requested host %q", hostname.Host)
	}
	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return errors.New("TLS: server certificate is signed by an unknown authority; use --ca-file for a self-signed endpoint")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if mdnsdial.IsLocalName(dnsErr.Name) {
			return fmt.Errorf("DNS: cannot resolve %q (%s)", dnsErr.Name, mdnsdial.Hint)
		}
		return fmt.Errorf("DNS: cannot resolve %q", dnsErr.Name)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("request timed out: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("connection failed: %w", err)
}
