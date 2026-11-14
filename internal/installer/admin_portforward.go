package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	streamspdy "k8s.io/apimachinery/pkg/util/httpstream/spdy"
	"k8s.io/client-go/rest"
)

// Use the configured Kubernetes HTTP transport for the upgrade. Its request
// cancellation covers reading the response headers; the client-go SPDY transport
// reads those headers directly from a socket without observing cancellation.
type adminPortForwardDialer struct {
	ctx      context.Context
	client   *http.Client
	endpoint *url.URL
}

func newAdminPortForwardDialer(ctx context.Context, config *rest.Config, endpoint *url.URL) (*adminPortForwardDialer, error) {
	copied := rest.CopyConfig(config)
	copied.NextProtos = []string{"http/1.1"}
	configured, err := rest.HTTPClientFor(copied)
	if err != nil {
		return nil, err
	}
	client := *configured
	client.Timeout = 0 // The operation context bounds both handshake and stream.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &adminPortForwardDialer{ctx: ctx, client: &client, endpoint: endpoint}, nil
}

func (d *adminPortForwardDialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	var underlying net.Conn
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { underlying = info.Conn }}
	ctx := httptrace.WithClientTrace(d.ctx, trace)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint.String(), nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", streamspdy.HeaderSpdy31)
	for _, protocol := range protocols {
		request.Header.Add(httpstream.HeaderProtocolVersion, protocol)
	}
	response, err := d.client.Do(request) //nolint:bodyclose,gosec // G704: selected Kubernetes REST URL, redirects refused; successful body ownership transfers to the SPDY connection.
	if err != nil {
		return nil, "", err
	}
	protocol := response.Header.Get(httpstream.HeaderProtocolVersion)
	accepted := false
	for _, offered := range protocols {
		accepted = accepted || protocol == offered
	}
	if response.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(response.Header.Get("Upgrade"), streamspdy.HeaderSpdy31) || !accepted {
		_ = response.Body.Close()
		return nil, "", fmt.Errorf("port-forward upgrade refused by Kubernetes (HTTP %d)", response.StatusCode)
	}
	body, ok := response.Body.(io.ReadWriteCloser)
	if !ok || underlying == nil {
		_ = response.Body.Close()
		return nil, "", errors.New("port-forward upgrade did not provide a bidirectional connection")
	}
	connection := &adminUpgradeConnection{Conn: underlying, body: body}
	connection.stop = context.AfterFunc(d.ctx, func() { _ = body.Close() })
	stream, err := streamspdy.NewClientConnectionWithPings(connection, 5*time.Second)
	if err != nil {
		_ = connection.Close()
		return nil, "", err
	}
	return stream, protocol, nil
}

type adminUpgradeConnection struct {
	net.Conn
	body io.ReadWriteCloser
	stop func() bool
}

func (c *adminUpgradeConnection) Read(p []byte) (int, error)  { return c.body.Read(p) }
func (c *adminUpgradeConnection) Write(p []byte) (int, error) { return c.body.Write(p) }
func (c *adminUpgradeConnection) Close() error                { c.stop(); return c.body.Close() }
