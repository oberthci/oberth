package client

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"io/fs"
	"os"
	"path/filepath"
)

// ReservePrivate reserves one new owner-only regular file without writing payload bytes.
// Atomic writes replace this reservation with the published file.
func ReservePrivate(path string) error {
	root, err := directory(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := reservePrivate(root, filepath.Base(path))
	if err != nil {
		return err
	}
	return file.Close()
}

func reservePrivate(root *os.Root, name string) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fileFailure(filepath.Join(root.Name(), name), "write", err)
	}
	return file, nil
}

func removeOwned(root *os.Root, name string, written *os.File) error {
	identity, err := written.Stat()
	if err != nil {
		return err
	}
	current, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(identity, current) {
		return nil
	}
	return root.Remove(name)
}

// Invite retains the handoff before sending only its digest to the service.
// knownHosts is the installation's SSH host-key record the issuer holds; the
// handoff carries it so the joined profile pins the Git endpoint.
func (c *Client) Invite(ctx context.Context, args apicontract.UplinkInviteArgs, path, knownHosts string) (result model.Invitation, err error) {
	if !ValidHostKeys(knownHosts) {
		return result, failure("invite_hosts", nil)
	}
	root, err := directory(filepath.Dir(path), true)
	if err != nil {
		return result, err
	}
	defer root.Close()
	name := filepath.Base(path)
	written, err := reservePrivate(root, name)
	if err != nil {
		return result, err
	}
	retain := false
	defer func() {
		if !retain {
			err = errors.Join(err, removeOwned(root, name, written))
		}
		err = errors.Join(err, written.Close())
	}()
	random := make([]byte, base64.RawURLEncoding.DecodedLen(apicontract.InvitationSecretMinChars)+1)
	if _, err := rand.Read(random); err != nil {
		return result, failure("keys", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(random)
	args.SecretDigest = apicontract.SecretDigest(secret)
	pending := model.InvitationFile{Invitation: model.Invitation{Identity: args.Identity, Scope: args.Scope, ServerURL: args.ServerURL}, KnownHosts: knownHosts, Secret: secret}
	data, _ := json.Marshal(pending)
	published, err := replacePrivate(root, name, data)
	if published != nil {
		err = errors.Join(err, written.Close())
		written = published
	}
	if err != nil {
		return result, err
	}
	if err := c.Invoke(ctx, "uplink_invite", args, &result); err != nil {
		retain = IsOutcomeUnknown(err)
		return result, err
	}
	retain = true
	pending.Invitation = result
	data, _ = json.Marshal(pending)
	return result, writeClientFileAt(root, name, data, true)
}

// ActivateBearer writes a generated credential before its digest can be activated.
// The service callback must bind that digest atomically; no plaintext is passed to it.
func ActivateBearer(ctx context.Context, path string, activate func(context.Context, string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ReservePrivate(path); err != nil {
		return err
	}
	secret := rand.Text()
	if err := WritePrivate(path, []byte(secret)); err != nil {
		return err
	}
	return activate(ctx, apicontract.SecretDigest(secret))
}

func reserveSelection(base, name string) (*os.Root, string, error) {
	if _, err := ProfileDir(base, name); err != nil {
		return nil, "", err
	}
	root, err := directory(base, true)
	if err != nil {
		return nil, "", err
	}
	if info, err := root.Lstat("current"); err == nil && info.Mode()&os.ModeSymlink == 0 {
		_ = root.Close()
		return nil, "", fileFailure(filepath.Join(base, "current"), "profile", nil)
	}
	temp := ".current-" + rand.Text()
	if err = root.Symlink(name, temp); err != nil {
		_ = root.Close()
		return nil, "", fileFailure(filepath.Join(base, temp), "write", err)
	}
	return root, temp, nil
}
