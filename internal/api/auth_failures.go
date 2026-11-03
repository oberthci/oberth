package api

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

const authFailureLimit = 10
const authFailureWindow = time.Minute
const maxAuthFailurePeers = 4096

type authFailureBucket struct {
	count int
	until time.Time
}

type authFailureLimiter struct {
	mu    sync.Mutex
	peers map[string]authFailureBucket
}

// Only the transport peer is trusted. Forwarded headers are client input.
func authPeer(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	return addr.Unmap().String()
}

// Hash the complete bearer credential to keep independent principals behind a
// shared ingress separate without retaining token material. Malformed/missing
// credentials share their peer's anonymous bucket. Forwarded headers are ignored.
func authFailurePrincipal(request *http.Request) string {
	token, ok := bearerToken(request.Header.Values("Authorization"))
	if !ok {
		return authPeer(request) + ":anonymous"
	}
	sum := sha256.Sum256([]byte(token))
	return authPeer(request) + ":" + hex.EncodeToString(sum[:])
}

func (limiter *authFailureLimiter) check(peer string, failure bool) bool {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	now := time.Now()
	if limiter.peers == nil {
		limiter.peers = make(map[string]authFailureBucket)
	}
	bucket := limiter.peers[peer]
	if !now.Before(bucket.until) {
		bucket = authFailureBucket{until: now.Add(authFailureWindow)}
	}
	if bucket.count >= authFailureLimit {
		return true
	}
	if !failure {
		return false
	}
	if len(limiter.peers) >= maxAuthFailurePeers {
		for key, value := range limiter.peers {
			if !now.Before(value.until) {
				delete(limiter.peers, key)
			}
		}
		if _, exists := limiter.peers[peer]; !exists && len(limiter.peers) >= maxAuthFailurePeers {
			// Capacity pressure must not turn unrelated failures into a global
			// lockout. Replace the bucket nearest expiry, retaining the bound.
			var oldest string
			var until time.Time
			for key, value := range limiter.peers {
				if oldest == "" || value.until.Before(until) {
					oldest, until = key, value.until
				}
			}
			delete(limiter.peers, oldest)
		}
	}
	bucket.count++
	limiter.peers[peer] = bucket
	return false
}

func (server *Server) logAuthFailure(request *http.Request, reason string) {
	// Fixed reasons and a parsed IP only: credentials, headers, URLs and
	// backend error strings never enter authentication failure logs.
	log.Printf("api: authentication failure: reason=%s peer=%s", reason, authPeer(request))
}

func (server *Server) recordAuthFailure(writer http.ResponseWriter, request *http.Request, reason string) bool {
	if server.authFailures.check(authFailurePrincipal(request), true) {
		writeAuthRateLimit(writer)
		return true
	}
	server.logAuthFailure(request, reason)
	return false
}

func writeAuthRateLimit(writer http.ResponseWriter) {
	writer.Header().Set("Retry-After", "60")
	writeJSON(writer, http.StatusTooManyRequests, map[string]string{"error": "too many authentication failures; retry later"})
}
