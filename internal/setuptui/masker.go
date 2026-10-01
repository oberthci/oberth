package setuptui

import (
	"strings"
	"sync"
)

// masker implements allowlist-shaped secret masking (S3). The wizard knows
// every secret it holds; values are registered with the masker before first
// use. Pattern-matching output is explicitly avoided — only known handles
// are masked.
//
// Secrets are held as []byte, not string, so wipe() can actually zero them
// (S2: a secret held in an immutable string can never be erased from the
// heap). mask() necessarily creates a transient string per comparison; that
// copy is short-lived garbage, unlike a map key that pins the value for the
// program's lifetime.
//
// All methods are goroutine-safe: register() and wipe() run on the TUI
// goroutine while mask() runs on the installer goroutine (via
// applyWriter.processLine).
type masker struct {
	mu      sync.Mutex
	secrets [][]byte
}

func newMasker() *masker {
	return &masker{}
}

// register adds a copy of a secret value to the masker. Empty values are
// ignored. The caller keeps ownership of its own buffer.
func (m *masker) register(secret []byte) {
	if len(secret) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(secret))
	copy(cp, secret)
	m.secrets = append(m.secrets, cp)
}

// mask replaces all registered secret values in s with "********".
func (m *masker) mask(s string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, secret := range m.secrets {
		if len(secret) == 0 {
			continue
		}
		s = strings.ReplaceAll(s, string(secret), "********")
	}
	return s
}

// wipe zeros every registered secret buffer (S2: teardown discipline).
// After wipe the masker is empty; it can be reused.
func (m *masker) wipe() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, secret := range m.secrets {
		for i := range secret {
			secret[i] = 0
		}
	}
	m.secrets = nil
}
