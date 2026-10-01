package vmnodebridge

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	remoteprotocol "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/client-go/rest"
	remoteexec "k8s.io/client-go/tools/remotecommand"
)

var errRuntimeStream = errors.New("vmnodebridge: protected runtime stream failed")

const maxInputBytes = 40 << 10
const maxOutputBytes = 132 << 10
const maxStderrBytes = 4 << 10

// StreamTLS is pinned to the operator-configured CRI streaming origin. CRI's
// one-use URL token is never a credential for the Oberth-facing endpoint.
type StreamTLS struct {
	Origin     string
	ServerName string
	RootCAs    []byte
}

func (config StreamTLS) validateURL(raw string) error {
	origin, err := url.Parse(config.Origin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || config.ServerName == "" {
		return errRuntimeStream
	}
	if pool := x509.NewCertPool(); len(config.RootCAs) == 0 || !pool.AppendCertsFromPEM(config.RootCAs) {
		return errRuntimeStream
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return errRuntimeStream
	}
	token := strings.TrimPrefix(u.EscapedPath(), "/attach/")
	if u.Scheme != "https" || u.Host != origin.Host || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
		!strings.HasPrefix(u.EscapedPath(), "/attach/") || strings.Count(u.EscapedPath(), "/") != 2 || token == "" || strings.Contains(token, "%") {
		return errRuntimeStream
	}
	return nil
}

// RuntimeStreamer exposes only the fixed stdin/stdout/stderr attach operation.
// It has no exec, logs, URL forwarding, reconnect or port-forward method.
type RuntimeStreamer interface {
	Stream(context.Context, string, io.Reader, io.Writer, io.Writer) error
}

type WebSocketStreamer struct{ TLS StreamTLS }

func (streamer WebSocketStreamer) Stream(ctx context.Context, raw string, stdin io.Reader, stdout, stderr io.Writer) error {
	if streamer.TLS.validateURL(raw) != nil || stdin == nil || stdout == nil || stderr == nil {
		return errRuntimeStream
	}
	config := &rest.Config{Host: streamer.TLS.Origin, TLSClientConfig: rest.TLSClientConfig{
		CAData: streamer.TLS.RootCAs, ServerName: streamer.TLS.ServerName},
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	executor, err := remoteexec.NewWebSocketExecutorForProtocols(config, http.MethodGet, raw, remoteprotocol.StreamProtocolV4Name)
	if err != nil {
		return errRuntimeStream
	}
	if err := executor.StreamWithContext(ctx, remoteexec.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Tty: false}); err != nil {
		return errRuntimeStream
	}
	return nil
}

type boundedReader struct {
	source io.Reader
	left   int64
	cancel context.CancelFunc
}

func (reader *boundedReader) Read(body []byte) (int, error) {
	if reader.left <= 0 {
		var extra [1]byte
		if n, err := reader.source.Read(extra[:]); n == 0 && err == io.EOF {
			return 0, io.EOF
		}
		reader.cancel()
		return 0, errRuntimeStream
	}
	if int64(len(body)) > reader.left {
		body = body[:reader.left]
	}
	n, err := reader.source.Read(body)
	reader.left -= int64(n)
	if err != nil && err != io.EOF {
		reader.cancel()
		return n, errRuntimeStream
	}
	return n, err
}

type boundedWriter struct {
	target io.Writer
	left   int64
	cancel context.CancelFunc
}

func (writer *boundedWriter) Write(body []byte) (int, error) {
	if int64(len(body)) > writer.left {
		writer.cancel()
		return 0, errRuntimeStream
	}
	n, err := writer.target.Write(body)
	writer.left -= int64(n)
	if err != nil || n != len(body) {
		writer.cancel()
		return n, errRuntimeStream
	}
	return n, nil
}
