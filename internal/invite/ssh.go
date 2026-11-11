package invite

import (
	"bytes"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// SSHFingerprint rejects private material, options and extra keys before a
// client sends any file contents, and applies the same rule at the server.
func SSHFingerprint(raw []byte) (string, error) {
	public, err := parseSSHKey(raw)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(public), nil
}

func parseSSHKey(raw []byte) (ssh.PublicKey, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 16<<10 || bytes.ContainsAny(raw, "\r\n") {
		return nil, errors.New("invalid SSH public key")
	}
	public, _, options, rest, err := ssh.ParseAuthorizedKey(raw)
	if err != nil || len(options) != 0 || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("invalid SSH public key")
	}
	if _, certificate := public.(*ssh.Certificate); certificate {
		return nil, errors.New("an SSH certificate is not a public key")
	}
	return public, nil
}
