package main

import (
	"errors"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func adminPathOwned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Geteuid())
}

func checkAdminPeer(connection net.Conn, allowedUID uint32) error {
	local, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.New("admin peer is not a Unix socket")
	}
	raw, err := local.SyscallConn()
	if err != nil {
		return err
	}
	var credential *unix.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		credential, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if peerErr != nil {
		return peerErr
	}
	if credential == nil || credential.Uid != allowedUID {
		return errors.New("admin peer UID does not match daemon UID")
	}
	return nil
}
