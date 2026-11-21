package sourceplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/strictjson"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	artifact "github.com/argoproj/argo-workflows/v4/pkg/apiclient/artifact"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func (c *Client) Load(ctx context.Context, a *artifact.Artifact, path string) error {
	r, err := input(a)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return failure(reasonFile)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return failure(reasonInput)
	}
	response, err := c.response(ctx, string(b))
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if err = responseFailure(response); err != nil {
		return err
	}
	if response.Header.Get("Content-Type") != sourcedelivery.LoadContentType || response.Header.Get("Content-Encoding") != "" {
		return failure(reasonReceipt)
	}
	return receive(ctx, response, r, path)
}
func (c *Client) response(ctx context.Context, body string) (*http.Response, error) {
	ceiling := time.Second
	nextLog := c.now().Add(30 * time.Second)
	for {
		token, err := c.bearer()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(body))
		if err != nil {
			return nil, failure(reasonTransfer)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		response, err := c.http.Do(req)
		if err != nil && (response != nil || !retryConnection(err)) {
			return nil, failure(reasonTransfer)
		}
		if err == nil && response.StatusCode != 503 {
			return response, nil
		}
		delay := c.jitter(ceiling)
		if response != nil {
			delay += retryFloor(response.Header.Get("Retry-After"), c.now())
			_ = response.Body.Close()
		}
		ceiling = min(ceiling*2, 2*time.Second)
		if err = c.retryWait(ctx, delay, &nextLog); err != nil {
			return nil, failure(reasonTransfer)
		}
	}
}
func retryFloor(value string, now time.Time) time.Duration {
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Duration(min(max(n, 0), 300)) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(max(date.Sub(now), 0), 5*time.Minute)
	}
	return 0
}
func receive(ctx context.Context, response *http.Response, r sourcedelivery.LoadRequest, path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".oberth-source-")
	if err != nil {
		return failure(reasonFile)
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	hash := sha256.New()
	// Main's framing cap also bounds hostile or interrupted HTTPS responses.
	limit := sourcedelivery.MaxMainArchiveBytes
	if r.Descriptor.Kind == "pin" {
		limit = sourcedelivery.MaxPinArchiveBytes
	}
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(response.Body, limit+1))
	if err != nil || n > limit {
		return failure(reasonTransfer)
	}
	values := response.Trailer.Values(sourcedelivery.ReceiptTrailer)
	if len(values) != 1 || sourcedelivery.VerifyReceipt(values[0], r, hex.EncodeToString(hash.Sum(nil))) != nil {
		return failure(reasonReceipt)
	}
	if ctx.Err() != nil {
		return failure(reasonTransfer)
	}
	if f.Sync() != nil || f.Close() != nil {
		return failure(reasonFile)
	}
	if os.Rename(f.Name(), path) != nil {
		return failure(reasonFile)
	}
	return nil
}

func responseStatus(statusCode int) error {
	switch statusCode {
	case 200:
		return nil
	case 401, 403:
		return failure(reasonDenied)
	case 400:
		return failure(reasonInput)
	case 501:
		return failure(reasonDisabled)
	}
	return failure(reasonTransfer)
}

func responseFailure(response *http.Response) error {
	if response.StatusCode == 200 {
		return nil
	}
	if response.StatusCode == 410 || response.StatusCode == 413 {
		var wire sourcedelivery.ErrorResponse
		data, err := io.ReadAll(io.LimitReader(response.Body, sourcedelivery.MaxLoadRequestBytes+1))
		if err != nil || strictjson.DecodeStrictObject(data, &wire, sourcedelivery.MaxLoadRequestBytes) != nil {
			return failure(reasonReceipt)
		}
		if limit := loadLimit(response.StatusCode, wire); limit != nil {
			return limit
		}
		if wire.Code == "gone" && response.StatusCode == 410 {
			return &loadRefusal{code: wire.Code, tagMoved: wire.Message == "tag moved since admission"}
		}
		return failure(reasonReceipt)
	}
	return responseStatus(response.StatusCode)
}

type loadRefusal struct {
	code     string
	unit     string
	limit    int64
	tagMoved bool
}

func (e *loadRefusal) Error() string {
	if e.code == "too_large" {
		failure := &sourcedelivery.Failure{Code: "too_large"}
		if e.unit == "files" {
			return fmt.Sprintf("%s (%d files). %s", failure.Error(), e.limit, remedyTooManyFiles)
		}
		return fmt.Sprintf("%s (%g MiB). %s", failure.Error(), float64(e.limit)/(1<<20), failure.Remedy())
	}
	failure := &sourcedelivery.Failure{Code: "gone"}
	if e.tagMoved {
		return reasonTagMoved + ". " + failure.Remedy()
	}
	return failure.Error() + ". " + failure.Remedy()
}

func loadLimit(status int, wire sourcedelivery.ErrorResponse) *loadRefusal {
	if wire.Code == "too_large" && status == 413 && wire.Limit > 0 && (wire.Unit == "" || wire.Unit == "files") {
		return &loadRefusal{code: wire.Code, limit: wire.Limit, unit: wire.Unit}
	}
	return nil
}
