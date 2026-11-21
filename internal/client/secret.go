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
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

// PutSecret sends private bytes once through the pinned, authenticated route.
func (c *Client) PutSecret(ctx context.Context, args apicontract.SecretPutArgs, value []byte) (apicontract.Receipt, error) {
	var receipt apicontract.Receipt
	if len(value) > model.MaxSecretValueBytes {
		return receipt, publicerr.New(publicerr.InvalidInput, "Secret value exceeds 1 MiB.", "Supply a smaller value through standard input or a private file.", nil)
	}
	if value == nil {
		value = []byte{}
	}
	body, err := json.Marshal(apicontract.SecretPutBody{SecretPutArgs: args, Value: value})
	if err != nil {
		return receipt, failure("reply", err)
	}
	defer clear(body)
	d, ok := toolDefinition("secret_put")
	if !ok {
		return receipt, failure("reply", nil)
	}
	address := strings.TrimSuffix(c.endpoint, "/mcp") + "/api/secrets/put"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return receipt, failure("url", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.authenticatedRequest(req)
	if err != nil {
		_, classified := exchangeFailure(d, response, err)
		return receipt, classified
	}
	defer response.Body.Close()
	return decodeSecretReceipt(response, args.Secret, d)
}

func decodeSecretReceipt(response *http.Response, path string, d apicontract.Definition) (apicontract.Receipt, error) {
	var receipt apicontract.Receipt
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return receipt, replyFailure(d, nil)
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, apicontract.ResponseBytes+1))
	if err != nil || len(result) > apicontract.ResponseBytes {
		return receipt, replyFailure(d, err)
	}
	if response.StatusCode != http.StatusOK {
		if problem, ok := resultRefusal(result); ok {
			return receipt, problem
		}
		return receipt, replyFailure(d, nil)
	}
	if err = json.Unmarshal(result, &receipt); err != nil || receipt.ID != path || receipt.State != "stored" {
		return receipt, replyFailure(d, err)
	}
	return receipt, nil
}
