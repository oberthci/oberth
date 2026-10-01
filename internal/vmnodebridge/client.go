package vmnodebridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

var errNodeChannel = errors.New("vmnodebridge: protected node channel failed")

func ClientTLSConfig(cert tls.Certificate, serverCA []byte, serverName string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if len(cert.Certificate) == 0 || serverName == "" || !pool.AppendCertsFromPEM(serverCA) {
		return nil, errNodeChannel
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		RootCAs: pool, ServerName: serverName, NextProtos: []string{"h2"}}, nil
}

type Client struct {
	endpoint string
	http     *http.Client
	node     string
}

func NewClient(endpoint, node string, tlsConfig *tls.Config) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
		node == "" || tlsConfig == nil || tlsConfig.MinVersion < tls.VersionTLS13 || tlsConfig.InsecureSkipVerify ||
		tlsConfig.ServerName != node || tlsConfig.RootCAs == nil || len(tlsConfig.Certificates) != 1 {
		return nil, errNodeChannel
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig.Clone(), ForceAttemptHTTP2: true, Proxy: nil}
	return &Client{endpoint: endpoint, node: node, http: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errNodeChannel }}}, nil
}

type joinedBody struct {
	io.Reader
	close io.Closer
}

func (body joinedBody) Close() error { return body.close.Close() }

type trailerBody struct{ response *http.Response }

func (body trailerBody) Read(buf []byte) (int, error) {
	n, err := body.response.Body.Read(buf)
	if err == io.EOF && body.response.Trailer.Get("X-Oberth-Attach-Result") != "ok" {
		return n, errNodeChannel
	}
	return n, err
}

func (body trailerBody) Close() error { return body.response.Body.Close() }

type Stream struct {
	Output           io.ReadCloser
	Input            io.WriteCloser
	RuntimeStartedAt time.Time
	cancel           context.CancelFunc
}

func (stream *Stream) Close() error {
	if stream == nil {
		return nil
	}
	stream.cancel()
	return errors.Join(stream.Input.Close(), stream.Output.Close())
}

// Open establishes only the protected channel and CRI stream. The operator
// must have durably claimed AttachRequest.Claim before calling it, then must
// complete ProveConductorStream and BindConductorAttach before fixture delivery.
func (client *Client) Open(ctx context.Context, request AttachRequest) (*Stream, error) {
	if client == nil || client.http == nil || request.validate(client.node, time.Now()) != nil {
		return nil, errNodeChannel
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) >= maxOpenFrame-1 {
		return nil, errNodeChannel
	}
	body = append(body, '\n')
	reader, writer := io.Pipe()
	streamCtx, cancel := context.WithDeadline(ctx, request.CreatedAt.Add(request.Plan.Spec.Deadline))
	httpRequest, err := http.NewRequestWithContext(streamCtx, http.MethodPost, client.endpoint+AttachPath, joinedBody{
		Reader: io.MultiReader(bytes.NewReader(body), reader), close: reader})
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, errNodeChannel
	}
	httpRequest.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.http.Do(httpRequest)
	if err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, errNodeChannel
	}
	if response.StatusCode != http.StatusOK || response.ProtoMajor != 2 || response.Header.Get("Content-Type") != "application/octet-stream" {
		cancel()
		_ = response.Body.Close()
		_ = reader.Close()
		_ = writer.Close()
		return nil, errNodeChannel
	}
	startNanos, err := strconv.ParseInt(response.Header.Get("X-Oberth-Runtime-Started-At"), 10, 64)
	if err != nil || startNanos <= 0 {
		cancel()
		_ = response.Body.Close()
		_ = reader.Close()
		_ = writer.Close()
		return nil, errNodeChannel
	}
	started := time.Unix(0, startNanos).UTC()
	bound := request.Claim
	bound.RuntimeStartedAt = started
	bound.BoundAt = time.Now()
	if vmrunner.ValidateConductorAttachClaim(request.Plan, request.Attempt, bound) != nil ||
		started.Before(request.CreatedAt) || !started.Before(request.CreatedAt.Add(request.Plan.Spec.Deadline)) {
		cancel()
		_ = response.Body.Close()
		_ = reader.Close()
		_ = writer.Close()
		return nil, errNodeChannel
	}
	return &Stream{Output: trailerBody{response}, Input: writer, RuntimeStartedAt: started, cancel: cancel}, nil
}
