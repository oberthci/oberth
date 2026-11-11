package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
)

func loadClientAuthority() (*clientcert.Authority, error) {
	cert, err := readBoundedFile(identityRoot+"/client-ca/ca.crt", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := readBoundedFile(identityRoot+"/client-ca/ca.key", 64<<10)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return clientcert.Load(cert, key)
}

func newAPIHTTPServer(handler http.Handler, served *servedCertificate, clientAuthority *clientcert.Authority) *http.Server {
	clientPool := x509.NewCertPool()
	if clientAuthority != nil {
		clientPool.AddCert(clientAuthority.Certificate)
	}
	return &http.Server{
		Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		// ReadTimeout bounds the time a client may take to send the complete
		// request (headers + body). 30 seconds is generous for the 1 MiB MCP
		// body limit. Long-poll responses (wait, promote_status, livelog) are
		// unaffected: the body is read in full before the handler's long-poll
		// begins, and the response phase is bounded by WriteTimeout instead.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 3 * time.Minute, MaxHeaderBytes: 1 << 20,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: served.getCertificate, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: clientPool},
	}
}
