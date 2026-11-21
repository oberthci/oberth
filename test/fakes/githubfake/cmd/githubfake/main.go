// Command githubfake serves the inventoried upstream transports and controls.
// Spec: docs/plans/README.md#principles
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oberthci/oberth/test/fakes/githubfake"
	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
)

type addresses struct{ HTTPS, Control, SSH string }
type options struct {
	root, caOut, hostKeys, tlsHosts string
	addresses
	tls githubfake.TLSOptions
}

func parseFlags(args []string) (options, error) {
	var o options
	flags := flag.NewFlagSet("githubfake", flag.ContinueOnError)
	flags.StringVar(&o.root, "root", "", "required dedicated bare-repository directory")
	flags.StringVar(&o.HTTPS, "https", "127.0.0.1:8443", "HTTPS upstream listen address")
	flags.StringVar(&o.Control, "control", "127.0.0.1:8080", "loopback-only HTTP control listen address")
	flags.StringVar(&o.SSH, "ssh", "127.0.0.1:2222", "SSH Git listen address")
	flags.StringVar(&o.hostKeys, "host-key", "", "optional comma-separated private host-key files")
	flags.StringVar(&o.caOut, "ca-out", "", "required public CA output path")
	flags.StringVar(&o.tlsHosts, "tls-hosts", "localhost,127.0.0.1,::1", "comma-separated certificate hostnames")
	flags.StringVar(&o.tls.CACertFile, "ca-cert", "", "optional signing CA certificate")
	flags.StringVar(&o.tls.CAKeyFile, "ca-key", "", "optional signing CA private key")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	if flags.NArg() != 0 {
		return o, errors.New("unexpected positional arguments")
	}
	if o.root == "" || o.caOut == "" {
		return o, errors.New("-root and -ca-out are required")
	}
	o.tls.Hosts = strings.Split(o.tlsHosts, ",")
	return o, nil
}
func loadKeys(files string) ([]ssh.Signer, error) {
	var keys []ssh.Signer
	for _, path := range strings.FieldsFunc(files, func(r rune) bool { return r == ',' }) {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read host key %s: %w", path, err)
		}
		signer, err := ssh.ParsePrivateKey(body)
		clear(body)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		keys = append(keys, signer)
	}
	return keys, nil
}
func run(ctx context.Context, args []string, ready func(addresses), now func() time.Time) error {
	o, err := parseFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	keys, err := loadKeys(o.hostKeys)
	if err != nil {
		return err
	}
	fake, err := githubfake.New(githubfake.Config{Root: o.root, Now: now, HostKeys: keys, Logger: log.Default()})
	if err != nil {
		return err
	}
	o.tls.Now = now()
	cfg, ca, err := githubfake.ServerTLS(o.tls)
	if err != nil {
		return err
	}
	if err := os.WriteFile(o.caOut, ca, 0644); err != nil {
		return fmt.Errorf("write public CA %s: %w", o.caOut, err)
	}
	var listeners []net.Listener
	for _, address := range []string{o.HTTPS, o.Control, o.SSH} {
		l, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		defer l.Close()
		listeners = append(listeners, l)
	}
	if !listeners[1].Addr().(*net.TCPAddr).IP.IsLoopback() {
		return errors.New("control listener must bind a loopback address")
	}
	return (servers{fake, cfg, listeners}).serve(ctx, ready)
}

type servers struct {
	fake      *githubfake.Fake
	cfg       *tls.Config
	listeners []net.Listener
}

func (s servers) serve(ctx context.Context, ready func(addresses)) error {
	fake, cfg, listeners := s.fake, s.cfg, s.listeners
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	public := &http.Server{Handler: fake, TLSConfig: cfg, TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	control := &http.Server{Handler: fake.ControlHandler(), ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 3)
	go func() { done <- public.ServeTLS(listeners[0], "", "") }()
	go func() { done <- control.Serve(listeners[1]) }()
	go func() { done <- fake.ServeSSH(ctx, listeners[2]) }()
	if ready != nil {
		ready(addresses{listeners[0].Addr().String(), listeners[1].Addr().String(), listeners[2].Addr().String()})
	}
	stop := context.AfterFunc(ctx, func() { _ = public.Close(); _ = control.Close() })
	defer stop()
	result := <-done
	cancel()
	<-done
	<-done
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], func(a addresses) { log.Printf("fake listening: HTTPS %s control %s SSH %s", a.HTTPS, a.Control, a.SSH) }, clock.RealClock{}.Now); err != nil {
		log.Fatal(err)
	}
}
