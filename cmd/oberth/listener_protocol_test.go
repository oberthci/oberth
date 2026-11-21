package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestBothListenersNegotiateHTTP2AndHTTP1(t *testing.T) {
	clock := fake.NewClock()
	cert, key := authority(t, clock.Now())
	identity, err := newServerIdentity(cert, key, []string{"oberth.local"}, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	c := &composition{health: &health{}, diagnostic: io.Discard}
	c.config.Listen.HTTPS = "127.0.0.1:0"
	c.config.Listen.Source = "127.0.0.1:0"
	c.config.Listen.Health = "127.0.0.1:0"
	lc := &net.ListenConfig{}
	s := newServing(c, newLifecycle(t.Context(), nil), serverDependencies{listen: lc.Listen})
	for _, server := range s.servers {
		if server.ReadHeaderTimeout == 0 || server.ReadTimeout == 0 || server.IdleTimeout == 0 || server.WriteTimeout == 0 {
			t.Fatal("unbounded listener")
		}
		server.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	}
	if err = s.listen(t.Context(), identity); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, server := range s.servers {
			_ = server.Close()
		}
		s.workers.Wait()
		s.cancelRequests()
	})
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	for _, h2 := range []bool{false, true} {
		tr := &http.Transport{ForceAttemptHTTP2: h2, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "oberth.local", Time: clock.Now}}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: time.Second * 10}
		want := 1
		if h2 {
			want = 2
		}
		for _, listener := range s.listeners[:2] {
			checkListenerProtocol(t, client, listener.Addr().String(), want)
		}
	}
}

func checkListenerProtocol(t *testing.T, client *http.Client, address string, want int) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "https://"+address+"/", nil)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 204 || response.ProtoMajor != want {
		t.Fatal(response.Status, response.Proto)
	}
}
