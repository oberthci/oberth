package secretstore

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
)

type backupGeneration struct {
	Nonce        string `json:"nonce"`
	Started      bool   `json:"started"`
	OTPLength    int    `json:"otp_length"`
	Required     int    `json:"required"`
	Complete     bool   `json:"complete"`
	EncodedToken string `json:"encoded_token"`
}

type BackupAuthorization struct {
	Config  Config
	Share   []byte
	Receipt string
}

// SetupBackupWithUnseal uses the bundled store's single-share native quorum.
// No root session is supplied, printed or saved; received authority is revoked
// after the two backup resources are checked, including on setup failure.
func (c *Client) SetupBackupWithUnseal(ctx context.Context, request BackupAuthorization) (err error) {
	if request.Config.Validate() != nil || len(request.Share) == 0 || len(request.Share) > 64<<10 || request.Receipt == "" {
		return refuse("input", nil)
	}
	journal, length, err := c.beginBackupAuthorization(ctx, request.Receipt)
	if err != nil {
		return err
	}
	defer journal.Close()
	authority, err := c.backupAuthority(ctx, request.Share, journal, length)
	if err != nil {
		return journal.problem()
	}
	defer clear(authority)
	defer func() {
		revoke := joinRevoke(ctx, c, authority, nil)
		if revoke == nil {
			revoke = journal.remove()
		}
		err = errors.Join(err, revoke)
		if revoke != nil {
			err = errors.Join(err, journal.problem())
		}
	}()
	return c.SetupBackup(ctx, authority, request.Config)
}

func (c *Client) backupAuthority(ctx context.Context, share []byte, journal backupAuthorizationReceipt, length int) ([]byte, error) {
	var otp string
	for len(otp) < length {
		otp += rand.Text()
	}
	pad := []byte(otp[:length])
	defer clear(pad)
	var started backupGeneration
	_, err := c.JSON(ctx, Request{Method: http.MethodPut, Path: "/sys/generate-root/attempt", Body: map[string]string{"otp": string(pad)}}, &started)
	if err != nil || started.Nonce == "" || started.Required != 1 {
		return nil, refuse("backup-setup", nil)
	}
	if err = journal.record(c.address, started.Nonce); err != nil {
		return nil, err
	}
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
