package secretstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const brokerPath = "/oberth/step-secret"
const BrokerPath = "/v1" + brokerPath

var errBrokerCapacity = errors.New("secret delivery capacity unavailable")

type BrokerRequest struct {
	Path string `json:"path"`
}
type BrokerResponse struct {
	Value []byte `json:"value"`
}

func (c *Client) fetchBroker(ctx context.Context, jwt []byte, paths []string) (values map[string][]byte, err error) {
	// This is transport backpressure, bounded by the step's deadline and by
	// thirty seconds even when the caller supplies no deadline.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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
		status, failure := c.brokerJSON(ctx, jwt, path, &response)
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

func (c *Client) brokerJSON(ctx context.Context, jwt []byte, path string, out *BrokerResponse) (int, error) {
	backoff := time.Second
	for {
		status, err := c.JSON(ctx, Request{Method: "POST", Path: brokerPath, Token: jwt, Body: BrokerRequest{Path: path}, Accept: http.StatusNotFound}, out)
		if !errors.Is(err, errBrokerCapacity) {
			return status, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status, refuse("broker", ctx.Err())
		case <-timer.C:
		}
		backoff = min(backoff*2, 4*time.Second)
	}
}
