package sourceplugin

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func readIdentity(path string) ([]byte, error) {
	// Projected tokens use relative symlinks within their mounted directory.
	// Root permits rotation through ..data and refuses links outside that directory.
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, identityFailure(path)
	}
	defer func() { _ = root.Close() }()
	// Non-blocking open lets the file-type check reject a FIFO before any read.
	f, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, identityFailure(path)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, identityFailure(path)
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		clear(b)
		return nil, identityFailure(path)
	}
	return b, nil
}
func (c *Client) bearer() (string, error) {
	token, err := readIdentity(c.tokenFile)
	if err != nil {
		return "", err
	}
	defer clear(token)
	value := strings.TrimSpace(string(token))
	if !validBearer(value) {
		return "", identityFailure(c.tokenFile)
	}
	return value, nil
}
func identityFailure(path string) error {
	return fmt.Errorf("%w Path: %s", failure(reasonIdentity), path)
}
func validBearer(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !bearerCharacter(r) {
			return false
		}
	}
	return true
}
func bearerCharacter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~+/=", r)
}
