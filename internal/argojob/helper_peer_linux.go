package argojob

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

const procFilesystem = 0x9fa0

// Runtime-injected execs have a parent outside the container PID namespace.
// Helper descendants, including orphaned or detached inspections, do not.
// Neither the request nor its environment supplies this authority.
func serverRerunPeer(conn net.Conn) bool {
	unix, ok := conn.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := unix.SyscallConn()
	if err != nil {
		return false
	}
	var peer *syscall.Ucred
	var credentialErr error
	err = raw.Control(func(fd uintptr) {
		peer, credentialErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil || credentialErr != nil || peer == nil || peer.Pid <= 0 || peer.Uid != uint32(os.Geteuid()) {
		return false
	}
	return peerParentZero(peer.Pid)
}

func peerParentZero(pid int32) bool {
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/stat", pid), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	var filesystem syscall.Statfs_t
	// An authored mount must not supply the caller's parent PID.
	if syscall.Fstatfs(int(f.Fd()), &filesystem) != nil || filesystem.Type != procFilesystem {
		return false
	}
	stat, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || !strings.HasPrefix(string(stat), strconv.Itoa(int(pid))+" (") {
		return false
	}
	// comm can contain spaces and parentheses. The final ')' ends it.
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(stat[end+1:]))
	return len(fields) > 1 && fields[1] == "0"
}
