package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/oberthci/oberth/internal/client"
)

// mcpArtifactSaved is what `mcp call artifact_get --output` prints on success.
type mcpArtifactSaved struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	Verified bool   `json:"verified"`
}

// saveMCPArtifact is the MCP command's save-to-file path for artifact_get.
// The inline tool result is a bounded, filterable text view and cannot carry
// a binary exactly, so the bytes come from the same server's raw artifact
// route, authenticated with the same token and CA, and are verified against
// the declared size and digest before the file is renamed into place (#1011).
func saveMCPArtifact(ctx context.Context, cfg mcpFlagResult, args json.RawMessage, output io.Writer) error {
	var arguments struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Pattern string `json:"pattern"`
		Context int    `json:"context"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
		Tail    bool   `json:"tail"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return fmt.Errorf("%w: artifact_get arguments: %w", errUsage, err)
		}
	}
	if strings.TrimSpace(arguments.ID) == "" || strings.TrimSpace(arguments.Name) == "" {
		return fmt.Errorf("%w: artifact_get --output needs id and name", errUsage)
	}
	if strings.Trim(arguments.ID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
		return fmt.Errorf("%w: artifact_get --output needs an exact run ID", errUsage)
	}
	if arguments.Pattern != "" || arguments.Context != 0 || arguments.Offset != 0 || arguments.Limit != 0 || arguments.Tail {
		return fmt.Errorf("%w: --output saves the whole artifact; drop pattern, context, offset, limit and tail", errUsage)
	}
	target, err := mcpArtifactURL(cfg.URL, arguments.ID, arguments.Name)
	if err != nil {
		return err
	}
	httpClient, err := mcpHTTPClientFunc(cfg.CAFile)
	if err != nil {
		return err
	}
	// The tool-call timeout bounds a JSON reply; a large download is bounded
	// by the server's per-chunk write deadline and the caller's context.
	download := *httpClient
	download.Timeout = 0
	var receipt client.ArtifactReceipt
	err = saveArtifactFile(cfg.output, func(destination io.Writer) error {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if requestErr != nil {
			return fmt.Errorf("build request: %w", requestErr)
		}
		request.Header.Set("Authorization", "Bearer "+cfg.Token)
		request.Header.Set("User-Agent", "oberth-cli/"+cfg.Version)
		response, doErr := download.Do(request)
		if doErr != nil {
			return classifyMCPTransport(doErr)
		}
		defer func() { _ = response.Body.Close() }()
		switch response.StatusCode {
		case http.StatusOK:
		case http.StatusUnauthorized:
			return fmt.Errorf("authentication failed: check OBERTH_TOKEN or --token-file")
		case http.StatusForbidden:
			return fmt.Errorf("access denied: the token does not have permission for this artifact")
		default:
			errorBody, _ := io.ReadAll(io.LimitReader(response.Body, mcpMaxErrorBody))
			return fmt.Errorf("server returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(errorBody)))
		}
		var receiveErr error
		receipt, receiveErr = client.ReceiveArtifact(response, destination)
		if receiveErr != nil {
			return receiveErr
		}
		if !receipt.Verified {
			return errUnverifiedArtifact
		}
		return nil
	})
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(mcpArtifactSaved{
		ID: arguments.ID, Name: arguments.Name, Path: cfg.output,
		Bytes: receipt.Bytes, SHA256: receipt.SHA256, Verified: receipt.Verified,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "%s\n", encoded)
	return err
}

// mcpArtifactURL derives the raw artifact route from the MCP endpoint: the
// same origin and path prefix, with the trailing /mcp replaced.
func mcpArtifactURL(endpoint, id, name string) (string, error) {
	base, err := url.Parse(endpoint)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return "", fmt.Errorf("invalid MCP URL %q", endpoint)
	}
	prefix := strings.TrimSuffix(strings.TrimRight(base.Path, "/"), "/mcp")
	base.Path = prefix + "/api/runs/" + id + "/artifacts/" + strings.TrimLeft(name, "/")
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}
