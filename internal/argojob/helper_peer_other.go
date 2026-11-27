//go:build !linux

package argojob

import "net"

// Held-command reruns require the Linux Pod's kernel peer credentials.
func serverRerunPeer(net.Conn) bool { return false }
