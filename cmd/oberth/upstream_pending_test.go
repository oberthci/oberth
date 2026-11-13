package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
)

func TestInstallPendingExitCode(t *testing.T) {
	if cliFailureExitCode(installer.ErrUpstreamKeyPending) != 3 || cliFailureExitCode(installer.ErrOnboardingPartial) != 1 || cliFailureExitCode(errors.New("install failed")) != 1 || cliFailureExitCode(errUsage) != 2 {
		t.Fatal("incorrect command exit classification")
	}
}

func TestUpstreamCheckReportsPendingUntilForgeAcceptsKey(t *testing.T) {
	hostPublic, hostPrivate, _ := testSSHIdentity(t)
	_, private, _ := testSSHIdentity(t)
	signer, err := ssh.ParsePrivateKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	var authorized atomic.Bool
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		if authorized.Load() {
			return nil, nil
		}
		return nil, errors.New("key not registered")
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			conn, _, requests, err := ssh.NewServerConn(raw, config)
			if err != nil {
				_ = raw.Close()
				continue
			}
			ssh.DiscardRequests(requests)
			_ = conn.Close()
		}
	}()
	dir := t.TempDir()
	keyPath, hostsPath := filepath.Join(dir, "key"), filepath.Join(dir, "hosts")
	if err := os.WriteFile(keyPath, private, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, []byte(knownhosts.Line([]string{listener.Addr().String()}, hostPublic)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	upstream := model.Upstream{Kind: "ssh", BaseURL: "ssh://git@" + listener.Addr().String() + "/acme"}
	if upstreamAuthStatus(t.Context(), upstream, "", keyPath, dir, hostsPath) != "pending: the forge has not accepted this deploy key" {
		t.Fatal("auth rejection did not report pending")
	}
	authorized.Store(true)
	if upstreamAuthStatus(t.Context(), upstream, "", keyPath, dir, hostsPath) != "ok" {
		t.Fatal("authorized key did not report verified authentication")
	}
}
