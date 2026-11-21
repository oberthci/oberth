package sshserver

import (
	"net"
	"sync"
)

// The address count covers only SSH authentication; established sessions use slots.
func (s *Server) takeHandshake(address net.Addr) (func(), bool) {
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		host = address.String()
	}
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	if s.unauthenticated[host] >= s.config.UnauthenticatedPerPeer {
		return nil, false
	}
	s.unauthenticated[host]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.peerMu.Lock()
			defer s.peerMu.Unlock()
			s.unauthenticated[host]--
			if s.unauthenticated[host] == 0 {
				delete(s.unauthenticated, host)
			}
		})
	}, true
}
