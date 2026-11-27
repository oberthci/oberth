package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
)

func transport(o Options) (*http.Transport, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		data, err := readFile(o.CAFile, false)
		if err != nil {
			return nil, err
		}
		config.RootCAs = x509.NewCertPool()
		if !config.RootCAs.AppendCertsFromPEM(data) {
			return nil, fileFailure(o.CAFile, "trust", nil)
		}
	}
	if (o.ClientCert == "") != (o.ClientKey == "") {
		return nil, fileFailure(o.ClientCert+" "+o.ClientKey, "pair", nil)
	}
	if o.ClientCert != "" {
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := readFile(o.ClientCert, false)
			if err != nil {
				return nil, err
			}
			key, err := ReadPrivate(o.ClientKey)
			if err != nil {
				return nil, err
			}
			pair, err := tls.X509KeyPair(cert, key)
			if err != nil {
				return nil, fileFailure(o.ClientCert+" "+o.ClientKey, "pair", err)
			}
			return &pair, nil
		}
	}
	return &http.Transport{TLSClientConfig: config, DialContext: DialContext, TLSHandshakeTimeout: 5 * time.Second}, nil
}

func (c *Client) request(ctx context.Context, body []byte) (*http.Response, error) {
	var request apicontract.MCPRequest
	if json.Unmarshal(body, &request) != nil {
		return nil, failure("reply", nil)
	}
	var err error
	request.Params, err = apicontract.WithClientMetadata(request.Params)
	if err != nil {
		return nil, failure("reply", err)
	}
	body, err = json.Marshal(request)
	if err != nil {
		return nil, failure("reply", err)
	}
	return c.send(ctx, http.MethodPost, body, nil)
}

func (c *Client) send(ctx context.Context, method string, body []byte, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, failure("url", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodPost {
		var wire apicontract.MCPRequest
		if json.Unmarshal(body, &wire) != nil {
			return nil, failure("reply", nil)
		}
		for name, values := range wire.Headers() {
			req.Header[name] = values
		}
	}
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	return c.authenticatedRequest(req)
}

func (c *Client) authenticatedRequest(req *http.Request) (*http.Response, error) {
	if err := c.refreshIdentity(); err != nil {
		return nil, err
	}
	if c.tokenFile != "" {
		token, readErr := ReadPrivate(c.tokenFile)
		if readErr != nil {
			return nil, readErr
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	if err := req.Context().Err(); err != nil {
		return nil, failure(failureClass(err), err)
	}
	return c.http.Do(req)
}

// Refresh a changed client identity without discarding connections on ordinary reads.
func (c *Client) refreshIdentity() error {
	if c.certificate == "" {
		return nil
	}
	data, err := ReadPublic(c.certificate)
	if err != nil {
		return err
	}
	leaf, err := clientcert.ParseCertificate(data)
	if err != nil {
		return fileFailure(c.certificate, "pair", err)
	}
	if err := certificateTime(leaf, c.now()); err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	c.identityMu.Lock()
	defer c.identityMu.Unlock()
	if digest != c.identityDigest {
		c.http.CloseIdleConnections()
		c.identityDigest = digest
	}
	return nil
}
