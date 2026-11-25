package secretstore

import (
	"context"
	"fmt"
	"net/http"
)

const brokerPath = "/oberth/step-secret"
const BrokerPath = "/v1" + brokerPath

type BrokerRequest struct {
	Path string `json:"path"`
}
type BrokerResponse struct {
	Value []byte `json:"value"`
}

func (c *Client) fetchBroker(ctx context.Context, jwt []byte, paths []string) (values map[string][]byte, err error) {
	if err = checkPaths(paths); err != nil {
		return nil, err
	}
	values = map[string][]byte{}
	defer func() {
		if err != nil {
			for _, v := range values {
				clear(v)
			}
			values = nil
		}
	}()
	for _, path := range paths {
		var response BrokerResponse
		status, failure := c.JSON(ctx, Request{Method: "POST", Path: brokerPath, Token: jwt, Body: BrokerRequest{Path: path}, Accept: http.StatusNotFound}, &response)
		if status == http.StatusNotFound {
			return values, missingValue(path)
		}
		if failure != nil {
			if status == http.StatusBadGateway {
				failure = refuse("broker", fmt.Errorf("HTTP %d", status))
			}
			return values, failure
		}
		if response.Value == nil {
			return values, refuse("response", nil)
		}
		if len(response.Value) > MaxValueBytes {
			clear(response.Value)
			return values, refuse("limit", nil)
		}
		values[path] = response.Value
	}
	return values, nil
}
