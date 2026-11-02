package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestAdminPeerCredentialsRequireDaemonUID(t *testing.T) {
	socket := privateAdminSocket(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := checkAdminPeer(peer, uint32(os.Geteuid())); err != nil {
		t.Fatalf("same UID denied: %v", err)
	}
	if err := checkAdminPeer(peer, uint32(os.Geteuid())+1); err == nil {
		t.Fatal("different UID accepted")
	}
}

func TestAdminSocketRejectsSharedAndSymlinkDirectories(t *testing.T) {
	socket := privateAdminSocket(t)
	directory := filepath.Dir(socket)
	if err := os.Chmod(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := listenAdminSocket(t.Context(), socket); err == nil {
		t.Fatal("shared directory accepted")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := listenAdminSocket(t.Context(), filepath.Join(link, "admin.sock")); err == nil {
		t.Fatal("symlink directory accepted")
	}
	listener, err := listenAdminSocket(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Lstat(socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions: %v, %v", info, err)
	}
}
