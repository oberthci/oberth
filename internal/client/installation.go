package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
)

// InstallationState uses the administrator HTTP contract, independently of MCP.
func (c *Client) InstallationState(ctx context.Context) (apicontract.SystemStatus, error) {
	var status apicontract.SystemStatus
	err := c.installationCall(ctx, "admin_version", apicontract.AdminVersionArgs{}, &status)
	return status, err
}

// RecordInstallation sends one completion receipt to the server left ready by
// maintenance. A lost reply is uncertain and is never replayed.
func (c *Client) RecordInstallation(ctx context.Context, args apicontract.AdminAuditRecordArgs) (apicontract.Receipt, error) {
	var receipt apicontract.Receipt
	err := c.installationCall(ctx, "admin_audit_record", args, &receipt)
	return receipt, err
}

func (c *Client) installationCall(ctx context.Context, name string, input, out any) error {
	d, ok := toolDefinition(name)
	if !ok || d.AdminAction == "" {
		return failure("reply", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout(d, nil))
	defer cancel()
	body, err := json.Marshal(input)
	if err != nil || len(body) > d.InputBytes {
		return failure("request_size", err)
	}
	address := strings.TrimSuffix(c.endpoint, "/mcp") + "/api/admin/" + d.AdminAction
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return failure("url", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.authenticatedRequest(req)
	if err != nil {
		_, classified := exchangeFailure(d, resp, err)
		return classified
	}
	defer resp.Body.Close()
	return installationReply(resp, d, out)
}

func installationReply(resp *http.Response, d apicontract.Definition, out any) error {
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return replyFailure(d, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(d.OutputBytes)+1))
	if err != nil || len(data) > d.OutputBytes {
		return replyFailure(d, err)
	}
	if resp.StatusCode != http.StatusOK {
		if refusal, ok := resultRefusal(data); ok {
			return refusal
		}
		return replyFailure(d, nil)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return replyFailure(d, err)
	}
	return nil
}
