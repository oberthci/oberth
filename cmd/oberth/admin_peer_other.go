//go:build !linux

package main

import (
	"errors"
	"net"
	"os"
)

func adminPathOwned(os.FileInfo) bool { return false }

func checkAdminPeer(net.Conn, uint32) error {
	return errors.New("admin peer credentials require Linux")
}
