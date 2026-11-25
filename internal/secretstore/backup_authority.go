package secretstore

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

type backupGeneration struct {
	Nonce        string `json:"nonce"`
	OTP          string `json:"otp"`
	Required     int    `json:"required"`
	Complete     bool   `json:"complete"`
	EncodedToken string `json:"encoded_token"`
}

// SetupBackupWithUnseal uses the bundled store's single-share native quorum.
// No root session is supplied, printed or saved; received authority is revoked
// after the two backup resources are checked, including on setup failure.
func (c *Client) SetupBackupWithUnseal(ctx context.Context, cfg Config, share []byte) (err error) {
	if cfg.Validate() != nil || len(share) == 0 || len(share) > 64<<10 {
		return refuse("input", nil)
	}
	authority, err := c.backupAuthority(ctx, share)
	if err != nil {
		return err
	}
	defer clear(authority)
	defer func() { err = joinRevoke(ctx, c, authority, err) }()
	return c.SetupBackup(ctx, authority, cfg)
}

func (c *Client) backupAuthority(ctx context.Context, share []byte) ([]byte, error) {
	var started backupGeneration
	status, err := c.JSON(ctx, Request{Method: "PUT", Path: "/sys/generate-root/attempt", Body: map[string]string{}}, &started)
	if status == http.StatusMethodNotAllowed {
		return nil, refuse("backup-root-disabled", nil)
	}
	if err != nil || started.Nonce == "" || started.Required != 1 || len(started.OTP) < 16 || len(started.OTP) > 128 {
		return nil, refuse("backup-setup", nil)
	}
	pad := []byte(started.OTP)
	defer clear(pad)
	var completed backupGeneration
	_, err = c.JSON(ctx, Request{Method: "PUT", Path: "/sys/generate-root/update", Body: map[string]string{"key": string(share), "nonce": started.Nonce}}, &completed)
	if err != nil || !completed.Complete || completed.Nonce != started.Nonce {
		return nil, refuse("backup-setup", nil)
	}
	// The store issued a root token here. It encodes it without padding; a
	// padded form is accepted too, so the token is always recovered and revoked.
	encoded, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(completed.EncodedToken, "="))
	defer clear(encoded)
	if err != nil || len(encoded) != len(pad) {
		return nil, refuse("backup-setup", nil)
	}
	authority := make([]byte, len(pad))
	subtle.XORBytes(authority, encoded, pad)
	return authority, nil
}
