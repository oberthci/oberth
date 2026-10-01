package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/oberthci/oberth/internal/vmnodebridge"
)

type options struct {
	listen, nodeName, criSocket, streamOrigin, streamServerName string
	serverCert, serverKey, clientCA, streamCA                   string
	qualification, qualificationSHA                             string
}

func parseOptions() options {
	var value options
	flag.StringVar(&value.listen, "listen", "", "specific node IP and TCP port")
	flag.StringVar(&value.nodeName, "node-name", "", "exact Kubernetes node name")
	flag.StringVar(&value.criSocket, "cri-socket", "", "operator-approved Unix CRI socket")
	flag.StringVar(&value.streamOrigin, "stream-origin", "", "pinned local HTTPS CRI streaming origin")
	flag.StringVar(&value.streamServerName, "stream-server-name", "", "pinned CRI streaming TLS name")
	flag.StringVar(&value.serverCert, "server-cert", "", "protected node TLS certificate file")
	flag.StringVar(&value.serverKey, "server-key", "", "protected node TLS private key file")
	flag.StringVar(&value.clientCA, "client-ca", "", "dedicated Oberth client CA file")
	flag.StringVar(&value.streamCA, "stream-ca", "", "CRI streaming CA file")
	flag.StringVar(&value.qualification, "qualification", "", "operator-installed runtime qualification receipt")
	flag.StringVar(&value.qualificationSHA, "qualification-sha256", "", "exact approved receipt SHA-256")
	flag.Parse()
	return value
}

func protectedFile(path string, max int64, private bool) ([]byte, error) {
	if path == "" || path[0] != '/' {
		return nil, errors.New("node bridge requires absolute file paths")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > max {
		return nil, errors.New("node bridge file shape rejected")
	}
	if private {
		owner, ok := info.Sys().(*syscall.Stat_t)
		uid := os.Geteuid()
		if !ok || uid < 0 || uint64(owner.Uid) != uint64(uid) || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("node bridge private file custody rejected")
		}
	}
	body, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(body)) != info.Size() {
		return nil, errors.New("node bridge file read rejected")
	}
	return body, nil
}

func run(ctx context.Context, value options) error {
	address, err := netip.ParseAddrPort(value.listen)
	if err != nil || address.Addr().IsUnspecified() || address.Addr().IsMulticast() || address.Port() == 0 || value.nodeName == "" {
		return errors.New("node bridge listen or node identity rejected")
	}
	receipt, err := protectedFile(value.qualification, 1<<20, true)
	if err != nil {
		return err
	}
	wanted, err := hex.DecodeString(value.qualificationSHA)
	digest := sha256.Sum256(receipt)
	if err != nil || len(wanted) != sha256.Size || subtle.ConstantTimeCompare(wanted, digest[:]) != 1 {
		return errors.New("node bridge runtime qualification receipt rejected")
	}
	certPEM, err := protectedFile(value.serverCert, 64<<10, false)
	if err != nil {
		return err
	}
	keyPEM, err := protectedFile(value.serverKey, 64<<10, true)
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	clear(keyPEM)
	if err != nil {
		return errors.New("node bridge TLS identity rejected")
	}
	clientCA, err := protectedFile(value.clientCA, 64<<10, false)
	if err != nil {
		return err
	}
	streamCA, err := protectedFile(value.streamCA, 64<<10, false)
	if err != nil {
		return err
	}
	serverTLS, err := vmnodebridge.ServerTLSConfig(cert, clientCA, value.nodeName)
	if err != nil {
		return err
	}
	runtime, conn, err := vmnodebridge.DialRuntime(value.criSocket, value.nodeName)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	streamTLS := vmnodebridge.StreamTLS{Origin: value.streamOrigin, ServerName: value.streamServerName, RootCAs: streamCA}
	bridge := &vmnodebridge.Server{Runtime: runtime, Streamer: vmnodebridge.WebSocketStreamer{TLS: streamTLS},
		StreamTLS: streamTLS, AllowedClientURI: "spiffe://oberth.ci/operator/vm-attach", Activated: true}
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", value.listen)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	server := &http.Server{Handler: bridge, TLSConfig: serverTLS, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 11 * time.Minute, WriteTimeout: 11 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ServeTLS(listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, parseOptions()); err != nil {
		// No runtime URL, candidate output, key bytes or bearer token is logged.
		_, _ = fmt.Fprintln(os.Stderr, "node bridge stopped or refused startup")
		os.Exit(1)
	}
}
