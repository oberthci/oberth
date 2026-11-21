package apicontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// RPCReplyBytes includes both structured content and its escaped text copy.
func RPCReplyBytes(output int) int { return 3*output + RPCEnvelopeBytes }

func ReadReply(resp *http.Response, limit int) ([]byte, error) {
	return ReadReplyWithProgress(resp, limit, nil)
}

// ReadReplyWithProgress reports SSE keepalives without changing the final reply.
func ReadReplyWithProgress(resp *http.Response, limit int, progress func() error) ([]byte, error) {
	reader := &io.LimitedReader{R: resp.Body, N: int64(limit) + 1}
	kind, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, replyError(err)
	}
	if kind == "text/event-stream" {
		return readSSE(reader, limit, progress)
	}
	if kind != "application/json" {
		return nil, replyError(nil)
	}
	data, err := io.ReadAll(reader)
	if len(data) > limit {
		return nil, ErrReplySize
	}
	if err != nil || !json.Valid(data) {
		return nil, replyError(err)
	}
	return data, nil
}

func readSSE(reader *io.LimitedReader, limit int, progress func() error) ([]byte, error) {
	var result []byte
	err := ReadSSE(reader, limit, func(frame SSEFrame) error {
		if frame.Keepalive && progress != nil {
			if err := progress(); err != nil {
				return err
			}
		}
		result = sseResult(frame.Data)
		if result != nil {
			return io.EOF
		}
		return nil
	})
	if reader.N <= 0 {
		return nil, ErrReplySize
	}
	if result != nil {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, io.ErrUnexpectedEOF
}

func sseResult(data []byte) []byte {
	var value struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(data, &value) != nil || len(value.ID) == 0 {
		return nil
	}
	return bytes.Clone(bytes.TrimSpace(data))
}

var ErrReplySize = errors.New("server reply exceeds declared limit")
var ErrReply = errors.New("invalid server reply")

func replyError(cause error) error { return errors.Join(ErrReply, cause) }
