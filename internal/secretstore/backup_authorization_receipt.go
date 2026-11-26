package secretstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

type backupAuthorizationReceipt struct{ *os.File }

// The receipt precedes any issuing request. It contains only public operation
// metadata, so a crash cannot silently discard the need to revoke root access.
func (c *Client) beginBackupAuthorization(ctx context.Context, path string) (backupAuthorizationReceipt, int, error) {
	if err := CheckBackupAuthorization(path); err != nil {
		return backupAuthorizationReceipt{}, 0, err
	}
	var status backupGeneration
	code, err := c.JSON(ctx, Request{Method: http.MethodGet, Path: "/sys/generate-root/attempt"}, &status)
	if code == http.StatusMethodNotAllowed {
		return backupAuthorizationReceipt{}, 0, refuse("backup-root-disabled", nil)
	}
	if err != nil {
		return backupAuthorizationReceipt{}, 0, err
	}
	if status.Started || status.Nonce != "" {
		return backupAuthorizationReceipt{}, 0, pendingBackupAuthorization("")
	}
	if status.Required != 1 || status.OTPLength < 16 || status.OTPLength > 128 {
		return backupAuthorizationReceipt{}, 0, refuse("backup-setup", nil)
	}
	journal, err := c.backupReceipt(path)
	return journal, status.OTPLength, err
}

func (c *Client) backupReceipt(path string) (backupAuthorizationReceipt, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			return backupAuthorizationReceipt{}, pendingBackupAuthorization(path)
		}
		return backupAuthorizationReceipt{}, backupReceiptProblem(path)
	}
	journal := backupAuthorizationReceipt{file}
	if err = journal.record(c.address, ""); err != nil {
		_ = journal.remove()
		_ = file.Close()
		return backupAuthorizationReceipt{}, backupReceiptProblem(path)
	}
	return journal, nil
}

// CheckBackupAuthorization stops setup under either authority when an earlier
// quorum operation has no confirmed revocation. It reads no private material.
func CheckBackupAuthorization(path string) error {
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return pendingBackupAuthorization(path)
	}
	return nil
}

func (j backupAuthorizationReceipt) record(address, nonce string) error {
	if _, err := j.Seek(0, 0); err != nil {
		return err
	}
	err := json.NewEncoder(j).Encode(struct{ Store, Nonce string }{address, nonce})
	return errors.Join(err, j.Sync(), syncAuthorizationDirectory(j.Name()))
}

func (j backupAuthorizationReceipt) remove() error {
	before, err := j.Stat()
	after, statErr := os.Lstat(j.Name())
	if err != nil || statErr != nil || !os.SameFile(before, after) {
		return j.problem()
	}
	if err = os.Remove(j.Name()); err != nil {
		return j.problem()
	}
	return syncAuthorizationDirectory(j.Name())
}

func syncAuthorizationDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (j backupAuthorizationReceipt) problem() error { return pendingBackupAuthorization(j.Name()) }

func backupReceiptProblem(path string) error {
	reason, remedy := refusal("backup-receipt")
	err := publicerr.New(publicerr.InvalidInput, reason, remedy, nil)
	err.Context = []string{"Authorization receipt: " + path}
	err.Detail = "No root generation was started. Check directory permissions and free space; remove any failed receipt before retrying."
	return err
}

func pendingBackupAuthorization(path string) error {
	reason, remedy := refusal("backup-pending")
	err := publicerr.New(publicerr.Forbidden, reason, remedy, nil)
	if path != "" {
		err.Context = []string{"Pending authorization receipt: " + path}
		if info, statErr := os.Lstat(path); statErr == nil {
			err.Context = append(err.Context, "Receipt time: "+info.ModTime().UTC().Format(time.RFC3339))
		}
	}
	err.Detail = "With a separate store administrator session, LIST auth/token/accessors and POST auth/token/lookup-accessor with each accessor. Coordinate with other root-session owners, then POST auth/token/revoke-accessor for every other root-policy accessor. DELETE sys/generate-root/attempt cancels unfinished generation only. Revoke the cleanup session last with POST auth/token/revoke-self; remove the receipt and retry setup."
	return err
}
