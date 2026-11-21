package main

import (
	"bytes"
	"context"

	"github.com/oberthci/oberth/internal/secretstore"
	"golang.org/x/crypto/ssh"
)

type administrator struct{ certificate, push ssh.PublicKey }

func readAdmin(certificate, push string) (administrator, error) {
	var keys administrator
	var err error
	keys.certificate, err = readAdminKey(certificate, "files.admin_public_key")
	if err != nil {
		return keys, err
	}
	keys.push, err = readAdminKey(push, "files.admin_push_key")
	return keys, err
}
func readAdminKey(path, field string) (ssh.PublicKey, error) {
	data, err := secretstore.ReadMountedFile(path, 64<<10, field)
	if err != nil {
		return nil, err
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, serverProblem("admin", field+": expected one public key", err)
	}
	if len(options) > 0 {
		return nil, serverProblem("admin", field+": key options are not allowed", nil)
	}
	if len(bytes.TrimSpace(rest)) > 0 {
		return nil, serverProblem("admin", field+": more than one key", nil)
	}
	return key, nil
}
func (keys administrator) install(ctx context.Context, install func(context.Context, []byte, []byte) error) error {
	if install == nil {
		return missingPart("service.InstallAdmin")
	}
	// C2's current InstallAdmin contract accepts normalized authorized-key bytes.
	return install(ctx, ssh.MarshalAuthorizedKey(keys.certificate), ssh.MarshalAuthorizedKey(keys.push))
}
